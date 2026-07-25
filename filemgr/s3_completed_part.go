package filemgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/s3checksum"
)

const (
	completedPartChecksumAvailable   = "available"
	completedPartChecksumUnavailable = "unavailable"
)

type completedManifestSummary struct {
	isMultipart bool
	partsCount  int
}

func (d *defaultFileManager) StatS3ObjectPart(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
	partNumber int,
) (*entity.S3CompletedPart, error) {
	if partNumber < 1 || partNumber > maxS3MultipartParts {
		return nil, fmt.Errorf("%w: part number %d", ErrInvalidS3Part, partNumber)
	}
	summary, err := d.validateCompletedManifest(ctx, fileID, objectSize)
	if err != nil {
		return nil, err
	}
	if partNumber > summary.partsCount {
		return nil, &S3PartNumberError{Requested: partNumber, Actual: summary.partsCount}
	}
	if !summary.isMultipart {
		return &entity.S3CompletedPart{
			FileID:       fileID,
			PartNumber:   1,
			PartSize:     objectSize,
			PartsCount:   1,
			SourceFileID: fileID,
		}, nil
	}
	part, err := d.readCompletedPart(ctx, fileID, objectSize, partNumber, summary.partsCount)
	if err != nil {
		return nil, err
	}
	return part, nil
}

func (d *defaultFileManager) OpenS3ObjectPart(
	ctx context.Context,
	part *entity.S3CompletedPart,
) (io.ReadSeekCloser, error) {
	if part == nil || part.SourceFileID == 0 || part.PartSize < 0 {
		return nil, fmt.Errorf("%w: invalid part open request", ErrInvalidS3Part)
	}
	source, err := d.OpenFile(ctx, part.SourceFileID)
	if err != nil {
		return nil, fmt.Errorf("open completed S3 part source: %w", err)
	}
	return &boundedPartReader{source: source, size: part.PartSize, open: true}, nil
}

func (d *defaultFileManager) ListS3ObjectParts(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
	marker int,
	maxParts int,
) (*S3ObjectPartPage, error) {
	if marker < 0 || marker > maxS3MultipartParts || maxParts < 0 || maxParts > 1000 {
		return nil, fmt.Errorf("%w: invalid completed part page", ErrInvalidS3Part)
	}
	summary, err := d.validateCompletedManifest(ctx, fileID, objectSize)
	if err != nil {
		return nil, err
	}
	page := &S3ObjectPartPage{
		Parts:            make([]entity.S3CompletedPart, 0, maxParts),
		PartsCount:       summary.partsCount,
		PartNumberMarker: marker,
		MaxParts:         maxParts,
		IsMultipart:      summary.isMultipart,
	}
	if !summary.isMultipart || maxParts == 0 {
		return page, nil
	}
	parts, err := d.readCompletedPartPage(
		ctx,
		fileID,
		objectSize,
		summary.partsCount,
		marker,
		maxParts+1,
	)
	if err != nil {
		return nil, err
	}
	page.Parts = append(page.Parts, parts...)
	if len(page.Parts) > maxParts {
		page.Parts = page.Parts[:maxParts]
		page.IsTruncated = true
		page.NextPartNumberMarker = page.Parts[len(page.Parts)-1].PartNumber
	}
	return page, nil
}

