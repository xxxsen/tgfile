package filemgr

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 Multipart ETag compatibility requires MD5.
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

const (
	maxS3MultipartParts       = 10_000
	maxS3MultipartPartSize    = int64(5 * 1024 * 1024 * 1024)
	minS3MultipartPartSize    = int64(5 * 1024 * 1024)
	defaultMultipartExpiry    = 24 * time.Hour
	multipartControlRetention = 24 * time.Hour
	multipartCleanupInterval  = 60 * time.Second
	multipartCleanupBatchSize = 100
)

var (
	ErrNoSuchUpload            = errors.New("multipart upload does not exist")
	ErrInvalidMultipartPart    = errors.New("invalid multipart upload part")
	ErrInvalidPartOrder        = errors.New("multipart parts are not in ascending order")
	ErrMultipartPartTooSmall   = errors.New("multipart part is too small")
	ErrMultipartEntityTooLarge = errors.New("multipart entity is too large")
	ErrMultipartConflict       = errors.New("multipart upload changed concurrently")
	ErrInvalidMultipartRequest = errors.New("invalid multipart request")
)

type CreateMultipartRequest struct {
	Bucket      string
	Key         string
	Metadata    *entity.S3ObjectMetadata
	ExpireAfter time.Duration
}

type MultipartUpload struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
	ExpiresAt time.Time
}

type PutMultipartPartRequest struct {
	UploadID      string
	Bucket        string
	Key           string
	PartNumber    int
	FileID        uint64
	Size          int64
	ETag          string
	MaxObjectSize int64
}

type MultipartPart struct {
	PartNumber   int
	FileID       uint64
	Size         int64
	ETag         string
	LastModified time.Time
}

type ListMultipartPartsRequest struct {
	UploadID string
	Bucket   string
	Key      string
	Marker   int
	MaxParts int
}

type MultipartPartPage struct {
	Parts                []MultipartPart
	IsTruncated          bool
	NextPartNumberMarker int
}

type CompleteMultipartPart struct {
	PartNumber int
	ETag       string
}

type CompleteMultipartRequest struct {
	UploadID      string
	Bucket        string
	Key           string
	Parts         []CompleteMultipartPart
	MaxObjectSize int64
	Condition     *S3Condition
}

type CompleteMultipartResult struct {
	FileID uint64
	Size   int64
	ETag   string
}

type AbortMultipartRequest struct {
	UploadID string
	Bucket   string
	Key      string
}

type ListMultipartUploadsRequest struct {
	Bucket         string
	Prefix         string
	Delimiter      string
	KeyMarker      string
	UploadIDMarker string
	MaxUploads     int
}

type MultipartUploadListItem struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

type MultipartUploadPage struct {
	Uploads            []MultipartUploadListItem
	CommonPrefixes     []string
	IsTruncated        bool
	NextKeyMarker      string
	NextUploadIDMarker string
}

type IS3MultipartReader interface {
	ListMultipartParts(context.Context, *ListMultipartPartsRequest) (*MultipartPartPage, error)
	ListMultipartUploads(context.Context, *ListMultipartUploadsRequest) (*MultipartUploadPage, error)
}

type IS3MultipartWriter interface {
	CreateMultipartUpload(context.Context, *CreateMultipartRequest) (*MultipartUpload, error)
	PutMultipartPart(context.Context, *PutMultipartPartRequest) (*MultipartPart, error)
	CompleteMultipartUpload(context.Context, *CompleteMultipartRequest) (*CompleteMultipartResult, error)
	AbortMultipartUpload(context.Context, *AbortMultipartRequest) error
}

type IS3MultipartManager interface {
	IS3MultipartReader
	IS3MultipartWriter
}

type storedMultipartUpload struct {
	uploadID           string
	bucket             string
	key                string
	state              string
	contentType        string
	cacheControl       string
	contentDisposition string
	contentEncoding    string
	contentLanguage    string
	expires            string
	userMetadata       string
	fingerprint        string
	resultFileID       uint64
	resultETag         string
	initiatedAt        int64
	expiresAt          int64
	completedAt        int64
	cleanupAt          int64
}

