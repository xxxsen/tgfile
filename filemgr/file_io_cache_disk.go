package filemgr

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

const (
	diskCacheFormatVersion  = 2
	diskCacheDirName        = "v2"
	diskCacheManifestName   = ".manifest"
	diskCacheLockName       = ".lock"
	diskCacheFileSuffix     = ".cache"
	diskCacheTempSuffix     = ".temp"
	diskCopyBufferSize      = 64 * 1024
	diskCacheAllocationUnit = int64(4 * 1024)
	legacyCacheDelimiter    = "#"
)

var (
	errCacheLocalIO    = errors.New("local cache I/O failed")
	errCacheSourceRead = errors.New("cache source read failed")
)

type diskCacheManifest struct {
	Version        int    `json:"version"`
	StorageBinding string `json:"storage_binding"`
}

type diskEntry struct {
	key        fileCacheKey
	generation string
	path       string
	size       int64
	cost       int64
	digest     [sha256.Size]byte
	modTime    int64
	fileInfo   os.FileInfo
	refs       int
	retired    bool
}

type diskCache struct {
	ctx      context.Context
	root     string
	maxCost  int64
	keyLimit int64
	minSize  int64
	lock     cacheDirectoryLock

	mu       sync.Mutex
	entries  map[fileCacheKey]*list.Element
	lru      *list.List
	usedCost int64
	closing  bool
	closed   bool
	orphans  map[string]struct{}
	readers  sync.WaitGroup
	stats    *fileIOCacheStats
	warnings *cacheWarningLimiter
}

type diskCacheReader struct {
	*os.File
	cache *diskCache
	entry *diskEntry
	once  sync.Once
}

func (r *diskCacheReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		closeErr = r.File.Close()
		r.cache.release(r.entry)
	})
	if closeErr != nil {
		return fmt.Errorf("close L2 cache reader: %w", closeErr)
	}
	return nil
}

type removeOnCloseFile struct {
	*os.File
	cache    *diskCache
	location string
	once     sync.Once
}

func (f *removeOnCloseFile) Close() error {
	var result error
	f.once.Do(func() {
		result = f.File.Close()
		f.cache.cleanupPaths([]string{f.location})
	})
	if result != nil {
		return fmt.Errorf("close rejected L2 cache candidate: %w", result)
	}
	return nil
}

func newDiskCache(
	ctx context.Context,
	baseDir string,
	maxCost, keyLimit, minSize int64,
	binding [sha256.Size]byte,
	stats *fileIOCacheStats,
	warnings *cacheWarningLimiter,
) (*diskCache, error) {
	root := filepath.Join(baseDir, diskCacheDirName)
	if err := ensureManagedDirectory(root); err != nil {
		return nil, err
	}
	lock, err := acquireCacheDirectoryLock(filepath.Join(root, diskCacheLockName))
	if err != nil {
		return nil, err
	}
	cache := &diskCache{
		ctx:      ctx,
		root:     root,
		maxCost:  maxCost,
		keyLimit: keyLimit,
		minSize:  minSize,
		lock:     lock,
		entries:  make(map[fileCacheKey]*list.Element),
		lru:      list.New(),
		orphans:  make(map[string]struct{}),
		stats:    stats,
		warnings: warnings,
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = lock.Close()
		}
	}()
	if err := cache.reconcileManifest(binding); err != nil {
		return nil, err
	}
	if err := cache.removeLegacyEntries(baseDir); err != nil {
		return nil, err
	}
	if err := cache.recoverEntries(); err != nil {
		return nil, err
	}
	initialized = true
	return cache, nil
}

