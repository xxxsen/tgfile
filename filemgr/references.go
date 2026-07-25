package filemgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/xxxsen/tgfile/constant"

	"github.com/xxxsen/common/database"
)

type storedFileRecord struct {
	fileID    uint64
	size      int64
	partCount int64
	state     int
	layout    int
}

type storedCompositeSegment struct {
	index        int
	sourceFileID uint64
	size         int64
	sourceSize   sql.NullInt64
	sourceState  sql.NullInt64
	sourceLayout sql.NullInt64
}

func readStoredFile(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) (storedFileRecord, bool, error) {
	const query = `SELECT file_id, file_size, file_part_count, file_state, file_layout_version
FROM tg_file_tab WHERE file_id = ?`
	var record storedFileRecord
	err := queryRow(ctx, queryer, query, fileID).Scan(
		&record.fileID,
		&record.size,
		&record.partCount,
		&record.state,
		&record.layout,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedFileRecord{}, false, nil
	}
	if err != nil {
		return storedFileRecord{}, false, fmt.Errorf("scan stored file %d: %w", fileID, err)
	}
	return record, true, nil
}

func fileHasLiveReference(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) (bool, error) {
	var count int64
	if err := queryRow(
		ctx,
		queryer,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = ?",
		strconv.FormatUint(fileID, 10),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count direct file mappings: %w", err)
	}
	if count != 0 {
		return true, nil
	}
	if err := queryRow(
		ctx,
		queryer,
		`SELECT COUNT(*)
FROM tg_s3_file_segment_tab segment
JOIN tg_file_mapping_tab mapping
  ON mapping.ref_data = CAST(segment.file_id AS TEXT)
WHERE segment.source_file_id = ?`,
		fileID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count composite file references: %w", err)
	}
	if count != 0 {
		return true, nil
	}
	if err := queryRow(
		ctx,
		queryer,
		`SELECT COUNT(*)
FROM tg_s3_multipart_part_tab part
JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
WHERE part.file_id = ? AND part.part_state = 'active' AND upload.upload_state = 'active'`,
		fileID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count active multipart references: %w", err)
	}
	return count != 0, nil
}

func ensureFileTreeCanBeLinked(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) error {
	record, exists, err := readStoredFile(ctx, queryer, fileID)
	if err != nil {
		return err
	}
	if !exists || record.state != constant.FileStateReady {
		return ErrS3ObjectConflict
	}
	switch record.layout {
	case 1:
		return ensurePhysicalFileLive(ctx, queryer, fileID)
	case 2:
		return ensureCompositeFileLive(ctx, queryer, record)
	default:
		return fmt.Errorf("%w: file=%d layout=%d", ErrInvalidFileLayout, fileID, record.layout)
	}
}

