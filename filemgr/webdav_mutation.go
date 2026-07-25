package filemgr

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/xxxsen/tgfile/directory"

	"github.com/xxxsen/common/database"
)

func (d *defaultFileManager) removeWebDAVLink(ctx context.Context, link string) error {
	if err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		removed, err := tx.Remove(ctx, link)
		if err != nil {
			return fmt.Errorf("remove mapping entry: %w", err)
		}
		fileIDs, err := mappingEntryFileIDs(removed)
		if err != nil {
			return err
		}
		if err := deleteMappingMetadata(ctx, tx.QueryExecer(), removed); err != nil {
			return err
		}
		return markMappingFilesPending(ctx, tx.QueryExecer(), fileIDs)
	}); err != nil {
		return fmt.Errorf("remove file link %q: %w", link, err)
	}
	return nil
}

func (d *defaultFileManager) moveWebDAVLink(
	ctx context.Context,
	source, destination string,
	overwrite bool,
) error {
	if err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		overwritten, err := tx.Move(ctx, source, destination, overwrite)
		if err != nil {
			return fmt.Errorf("move mapping entry: %w", err)
		}
		fileIDs, err := mappingEntryFileIDs(overwritten)
		if err != nil {
			return err
		}
		if err := deleteMappingMetadata(ctx, tx.QueryExecer(), overwritten); err != nil {
			return err
		}
		return markMappingFilesPending(ctx, tx.QueryExecer(), fileIDs)
	}); err != nil {
		return fmt.Errorf("rename file link %q to %q: %w", source, destination, err)
	}
	return nil
}

func (d *defaultFileManager) copyWebDAVLink(
	ctx context.Context,
	source, destination string,
	overwrite bool,
) error {
	if err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		copies, overwritten, err := tx.Copy(ctx, source, destination, overwrite)
		if err != nil {
			return fmt.Errorf("copy mapping entry: %w", err)
		}
		if err := validateWebDAVCopies(ctx, tx.QueryExecer(), copies); err != nil {
			return err
		}
		fileIDs, err := mappingEntryFileIDs(overwritten)
		if err != nil {
			return err
		}
		if err := deleteMappingMetadata(ctx, tx.QueryExecer(), overwritten); err != nil {
			return err
		}
		if err := copyWebDAVMetadata(ctx, tx.QueryExecer(), copies); err != nil {
			return err
		}
		return markMappingFilesPending(ctx, tx.QueryExecer(), fileIDs)
	}); err != nil {
		return fmt.Errorf("copy file link %q to %q: %w", source, destination, err)
	}
	return nil
}

func validateWebDAVCopies(
	ctx context.Context,
	queryer database.IQueryer,
	copies []directory.EntryCopy,
) error {
	for _, copied := range copies {
		if copied.Source.IsDir() {
			continue
		}
		fileID, err := strconv.ParseUint(copied.Source.RefData(), 10, 64)
		if err != nil {
			return fmt.Errorf("parse copied mapping file id: %w", err)
		}
		if err := ensureFileTreeCanBeLinked(ctx, queryer, fileID); err != nil {
			return fmt.Errorf("ensure copied mapping file is linkable: %w", err)
		}
	}
	return nil
}

func copyWebDAVMetadata(
	ctx context.Context,
	exec database.IExecer,
	copies []directory.EntryCopy,
) error {
	for _, copied := range copies {
		if copied.Source.IsDir() {
			continue
		}
		if err := copyMappingMetadata(
			ctx,
			exec,
			copied.Source.EntryID(),
			copied.Destination.EntryID(),
		); err != nil {
			return err
		}
	}
	return nil
}

func mappingEntryFileIDs(entries []directory.IDirectoryEntry) ([]uint64, error) {
	seen := make(map[uint64]struct{})
	fileIDs := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileID, err := strconv.ParseUint(entry.RefData(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse removed mapping file id: %w", err)
		}
		if _, exists := seen[fileID]; exists {
			continue
		}
		seen[fileID] = struct{}{}
		fileIDs = append(fileIDs, fileID)
	}
	return fileIDs, nil
}

func deleteMappingMetadata(
	ctx context.Context,
	exec database.IExecer,
	entries []directory.IDirectoryEntry,
) error {
	for _, entry := range entries {
		if _, err := exec.ExecContext(
			ctx,
			"DELETE FROM tg_s3_object_metadata_tab WHERE entry_id = ?",
			entry.EntryID(),
		); err != nil {
			return fmt.Errorf("delete mapping metadata: %w", err)
		}
	}
	return nil
}

func copyMappingMetadata(
	ctx context.Context,
	exec database.IExecer,
	sourceEntryID, destinationEntryID uint64,
) error {
	const statement = `INSERT INTO tg_s3_object_metadata_tab (
entry_id, etag, checksum_sha256, request_checksum_algorithm, request_checksum_value,
content_type, cache_control, content_disposition, content_encoding, content_language,
expires, user_metadata, ctime, mtime
)
SELECT ?, etag, checksum_sha256, request_checksum_algorithm, request_checksum_value,
content_type, cache_control, content_disposition, content_encoding, content_language,
expires, user_metadata, ctime, mtime
FROM tg_s3_object_metadata_tab WHERE entry_id = ?`
	if _, err := exec.ExecContext(ctx, statement, destinationEntryID, sourceEntryID); err != nil {
		return fmt.Errorf("copy mapping metadata: %w", err)
	}
	return nil
}

func markMappingFilesPending(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	fileIDs []uint64,
) error {
	now := time.Now().UnixMilli()
	for _, fileID := range fileIDs {
		if err := markFileTreePendingIfUnreferenced(ctx, queryExecer, fileID, now); err != nil {
			return err
		}
	}
	return nil
}