func ensureManagedDirectory(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("%w: create L2 managed directory: %w", errCacheLocalIO, err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return fmt.Errorf("%w: inspect L2 managed directory: %w", errCacheLocalIO, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: L2 managed path is not a directory", ErrInvalidCachePath)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("%w: secure L2 managed directory: %w", errCacheLocalIO, err)
	}
	return nil
}

func (d *diskCache) reconcileManifest(binding [sha256.Size]byte) error {
	expected := diskCacheManifest{
		Version:        diskCacheFormatVersion,
		StorageBinding: hex.EncodeToString(binding[:]),
	}
	manifestPath := filepath.Join(d.root, diskCacheManifestName)
	current, err := readDiskCacheManifest(manifestPath)
	if err == nil && current == expected {
		return nil
	}
	if err := d.removeManagedEntries(); err != nil {
		return fmt.Errorf("clear incompatible L2 entries: %w", err)
	}
	return writeDiskCacheManifest(manifestPath, expected)
}

func readDiskCacheManifest(path string) (diskCacheManifest, error) {
	var manifest diskCacheManifest
	info, err := os.Lstat(path)
	if err != nil {
		return manifest, fmt.Errorf("inspect L2 manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return manifest, ErrInvalidCachePath
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return manifest, fmt.Errorf("secure L2 manifest: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest, fmt.Errorf("read L2 manifest: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return diskCacheManifest{}, fmt.Errorf("decode L2 manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return diskCacheManifest{}, fmt.Errorf("decode L2 manifest trailer: %w", errors.Join(ErrInvalidCachePath, err))
	}
	if manifest.Version != diskCacheFormatVersion || len(manifest.StorageBinding) != sha256.Size*2 {
		return diskCacheManifest{}, ErrInvalidCachePath
	}
	if _, err := hex.DecodeString(manifest.StorageBinding); err != nil {
		return diskCacheManifest{}, fmt.Errorf("decode L2 manifest binding: %w", err)
	}
	return manifest, nil
}

func writeDiskCacheManifest(path string, manifest diskCacheManifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode L2 manifest: %w", err)
	}
	temporary := path + "." + uuid.NewString() + diskCacheTempSuffix
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create L2 manifest temp: %w", errCacheLocalIO, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: write L2 manifest: %w", errCacheLocalIO, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: sync L2 manifest: %w", errCacheLocalIO, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w: close L2 manifest: %w", errCacheLocalIO, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: replace L2 manifest: %w", errCacheLocalIO, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("%w: publish L2 manifest: %w", errCacheLocalIO, err)
	}
	cleanup = false
	return nil
}

func (d *diskCache) removeManagedEntries() error {
	err := filepath.WalkDir(d.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == d.root || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.Name() == diskCacheLockName || entry.Name() == diskCacheManifestName {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), diskCacheFileSuffix) &&
			!strings.HasSuffix(entry.Name(), diskCacheTempSuffix) {
			return nil
		}
		return removeManagedFile(path)
	})
	if err != nil {
		return fmt.Errorf("walk managed L2 entries: %w", err)
	}
	return nil
}

