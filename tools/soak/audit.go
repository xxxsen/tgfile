package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xxxsen/common/database"
)

var (
	errAuditInvariant = errors.New("soak audit invariant failed")
	outputMutex       sync.Mutex
)

type auditResult struct {
	changeJournalRows int64
	deleteStateRows   int64
	databaseBytes     int64
	integrityChecked  bool
}

func (r *soakRunner) cleanupTrackedResources(ctx context.Context) error {
	r.activeMu.Lock()
	uploads := make(map[string]string, len(r.activeUpload))
	for uploadID, key := range r.activeUpload {
		uploads[uploadID] = key
	}
	keys := make([]string, 0, len(r.activeKeys))
	for key := range r.activeKeys {
		keys = append(keys, key)
	}
	r.activeMu.Unlock()
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
	if cleanupErr != nil {
		return cleanupErr
	}
	return r.waitForDeletionDrain(ctx)
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
