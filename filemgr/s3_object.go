package filemgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/mimetype"
)

const defaultS3CacheControl = "public, max-age=604800"

func (d *defaultFileManager) StatS3Object(ctx context.Context, objectPath string) (*S3ObjectInfo, error) {
	entry, err := d.objectDir.Stat(ctx, objectPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("stat S3 object path %q: %w", objectPath, err)
	}
	if entry.IsDir() {
		return nil, os.ErrNotExist
	}
	link, err := directoryEntryToLink(objectPath, entry)
	if err != nil {
		return nil, err
	}
	metadata, found, err := readS3Metadata(ctx, d.dbc, entry.EntryID())
	if err != nil {
		return nil, err
	}
	if !found {
		metadata = legacyS3Metadata(link)
	}
	return &S3ObjectInfo{Link: link, Metadata: metadata}, nil
}

func directoryEntryToLink(objectPath string, entry directory.IDirectoryEntry) (*entity.FileLinkMeta, error) {
	fileID, err := strconv.ParseUint(entry.RefData(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse S3 object file id: %w", err)
	}
	return &entity.FileLinkMeta{
		EntryID:  entry.EntryID(),
		FileName: objectPath,
		FileId:   fileID,
		FileSize: entry.Size(),
		Mode:     entry.Mode(),
		Ctime:    entry.Ctime(),
		Mtime:    entry.Mtime(),
		IsDir:    false,
	}, nil
}

func legacyS3Metadata(link *entity.FileLinkMeta) *entity.S3ObjectMetadata {
	extension := ""
	if index := strings.LastIndexByte(link.FileName, '.'); index >= 0 {
		extension = link.FileName[index:]
	}
	return &entity.S3ObjectMetadata{
		EntryID:      link.EntryID,
		ETag:         fmt.Sprintf(`W/"%d"`, link.FileId),
		ContentType:  mimetype.LookupWithDefault(extension, "application/octet-stream"),
		CacheControl: defaultS3CacheControl,
		UserMetadata: "{}",
		Ctime:        link.Ctime,
		Mtime:        link.Mtime,
	}
}

func readS3Metadata(
	ctx context.Context,
	queryer database.IQueryer,
	entryID uint64,
) (*entity.S3ObjectMetadata, bool, error) {
	const query = `SELECT entry_id, etag, checksum_sha256, request_checksum_algorithm,
request_checksum_value, content_type, cache_control, content_disposition, content_encoding,
content_language, expires, user_metadata, ctime, mtime
FROM tg_s3_object_metadata_tab WHERE entry_id = ?`
	row := queryRow(ctx, queryer, query, entryID)
	var metadata entity.S3ObjectMetadata
	err := row.Scan(
		&metadata.EntryID,
		&metadata.ETag,
		&metadata.ChecksumSHA256,
		&metadata.RequestChecksumAlgorithm,
		&metadata.RequestChecksumValue,
		&metadata.ContentType,
		&metadata.CacheControl,
		&metadata.ContentDisposition,
		&metadata.ContentEncoding,
		&metadata.ContentLanguage,
		&metadata.Expires,
		&metadata.UserMetadata,
		&metadata.Ctime,
		&metadata.Mtime,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("scan S3 object metadata: %w", err)
	}
	return &metadata, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func queryRow(ctx context.Context, queryer database.IQueryer, query string, args ...any) rowScanner {
	if databaseHandle, ok := queryer.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}); ok {
		return databaseHandle.QueryRowContext(ctx, query, args...)
	}
	return &rowsRow{ctx: ctx, queryer: queryer, query: query, args: args}
}

type rowsRow struct {
	ctx     context.Context
	queryer database.IQueryer
	query   string
	args    []any
}

func (r *rowsRow) Scan(dest ...any) error {
	rows, err := r.queryer.QueryContext(r.ctx, r.query, r.args...)
	if err != nil {
		return fmt.Errorf("query single row: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate single row: %w", err)
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return fmt.Errorf("scan single row: %w", err)
	}
	return nil
}