func (d *diskCache) removeLegacyEntries(baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return fmt.Errorf("read L2 base directory: %w", err)
	}
	for _, shard := range entries {
		if !shard.IsDir() || !isHexPrefix(shard.Name()) || shard.Name() == diskCacheDirName {
			continue
		}
		shardPath := filepath.Join(baseDir, shard.Name())
		files, err := os.ReadDir(shardPath)
		if err != nil {
			return fmt.Errorf("read legacy L2 shard: %w", err)
		}
		for _, file := range files {
			if file.IsDir() || file.Type()&os.ModeSymlink != 0 || !isLegacyCacheFileName(file.Name()) {
				continue
			}
			if err := removeManagedFile(filepath.Join(shardPath, file.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func isHexPrefix(value string) bool {
	if len(value) != 2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func isLegacyCacheFileName(name string) bool {
	candidate := name
	if strings.HasSuffix(candidate, diskCacheTempSuffix) {
		candidate = strings.TrimSuffix(candidate, diskCacheTempSuffix)
		lastDot := strings.LastIndex(candidate, ".")
		if lastDot < 0 {
			return false
		}
		if _, err := uuid.Parse(candidate[lastDot+1:]); err != nil {
			return false
		}
		candidate = candidate[:lastDot]
	}
	base, found := strings.CutSuffix(candidate, diskCacheFileSuffix)
	if !found {
		return false
	}
	sizeText, fileIDText, found := strings.Cut(base, legacyCacheDelimiter)
	if !found {
		return false
	}
	size, sizeErr := strconv.ParseInt(sizeText, 10, 64)
	_, fileIDErr := strconv.ParseUint(fileIDText, 10, 64)
	return sizeErr == nil && fileIDErr == nil && size >= 0
}

func (d *diskCache) recoverEntries() error {
	winners := make(map[fileCacheKey]*diskEntry)
	err := filepath.WalkDir(d.root, func(path string, dirEntry os.DirEntry, walkErr error) error {
		return d.recoverPath(path, dirEntry, walkErr, winners)
	})
	if err != nil {
		return fmt.Errorf("recover L2 cache: %w", err)
	}
	ordered := make([]*diskEntry, 0, len(winners))
	for _, entry := range winners {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].modTime == ordered[right].modTime {
			return ordered[left].generation < ordered[right].generation
		}
		return ordered[left].modTime < ordered[right].modTime
	})
	for _, entry := range ordered {
		if !d.admit(entry) {
			if err := removeManagedFile(entry.path); err != nil {
				return fmt.Errorf("remove over-budget L2 cache entry: %w", err)
			}
		}
	}
	return nil
}

func (d *diskCache) recoverPath(
	path string,
	dirEntry os.DirEntry,
	walkErr error,
	winners map[fileCacheKey]*diskEntry,
) error {
	if walkErr != nil {
		return fmt.Errorf("visit L2 cache entry: %w", walkErr)
	}
	if path == d.root || dirEntry.IsDir() || dirEntry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	if strings.HasSuffix(dirEntry.Name(), diskCacheTempSuffix) {
		return removeManagedFile(path)
	}
	if !strings.HasSuffix(dirEntry.Name(), diskCacheFileSuffix) {
		return nil
	}
	entry, err := d.validatePersistedEntry(path, dirEntry)
	if err != nil {
		d.stats.invalidPersisted.Add(1)
		d.warn("discard invalid L2 cache entry", err)
		return removeManagedFile(path)
	}
	return updateRecoveryWinner(winners, entry)
}

func updateRecoveryWinner(winners map[fileCacheKey]*diskEntry, entry *diskEntry) error {
	previous, exists := winners[entry.key]
	if !exists {
		winners[entry.key] = entry
		return nil
	}
	if entry.generation <= previous.generation {
		return removeManagedFile(entry.path)
	}
	if err := removeManagedFile(previous.path); err != nil {
		return fmt.Errorf("remove duplicate L2 cache entry: %w", err)
	}
	winners[entry.key] = entry
	return nil
}

func (d *diskCache) validatePersistedEntry(path string, dirEntry os.DirEntry) (*diskEntry, error) {
	entry, err := d.parsePersistedEntryLocation(path, dirEntry.Name())
	if err != nil {
		return nil, err
	}
	if entry.size < 0 || entry.size <= d.minSize ||
		entry.size > d.keyLimit || entry.size > d.maxCost {
		return nil, ErrInvalidCache
	}
	entry.cost = diskCacheEntryCost(entry.size, d.maxCost)
	info, err := dirEntry.Info()
	if err != nil {
		return nil, fmt.Errorf("inspect L2 cache entry: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != entry.size {
		return nil, ErrInvalidCachePath
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secure L2 cache shard: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure L2 cache entry: %w", err)
	}
	digest, err := hashFile(path)
	if err != nil || digest != entry.digest {
		return nil, errors.Join(ErrInvalidCachePath, err)
	}
	entry.path = path
	entry.modTime = info.ModTime().UnixNano()
	entry.fileInfo = info
	return entry, nil
}

func (d *diskCache) parsePersistedEntryLocation(path, name string) (*diskEntry, error) {
	relative, err := filepath.Rel(d.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return nil, errors.Join(ErrInvalidCachePath, err)
	}
	parts := strings.Split(relative, string(os.PathSeparator))
	if len(parts) != 2 || !isHexPrefix(parts[0]) {
		return nil, ErrInvalidCachePath
	}
	entry, err := parseDiskCacheFileName(name)
	if err != nil || parts[0] != entry.key.hex()[:2] {
		return nil, errors.Join(ErrInvalidCachePath, err)
	}
	return entry, nil
}

func parseDiskCacheFileName(name string) (*diskEntry, error) {
	base, found := strings.CutSuffix(name, diskCacheFileSuffix)
	if !found {
		return nil, ErrInvalidCachePath
	}
	parts := strings.Split(base, ".")
	if len(parts) != 4 || len(parts[0]) != sha256.Size*2 || len(parts[3]) != sha256.Size*2 {
		return nil, ErrInvalidCachePath
	}
	keyRaw, err := hex.DecodeString(parts[0])
	if err != nil || hex.EncodeToString(keyRaw) != parts[0] {
		return nil, ErrInvalidCachePath
	}
	generation, err := uuid.Parse(parts[1])
	if err != nil || generation.String() != parts[1] {
		return nil, ErrInvalidCachePath
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size < 0 {
		return nil, ErrInvalidCachePath
	}
	digestRaw, err := hex.DecodeString(parts[3])
	if err != nil || hex.EncodeToString(digestRaw) != parts[3] {
		return nil, ErrInvalidCachePath
	}
	entry := &diskEntry{generation: parts[1], size: size}
	copy(entry.key[:], keyRaw)
	copy(entry.digest[:], digestRaw)
	return entry, nil
}

func hashFile(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open L2 cache entry for hashing: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash L2 cache entry: %w", err)
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func (d *diskCache) writeCandidate(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	source io.Reader,
) (*diskEntry, error) {
	shard := filepath.Join(d.root, key.hex()[:2])
	if err := ensureManagedDirectory(shard); err != nil {
		return nil, err
	}
	generation := uuid.NewString()
	temporary := filepath.Join(shard, "."+generation+diskCacheTempSuffix)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: create L2 candidate: %w", errCacheLocalIO, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	digest, copyErr := copyExactToFile(ctx, file, source, size)
	if copyErr != nil {
		_ = file.Close()
		return nil, copyErr
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: sync L2 candidate: %w", errCacheLocalIO, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("%w: close L2 candidate: %w", errCacheLocalIO, err)
	}
	filename := strings.Join([]string{
		key.hex(),
		generation,
		strconv.FormatInt(size, 10),
		hex.EncodeToString(digest[:]),
	}, ".") + diskCacheFileSuffix
	location := filepath.Join(shard, filename)
	if _, err := os.Lstat(location); !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: L2 candidate destination already exists", errCacheLocalIO)
	}
	if err := os.Rename(temporary, location); err != nil {
		return nil, fmt.Errorf("%w: publish L2 candidate: %w", errCacheLocalIO, err)
	}
	cleanup = false
	info, err := os.Lstat(location)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		_ = os.Remove(location)
		return nil, fmt.Errorf("%w: verify published L2 candidate", errCacheLocalIO)
	}
	return &diskEntry{
		key:        key,
		generation: generation,
		path:       location,
		size:       size,
		cost:       diskCacheEntryCost(size, d.maxCost),
		digest:     digest,
		modTime:    info.ModTime().UnixNano(),
		fileInfo:   info,
	}, nil
}

func copyExactToFile(
	ctx context.Context,
	destination *os.File,
	source io.Reader,
	expected int64,
) ([sha256.Size]byte, error) {
	hash := sha256.New()
	buffer := make([]byte, diskCopyBufferSize)
	remaining := expected
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("copy L2 cache source: %w", err)
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		count, readErr := io.ReadFull(source, buffer[:int(want)])
		if err := writeCandidateChunk(destination, hash, buffer[:count]); err != nil {
			return [sha256.Size]byte{}, err
		}
		if count > 0 {
			remaining -= int64(count)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return [sha256.Size]byte{}, fmt.Errorf("copy L2 cache source: %w", ctxErr)
		}
		if readErr != nil {
			return [sha256.Size]byte{}, classifyCacheSourceRead(readErr, expected, expected-remaining)
		}
	}
	return verifyCacheSourceTrailer(ctx, source, expected, hash)
}

func writeCandidateChunk(destination, hash io.Writer, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	written, err := destination.Write(raw)
	if written != len(raw) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	if err != nil {
		return fmt.Errorf(
			"%w: write L2 candidate: %w",
			errCacheLocalIO,
			err,
		)
	}
	_, _ = hash.Write(raw)
	return nil
}

func classifyCacheSourceRead(readErr error, expected, actual int64) error {
	if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
		return errors.Join(
			errCacheSourceRead,
			fmt.Errorf("%w: expected=%d actual=%d", io.ErrUnexpectedEOF, expected, actual),
		)
	}
	return errors.Join(errCacheSourceRead, fmt.Errorf("read cache source: %w", readErr))
}

func verifyCacheSourceTrailer(
	ctx context.Context,
	source io.Reader,
	expected int64,
	digest hash.Hash,
) ([sha256.Size]byte, error) {
	var extra [1]byte
	count, readErr := io.ReadFull(source, extra[:])
	if ctxErr := ctx.Err(); ctxErr != nil {
		return [sha256.Size]byte{}, fmt.Errorf("verify L2 cache source trailer: %w", ctxErr)
	}
	if count != 0 || readErr == nil {
		return [sha256.Size]byte{}, errors.Join(
			errCacheSourceRead,
			fmt.Errorf("%w: expected=%d", ErrCacheSourceSizeMismatch, expected),
		)
	}
	if !errors.Is(readErr, io.EOF) {
		return [sha256.Size]byte{}, errors.Join(
			errCacheSourceRead,
			fmt.Errorf("read cache source trailer: %w", readErr),
		)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func diskCacheEntryCost(size, maxCost int64) int64 {
	if size < 0 || maxCost <= 0 {
		return 0
	}
	if maxCost <= diskCacheAllocationUnit {
		return maxCost
	}
	logical := max(size, int64(1))
	remainder := logical % diskCacheAllocationUnit
	if remainder == 0 {
		return logical
	}
	increment := diskCacheAllocationUnit - remainder
	if logical > math.MaxInt64-increment {
		return 0
	}
	return logical + increment
}

func (d *diskCache) admit(entry *diskEntry) bool {
	var cleanup []string
	d.mu.Lock()
	if d.closing || entry.size < 0 || entry.size <= d.minSize ||
		entry.size > d.keyLimit || entry.size > d.maxCost ||
		entry.cost <= 0 || entry.cost > d.maxCost {
		d.mu.Unlock()
		return false
	}
	if existing, ok := d.entries[entry.key]; ok {
		cleanup = append(cleanup, d.retireLocked(existing)...)
	}
	for entry.cost > d.maxCost-d.usedCost {
		oldest := d.lru.Back()
		if oldest == nil {
			break
		}
		cleanup = append(cleanup, d.retireLocked(oldest)...)
		d.stats.evict.Add(1)
	}
	element := d.lru.PushFront(entry)
	d.entries[entry.key] = element
	d.usedCost += entry.cost
	d.mu.Unlock()
	d.cleanupPaths(cleanup)
	return true
}

func (d *diskCache) retireLocked(element *list.Element) []string {
	entry := mustDiskEntry(element)
	current, exists := d.entries[entry.key]
	if exists && current == element {
		delete(d.entries, entry.key)
		d.lru.Remove(element)
		d.usedCost -= entry.cost
	}
	entry.retired = true
	if entry.refs == 0 {
		return []string{entry.path}
	}
	return nil
}

func (d *diskCache) open(
	ctx context.Context,
	key fileCacheKey,
	expectedSize int64,
) (io.ReadSeekCloser, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("open L2 cache entry: %w", err)
	}
	d.mu.Lock()
	element, exists := d.entries[key]
	if !exists || d.closing {
		d.mu.Unlock()
		return nil, false, nil
	}
	entry := mustDiskEntry(element)
	if entry.size != expectedSize {
		cleanup := d.retireLocked(element)
		d.mu.Unlock()
		d.cleanupPaths(cleanup)
		return nil, false, nil
	}
	entry.refs++
	d.readers.Add(1)
	d.lru.MoveToFront(element)
	d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		d.release(entry)
		return nil, false, fmt.Errorf("open L2 cache entry: %w", err)
	}

	file, info, valid := openDiskEntryFile(entry.path, entry.size)
	if !valid {
		d.invalidate(entry)
		d.release(entry)
		return nil, false, nil
	}
	valid, err := d.validateChangedEntry(ctx, file, info, entry)
	if err != nil || !valid {
		_ = file.Close()
		if err == nil {
			d.invalidate(entry)
		}
		d.release(entry)
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		d.release(entry)
		return nil, false, fmt.Errorf("open L2 cache entry: %w", err)
	}
	return &diskCacheReader{File: file, cache: d, entry: entry}, true, nil
}

func openDiskEntryFile(path string, expectedSize int64) (*os.File, os.FileInfo, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		_ = file.Close()
		return nil, nil, false
	}
	return file, info, true
}

func (d *diskCache) validateChangedEntry(
	ctx context.Context,
	file *os.File,
	info os.FileInfo,
	entry *diskEntry,
) (bool, error) {
	if entry.fileInfo != nil && os.SameFile(entry.fileInfo, info) && info.ModTime().UnixNano() == entry.modTime {
		return true, nil
	}
	if err := validateOpenDigest(ctx, file, entry.digest); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("validate changed L2 cache entry: %w", ctxErr)
		}
		return false, nil
	}
	d.mu.Lock()
	entry.modTime = info.ModTime().UnixNano()
	entry.fileInfo = info
	d.mu.Unlock()
	return true, nil
}