func (d *defaultFileManager) CreateMultipartUpload(
	ctx context.Context,
	request *CreateMultipartRequest,
) (*MultipartUpload, error) {
	if request == nil || request.Bucket == "" || request.Key == "" || request.Metadata == nil {
		return nil, ErrInvalidMultipartRequest
	}
	expiry := request.ExpireAfter
	if expiry == 0 {
		expiry = defaultMultipartExpiry
	}
	if expiry < time.Hour || expiry > defaultMultipartExpiry {
		return nil, fmt.Errorf("%w: expiry=%s", ErrInvalidMultipartRequest, expiry)
	}
	now := time.Now()
	for attempt := 0; attempt < 3; attempt++ {
		uploadID, err := generateMultipartUploadID(now)
		if err != nil {
			return nil, err
		}
		_, err = d.dbc.ExecContext(
			ctx,
			`INSERT INTO tg_s3_multipart_upload_tab (
upload_id, bucket_name, object_key, upload_state,
content_type, cache_control, content_disposition, content_encoding,
content_language, expires, user_metadata, initiated_at, expires_at, ctime, mtime
) VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uploadID,
			request.Bucket,
			request.Key,
			request.Metadata.ContentType,
			request.Metadata.CacheControl,
			request.Metadata.ContentDisposition,
			request.Metadata.ContentEncoding,
			request.Metadata.ContentLanguage,
			request.Metadata.Expires,
			request.Metadata.UserMetadata,
			now.UnixMilli(),
			now.Add(expiry).UnixMilli(),
			now.UnixMilli(),
			now.UnixMilli(),
		)
		if err == nil {
			return &MultipartUpload{
				UploadID:  uploadID,
				Bucket:    request.Bucket,
				Key:       request.Key,
				Initiated: now,
				ExpiresAt: now.Add(expiry),
			}, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, fmt.Errorf("insert multipart upload: %w", err)
		}
	}
	return nil, fmt.Errorf("generate unique multipart upload id: %w", ErrMultipartConflict)
}

func generateMultipartUploadID(now time.Time) (string, error) {
	raw := make([]byte, 32)
	binary.BigEndian.PutUint64(raw[:8], uint64(now.UnixMilli()))
	if _, err := rand.Read(raw[8:]); err != nil {
		return "", fmt.Errorf("generate multipart upload id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func readMultipartUpload(
	ctx context.Context,
	queryer database.IQueryer,
	uploadID string,
) (storedMultipartUpload, bool, error) {
	const query = `SELECT upload_id, bucket_name, object_key, upload_state,
content_type, cache_control, content_disposition, content_encoding,
content_language, expires, user_metadata, completion_fingerprint,
result_file_id, result_etag, initiated_at, expires_at, completed_at, cleanup_at
FROM tg_s3_multipart_upload_tab WHERE upload_id = ?`
	var upload storedMultipartUpload
	err := queryRow(ctx, queryer, query, uploadID).Scan(
		&upload.uploadID,
		&upload.bucket,
		&upload.key,
		&upload.state,
		&upload.contentType,
		&upload.cacheControl,
		&upload.contentDisposition,
		&upload.contentEncoding,
		&upload.contentLanguage,
		&upload.expires,
		&upload.userMetadata,
		&upload.fingerprint,
		&upload.resultFileID,
		&upload.resultETag,
		&upload.initiatedAt,
		&upload.expiresAt,
		&upload.completedAt,
		&upload.cleanupAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedMultipartUpload{}, false, nil
	}
	if err != nil {
		return storedMultipartUpload{}, false, fmt.Errorf("scan multipart upload: %w", err)
	}
	return upload, true, nil
}

func matchingMultipartUpload(
	upload storedMultipartUpload,
	exists bool,
	bucket, key string,
) bool {
	return exists && upload.bucket == bucket && upload.key == key
}

func (d *defaultFileManager) PutMultipartPart(
	ctx context.Context,
	request *PutMultipartPartRequest,
) (*MultipartPart, error) {
	if err := validatePutMultipartPartRequest(request); err != nil {
		return nil, err
	}
	now := time.Now()
	var expired bool
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		var txErr error
		expired, txErr = d.putMultipartPartTx(ctx, tx, request, now)
		return txErr
	})
	if err != nil {
		return nil, fmt.Errorf("put multipart part transaction: %w", err)
	}
	if expired {
		return nil, ErrNoSuchUpload
	}
	return &MultipartPart{
		PartNumber:   request.PartNumber,
		FileID:       request.FileID,
		Size:         request.Size,
		ETag:         request.ETag,
		LastModified: now,
	}, nil
}

type replacedMultipartPart struct {
	fileID    uint64
	size      int64
	partCount int64
}

func (d *defaultFileManager) putMultipartPartTx(
	ctx context.Context,
	tx database.IQueryExecer,
	request *PutMultipartPartRequest,
	now time.Time,
) (bool, error) {
	expired, err := ensureActiveMultipartUpload(ctx, tx, request.UploadID, request.Bucket, request.Key, now)
	if err != nil || expired {
		return expired, err
	}
	file, err := validateMultipartStagingFile(ctx, tx, request.FileID, request.Size)
	if err != nil {
		return false, err
	}
	previous, err := readReplacedMultipartPart(ctx, tx, request.UploadID, request.PartNumber)
	if err != nil {
		return false, err
	}
	if err := d.validateMultipartStagingTotals(ctx, tx, request, file, previous); err != nil {
		return false, err
	}
	if err := upsertMultipartPart(ctx, tx, request, previous.fileID, now); err != nil {
		return false, err
	}
	if previous.fileID != 0 && previous.fileID != request.FileID {
		if err := markFileTreePendingIfUnreferenced(ctx, tx, previous.fileID, now.UnixMilli()); err != nil {
			return false, err
		}
	}
	return false, nil
}

func ensureActiveMultipartUpload(
	ctx context.Context,
	tx database.IQueryExecer,
	uploadID, bucket, key string,
	now time.Time,
) (bool, error) {
	upload, exists, err := readMultipartUpload(ctx, tx, uploadID)
	if err != nil {
		return false, err
	}
	if !matchingMultipartUpload(upload, exists, bucket, key) || upload.state != "active" {
		return false, ErrNoSuchUpload
	}
	if upload.expiresAt > now.UnixMilli() {
		return false, nil
	}
	if err := abortMultipartUploadTx(ctx, tx, upload, now); err != nil {
		return false, err
	}
	return true, nil
}

func validateMultipartStagingFile(
	ctx context.Context,
	tx database.IQueryExecer,
	fileID uint64,
	size int64,
) (storedFileRecord, error) {
	file, exists, err := readStoredFile(ctx, tx, fileID)
	if err != nil {
		return storedFileRecord{}, err
	}
	if !exists || file.state != constant.FileStateReady || file.layout != 1 || file.size != size {
		return storedFileRecord{}, ErrInvalidMultipartPart
	}
	if err := ensureMultipartStagingFileLive(ctx, tx, file); err != nil {
		return storedFileRecord{}, err
	}
	var mappingCount int64
	if err := queryRow(
		ctx,
		tx,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = ?",
		strconv.FormatUint(fileID, 10),
	).Scan(&mappingCount); err != nil {
		return storedFileRecord{}, fmt.Errorf("count upload part mappings: %w", err)
	}
	if mappingCount != 0 {
		return storedFileRecord{}, ErrInvalidMultipartPart
	}
	return file, nil
}

func ensureMultipartStagingFileLive(
	ctx context.Context,
	queryer database.IQueryer,
	file storedFileRecord,
) error {
	var physicalParts, deleteStates, nonLive int64
	if err := queryRow(
		ctx,
		queryer,
		`SELECT COUNT(part.file_part_id), COUNT(state.file_part_id),
COALESCE(SUM(CASE WHEN state.delete_state != 'live' THEN 1 ELSE 0 END), 0)
FROM tg_file_part_tab part
LEFT JOIN tg_file_part_delete_state_tab state
  ON state.file_id = part.file_id AND state.file_part_id = part.file_part_id
WHERE part.file_id = ?`,
		file.fileID,
	).Scan(&physicalParts, &deleteStates, &nonLive); err != nil {
		return fmt.Errorf("check multipart staging delete references: %w", err)
	}
	if physicalParts != file.partCount || deleteStates != file.partCount || nonLive != 0 {
		return ErrInvalidMultipartPart
	}
	return nil
}

func readReplacedMultipartPart(
	ctx context.Context,
	tx database.IQueryer,
	uploadID string,
	partNumber int,
) (replacedMultipartPart, error) {
	var part replacedMultipartPart
	err := queryRow(
		ctx,
		tx,
		`SELECT part.file_id, part.part_size, file.file_part_count
FROM tg_s3_multipart_part_tab part
JOIN tg_file_tab file ON file.file_id = part.file_id
WHERE part.upload_id = ? AND part.part_number = ? AND part.part_state = 'active'`,
		uploadID,
		partNumber,
	).Scan(&part.fileID, &part.size, &part.partCount)
	if errors.Is(err, sql.ErrNoRows) {
		return replacedMultipartPart{}, nil
	}
	if err != nil {
		return replacedMultipartPart{}, fmt.Errorf("read replaced multipart part: %w", err)
	}
	return part, nil
}

func (d *defaultFileManager) validateMultipartStagingTotals(
	ctx context.Context,
	tx database.IQueryer,
	request *PutMultipartPartRequest,
	file storedFileRecord,
	previous replacedMultipartPart,
) error {
	var totalSize, totalPartCount int64
	if err := queryRow(
		ctx,
		tx,
		`SELECT COALESCE(SUM(part.part_size), 0), COALESCE(SUM(file.file_part_count), 0)
FROM tg_s3_multipart_part_tab part
JOIN tg_file_tab file ON file.file_id = part.file_id
WHERE part.upload_id = ? AND part.part_state = 'active'`,
		request.UploadID,
	).Scan(&totalSize, &totalPartCount); err != nil {
		return fmt.Errorf("sum multipart upload parts: %w", err)
	}
	totalSize = totalSize - previous.size + request.Size
	totalPartCount = totalPartCount - previous.partCount + file.partCount
	if totalSize > multipartObjectSizeLimit(request.MaxObjectSize, d.bkio.MaxFileSize()) {
		return ErrMultipartEntityTooLarge
	}
	if totalPartCount > maxFilePartCount {
		return ErrTooManyFileParts
	}
	return nil
}

func upsertMultipartPart(
	ctx context.Context,
	tx database.IExecer,
	request *PutMultipartPartRequest,
	previousFileID uint64,
	now time.Time,
) error {
	if previousFileID == 0 {
		return insertMultipartPart(ctx, tx, request, now)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_part_tab
SET file_id = ?, part_size = ?, part_etag = ?, uploaded_at = ?, mtime = ?
WHERE upload_id = ? AND part_number = ? AND part_state = 'active'`,
		request.FileID,
		request.Size,
		request.ETag,
		now.UnixMilli(),
		now.UnixMilli(),
		request.UploadID,
		request.PartNumber,
	)
	if err != nil {
		return fmt.Errorf("replace multipart part: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMultipartConflict
	}
	return nil
}

func insertMultipartPart(
	ctx context.Context,
	tx database.IExecer,
	request *PutMultipartPartRequest,
	now time.Time,
) error {
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO tg_s3_multipart_part_tab (
upload_id, part_number, part_state, file_id, part_size, part_etag,
uploaded_at, ctime, mtime
) VALUES (?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		request.UploadID,
		request.PartNumber,
		request.FileID,
		request.Size,
		request.ETag,
		now.UnixMilli(),
		now.UnixMilli(),
		now.UnixMilli(),
	); err != nil {
		return fmt.Errorf("insert multipart part: %w", err)
	}
	return nil
}

func validatePutMultipartPartRequest(request *PutMultipartPartRequest) error {
	if request == nil || request.UploadID == "" || request.Bucket == "" || request.Key == "" ||
		request.PartNumber < 1 || request.PartNumber > maxS3MultipartParts ||
		request.Size < 0 || request.Size > maxS3MultipartPartSize ||
		len(request.ETag) != md5.Size*2 {
		return ErrInvalidMultipartRequest
	}
	if _, err := hex.DecodeString(request.ETag); err != nil || request.ETag != strings.ToLower(request.ETag) {
		return ErrInvalidMultipartRequest
	}
	return nil
}

func multipartObjectSizeLimit(configured, blockSize int64) int64 {
	if configured > 0 {
		return configured
	}
	if blockSize <= 0 || blockSize > int64(^uint64(0)>>1)/maxFilePartCount {
		return int64(^uint64(0) >> 1)
	}
	return blockSize * maxFilePartCount
}

func (d *defaultFileManager) ListMultipartParts(
	ctx context.Context,
	request *ListMultipartPartsRequest,
) (*MultipartPartPage, error) {
	if request == nil || request.UploadID == "" || request.Bucket == "" || request.Key == "" ||
		request.Marker < 0 || request.Marker > maxS3MultipartParts ||
		request.MaxParts < 0 || request.MaxParts > 1000 {
		return nil, ErrInvalidMultipartRequest
	}
	page := &MultipartPartPage{Parts: make([]MultipartPart, 0, request.MaxParts)}
	var expired bool
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		now := time.Now()
		var err error
		expired, err = ensureActiveMultipartUpload(
			ctx,
			tx,
			request.UploadID,
			request.Bucket,
			request.Key,
			now,
		)
		if err != nil || expired || request.MaxParts == 0 {
			return err
		}
		page, err = readMultipartPartPage(ctx, tx, request)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list multipart parts transaction: %w", err)
	}
	if expired {
		return nil, ErrNoSuchUpload
	}
	return page, nil
}

func readMultipartPartPage(
	ctx context.Context,
	queryer database.IQueryer,
	request *ListMultipartPartsRequest,
) (*MultipartPartPage, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT part_number, file_id, part_size, part_etag, uploaded_at
FROM tg_s3_multipart_part_tab
WHERE upload_id = ? AND part_state = 'active' AND part_number > ?
ORDER BY part_number LIMIT ?`,
		request.UploadID,
		request.Marker,
		request.MaxParts+1,
	)
	if err != nil {
		return nil, fmt.Errorf("query multipart parts: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	page := &MultipartPartPage{Parts: make([]MultipartPart, 0, request.MaxParts)}
	for rows.Next() {
		var part MultipartPart
		var uploadedAt int64
		if err := rows.Scan(
			&part.PartNumber,
			&part.FileID,
			&part.Size,
			&part.ETag,
			&uploadedAt,
		); err != nil {
			return nil, fmt.Errorf("scan multipart part: %w", err)
		}
		if len(page.Parts) == request.MaxParts {
			page.IsTruncated = true
			break
		}
		part.LastModified = time.UnixMilli(uploadedAt)
		page.Parts = append(page.Parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multipart parts: %w", err)
	}
	if page.IsTruncated && len(page.Parts) != 0 {
		page.NextPartNumberMarker = page.Parts[len(page.Parts)-1].PartNumber
	}
	return page, nil
}

func (d *defaultFileManager) CompleteMultipartUpload(
	ctx context.Context,
	request *CompleteMultipartRequest,
) (*CompleteMultipartResult, error) {
	if err := validateCompleteMultipartRequest(request); err != nil {
		return nil, err
	}
	fingerprint := multipartCompletionFingerprint(request.Parts)
	var (
		completed *CompleteMultipartResult
		expired   bool
	)
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		var txErr error
		completed, expired, txErr = d.completeMultipartUploadTx(ctx, tx, request, fingerprint)
		return txErr
	})
	if err != nil {
		return nil, fmt.Errorf("complete multipart upload transaction: %w", err)
	}
	if expired {
		return nil, ErrNoSuchUpload
	}
	return completed, nil
}

func (d *defaultFileManager) completeMultipartUploadTx(
	ctx context.Context,
	tx directory.ITransaction,
	request *CompleteMultipartRequest,
	fingerprint string,
) (*CompleteMultipartResult, bool, error) {
	upload, exists, err := readMultipartUpload(ctx, tx.QueryExecer(), request.UploadID)
	if err != nil {
		return nil, false, err
	}
	if !matchingMultipartUpload(upload, exists, request.Bucket, request.Key) {
		return nil, false, ErrNoSuchUpload
	}
	if upload.state == "completed" {
		result, err := readCompletedMultipartResult(ctx, tx.QueryExecer(), upload, fingerprint)
		return result, false, err
	}
	if upload.state != "active" {
		return nil, false, ErrNoSuchUpload
	}
	now := time.Now()
	if upload.expiresAt <= now.UnixMilli() {
		if err := abortMultipartUploadTx(ctx, tx.QueryExecer(), upload, now); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	if err := claimMultipartCompletion(ctx, tx.QueryExecer(), request.UploadID, now); err != nil {
		return nil, false, err
	}
	result, err := d.completeActiveMultipart(ctx, tx, request, upload, fingerprint, now)
	return result, false, err
}

func readCompletedMultipartResult(
	ctx context.Context,
	queryer database.IQueryer,
	upload storedMultipartUpload,
	fingerprint string,
) (*CompleteMultipartResult, error) {
	if upload.fingerprint != fingerprint {
		return nil, ErrNoSuchUpload
	}
	file, exists, err := readStoredFile(ctx, queryer, upload.resultFileID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrMultipartConflict
	}
	return &CompleteMultipartResult{
		FileID: upload.resultFileID,
		Size:   file.size,
		ETag:   upload.resultETag,
	}, nil
}

func claimMultipartCompletion(
	ctx context.Context,
	exec database.IExecer,
	uploadID string,
	now time.Time,
) error {
	result, err := exec.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_upload_tab
SET upload_state = 'completing', mtime = ?
WHERE upload_id = ? AND upload_state = 'active' AND expires_at > ?`,
		now.UnixMilli(),
		uploadID,
		now.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("claim multipart completion: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMultipartConflict
	}
	return nil
}

func (d *defaultFileManager) completeActiveMultipart(
	ctx context.Context,
	tx directory.ITransaction,
	request *CompleteMultipartRequest,
	upload storedMultipartUpload,
	fingerprint string,
	now time.Time,
) (*CompleteMultipartResult, error) {
	selected, totalSize, totalPartCount, err := loadCompletionParts(ctx, tx.QueryExecer(), request)
	if err != nil {
		return nil, err
	}
	if totalSize > multipartObjectSizeLimit(request.MaxObjectSize, d.bkio.MaxFileSize()) {
		return nil, ErrMultipartEntityTooLarge
	}
	if totalPartCount > maxFilePartCount {
		return nil, ErrTooManyFileParts
	}
	finalFileID := idgen.NextId()
	if err := insertCompositeFile(
		ctx,
		tx.QueryExecer(),
		finalFileID,
		selected,
		totalSize,
		totalPartCount,
		now,
	); err != nil {
		return nil, err
	}
	etag, err := multipartETag(selected)
	if err != nil {
		return nil, err
	}
	if _, err := publishS3ObjectTx(
		ctx,
		tx,
		"/"+request.Bucket+"/"+request.Key,
		finalFileID,
		totalSize,
		multipartObjectMetadata(upload, etag),
		request.Condition,
	); err != nil {
		return nil, err
	}
	if err := finalizeMultipartParts(ctx, tx.QueryExecer(), request.UploadID, selected, now); err != nil {
		return nil, err
	}
	if err := finishMultipartControl(
		ctx,
		tx.QueryExecer(),
		request.UploadID,
		fingerprint,
		finalFileID,
		etag,
		now,
	); err != nil {
		return nil, err
	}
	return &CompleteMultipartResult{FileID: finalFileID, Size: totalSize, ETag: etag}, nil
}

func insertCompositeFile(
	ctx context.Context,
	exec database.IExecer,
	finalFileID uint64,
	selected []MultipartPart,
	totalSize, totalPartCount int64,
	now time.Time,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`INSERT INTO tg_file_tab (
file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo, file_layout_version
) VALUES (?, ?, ?, ?, ?, ?, '{}', 2)`,
		finalFileID,
		totalSize,
		totalPartCount,
		constant.FileStateReady,
		now.UnixMilli(),
		now.UnixMilli(),
	); err != nil {
		return fmt.Errorf("insert composite final file: %w", err)
	}
	for index, part := range selected {
		if _, err := exec.ExecContext(
			ctx,
			`INSERT INTO tg_s3_file_segment_tab (
file_id, segment_index, source_file_id, segment_size, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?)`,
			finalFileID,
			index,
			part.FileID,
			part.Size,
			now.UnixMilli(),
			now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("insert composite segment %d: %w", index, err)
		}
	}
	return nil
}