func insertS3Metadata(
	ctx context.Context,
	exec database.IExecer,
	metadata *entity.S3ObjectMetadata,
) error {
	const statement = `INSERT INTO tg_s3_object_metadata_tab (
entry_id, etag, checksum_sha256, request_checksum_algorithm, request_checksum_value,
content_type, cache_control, content_disposition, content_encoding, content_language,
expires, user_metadata, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := exec.ExecContext(
		ctx,
		statement,
		metadata.EntryID,
		metadata.ETag,
		metadata.ChecksumSHA256,
		metadata.RequestChecksumAlgorithm,
		metadata.RequestChecksumValue,
		metadata.ContentType,
		metadata.CacheControl,
		metadata.ContentDisposition,
		metadata.ContentEncoding,
		metadata.ContentLanguage,
		metadata.Expires,
		metadata.UserMetadata,
		metadata.Ctime,
		metadata.Mtime,
	)
	if err != nil {
		return fmt.Errorf("insert S3 object metadata: %w", err)
	}
	return nil
}

func evaluateS3Condition(info *S3ObjectInfo, condition *S3Condition) error {
	if condition == nil {
		return nil
	}
	exists := info != nil
	metadata := metadataFromInfo(info)
	if !s3IfMatchSatisfied(exists, metadata, condition.IfMatch) {
		return ErrS3Precondition
	}
	if !s3IfNoneMatchSatisfied(exists, metadata, condition.IfNoneMatch) {
		return ErrS3Precondition
	}
	modifiedAt := time.Time{}
	if exists {
		modifiedAt = time.UnixMilli(info.Link.Mtime).Truncate(time.Second)
	}
	if exists && condition.IfMatch == "" && condition.IfUnmodifiedSince != nil &&
		modifiedAt.After(condition.IfUnmodifiedSince.Truncate(time.Second)) {
		return ErrS3Precondition
	}
	if exists && condition.IfNoneMatch == "" && condition.IfModifiedSince != nil &&
		!modifiedAt.After(condition.IfModifiedSince.Truncate(time.Second)) {
		return ErrS3Precondition
	}
	return nil
}

func s3IfMatchSatisfied(exists bool, metadata *entity.S3ObjectMetadata, value string) bool {
	if value == "" {
		return true
	}
	if !exists {
		return false
	}
	if value == "*" {
		return true
	}
	if strings.HasPrefix(metadata.ETag, "W/") {
		return false
	}
	return s3ETagListContains(value, metadata.ETag, false)
}

func s3IfNoneMatchSatisfied(exists bool, metadata *entity.S3ObjectMetadata, value string) bool {
	if value == "" || !exists {
		return true
	}
	return value != "*" && !s3ETagListContains(value, metadata.ETag, true)
}

func s3ETagListContains(value, etag string, weak bool) bool {
	if weak {
		etag = strings.TrimPrefix(etag, "W/")
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if weak {
			candidate = strings.TrimPrefix(candidate, "W/")
		}
		if candidate == etag {
			return true
		}
	}
	return false
}

func (d *defaultFileManager) PublishS3Object(
	ctx context.Context,
	objectPath string,
	fileID uint64,
	size int64,
	metadata *entity.S3ObjectMetadata,
	condition *S3Condition,
) (*S3ObjectInfo, error) {
	var published *S3ObjectInfo
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		var err error
		published, err = publishS3ObjectTx(ctx, tx, objectPath, fileID, size, metadata, condition)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("publish S3 object: %w", err)
	}
	return published, nil
}

func publishS3ObjectTx(
	ctx context.Context,
	tx directory.ITransaction,
	objectPath string,
	fileID uint64,
	size int64,
	metadata *entity.S3ObjectMetadata,
	condition *S3Condition,
) (*S3ObjectInfo, error) {
	current, _, err := statS3ObjectTx(ctx, tx, objectPath)
	if err != nil {
		return nil, err
	}
	if err := evaluateS3Condition(current, condition); err != nil {
		return nil, err
	}
	if err := ensureFileCanBeLinked(ctx, tx.QueryExecer(), fileID); err != nil {
		return nil, err
	}
	replacedFileID, err := removeReplacedS3Object(ctx, tx, objectPath, current)
	if err != nil {
		return nil, err
	}
	entry, err := tx.Create(ctx, objectPath, size, strconv.FormatUint(fileID, 10))
	if err != nil {
		return nil, fmt.Errorf("create S3 mapping: %w", err)
	}
	now := time.Now().UnixMilli()
	stored := *metadata
	stored.EntryID = entry.EntryID()
	stored.Ctime = now
	stored.Mtime = now
	if err := insertS3Metadata(ctx, tx.QueryExecer(), &stored); err != nil {
		return nil, err
	}
	if replacedFileID != 0 && replacedFileID != fileID {
		if err := markFilePendingIfUnreferenced(ctx, tx.QueryExecer(), replacedFileID, now); err != nil {
			return nil, err
		}
	}
	link, err := directoryEntryToLink(objectPath, entry)
	if err != nil {
		return nil, err
	}
	return &S3ObjectInfo{Link: link, Metadata: &stored}, nil
}

func removeReplacedS3Object(
	ctx context.Context,
	tx directory.ITransaction,
	objectPath string,
	current *S3ObjectInfo,
) (uint64, error) {
	if current == nil {
		return 0, nil
	}
	if _, err := tx.Remove(ctx, objectPath); err != nil {
		return 0, fmt.Errorf("remove replaced S3 mapping: %w", err)
	}
	if err := deleteS3Metadata(ctx, tx.QueryExecer(), current.Link.EntryID); err != nil {
		return 0, err
	}
	return current.Link.FileId, nil
}

func metadataFromInfo(info *S3ObjectInfo) *entity.S3ObjectMetadata {
	if info == nil {
		return nil
	}
	return info.Metadata
}

func statS3ObjectTx(
	ctx context.Context,
	tx directory.ITransaction,
	objectPath string,
) (*S3ObjectInfo, bool, error) {
	entry, exists, err := tx.Stat(ctx, objectPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat S3 object transaction: %w", err)
	}
	if !exists || entry.IsDir() {
		return nil, false, nil
	}
	link, err := directoryEntryToLink(objectPath, entry)
	if err != nil {
		return nil, false, err
	}
	metadata, found, err := readS3Metadata(ctx, tx.QueryExecer(), entry.EntryID())
	if err != nil {
		return nil, false, err
	}
	if !found {
		metadata = legacyS3Metadata(link)
	}
	return &S3ObjectInfo{Link: link, Metadata: metadata}, true, nil
}

func ensureFileCanBeLinked(ctx context.Context, queryer database.IQueryer, fileID uint64) error {
	const query = `SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND delete_state != 'live'`
	var count int64
	if err := queryRow(ctx, queryer, query, fileID).Scan(&count); err != nil {
		return fmt.Errorf("check S3 file delete state: %w", err)
	}
	if count != 0 {
		return ErrS3ObjectConflict
	}
	return nil
}

func deleteS3Metadata(ctx context.Context, exec database.IExecer, entryID uint64) error {
	if _, err := exec.ExecContext(ctx, "DELETE FROM tg_s3_object_metadata_tab WHERE entry_id = ?", entryID); err != nil {
		return fmt.Errorf("delete S3 object metadata: %w", err)
	}
	return nil
}

func markFilePendingIfUnreferenced(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	fileID uint64,
	now int64,
) error {
	var count int64
	if err := queryRow(
		ctx,
		queryExecer,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = ?",
		strconv.FormatUint(fileID, 10),
	).Scan(&count); err != nil {
		return fmt.Errorf("count S3 file mappings: %w", err)
	}
	if count != 0 {
		return nil
	}
	if _, err := queryExecer.ExecContext(
		ctx,
		`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'pending', next_attempt_at = ?, mtime = ?
WHERE file_id = ? AND delete_state = 'live'`,
		now,
		now,
		fileID,
	); err != nil {
		return fmt.Errorf("mark unreferenced S3 blocks pending: %w", err)
	}
	return nil
}

func (d *defaultFileManager) CopyS3Object(
	ctx context.Context,
	source string,
	destination string,
	metadata *entity.S3ObjectMetadata,
	sourceCondition *S3Condition,
	destinationCondition *S3Condition,
) (*S3ObjectInfo, error) {
	var copied *S3ObjectInfo
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		var err error
		copied, err = copyS3ObjectTx(
			ctx,
			tx,
			source,
			destination,
			metadata,
			sourceCondition,
			destinationCondition,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("copy S3 object: %w", err)
	}
	return copied, nil
}

func copyS3ObjectTx(
	ctx context.Context,
	tx directory.ITransaction,
	source, destination string,
	metadata *entity.S3ObjectMetadata,
	sourceCondition, destinationCondition *S3Condition,
) (*S3ObjectInfo, error) {
	sourceInfo, sourceExists, err := statS3ObjectTx(ctx, tx, source)
	if err != nil {
		return nil, err
	}
	if !sourceExists {
		return nil, os.ErrNotExist
	}
	if err := evaluateS3Condition(sourceInfo, sourceCondition); err != nil {
		return nil, err
	}
	destinationInfo, _, err := statS3ObjectTx(ctx, tx, destination)
	if err != nil {
		return nil, err
	}
	if err := evaluateS3Condition(destinationInfo, destinationCondition); err != nil {
		return nil, err
	}
	if source == destination {
		return copySameS3ObjectTx(ctx, tx, source, sourceInfo, metadata)
	}
	return copyDifferentS3ObjectTx(ctx, tx, destination, sourceInfo, destinationInfo, metadata)
}

func copySameS3ObjectTx(
	ctx context.Context,
	tx directory.ITransaction,
	source string,
	sourceInfo *S3ObjectInfo,
	metadata *entity.S3ObjectMetadata,
) (*S3ObjectInfo, error) {
	if metadata == nil {
		return sourceInfo, nil
	}
	now := time.Now().UnixMilli()
	stored := *metadata
	stored.EntryID = sourceInfo.Link.EntryID
	stored.Ctime = sourceInfo.Metadata.Ctime
	stored.Mtime = now
	if err := deleteS3Metadata(ctx, tx.QueryExecer(), stored.EntryID); err != nil {
		return nil, err
	}
	if err := insertS3Metadata(ctx, tx.QueryExecer(), &stored); err != nil {
		return nil, err
	}
	if err := tx.Touch(ctx, source, now); err != nil {
		return nil, fmt.Errorf("touch same-key S3 copy: %w", err)
	}
	sourceInfo.Link.Mtime = now
	sourceInfo.Metadata = &stored
	return sourceInfo, nil
}

func copyDifferentS3ObjectTx(
	ctx context.Context,
	tx directory.ITransaction,
	destination string,
	sourceInfo, destinationInfo *S3ObjectInfo,
	metadata *entity.S3ObjectMetadata,
) (*S3ObjectInfo, error) {
	replacedFileID, err := removeReplacedS3Object(ctx, tx, destination, destinationInfo)
	if err != nil {
		return nil, err
	}
	if err := ensureFileCanBeLinked(ctx, tx.QueryExecer(), sourceInfo.Link.FileId); err != nil {
		return nil, err
	}
	entry, err := tx.Create(
		ctx,
		destination,
		sourceInfo.Link.FileSize,
		strconv.FormatUint(sourceInfo.Link.FileId, 10),
	)
	if err != nil {
		return nil, fmt.Errorf("create S3 copy destination: %w", err)
	}
	now := time.Now().UnixMilli()
	stored := *sourceInfo.Metadata
	if metadata != nil {
		stored = *metadata
	}
	stored.EntryID = entry.EntryID()
	stored.Ctime = now
	stored.Mtime = now
	if err := insertS3Metadata(ctx, tx.QueryExecer(), &stored); err != nil {
		return nil, err
	}
	if replacedFileID != 0 && replacedFileID != sourceInfo.Link.FileId {
		if err := markFilePendingIfUnreferenced(ctx, tx.QueryExecer(), replacedFileID, now); err != nil {
			return nil, err
		}
	}
	link, err := directoryEntryToLink(destination, entry)
	if err != nil {
		return nil, err
	}
	return &S3ObjectInfo{Link: link, Metadata: &stored}, nil
}

func (d *defaultFileManager) DeleteS3Object(
	ctx context.Context,
	objectPath string,
	condition *S3Condition,
) (bool, error) {
	deleted := false
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		current, exists, err := statS3ObjectTx(ctx, tx, objectPath)
		if err != nil {
			return err
		}
		if err := evaluateS3Condition(current, condition); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if _, err := tx.Remove(ctx, objectPath); err != nil {
			return fmt.Errorf("remove S3 object mapping: %w", err)
		}
		if err := deleteS3Metadata(ctx, tx.QueryExecer(), current.Link.EntryID); err != nil {
			return err
		}
		if err := markFilePendingIfUnreferenced(
			ctx,
			tx.QueryExecer(),
			current.Link.FileId,
			time.Now().UnixMilli(),
		); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("delete S3 object: %w", err)
	}
	return deleted, nil
}

type s3ListPageEntry struct {
	key                    string
	refData                string
	common                 int
	entryID                uint64
	ctime, mtime, fileSize int64
}

func (d *defaultFileManager) ListS3Objects(
	ctx context.Context,
	request *S3ListRequest,
) (*S3ListResult, error) {
	result := &S3ListResult{
		Items:          make([]S3ListItem, 0, request.MaxKeys),
		CommonPrefixes: make([]string, 0),
	}
	if request.MaxKeys == 0 {
		return result, nil
	}
	start := request.StartAfter
	if request.ContinuationToken != "" {
		start = request.ContinuationToken
	}
	rows, err := d.queryS3ListPage(ctx, request, start)
	if err != nil {
		return nil, err
	}
	page, truncated, err := readS3ListPage(rows, request.MaxKeys)
	if err != nil {
		return nil, err
	}
	result.IsTruncated = truncated
	for _, entry := range page {
		if err := d.appendS3ListEntry(ctx, request, result, entry); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func readS3ListPage(rows *sql.Rows, maxKeys int) ([]s3ListPageEntry, bool, error) {
	defer func() {
		_ = rows.Close()
	}()
	page := make([]s3ListPageEntry, 0, maxKeys)
	truncated := false
	for rows.Next() {
		var entry s3ListPageEntry
		if err := rows.Scan(
			&entry.key,
			&entry.common,
			&entry.entryID,
			&entry.refData,
			&entry.ctime,
			&entry.mtime,
			&entry.fileSize,
		); err != nil {
			return nil, false, fmt.Errorf("scan S3 object list page: %w", err)
		}
		if len(page) == maxKeys {
			truncated = true
			break
		}
		page = append(page, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate S3 object list page: %w", err)
	}
	return page, truncated, nil
}

func (d *defaultFileManager) appendS3ListEntry(
	ctx context.Context,
	request *S3ListRequest,
	result *S3ListResult,
	entry s3ListPageEntry,
) error {
	result.NextKey = entry.key
	if entry.common != 0 {
		result.CommonPrefixes = append(result.CommonPrefixes, entry.key)
		return nil
	}
	fileID, err := strconv.ParseUint(entry.refData, 10, 64)
	if err != nil {
		return fmt.Errorf("parse listed S3 file id: %w", err)
	}
	objectPath := "/" + request.Bucket + "/" + entry.key
	link := &entity.FileLinkMeta{
		EntryID:  entry.entryID,
		FileName: objectPath,
		FileId:   fileID,
		FileSize: entry.fileSize,
		Ctime:    entry.ctime,
		Mtime:    entry.mtime,
	}
	metadata, found, err := readS3Metadata(ctx, d.dbc, link.EntryID)
	if err != nil {
		return err
	}
	if !found {
		metadata = legacyS3Metadata(link)
	}
	result.Items = append(result.Items, S3ListItem{
		Key:          entry.key,
		Size:         link.FileSize,
		LastModified: link.Mtime,
		ETag:         metadata.ETag,
	})
	return nil
}

func (d *defaultFileManager) queryS3ListPage(
	ctx context.Context,
	request *S3ListRequest,
	start string,
) (*sql.Rows, error) {
	const query = `WITH RECURSIVE tree (
entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime, file_size, file_name, full_path
) AS (
SELECT entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime, file_size, file_name, '/'
FROM tg_file_mapping_tab WHERE parent_entry_id = 0 AND file_name = '/'
UNION ALL
SELECT child.entry_id, child.parent_entry_id, child.ref_data, child.file_kind,
child.ctime, child.mtime, child.file_size, child.file_name,
CASE WHEN tree.full_path = '/' THEN '/' || child.file_name
ELSE tree.full_path || '/' || child.file_name END
FROM tg_file_mapping_tab child JOIN tree ON child.parent_entry_id = tree.entry_id
),
objects AS (
SELECT entry_id, ref_data, ctime, mtime, file_size,
       substr(full_path, ?) AS object_key
FROM tree
WHERE file_kind = 2 AND full_path LIKE ? ESCAPE '\'
),
classified AS (
SELECT *,
       CASE WHEN ? = '/' THEN instr(substr(object_key, length(?) + 1), '/') ELSE 0 END AS delimiter_index
FROM objects
),
projected AS (
SELECT CASE WHEN delimiter_index > 0
            THEN ? || substr(substr(object_key, length(?) + 1), 1, delimiter_index)
            ELSE object_key END AS item_key,
       CASE WHEN delimiter_index > 0 THEN 1 ELSE 0 END AS is_common,
       entry_id, ref_data, ctime, mtime, file_size
FROM classified
),
deduplicated AS (
SELECT item_key, MAX(is_common) AS is_common, MIN(entry_id) AS entry_id,
       MIN(ref_data) AS ref_data, MIN(ctime) AS ctime, MIN(mtime) AS mtime,
       MIN(file_size) AS file_size
FROM projected
GROUP BY item_key
)
SELECT item_key, is_common, entry_id, ref_data, ctime, mtime, file_size
FROM deduplicated
WHERE item_key > ?
ORDER BY item_key
LIMIT ?`
	pathPrefix := "/" + request.Bucket + "/"
	pattern := escapeSQLiteLike(pathPrefix+request.Prefix) + "%"
	rows, err := d.dbc.QueryContext(
		ctx,
		query,
		len(pathPrefix)+1,
		pattern,
		request.Delimiter,
		request.Prefix,
		request.Prefix,
		request.Prefix,
		start,
		request.MaxKeys+1,
	)
	if err != nil {
		return nil, fmt.Errorf("query S3 object list page: %w", err)
	}
	return rows, nil
}

func escapeSQLiteLike(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	)
	return replacer.Replace(value)
}