func validateOpenDigest(ctx context.Context, file *os.File, expected [sha256.Size]byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("validate L2 cache digest: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek L2 cache entry for validation: %w", err)
	}
	hash := sha256.New()
	buffer := make([]byte, diskCopyBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("validate L2 cache digest: %w", err)
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read L2 cache entry for validation: %w", readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("validate L2 cache digest: %w", err)
	}
	if !bytes.Equal(hash.Sum(nil), expected[:]) {
		return ErrInvalidCachePath
	}
	_, err := file.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("rewind validated L2 cache entry: %w", err)
	}
	return nil
}

func (d *diskCache) invalidate(entry *diskEntry) {
	d.mu.Lock()
	var cleanup []string
	if element, exists := d.entries[entry.key]; exists && mustDiskEntry(element) == entry {
		cleanup = d.retireLocked(element)
	} else if !entry.retired {
		entry.retired = true
		if entry.refs == 0 {
			cleanup = append(cleanup, entry.path)
		}
	}
	d.mu.Unlock()
	d.cleanupPaths(cleanup)
}

func (d *diskCache) release(entry *diskEntry) {
	var cleanup []string
	d.mu.Lock()
	entry.refs--
	if entry.refs < 0 {
		entry.refs = 0
	}
	if entry.retired && entry.refs == 0 {
		cleanup = append(cleanup, entry.path)
	}
	d.mu.Unlock()
	d.readers.Done()
	d.cleanupPaths(cleanup)
}

