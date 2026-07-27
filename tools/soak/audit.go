package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xxxsen/common/database"
)

var (
	errAuditInvariant       = errors.New("soak audit invariant failed")
	errInvalidCacheFileName = errors.New("invalid soak L2 cache filename")
	outputMutex             sync.Mutex
)

type auditResult struct {
	changeJournalRows int64
	deleteStateRows   int64
	databaseBytes     int64
	integrityChecked  bool
	cacheFiles        int64
	cacheBytes        int64
	cacheChargedBytes int64
	cacheTempFiles    int64
}

type cacheAuditResult struct {
	files        int64
	bytes        int64
	chargedBytes int64
	tempFiles    int64
}

type l2CacheInspector struct {
	cacheDir    string
	managedRoot string
	seenKeys    map[string]string
	result      cacheAuditResult
}

type trackedResources struct {
	links           []string
	linkDirectories []string
	uploads         map[string]string
	keys            []string
}

func (r *soakRunner) cleanupTrackedResources(ctx context.Context) error {
	tracked := r.snapshotTrackedResources()
	linkErr := r.cleanupTrackedLinks(ctx, tracked.links)
	var directoryErr error
	if linkErr == nil {
		directoryErr = r.cleanupTrackedLinkDirectories(ctx, tracked.linkDirectories)
	}
	cleanupErr := errors.Join(
		linkErr,
		directoryErr,
		r.cleanupTrackedUploads(ctx, tracked.uploads),
		r.cleanupTrackedKeys(ctx, tracked.keys),
	)
	if cleanupErr != nil {
		return cleanupErr
	}
	return r.waitForDeletionDrain(ctx)
}

func (r *soakRunner) snapshotTrackedResources() trackedResources {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	tracked := trackedResources{
		links:           make([]string, 0, len(r.activeLinks)),
		linkDirectories: make([]string, 0, len(r.activeLinkDirs)),
		uploads:         make(map[string]string, len(r.activeUpload)),
		keys:            make([]string, 0, len(r.activeKeys)),
	}
	for link := range r.activeLinks {
		tracked.links = append(tracked.links, link)
	}
	for link := range r.activeLinkDirs {
		tracked.linkDirectories = append(tracked.linkDirectories, link)
	}
	for uploadID, key := range r.activeUpload {
		tracked.uploads[uploadID] = key
	}
	for key := range r.activeKeys {
		tracked.keys = append(tracked.keys, key)
	}
	return tracked
}

func (r *soakRunner) cleanupTrackedLinks(ctx context.Context, links []string) error {
	var cleanupErr error
	for _, link := range links {
		if err := r.manager.RemoveFileLink(ctx, link); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("remove tracked direct link %s: %w", link, err),
			)
			continue
		}
		r.untrackLink(link)
	}
	return cleanupErr
}

func (r *soakRunner) cleanupTrackedLinkDirectories(ctx context.Context, links []string) error {
	sort.Slice(links, func(left, right int) bool {
		return strings.Count(links[left], "/") > strings.Count(links[right], "/")
	})
	var cleanupErr error
	for _, link := range links {
		if err := r.manager.RemoveFileLink(ctx, link); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("remove tracked direct-link directory %s: %w", link, err),
			)
			continue
		}
		r.activeMu.Lock()
		delete(r.activeLinkDirs, link)
		r.activeMu.Unlock()
	}
	return cleanupErr
}

func (r *soakRunner) cleanupTrackedUploads(ctx context.Context, uploads map[string]string) error {
	var cleanupErr error
	for uploadID, key := range uploads {
		result, err := r.doS3(ctx, "DELETE", key, "uploadId="+uploadID, nil, nil)
		if err == nil {
			err = expectStatus(result, 204, 404)
		}
		if err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("abort tracked multipart %s: %w", uploadID, err),
			)
			continue
		}
		r.untrackUpload(uploadID)
	}
	return cleanupErr
}