func multipartObjectMetadata(upload storedMultipartUpload, etag string) *entity.S3ObjectMetadata {
	return &entity.S3ObjectMetadata{
		ETag:               etag,
		ContentType:        upload.contentType,
		CacheControl:       upload.cacheControl,
		ContentDisposition: upload.contentDisposition,
		ContentEncoding:    upload.contentEncoding,
		ContentLanguage:    upload.contentLanguage,
		Expires:            upload.expires,
		UserMetadata:       upload.userMetadata,
	}
}

func finalizeMultipartParts(
	ctx context.Context,
	tx database.IQueryExecer,
	uploadID string,
	selected []MultipartPart,
	now time.Time,
) error {
	allParts, err := listMultipartPartFileIDs(ctx, tx, uploadID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_part_tab
SET part_state = 'discarded', mtime = ?
WHERE upload_id = ? AND part_state = 'active'`,
		now.UnixMilli(),
		uploadID,
	); err != nil {
		return fmt.Errorf("discard multipart parts before selection: %w", err)
	}
	selectedSet, err := selectCompletedMultipartParts(ctx, tx, uploadID, selected, now)
	if err != nil {
		return err
	}
	for _, fileID := range allParts {
		if _, exists := selectedSet[fileID]; exists {
			continue
		}
		if err := markFileTreePendingIfUnreferenced(ctx, tx, fileID, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

func selectCompletedMultipartParts(
	ctx context.Context,
	exec database.IExecer,
	uploadID string,
	selected []MultipartPart,
	now time.Time,
) (map[uint64]struct{}, error) {
	selectedSet := make(map[uint64]struct{}, len(selected))
	for _, part := range selected {
		selectedSet[part.FileID] = struct{}{}
		result, err := exec.ExecContext(
			ctx,
			`UPDATE tg_s3_multipart_part_tab
SET part_state = 'selected', mtime = ?
WHERE upload_id = ? AND part_number = ? AND file_id = ? AND part_state = 'discarded'`,
			now.UnixMilli(),
			uploadID,
			part.PartNumber,
			part.FileID,
		)
		if err != nil {
			return nil, fmt.Errorf("select completed multipart part: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return nil, ErrMultipartConflict
		}
	}
	return selectedSet, nil
}

func finishMultipartControl(
	ctx context.Context,
	exec database.IExecer,
	uploadID, fingerprint string,
	finalFileID uint64,
	etag string,
	now time.Time,
) error {
	result, err := exec.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_upload_tab
SET upload_state = 'completed', completion_fingerprint = ?,
result_file_id = ?, result_etag = ?, completed_at = ?, cleanup_at = ?, mtime = ?
WHERE upload_id = ? AND upload_state = 'completing'`,
		fingerprint,
		finalFileID,
		etag,
		now.UnixMilli(),
		now.Add(multipartControlRetention).UnixMilli(),
		now.UnixMilli(),
		uploadID,
	)
	if err != nil {
		return fmt.Errorf("finish multipart upload: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMultipartConflict
	}
	return nil
}

func validateCompleteMultipartRequest(request *CompleteMultipartRequest) error {
	if request == nil || request.UploadID == "" || request.Bucket == "" || request.Key == "" ||
		len(request.Parts) == 0 || len(request.Parts) > maxS3MultipartParts {
		return ErrInvalidMultipartRequest
	}
	previous := 0
	for _, part := range request.Parts {
		if part.PartNumber <= previous {
			return ErrInvalidPartOrder
		}
		if part.PartNumber < 1 || part.PartNumber > maxS3MultipartParts ||
			len(part.ETag) != md5.Size*2 {
			return ErrInvalidMultipartPart
		}
		decoded, err := hex.DecodeString(part.ETag)
		if err != nil || len(decoded) != md5.Size || part.ETag != strings.ToLower(part.ETag) {
			return ErrInvalidMultipartPart
		}
		previous = part.PartNumber
	}
	return nil
}

func multipartCompletionFingerprint(parts []CompleteMultipartPart) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:%s\n", part.PartNumber, part.ETag)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func loadCompletionParts(
	ctx context.Context,
	queryer database.IQueryer,
	request *CompleteMultipartRequest,
) ([]MultipartPart, int64, int64, error) {
	selected := make([]MultipartPart, 0, len(request.Parts))
	var totalSize int64
	var totalPartCount int64
	for index, requested := range request.Parts {
		var part MultipartPart
		var fileState, fileLayout int
		var filePartCount int64
		err := queryRow(
			ctx,
			queryer,
			`SELECT part.part_number, part.file_id, part.part_size, part.part_etag,
file.file_state, file.file_layout_version, file.file_part_count
FROM tg_s3_multipart_part_tab part
JOIN tg_file_tab file ON file.file_id = part.file_id
WHERE part.upload_id = ? AND part.part_number = ? AND part.part_state = 'active'`,
			request.UploadID,
			requested.PartNumber,
		).Scan(
			&part.PartNumber,
			&part.FileID,
			&part.Size,
			&part.ETag,
			&fileState,
			&fileLayout,
			&filePartCount,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, 0, ErrInvalidMultipartPart
		}
		if err != nil {
			return nil, 0, 0, fmt.Errorf("scan completion part: %w", err)
		}
		if part.ETag != requested.ETag {
			return nil, 0, 0, ErrInvalidMultipartPart
		}
		if fileState != constant.FileStateReady || fileLayout != 1 {
			return nil, 0, 0, ErrInvalidMultipartPart
		}
		if index != len(request.Parts)-1 && part.Size < minS3MultipartPartSize {
			return nil, 0, 0, ErrMultipartPartTooSmall
		}
		if err := ensureMultipartStagingFileLive(ctx, queryer, storedFileRecord{
			fileID:    part.FileID,
			partCount: filePartCount,
		}); err != nil {
			return nil, 0, 0, err
		}
		var mappingCount int64
		if err := queryRow(
			ctx,
			queryer,
			"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = ?",
			strconv.FormatUint(part.FileID, 10),
		).Scan(&mappingCount); err != nil {
			return nil, 0, 0, fmt.Errorf("count completion source mappings: %w", err)
		}
		if mappingCount != 0 || totalSize > maxS3MultipartPartSize*maxS3MultipartParts-part.Size {
			return nil, 0, 0, ErrInvalidMultipartPart
		}
		totalSize += part.Size
		totalPartCount += filePartCount
		selected = append(selected, part)
	}
	return selected, totalSize, totalPartCount, nil
}