func (d *diskCache) cleanupPaths(paths []string) {
	for _, path := range paths {
		orphanPath, err := d.removeOwnedPath(path)
		if err != nil {
			d.stats.cleanupFailure.Add(1)
			d.mu.Lock()
			d.orphans[orphanPath] = struct{}{}
			d.mu.Unlock()
			d.warn("remove L2 cache file failed", err)
		}
	}
}

func (d *diskCache) removeOwnedPath(path string) (string, error) {
	relative, err := filepath.Rel(d.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return path, errors.Join(ErrInvalidCachePath, err)
	}
	if !strings.HasSuffix(path, diskCacheFileSuffix) {
		return path, removeManagedFile(path)
	}
	retiredPath := path + "." + uuid.NewString() + diskCacheTempSuffix
	if err := os.Rename(path, retiredPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return path, fmt.Errorf("retire managed cache file: %w", err)
	}
	return retiredPath, removeManagedFile(retiredPath)
}

func removeManagedFile(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove managed cache file: %w", err)
	}
	return nil
}

func (d *diskCache) close(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closing = true
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.readers.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for L2 cache readers: %w", ctx.Err())
	case <-done:
	}

	d.mu.Lock()
	orphans := make([]string, 0, len(d.orphans))
	for path := range d.orphans {
		orphans = append(orphans, path)
	}
	d.orphans = make(map[string]struct{})
	d.entries = make(map[fileCacheKey]*list.Element)
	d.lru.Init()
	d.usedCost = 0
	d.mu.Unlock()
	d.cleanupPaths(orphans)

	lockErr := d.lock.Close()
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	if lockErr != nil {
		return fmt.Errorf("release L2 cache directory lock: %w", lockErr)
	}
	return nil
}

func (d *diskCache) entryPath(key fileCacheKey) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	element, exists := d.entries[key]
	if !exists {
		return "", false
	}
	return mustDiskEntry(element).path, true
}

func mustDiskEntry(element *list.Element) *diskEntry {
	entry, ok := element.Value.(*diskEntry)
	if !ok {
		panic("disk cache list contains an invalid entry")
	}
	return entry
}

func (d *diskCache) cost() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.usedCost
}

func cacheReason(err error) string {
	switch {
	case errors.Is(err, ErrInvalidCachePath):
		return "invalid_path"
	case errors.Is(err, ErrInvalidCache):
		return "invalid_config"
	case errors.Is(err, errCacheLocalIO):
		return "local_io"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "unknown"
	}
}

func (d *diskCache) warn(message string, err error) {
	reason := cacheReason(err)
	if !d.warnings.allow(reason) {
		return
	}
	logutil.GetLogger(d.ctx).Warn(message, zap.String("reason", reason))
}

func (k fileCacheKey) hex() string {
	return hex.EncodeToString(k[:])
}