func (r *soakRunner) cleanupTrackedKeys(ctx context.Context, keys []string) error {
	var cleanupErr error
	for _, key := range keys {
		result, err := r.doS3(ctx, "DELETE", key, "", nil, nil)
		if err == nil {
			err = expectStatus(result, 204)
		}
		if err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("delete tracked key %s: %w", key, err),
			)
			continue
		}
		r.untrackKey(key)
	}
	return cleanupErr
}

func (r *soakRunner) waitForDeletionDrain(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		nonTerminal, err := queryInt64(
			ctx,
			r.database,
			`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE delete_state NOT IN ('deleted', 'expired')`,
		)
		if err != nil {
			return err
		}
		blockFiles, err := countDirectoryFiles(r.blockDir)
		if err != nil {
			return err
		}
		if nonTerminal == 0 && blockFiles == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"%w: deletion drain has %d states and %d blocks: %w",
				errAuditInvariant,
				nonTerminal,
				blockFiles,
				ctx.Err(),
			)
		case <-ticker.C:
		}
	}
}

func (r *soakRunner) audit(ctx context.Context) (auditResult, error) {
	result := auditResult{}
	integrity, err := queryString(ctx, r.database, "PRAGMA integrity_check")
	if err != nil {
		return result, err
	}
	if integrity != "ok" {
		return result, fmt.Errorf("%w: SQLite integrity is %q", errAuditInvariant, integrity)
	}
	result.integrityChecked = true
	if err := r.auditZeroCounts(ctx); err != nil {
		return result, err
	}
	if err := r.auditEmptyStorage(); err != nil {
		return result, err
	}
	cacheAudit, err := inspectL2CacheDirectory(r.cacheDir, soakL2CacheSize)
	result.cacheFiles = cacheAudit.files
	result.cacheBytes = cacheAudit.bytes
	result.cacheChargedBytes = cacheAudit.chargedBytes
	result.cacheTempFiles = cacheAudit.tempFiles
	if err != nil {
		return result, err
	}
	if result.cacheFiles == 0 {
		return result, fmt.Errorf("%w: L2 cache contains no managed entries", errAuditInvariant)
	}
	result.changeJournalRows, err = queryInt64(
		ctx,
		r.database,
		"SELECT COUNT(*) FROM tg_webdav_change_tab",
	)
	if err != nil {
		return result, err
	}
	result.deleteStateRows, err = queryInt64(
		ctx,
		r.database,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab",
	)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(r.blockDir), "soak.db"))
	if err != nil {
		return result, fmt.Errorf("stat soak database: %w", err)
	}
	result.databaseBytes = info.Size()
	return result, nil
}

func (r *soakRunner) auditZeroCounts(ctx context.Context) error {
	checks := []struct {
		name  string
		query string
	}{
		{
			name:  "remaining object mappings",
			query: `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE file_name NOT IN ('/', 'soak')`,
		},
		{
			name: "orphan WebDAV properties",
			query: `SELECT COUNT(*) FROM tg_webdav_property_tab p
LEFT JOIN tg_file_mapping_tab m ON m.entry_id = p.entry_id
WHERE m.entry_id IS NULL`,
		},
		{
			name: "orphan WebDAV locks",
			query: `SELECT COUNT(*) FROM tg_webdav_lock_tab l
LEFT JOIN tg_file_mapping_tab m ON m.entry_id = l.root_entry_id
WHERE l.root_entry_id != 0 AND m.entry_id IS NULL`,
		},
		{
			name:  "remaining WebDAV locks",
			query: `SELECT COUNT(*) FROM tg_webdav_lock_tab`,
		},
		{
			name: "orphan S3 metadata",
			query: `SELECT COUNT(*) FROM tg_s3_object_metadata_tab s
LEFT JOIN tg_file_mapping_tab m ON m.entry_id = s.entry_id
WHERE m.entry_id IS NULL`,
		},
		{
			name:  "remaining S3 metadata",
			query: `SELECT COUNT(*) FROM tg_s3_object_metadata_tab`,
		},
		{
			name:  "active multipart uploads",
			query: `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab WHERE upload_state = 'active'`,
		},
		{
			name: `non-terminal delete states`,
			query: `SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE delete_state NOT IN ('deleted', 'expired')`,
		},
	}
	for _, check := range checks {
		count, err := queryInt64(ctx, r.database, check.query)
		if err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf(
				"%w: %s = %d",
				errAuditInvariant,
				check.name,
				count,
			)
		}
	}
	return nil
}