func multipartETag(parts []MultipartPart) (string, error) {
	hash := md5.New() //nolint:gosec // S3 Multipart ETag compatibility requires MD5.
	for _, part := range parts {
		raw, err := hex.DecodeString(part.ETag)
		if err != nil || len(raw) != md5.Size {
			return "", ErrInvalidMultipartPart
		}
		_, _ = hash.Write(raw)
	}
	return fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(hash.Sum(nil)), len(parts)), nil
}

func listMultipartPartFileIDs(
	ctx context.Context,
	queryer database.IQueryer,
	uploadID string,
) ([]uint64, error) {
	fileIDs, err := queryFileIDList(
		ctx,
		queryer,
		`SELECT file_id FROM tg_s3_multipart_part_tab
WHERE upload_id = ? AND part_state = 'active' ORDER BY part_number`,
		uploadID,
	)
	if err != nil {
		return nil, fmt.Errorf("query multipart part files: %w", err)
	}
	return fileIDs, nil
}

func (d *defaultFileManager) AbortMultipartUpload(
	ctx context.Context,
	request *AbortMultipartRequest,
) error {
	if request == nil || request.UploadID == "" || request.Bucket == "" || request.Key == "" {
		return ErrInvalidMultipartRequest
	}
	if err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		upload, exists, err := readMultipartUpload(ctx, tx, request.UploadID)
		if err != nil {
			return err
		}
		if !matchingMultipartUpload(upload, exists, request.Bucket, request.Key) {
			return ErrNoSuchUpload
		}
		switch upload.state {
		case "aborted":
			return nil
		case "completed":
			return ErrNoSuchUpload
		case "active":
			return abortMultipartUploadTx(ctx, tx, upload, time.Now())
		default:
			return ErrMultipartConflict
		}
	}); err != nil {
		return fmt.Errorf("abort multipart upload transaction: %w", err)
	}
	return nil
}