func ensurePhysicalFileLive(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) error {
	var nonLive int64
	if err := queryRow(
		ctx,
		queryer,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND delete_state != 'live'`,
		fileID,
	).Scan(&nonLive); err != nil {
		return fmt.Errorf("check physical file delete state: %w", err)
	}
	if nonLive != 0 {
		return ErrS3ObjectConflict
	}
	return nil
}

func ensureCompositeFileLive(
	ctx context.Context,
	queryer database.IQueryer,
	record storedFileRecord,
) error {
	const query = `SELECT segment_index, source_file_id, segment_size,
source.file_size, source.file_state, source.file_layout_version
FROM tg_s3_file_segment_tab segment
LEFT JOIN tg_file_tab source ON source.file_id = segment.source_file_id
WHERE segment.file_id = ?
ORDER BY segment.segment_index`
	rows, err := queryer.QueryContext(ctx, query, record.fileID)
	if err != nil {
		return fmt.Errorf("query linkable composite manifest: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	index := 0
	var total int64
	for rows.Next() {
		var segment storedCompositeSegment
		if err := rows.Scan(
			&segment.index,
			&segment.sourceFileID,
			&segment.size,
			&segment.sourceSize,
			&segment.sourceState,
			&segment.sourceLayout,
		); err != nil {
			return fmt.Errorf("scan linkable composite segment: %w", err)
		}
		if !validStoredCompositeSegment(segment, index, total, record.size) {
			return ErrS3ObjectConflict
		}
		if err := ensurePhysicalFileLive(ctx, queryer, segment.sourceFileID); err != nil {
			return err
		}
		if err := ensurePhysicalFileDeletionRefsComplete(ctx, queryer, segment.sourceFileID); err != nil {
			return err
		}
		total += segment.size
		index++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate linkable composite manifest: %w", err)
	}
	if index == 0 || total != record.size {
		return ErrS3ObjectConflict
	}
	return nil
}

func ensurePhysicalFileDeletionRefsComplete(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) error {
	var declaredParts, physicalParts, liveDeleteStates int64
	err := queryRow(
		ctx,
		queryer,
		`SELECT file.file_part_count,
COUNT(part.file_part_id),
COUNT(state.file_part_id)
FROM tg_file_tab file
LEFT JOIN tg_file_part_tab part ON part.file_id = file.file_id
LEFT JOIN tg_file_part_delete_state_tab state
  ON state.file_id = part.file_id
 AND state.file_part_id = part.file_part_id
 AND state.delete_state = 'live'
WHERE file.file_id = ?
GROUP BY file.file_id, file.file_part_count`,
		fileID,
	).Scan(&declaredParts, &physicalParts, &liveDeleteStates)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrS3ObjectConflict
	}
	if err != nil {
		return fmt.Errorf("check physical file deletion references: %w", err)
	}
	if physicalParts != declaredParts || liveDeleteStates != declaredParts {
		return ErrS3ObjectConflict
	}
	return nil
}

func validStoredCompositeSegment(
	segment storedCompositeSegment,
	expectedIndex int,
	currentSize, finalSize int64,
) bool {
	return segment.index == expectedIndex &&
		segment.sourceSize.Valid &&
		segment.sourceState.Valid &&
		segment.sourceLayout.Valid &&
		segment.sourceState.Int64 == constant.FileStateReady &&
		segment.sourceLayout.Int64 == 1 &&
		segment.sourceSize.Int64 == segment.size &&
		segment.size >= 0 &&
		currentSize <= finalSize-segment.size
}

func markFileTreePendingIfUnreferenced(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	fileID uint64,
	now int64,
) error {
	referenced, err := fileHasLiveReference(ctx, queryExecer, fileID)
	if err != nil {
		return err
	}
	if referenced {
		return nil
	}
	record, exists, err := readStoredFile(ctx, queryExecer, fileID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("mark missing file %d pending: %w", fileID, ErrS3ObjectConflict)
	}
	switch record.layout {
	case 1:
		if _, err := queryExecer.ExecContext(
			ctx,
			`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'pending', next_attempt_at = ?, lease_until = 0, mtime = ?
WHERE file_id = ? AND delete_state = 'live'`,
			now,
			now,
			fileID,
		); err != nil {
			return fmt.Errorf("mark unreferenced file blocks pending: %w", err)
		}
		return nil
	case 2:
		sources, err := compositeSourceFileIDs(ctx, queryExecer, fileID)
		if err != nil {
			return err
		}
		for _, source := range sources {
			if err := markFileTreePendingIfUnreferenced(ctx, queryExecer, source, now); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: file=%d layout=%d", ErrInvalidFileLayout, fileID, record.layout)
	}
}

func compositeSourceFileIDs(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) ([]uint64, error) {
	sources, err := queryFileIDList(
		ctx,
		queryer,
		`SELECT source_file_id FROM tg_s3_file_segment_tab
WHERE file_id = ? ORDER BY segment_index`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query composite sources for deletion: %w", err)
	}
	return sources, nil
}

func (d *defaultFileManager) DiscardUnpublishedFile(ctx context.Context, fileID uint64) error {
	now := time.Now().UnixMilli()
	if err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return markFileTreePendingIfUnreferenced(ctx, tx, fileID, now)
	}); err != nil {
		return fmt.Errorf("discard unpublished file %d: %w", fileID, err)
	}
	return nil
}