func (r *soakRunner) auditEmptyStorage() error {
	blockFiles, err := countDirectoryFiles(r.blockDir)
	if err != nil {
		return err
	}
	if blockFiles != 0 {
		return fmt.Errorf("%w: local blocks = %d", errAuditInvariant, blockFiles)
	}
	spoolFiles, err := countDirectoryFiles(r.spoolDir)
	if err != nil {
		return err
	}
	if spoolFiles != 0 {
		return fmt.Errorf("%w: spool files = %d", errAuditInvariant, spoolFiles)
	}
	return nil
}

func queryInt64(
	ctx context.Context,
	databaseClient database.IQueryer,
	query string,
) (int64, error) {
	return queryScalar[int64](ctx, databaseClient, query)
}

func queryString(
	ctx context.Context,
	databaseClient database.IQueryer,
	query string,
) (string, error) {
	return queryScalar[string](ctx, databaseClient, query)
}

func queryScalar[T int64 | string](
	ctx context.Context,
	databaseClient database.IQueryer,
	query string,
) (T, error) {
	var value T
	rows, err := databaseClient.QueryContext(ctx, query)
	if err != nil {
		return value, fmt.Errorf("execute soak audit query: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return value, fmt.Errorf("read soak audit result: %w", err)
		}
		return value, fmt.Errorf("%w: audit query returned no row", errAuditInvariant)
	}
	if err := rows.Scan(&value); err != nil {
		return value, fmt.Errorf("scan soak audit scalar: %w", err)
	}
	if err := rows.Err(); err != nil {
		return value, fmt.Errorf("finish soak audit scalar: %w", err)
	}
	return value, nil
}

func countDirectoryFiles(directory string) (int64, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read soak directory %s: %w", directory, err)
	}
	var count int64
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}
	return count, nil
}

func inspectL2CacheDirectory(cacheDir string, maxBytes int64) (cacheAuditResult, error) {
	inspector := &l2CacheInspector{
		cacheDir:    cacheDir,
		managedRoot: filepath.Join(cacheDir, "v2"),
		seenKeys:    make(map[string]string),
	}
	err := filepath.WalkDir(cacheDir, inspector.visit)
	if errors.Is(err, os.ErrNotExist) {
		return inspector.result, fmt.Errorf("%w: L2 cache directory is missing", errAuditInvariant)
	}
	if err != nil {
		return inspector.result, fmt.Errorf("walk L2 cache directory: %w", err)
	}
	if inspector.result.tempFiles != 0 {
		return inspector.result, fmt.Errorf(
			"%w: L2 cache temp files = %d",
			errAuditInvariant,
			inspector.result.tempFiles,
		)
	}
	if inspector.result.bytes > maxBytes {
		return inspector.result, fmt.Errorf(
			"%w: L2 cache bytes = %d, limit = %d",
			errAuditInvariant,
			inspector.result.bytes,
			maxBytes,
		)
	}
	if inspector.result.chargedBytes > maxBytes {
		return inspector.result, fmt.Errorf(
			"%w: L2 cache charged bytes = %d, limit = %d",
			errAuditInvariant,
			inspector.result.chargedBytes,
			maxBytes,
		)
	}
	return inspector.result, nil
}

func (i *l2CacheInspector) visit(path string, entry os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return fmt.Errorf("visit L2 cache path: %w", walkErr)
	}
	if entry.Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: L2 cache contains symlink %s", errAuditInvariant, path)
	}
	if entry.IsDir() {
		return auditCacheDirectoryLocation(i.cacheDir, i.managedRoot, path)
	}
	return i.visitFile(path, entry)
}