func abortMultipartUploadTx(
	ctx context.Context,
	tx database.IQueryExecer,
	upload storedMultipartUpload,
	now time.Time,
) error {
	fileIDs, err := listMultipartPartFileIDs(ctx, tx, upload.uploadID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_upload_tab
SET upload_state = 'aborted', cleanup_at = ?, mtime = ?
WHERE upload_id = ? AND upload_state = 'active'`,
		now.Add(multipartControlRetention).UnixMilli(),
		now.UnixMilli(),
		upload.uploadID,
	)
	if err != nil {
		return fmt.Errorf("abort multipart upload: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return ErrMultipartConflict
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE tg_s3_multipart_part_tab
SET part_state = 'discarded', mtime = ?
WHERE upload_id = ? AND part_state = 'active'`,
		now.UnixMilli(),
		upload.uploadID,
	); err != nil {
		return fmt.Errorf("discard aborted multipart parts: %w", err)
	}
	for _, fileID := range fileIDs {
		if err := markFileTreePendingIfUnreferenced(ctx, tx, fileID, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

type multipartListProjection struct {
	key       string
	uploadID  string
	initiated int64
	common    bool
}

const multipartUploadProjectionSQL = `WITH parameters AS (
    SELECT ? AS prefix, ? AS delimiter, ? AS key_marker,
           ? AS upload_id_marker, ? AS current_time
),
matching AS (
    SELECT upload.object_key, upload.upload_id, upload.initiated_at,
           parameters.prefix, parameters.delimiter,
           substr(upload.object_key, length(parameters.prefix) + 1) AS remainder
    FROM tg_s3_multipart_upload_tab upload
    CROSS JOIN parameters
    WHERE upload.bucket_name = ?
      AND upload.upload_state = 'active'
      AND upload.expires_at > parameters.current_time
      AND substr(upload.object_key, 1, length(parameters.prefix)) = parameters.prefix
),
projected AS (
    SELECT
        CASE
            WHEN delimiter = '/' AND instr(remainder, '/') > 0
            THEN prefix || substr(remainder, 1, instr(remainder, '/'))
            ELSE object_key
        END AS projected_key,
        CASE
            WHEN delimiter = '/' AND instr(remainder, '/') > 0
            THEN ''
            ELSE upload_id
        END AS projected_upload_id,
        CASE
            WHEN delimiter = '/' AND instr(remainder, '/') > 0
            THEN 0
            ELSE initiated_at
        END AS projected_initiated_at,
        CASE
            WHEN delimiter = '/' AND instr(remainder, '/') > 0
            THEN 1
            ELSE 0
        END AS is_common_prefix
    FROM matching
),
deduplicated AS (
    SELECT projected_key, projected_upload_id,
           MAX(projected_initiated_at) AS projected_initiated_at,
           is_common_prefix
    FROM projected
    GROUP BY projected_key, projected_upload_id, is_common_prefix
)
SELECT projected_key, projected_upload_id, projected_initiated_at, is_common_prefix
FROM deduplicated
CROSS JOIN parameters
WHERE projected_key > parameters.key_marker
   OR (
       projected_key = parameters.key_marker
       AND parameters.upload_id_marker != ''
       AND projected_upload_id > parameters.upload_id_marker
   )
ORDER BY projected_key, projected_upload_id
LIMIT ?`

func (d *defaultFileManager) ListMultipartUploads(
	ctx context.Context,
	request *ListMultipartUploadsRequest,
) (*MultipartUploadPage, error) {
	if request == nil || request.Bucket == "" || request.MaxUploads < 0 || request.MaxUploads > 1000 ||
		request.Delimiter != "" && request.Delimiter != "/" {
		return nil, ErrInvalidMultipartRequest
	}
	if err := d.processExpiredMultipartUploads(ctx, time.Now(), multipartCleanupBatchSize); err != nil {
		return nil, err
	}
	page := &MultipartUploadPage{
		Uploads:        make([]MultipartUploadListItem, 0, request.MaxUploads),
		CommonPrefixes: make([]string, 0),
	}
	if request.MaxUploads == 0 {
		return page, nil
	}
	projections, err := readMultipartUploadProjections(ctx, d.dbc, request)
	if err != nil {
		return nil, err
	}
	return buildMultipartUploadPage(page, projections, request.MaxUploads), nil
}

func readMultipartUploadProjections(
	ctx context.Context,
	queryer database.IQueryer,
	request *ListMultipartUploadsRequest,
) ([]multipartListProjection, error) {
	rows, err := queryer.QueryContext(
		ctx,
		multipartUploadProjectionSQL,
		request.Prefix,
		request.Delimiter,
		request.KeyMarker,
		request.UploadIDMarker,
		time.Now().UnixMilli(),
		request.Bucket,
		request.MaxUploads+1,
	)
	if err != nil {
		return nil, fmt.Errorf("query multipart uploads: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	projections := make([]multipartListProjection, 0, request.MaxUploads+1)
	for rows.Next() {
		var projection multipartListProjection
		if err := rows.Scan(
			&projection.key,
			&projection.uploadID,
			&projection.initiated,
			&projection.common,
		); err != nil {
			return nil, fmt.Errorf("scan multipart upload list: %w", err)
		}
		projections = append(projections, projection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multipart upload list: %w", err)
	}
	return projections, nil
}

func buildMultipartUploadPage(
	page *MultipartUploadPage,
	projections []multipartListProjection,
	maxUploads int,
) *MultipartUploadPage {
	if len(projections) > maxUploads {
		page.IsTruncated = true
		projections = projections[:maxUploads]
	}
	for _, projection := range projections {
		page.NextKeyMarker = projection.key
		page.NextUploadIDMarker = projection.uploadID
		if projection.common {
			page.CommonPrefixes = append(page.CommonPrefixes, projection.key)
			continue
		}
		page.Uploads = append(page.Uploads, MultipartUploadListItem{
			Key:       projection.key,
			UploadID:  projection.uploadID,
			Initiated: time.UnixMilli(projection.initiated),
		})
	}
	if !page.IsTruncated {
		page.NextKeyMarker = ""
		page.NextUploadIDMarker = ""
	}
	return page
}

func (d *defaultFileManager) RunMultipartCleanupWorker(ctx context.Context) error {
	d.runMultipartCleanupPass(ctx)
	ticker := time.NewTicker(multipartCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.runMultipartCleanupPass(ctx)
		}
	}
}

func (d *defaultFileManager) runMultipartCleanupPass(ctx context.Context) {
	now := time.Now()
	if err := d.processExpiredMultipartUploads(ctx, now, multipartCleanupBatchSize); err != nil {
		logutil.GetLogger(ctx).Error(
			"multipart expiry cleanup failed",
			zap.String("error_code", "database"),
		)
	}
	if err := d.purgeMultipartControlRows(ctx, now, multipartCleanupBatchSize); err != nil {
		logutil.GetLogger(ctx).Error(
			"multipart control cleanup failed",
			zap.String("error_code", "database"),
		)
	}
}

func (d *defaultFileManager) processExpiredMultipartUploads(
	ctx context.Context,
	now time.Time,
	limit int,
) error {
	if err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		uploadIDs, err := queryMultipartUploadIDs(
			ctx,
			tx,
			`SELECT upload_id FROM tg_s3_multipart_upload_tab
WHERE upload_state = 'active' AND expires_at <= ?
ORDER BY expires_at LIMIT ?`,
			now.UnixMilli(),
			limit,
		)
		if err != nil {
			return fmt.Errorf("query expired multipart uploads: %w", err)
		}
		for _, uploadID := range uploadIDs {
			upload, exists, err := readMultipartUpload(ctx, tx, uploadID)
			if err != nil {
				return err
			}
			if !exists || upload.state != "active" {
				continue
			}
			if err := abortMultipartUploadTx(ctx, tx, upload, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("expire multipart uploads transaction: %w", err)
	}
	return nil
}

func (d *defaultFileManager) purgeMultipartControlRows(
	ctx context.Context,
	now time.Time,
	limit int,
) error {
	if err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		uploadIDs, err := queryMultipartUploadIDs(
			ctx,
			tx,
			`SELECT upload_id FROM tg_s3_multipart_upload_tab
WHERE upload_state IN ('completed', 'aborted') AND cleanup_at > 0 AND cleanup_at <= ?
ORDER BY cleanup_at LIMIT ?`,
			now.UnixMilli(),
			limit,
		)
		if err != nil {
			return fmt.Errorf("query terminal multipart uploads: %w", err)
		}
		for _, uploadID := range uploadIDs {
			if _, err := tx.ExecContext(
				ctx,
				"DELETE FROM tg_s3_multipart_part_tab WHERE upload_id = ?",
				uploadID,
			); err != nil {
				return fmt.Errorf("delete multipart part controls: %w", err)
			}
			if _, err := tx.ExecContext(
				ctx,
				`DELETE FROM tg_s3_multipart_upload_tab
WHERE upload_id = ? AND upload_state IN ('completed', 'aborted')`,
				uploadID,
			); err != nil {
				return fmt.Errorf("delete multipart upload control: %w", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("purge multipart controls transaction: %w", err)
	}
	return nil
}

func queryMultipartUploadIDs(
	ctx context.Context,
	queryer database.IQueryer,
	query string,
	args ...any,
) ([]string, error) {
	return queryColumnList[string](ctx, queryer, query, args...)
}