func (d *defaultFileManager) readCompletedPartPage(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
	partsCount int,
	marker int,
	limit int,
) ([]entity.S3CompletedPart, error) {
	rows, err := d.dbc.QueryContext(
		ctx,
		`SELECT segment.segment_index + 1, segment.source_file_id, segment.segment_size,
COALESCE((
    SELECT SUM(previous.segment_size)
    FROM tg_s3_file_segment_tab previous
    WHERE previous.file_id = segment.file_id
      AND previous.segment_index < segment.segment_index
), 0),
part.checksum_state, part.checksum_algorithm, part.checksum_value
FROM tg_s3_file_segment_tab segment
JOIN tg_s3_completed_part_tab part
  ON part.file_id = segment.file_id
 AND part.part_number = segment.segment_index + 1
WHERE segment.file_id = ?
  AND segment.segment_index + 1 > ?
ORDER BY segment.segment_index
LIMIT ?`,
		fileID,
		marker,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query completed S3 part page: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	parts := make([]entity.S3CompletedPart, 0, limit)
	for rows.Next() {
		var part entity.S3CompletedPart
		if err := rows.Scan(
			&part.PartNumber,
			&part.SourceFileID,
			&part.PartSize,
			&part.StartOffset,
			&part.ChecksumState,
			&part.ChecksumAlgorithm,
			&part.ChecksumValue,
		); err != nil {
			return nil, fmt.Errorf("scan completed S3 part page: %w", err)
		}
		part.FileID = fileID
		part.PartsCount = partsCount
		part.IsMultipart = true
		if err := validateCompletedPartRecord(&part, objectSize); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed S3 part page: %w", err)
	}
	return parts, nil
}

func (d *defaultFileManager) validateCompletedManifest(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
) (*completedManifestSummary, error) {
	file, exists, err := d.internalGetFileInfo(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: final file does not exist", ErrInvalidS3Part)
	}
	if file.FileState != constant.FileStateReady || file.FileSize != objectSize {
		return nil, fmt.Errorf(
			"%w: final file state=%d size=%d object=%d",
			ErrInvalidS3Part,
			file.FileState,
			file.FileSize,
			objectSize,
		)
	}
	switch file.FileLayoutVersion {
	case 1:
		var segmentCount, completedCount int
		if err := queryRow(
			ctx,
			d.dbc,
			`SELECT
(SELECT COUNT(*) FROM tg_s3_file_segment_tab WHERE file_id = ?),
(SELECT COUNT(*) FROM tg_s3_completed_part_tab WHERE file_id = ?)`,
			fileID,
			fileID,
		).Scan(&segmentCount, &completedCount); err != nil {
			return nil, fmt.Errorf("inspect layout-v1 completed parts: %w", err)
		}
		if segmentCount != 0 || completedCount != 0 {
			return nil, fmt.Errorf("%w: layout-v1 file has a completed manifest", ErrInvalidS3Part)
		}
		return &completedManifestSummary{partsCount: 1}, nil
	case 2:
		return d.validateCompositeCompletedManifest(ctx, fileID, objectSize)
	default:
		return nil, fmt.Errorf(
			"%w: final file layout=%d",
			ErrInvalidS3Part,
			file.FileLayoutVersion,
		)
	}
}

func (d *defaultFileManager) validateCompositeCompletedManifest(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
) (*completedManifestSummary, error) {
	var (
		segmentCount   int
		minimumIndex   sql.NullInt64
		maximumIndex   sql.NullInt64
		totalSize      int64
		invalidRows    int
		completedCount int
		orphanParts    int
	)
	err := queryRow(
		ctx,
		d.dbc,
		`SELECT
COUNT(segment.segment_index),
MIN(segment.segment_index),
MAX(segment.segment_index),
COALESCE(SUM(segment.segment_size), 0),
COALESCE(SUM(CASE
    WHEN source.file_id IS NULL
      OR source.file_state != ?
      OR source.file_layout_version != 1
      OR source.file_size != segment.segment_size
      OR part.file_id IS NULL
      OR part.part_size != segment.segment_size
    THEN 1 ELSE 0 END), 0),
(SELECT COUNT(*) FROM tg_s3_completed_part_tab completed WHERE completed.file_id = ?),
(SELECT COUNT(*)
 FROM tg_s3_completed_part_tab completed
 LEFT JOIN tg_s3_file_segment_tab matching
   ON matching.file_id = completed.file_id
  AND matching.segment_index + 1 = completed.part_number
 WHERE completed.file_id = ? AND matching.file_id IS NULL)
FROM tg_s3_file_segment_tab segment
LEFT JOIN tg_file_tab source ON source.file_id = segment.source_file_id
LEFT JOIN tg_s3_completed_part_tab part
  ON part.file_id = segment.file_id
 AND part.part_number = segment.segment_index + 1
WHERE segment.file_id = ?`,
		constant.FileStateReady,
		fileID,
		fileID,
		fileID,
	).Scan(
		&segmentCount,
		&minimumIndex,
		&maximumIndex,
		&totalSize,
		&invalidRows,
		&completedCount,
		&orphanParts,
	)
	if err != nil {
		return nil, fmt.Errorf("validate completed S3 part manifest: %w", err)
	}
	if segmentCount < 1 || segmentCount > maxS3MultipartParts ||
		!minimumIndex.Valid || minimumIndex.Int64 != 0 ||
		!maximumIndex.Valid || maximumIndex.Int64 != int64(segmentCount-1) ||
		totalSize != objectSize || invalidRows != 0 ||
		completedCount != segmentCount || orphanParts != 0 {
		return nil, fmt.Errorf(
			"%w: segments=%d completed=%d size=%d object=%d invalid=%d orphan=%d",
			ErrInvalidS3Part,
			segmentCount,
			completedCount,
			totalSize,
			objectSize,
			invalidRows,
			orphanParts,
		)
	}
	return &completedManifestSummary{isMultipart: true, partsCount: segmentCount}, nil
}

func (d *defaultFileManager) readCompletedPart(
	ctx context.Context,
	fileID uint64,
	objectSize int64,
	partNumber int,
	partsCount int,
) (*entity.S3CompletedPart, error) {
	var part entity.S3CompletedPart
	err := queryRow(
		ctx,
		d.dbc,
		`SELECT segment.source_file_id, segment.segment_size,
COALESCE((
    SELECT SUM(previous.segment_size)
    FROM tg_s3_file_segment_tab previous
    WHERE previous.file_id = segment.file_id
      AND previous.segment_index < segment.segment_index
), 0),
completed.checksum_state, completed.checksum_algorithm, completed.checksum_value
FROM tg_s3_file_segment_tab segment
JOIN tg_s3_completed_part_tab completed
  ON completed.file_id = segment.file_id
 AND completed.part_number = segment.segment_index + 1
WHERE segment.file_id = ? AND segment.segment_index = ?`,
		fileID,
		partNumber-1,
	).Scan(
		&part.SourceFileID,
		&part.PartSize,
		&part.StartOffset,
		&part.ChecksumState,
		&part.ChecksumAlgorithm,
		&part.ChecksumValue,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: completed part %d is missing", ErrInvalidS3Part, partNumber)
	}
	if err != nil {
		return nil, fmt.Errorf("read completed S3 part: %w", err)
	}
	part.FileID = fileID
	part.PartNumber = partNumber
	part.PartsCount = partsCount
	part.IsMultipart = true
	if err := validateCompletedPartRecord(&part, objectSize); err != nil {
		return nil, err
	}
	return &part, nil
}

func validateCompletedPartRecord(part *entity.S3CompletedPart, objectSize int64) error {
	if part.PartNumber < 1 || part.PartNumber > part.PartsCount ||
		part.SourceFileID == 0 || part.PartSize < 0 || part.StartOffset < 0 ||
		part.StartOffset > objectSize-part.PartSize {
		return fmt.Errorf("%w: invalid completed part bounds", ErrInvalidS3Part)
	}
	switch part.ChecksumState {
	case completedPartChecksumUnavailable:
		if part.ChecksumAlgorithm != "" || part.ChecksumValue != "" {
			return fmt.Errorf("%w: unavailable part has checksum data", ErrInvalidS3Part)
		}
	case completedPartChecksumAvailable:
		algorithm, err := s3checksum.ParseAlgorithm(part.ChecksumAlgorithm)
		if err != nil {
			return fmt.Errorf("%w: completed part checksum algorithm: %w", ErrInvalidS3Part, err)
		}
		if _, err := s3checksum.Decode(algorithm, part.ChecksumValue); err != nil {
			return fmt.Errorf("%w: completed part checksum: %w", ErrInvalidS3Part, err)
		}
	default:
		return fmt.Errorf("%w: invalid checksum state", ErrInvalidS3Part)
	}
	return nil
}

type boundedPartReader struct {
	source io.ReadSeekCloser
	size   int64
	offset int64
	open   bool
}

func (r *boundedPartReader) Read(buffer []byte) (int, error) {
	if !r.open {
		return 0, ErrFileNotOpen
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.offset == r.size {
		return 0, io.EOF
	}
	remaining := r.size - r.offset
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	for attempt := 0; attempt < 2; attempt++ {
		count, err := r.source.Read(buffer)
		r.offset += int64(count)
		if errors.Is(err, io.EOF) {
			if r.offset == r.size {
				return count, nil
			}
			if count != 0 {
				return count, nil
			}
			if attempt == 0 {
				continue
			}
			return 0, io.ErrUnexpectedEOF
		}
		if err != nil {
			return count, fmt.Errorf("read bounded S3 part: %w", err)
		}
		if count == 0 && r.offset < r.size {
			if attempt == 0 {
				continue
			}
			return 0, io.ErrUnexpectedEOF
		}
		return count, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (r *boundedPartReader) Seek(offset int64, whence int) (int64, error) {
	if !r.open {
		return 0, ErrFileNotOpen
	}
	target := r.offset
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target += offset
	case io.SeekEnd:
		target = r.size + offset
	default:
		return r.offset, fmt.Errorf("%w: invalid seek origin", ErrInvalidOffset)
	}
	if target < 0 {
		return r.offset, fmt.Errorf("%w: %d", ErrInvalidOffset, target)
	}
	if target > r.size {
		return r.offset, fmt.Errorf("%w: offset=%d size=%d", ErrSeekPastEnd, target, r.size)
	}
	actual, err := r.source.Seek(target, io.SeekStart)
	if err != nil {
		return r.offset, fmt.Errorf("seek bounded S3 part: %w", err)
	}
	if actual != target {
		return r.offset, fmt.Errorf("%w: source offset=%d expected=%d", ErrInvalidS3Part, actual, target)
	}
	r.offset = target
	return target, nil
}

func (r *boundedPartReader) Close() error {
	if !r.open {
		return nil
	}
	r.open = false
	if err := r.source.Close(); err != nil {
		return fmt.Errorf("close bounded S3 part: %w", err)
	}
	return nil
}