func (i *l2CacheInspector) visitFile(path string, entry os.DirEntry) error {
	if strings.HasSuffix(entry.Name(), ".temp") {
		i.result.tempFiles++
		return nil
	}
	if path == filepath.Join(i.managedRoot, ".manifest") ||
		path == filepath.Join(i.managedRoot, ".lock") {
		return auditRegularCacheFile(entry, path)
	}
	if !strings.HasSuffix(entry.Name(), ".cache") {
		return fmt.Errorf("%w: unexpected L2 cache file %s", errAuditInvariant, path)
	}
	return i.auditEntry(path, entry)
}

func (i *l2CacheInspector) auditEntry(path string, entry os.DirEntry) error {
	key, size, digest, err := parseSoakCacheFileName(entry.Name())
	if err != nil {
		return errors.Join(errAuditInvariant, fmt.Errorf("parse L2 cache entry %s: %w", path, err))
	}
	if filepath.Base(filepath.Dir(path)) != key[:2] ||
		filepath.Dir(filepath.Dir(path)) != i.managedRoot {
		return fmt.Errorf("%w: invalid L2 cache shard for %s", errAuditInvariant, path)
	}
	if previous, exists := i.seenKeys[key]; exists {
		return fmt.Errorf(
			"%w: duplicate L2 cache key in %s and %s",
			errAuditInvariant,
			previous,
			path,
		)
	}
	i.seenKeys[key] = path
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("inspect L2 cache entry %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() != size ||
		size <= soakL1KeySizeLimit || size > soakL2KeySizeLimit {
		return fmt.Errorf("%w: invalid L2 cache entry size for %s", errAuditInvariant, path)
	}
	actualDigest, err := hashSoakCacheFile(path)
	if err != nil {
		return err
	}
	if actualDigest != digest {
		return fmt.Errorf("%w: L2 cache digest mismatch for %s", errAuditInvariant, path)
	}
	i.result.files++
	i.result.bytes += size
	i.result.chargedBytes += soakCacheEntryCost(size)
	return nil
}

func soakCacheEntryCost(size int64) int64 {
	logical := max(size, int64(1))
	remainder := logical % diskCacheAllocationUnit
	if remainder == 0 {
		return logical
	}
	return logical + diskCacheAllocationUnit - remainder
}

func auditCacheDirectoryLocation(cacheDir, managedRoot, location string) error {
	if location == cacheDir || location == managedRoot {
		return nil
	}
	if filepath.Dir(location) != managedRoot || !isLowerHex(filepath.Base(location), 2) {
		return fmt.Errorf("%w: unexpected L2 cache directory %s", errAuditInvariant, location)
	}
	return nil
}

func auditRegularCacheFile(entry os.DirEntry, path string) error {
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("inspect L2 cache metadata file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: invalid L2 cache metadata file %s", errAuditInvariant, path)
	}
	return nil
}

func parseSoakCacheFileName(name string) (string, int64, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	base, found := strings.CutSuffix(name, ".cache")
	if !found {
		return "", 0, digest, fmt.Errorf("%w: suffix", errInvalidCacheFileName)
	}
	parts := strings.Split(base, ".")
	if len(parts) != 4 || !isLowerHex(parts[0], sha256.Size*2) {
		return "", 0, digest, fmt.Errorf("%w: key", errInvalidCacheFileName)
	}
	generation, err := uuid.Parse(parts[1])
	if err != nil || generation.String() != parts[1] {
		return "", 0, digest, fmt.Errorf("%w: generation", errInvalidCacheFileName)
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != parts[2] {
		return "", 0, digest, fmt.Errorf("%w: size", errInvalidCacheFileName)
	}
	if !isLowerHex(parts[3], sha256.Size*2) {
		return "", 0, digest, fmt.Errorf("%w: digest", errInvalidCacheFileName)
	}
	rawDigest, _ := hex.DecodeString(parts[3])
	copy(digest[:], rawDigest)
	return parts[0], size, digest, nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hashSoakCacheFile(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open L2 cache entry %s: %w", path, err)
	}
	defer func() {
		_ = file.Close()
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash L2 cache entry %s: %w", path, err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func countOpenFileHandles() int64 {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return int64(len(entries))
}

func writeJSON(value any) error {
	outputMutex.Lock()
	defer outputMutex.Unlock()
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		return fmt.Errorf("write soak output: %w", err)
	}
	return nil
}
