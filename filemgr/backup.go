package filemgr

import (
	"context"
	"crypto/md5" //nolint:gosec // Logical backups preserve tgfile's compatibility digest.
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"
)

var (
	ErrBackupConflict     = errors.New("backup import path conflicts with existing data")
	ErrBackupState        = errors.New("backup staging state is invalid")
	ErrBackupCompensation = errors.New(
		"backup upload could not be durably tracked or directly deleted",
	)
)

func (d *defaultFileManager) BackupMaxPartSize() int64 {
	return d.bkio.MaxFileSize()
}

type backupMappingRow struct {
	entryID  uint64
	parentID uint64
	refData  string
	kind     int
	ctime    int64
	mtime    int64
	size     int64
	mode     uint32
	name     string
	fullPath string
}

type snapshotFile struct {
	id              uint64
	item            backupfmt.File
	segmentSourceID []uint64
}

func (d *defaultFileManager) CreateBackupSnapshot(
	ctx context.Context,
	request BackupSnapshotRequest,
) (*backupfmt.Manifest, error) {
	scope := path.Clean(request.Scope)
	if request.JobID == "" || !strings.HasPrefix(scope, "/") {
		return nil, fmt.Errorf("%w: invalid snapshot request", ErrBackupState)
	}
	manifest := newBackupManifest(request, scope, d.bkio.Name(), d.bkio.MaxFileSize())
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return d.populateBackupSnapshotTx(ctx, tx, request, manifest)
	})
	if err != nil {
		return nil, fmt.Errorf("create backup snapshot: %w", err)
	}
	if err := d.fillCompositeBackupDigests(ctx, request.JobID, manifest); err != nil {
		return nil, err
	}
	sortBackupManifest(manifest)
	fillBackupSummary(manifest)
	return manifest, nil
}

func newBackupManifest(
	request BackupSnapshotRequest,
	scope, backendName string,
	maxPartSize int64,
) *backupfmt.Manifest {
	return &backupfmt.Manifest{
		Format:    backupfmt.FormatName,
		Version:   backupfmt.FormatVersion,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Scope:     scope,
		Source: backupfmt.Source{
			SchemaVersion: request.SchemaVersion,
			BlockIOKind:   backendName,
			MaxPartSize:   maxPartSize,
		},
		RequiredBuckets:  append([]backupfmt.RequiredBucket(nil), request.RequiredBuckets...),
		Files:            []backupfmt.File{},
		Directories:      []backupfmt.Directory{},
		Mappings:         []backupfmt.Mapping{},
		S3Objects:        []backupfmt.S3Object{},
		WebDAVProperties: []backupfmt.WebDAVProperty{},
	}
}

func (d *defaultFileManager) populateBackupSnapshotTx(
	ctx context.Context,
	tx database.IQueryExecer,
	request BackupSnapshotRequest,
	manifest *backupfmt.Manifest,
) error {
	rows, err := readBackupMappings(ctx, tx)
	if err != nil {
		return err
	}
	selected, directories, err := selectBackupMappings(rows, manifest.Scope)
	if err != nil {
		return err
	}
	manifest.RequiredBuckets = usedBackupBuckets(selected, directories, request.RequiredBuckets)
	snapshots, err := d.loadSelectedBackupFiles(ctx, tx, selected)
	if err != nil {
		return err
	}
	refByID, err := appendBackupFilesAndPins(ctx, tx, request.JobID, snapshots, manifest)
	if err != nil {
		return err
	}
	appendBackupDirectories(directories, manifest)
	if err := appendBackupMappings(
		ctx,
		tx,
		selected,
		refByID,
		request.RequiredBuckets,
		manifest,
	); err != nil {
		return err
	}
	includedEntries := backupIncludedEntries(selected, directories)
	properties, err := readBackupWebDAVProperties(ctx, tx, includedEntries)
	if err != nil {
		return err
	}
	manifest.WebDAVProperties = properties
	return nil
}

func usedBackupBuckets(
	mappings []*backupMappingRow,
	directories []*backupMappingRow,
	configured []backupfmt.RequiredBucket,
) []backupfmt.RequiredBucket {
	result := make([]backupfmt.RequiredBucket, 0, len(configured))
	for _, bucket := range configured {
		prefix := "/" + bucket.Name + "/"
		root := "/" + bucket.Name
		for _, mapping := range mappings {
			if strings.HasPrefix(mapping.fullPath, prefix) {
				result = append(result, bucket)
				break
			}
		}
		if len(result) != 0 && result[len(result)-1].Name == bucket.Name {
			continue
		}
		for _, directory := range directories {
			if directory.fullPath == root || strings.HasPrefix(directory.fullPath, prefix) {
				result = append(result, bucket)
				break
			}
		}
	}
	return result
}

func (d *defaultFileManager) loadSelectedBackupFiles(
	ctx context.Context,
	tx database.IQueryExecer,
	selected []*backupMappingRow,
) (map[uint64]*snapshotFile, error) {
	snapshots := make(map[uint64]*snapshotFile)
	for _, row := range selected {
		fileID, err := strconv.ParseUint(row.refData, 10, 64)
		if err != nil || fileID == 0 {
			return nil, fmt.Errorf("parse file id for %s: %w", row.fullPath, ErrBackupState)
		}
		if err := d.loadBackupFileTree(ctx, tx, fileID, snapshots); err != nil {
			return nil, err
		}
	}
	return snapshots, nil
}

func appendBackupFilesAndPins(
	ctx context.Context,
	tx database.IQueryExecer,
	jobID string,
	snapshots map[uint64]*snapshotFile,
	manifest *backupfmt.Manifest,
) (map[uint64]string, error) {
	ids := make([]uint64, 0, len(snapshots))
	for fileID := range snapshots {
		ids = append(ids, fileID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	refByID := make(map[uint64]string, len(ids))
	for index, fileID := range ids {
		refByID[fileID] = fmt.Sprintf("f%08d", index+1)
	}
	for _, fileID := range ids {
		snapshot := snapshots[fileID]
		snapshot.item.Ref = refByID[fileID]
		for segmentIndex, sourceID := range snapshot.segmentSourceID {
			snapshot.item.Segments[segmentIndex].SourceRef = refByID[sourceID]
		}
		manifest.Files = append(manifest.Files, snapshot.item)
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO tg_backup_export_pin_tab (job_id, file_id, ctime)
VALUES (?, ?, ?)`,
			jobID,
			fileID,
			time.Now().UnixMilli(),
		); err != nil {
			return nil, fmt.Errorf("pin backup file %d: %w", fileID, err)
		}
	}
	return refByID, nil
}

func appendBackupDirectories(
	directories []*backupMappingRow,
	manifest *backupfmt.Manifest,
) {
	for _, row := range directories {
		manifest.Directories = append(manifest.Directories, backupfmt.Directory{
			Path: row.fullPath, Mode: row.mode & 0o777, Ctime: row.ctime, Mtime: row.mtime,
		})
	}
}

func appendBackupMappings(
	ctx context.Context,
	queryer database.IQueryer,
	selected []*backupMappingRow,
	refByID map[uint64]string,
	buckets []backupfmt.RequiredBucket,
	manifest *backupfmt.Manifest,
) error {
	for _, row := range selected {
		fileID, _ := strconv.ParseUint(row.refData, 10, 64)
		manifest.Mappings = append(manifest.Mappings, backupfmt.Mapping{
			Path: row.fullPath, FileRef: refByID[fileID], Size: row.size,
			Mode: row.mode & 0o777, Ctime: row.ctime, Mtime: row.mtime,
		})
		if !backupPathUsesS3Bucket(row.fullPath, buckets) {
			continue
		}
		metadata, found, err := readS3Metadata(ctx, queryer, row.entryID)
		if err != nil {
			return err
		}
		if !found {
			metadata = legacyS3Metadata(&entity.FileLinkMeta{
				EntryID: row.entryID, FileName: row.fullPath, FileId: fileID,
				FileSize: row.size, Mode: row.mode, Ctime: row.ctime, Mtime: row.mtime,
			})
		}
		manifest.S3Objects = append(manifest.S3Objects, backupS3Object(row.fullPath, metadata))
	}
	return nil
}

func backupIncludedEntries(
	selected, directories []*backupMappingRow,
) map[uint64]string {
	result := make(map[uint64]string, len(selected)+len(directories))
	for _, row := range selected {
		result[row.entryID] = row.fullPath
	}
	for _, row := range directories {
		result[row.entryID] = row.fullPath
	}
	return result
}

func (d *defaultFileManager) fillCompositeBackupDigests(
	ctx context.Context,
	jobID string,
	manifest *backupfmt.Manifest,
) error {
	for index := range manifest.Files {
		if manifest.Files[index].LayoutVersion != 2 || manifest.Files[index].CompatibilityMD5 != "" {
			continue
		}
		value, err := d.calculateBackupFileMD5(ctx, manifest.Files[index].SourceFileID)
		if err != nil {
			_ = d.ReleaseBackupSnapshot(context.WithoutCancel(ctx), jobID)
			return fmt.Errorf("calculate composite backup digest: %w", err)
		}
		manifest.Files[index].CompatibilityMD5 = value
	}
	return nil
}

func readBackupMappings(
	ctx context.Context,
	queryer database.IQueryer,
) (map[uint64]*backupMappingRow, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime,
file_size, file_mode, file_name FROM tg_file_mapping_tab ORDER BY entry_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("query backup mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[uint64]*backupMappingRow)
	for rows.Next() {
		row := new(backupMappingRow)
		if err := rows.Scan(
			&row.entryID, &row.parentID, &row.refData, &row.kind, &row.ctime,
			&row.mtime, &row.size, &row.mode, &row.name,
		); err != nil {
			return nil, fmt.Errorf("scan backup mapping: %w", err)
		}
		result[row.entryID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup mappings: %w", err)
	}
	var resolve func(uint64, map[uint64]bool) (string, error)
	resolve = func(entryID uint64, visiting map[uint64]bool) (string, error) {
		row, exists := result[entryID]
		if !exists {
			return "", fmt.Errorf("mapping parent %d is missing: %w", entryID, ErrBackupState)
		}
		if row.fullPath != "" {
			return row.fullPath, nil
		}
		if visiting[entryID] {
			return "", fmt.Errorf("mapping cycle at %d: %w", entryID, ErrBackupState)
		}
		visiting[entryID] = true
		defer delete(visiting, entryID)
		if row.parentID == 0 {
			if row.name != "/" {
				return "", fmt.Errorf("invalid mapping root: %w", ErrBackupState)
			}
			row.fullPath = "/"
			return row.fullPath, nil
		}
		parentPath, err := resolve(row.parentID, visiting)
		if err != nil {
			return "", err
		}
		row.fullPath = path.Join(parentPath, row.name)
		return row.fullPath, nil
	}
	for entryID := range result {
		if _, err := resolve(entryID, make(map[uint64]bool)); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func selectBackupMappings(
	rows map[uint64]*backupMappingRow,
	scope string,
) ([]*backupMappingRow, []*backupMappingRow, error) {
	selected := make([]*backupMappingRow, 0)
	requiredDirs := make(map[string]struct{})
	for parent := path.Dir(scope); parent != "/"; parent = path.Dir(parent) {
		requiredDirs[parent] = struct{}{}
	}
	scopeExists := scope == "/"
	for _, row := range rows {
		if row.fullPath == scope || strings.HasPrefix(row.fullPath, scope+"/") {
			scopeExists = true
		}
		if row.kind == 1 || !pathInBackupScope(row.fullPath, scope) {
			continue
		}
		selected = append(selected, row)
		for parent := path.Dir(row.fullPath); parent != "/"; parent = path.Dir(parent) {
			requiredDirs[parent] = struct{}{}
		}
	}
	if !scopeExists {
		return nil, nil, fmt.Errorf("backup scope %q contains no files: %w", scope, os.ErrNotExist)
	}
	directories := make([]*backupMappingRow, 0, len(requiredDirs))
	for _, row := range rows {
		if row.kind == 1 {
			if pathInBackupScope(row.fullPath, scope) || containsPath(requiredDirs, row.fullPath) {
				if row.fullPath != "/" {
					directories = append(directories, row)
				}
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].fullPath < selected[j].fullPath })
	sort.Slice(directories, func(i, j int) bool { return directories[i].fullPath < directories[j].fullPath })
	return selected, directories, nil
}

func pathInBackupScope(value, scope string) bool {
	return scope == "/" || value == scope || strings.HasPrefix(value, scope+"/")
}

func containsPath(paths map[string]struct{}, value string) bool {
	_, exists := paths[value]
	return exists
}

func (d *defaultFileManager) loadBackupFileTree(
	ctx context.Context,
	tx database.IQueryExecer,
	fileID uint64,
	result map[uint64]*snapshotFile,
) error {
	if _, exists := result[fileID]; exists {
		return nil
	}
	snapshot, partCount, err := readBackupFileSnapshot(ctx, tx, fileID)
	if err != nil {
		return err
	}
	result[fileID] = snapshot
	switch snapshot.item.LayoutVersion {
	case 1:
		return d.loadBackupPhysicalFile(ctx, tx, snapshot, partCount)
	case 2:
		return d.loadBackupCompositeFile(ctx, tx, snapshot, result)
	default:
		return fmt.Errorf(
			"backup file %d layout %d: %w",
			fileID,
			snapshot.item.LayoutVersion,
			ErrInvalidFileLayout,
		)
	}
}

func readBackupFileSnapshot(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) (*snapshotFile, int, error) {
	var size, ctime, mtime int64
	var partCount, state, layout int
	var extensionJSON string
	err := queryRow(
		ctx,
		queryer,
		`SELECT file_size, file_part_count, file_state, ctime, mtime, extinfo,
file_layout_version FROM tg_file_tab WHERE file_id = ?`,
		fileID,
	).Scan(&size, &partCount, &state, &ctime, &mtime, &extensionJSON, &layout)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("backup file %d is missing: %w", fileID, ErrBackupState)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read backup file %d: %w", fileID, err)
	}
	if state != constant.FileStateReady {
		return nil, 0, fmt.Errorf("backup file %d is not ready: %w", fileID, ErrBackupState)
	}
	var extension entity.FileExtInfo
	_ = json.Unmarshal([]byte(extensionJSON), &extension)
	snapshot := &snapshotFile{id: fileID, item: backupfmt.File{
		SourceFileID: strconv.FormatUint(fileID, 10), LayoutVersion: layout, Size: size,
		CompatibilityMD5: extension.Md5, Ctime: ctime, Mtime: mtime,
		Parts: []backupfmt.Part{}, Segments: []backupfmt.Segment{},
		CompletedParts: []backupfmt.CompletedPart{},
	}}
	return snapshot, partCount, nil
}

func (d *defaultFileManager) loadBackupPhysicalFile(
	ctx context.Context,
	tx database.IQueryExecer,
	snapshot *snapshotFile,
	partCount int,
) error {
	if err := ensureBackupPhysicalFileExportable(ctx, tx, snapshot.id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(
		ctx,
		`SELECT file_part_id, file_part_md5, file_part_size
FROM tg_file_part_tab WHERE file_id = ? ORDER BY file_part_id`,
		snapshot.id,
	)
	if err != nil {
		return fmt.Errorf("query backup parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var total int64
	for rows.Next() {
		part, err := d.scanBackupPhysicalPart(ctx, tx, rows, snapshot, partCount)
		if err != nil {
			return err
		}
		snapshot.item.Parts = append(snapshot.item.Parts, part)
		total += part.Size
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate backup parts: %w", err)
	}
	if len(snapshot.item.Parts) != partCount || total != snapshot.item.Size {
		return fmt.Errorf(
			"backup file %d part manifest differs from file row: %w",
			snapshot.id,
			ErrBackupState,
		)
	}
	return nil
}

func ensureBackupPhysicalFileExportable(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) error {
	var partCount, deleteStateCount, invalidDeleteStateCount int64
	if err := queryRow(
		ctx,
		queryer,
		`SELECT COUNT(part.file_part_id), COUNT(state.file_part_id),
COALESCE(SUM(CASE
  WHEN state.file_id IS NOT NULL AND (
    state.delete_state != 'live' OR state.delete_ref = '' OR state.uploaded_at <= 0
  ) THEN 1
  ELSE 0
END), 0)
FROM tg_file_part_tab part
LEFT JOIN tg_file_part_delete_state_tab state
  ON state.file_id = part.file_id AND state.file_part_id = part.file_part_id
WHERE part.file_id = ?`,
		fileID,
	).Scan(&partCount, &deleteStateCount, &invalidDeleteStateCount); err != nil {
		return fmt.Errorf("check backup physical file delete references: %w", err)
	}
	// Files created before durable Telegram deletion tracking have no state rows.
	// They remain readable and exportable, but still cannot be deleted remotely.
	if deleteStateCount == 0 {
		return nil
	}
	if deleteStateCount != partCount {
		return fmt.Errorf(
			"backup file %d has incomplete delete references: %w",
			fileID,
			ErrBackupState,
		)
	}
	if invalidDeleteStateCount != 0 {
		return fmt.Errorf("backup file %d has non-live parts: %w", fileID, ErrBackupState)
	}
	return nil
}

func (d *defaultFileManager) scanBackupPhysicalPart(
	ctx context.Context,
	tx database.IQueryExecer,
	rows *sql.Rows,
	snapshot *snapshotFile,
	partCount int,
) (backupfmt.Part, error) {
	var index int
	var checksum string
	var partSize int64
	if err := rows.Scan(&index, &checksum, &partSize); err != nil {
		return backupfmt.Part{}, fmt.Errorf("scan backup part: %w", err)
	}
	if index != len(snapshot.item.Parts) {
		return backupfmt.Part{}, fmt.Errorf(
			"backup file %d part order is invalid: %w",
			snapshot.id,
			ErrBackupState,
		)
	}
	if partSize < 0 {
		partSize = inferredLegacyPartSize(
			snapshot.item.Size,
			partCount,
			index,
			d.bkio.MaxFileSize(),
		)
		if partSize < 0 {
			return backupfmt.Part{}, fmt.Errorf(
				"derive backup file %d part size: %w",
				snapshot.id,
				ErrBackupState,
			)
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE tg_file_part_tab SET file_part_size = ?
WHERE file_id = ? AND file_part_id = ?`,
			partSize,
			snapshot.id,
			index,
		); err != nil {
			return backupfmt.Part{}, fmt.Errorf("materialize legacy part size: %w", err)
		}
	}
	return backupfmt.Part{
		Index: index,
		Size:  partSize,
		MD5:   checksum,
		Entry: fmt.Sprintf("parts/placeholder/%08d.bin", index),
	}, nil
}

func (d *defaultFileManager) loadBackupCompositeFile(
	ctx context.Context,
	tx database.IQueryExecer,
	snapshot *snapshotFile,
	result map[uint64]*snapshotFile,
) error {
	if err := readBackupSegments(ctx, tx, snapshot); err != nil {
		return err
	}
	for _, sourceID := range snapshot.segmentSourceID {
		if err := d.loadBackupFileTree(ctx, tx, sourceID, result); err != nil {
			return err
		}
	}
	completed, err := readBackupCompletedParts(ctx, tx, snapshot.id)
	if err != nil {
		return err
	}
	snapshot.item.CompletedParts = completed
	return nil
}

func readBackupSegments(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *snapshotFile,
) error {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT segment_index, source_file_id, segment_size
FROM tg_s3_file_segment_tab WHERE file_id = ? ORDER BY segment_index`,
		snapshot.id,
	)
	if err != nil {
		return fmt.Errorf("query backup segments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var index int
		var sourceID uint64
		var segmentSize int64
		if err := rows.Scan(&index, &sourceID, &segmentSize); err != nil {
			return fmt.Errorf("scan backup segment: %w", err)
		}
		if index != len(snapshot.item.Segments) {
			return fmt.Errorf("backup segment order: %w", ErrBackupState)
		}
		snapshot.item.Segments = append(
			snapshot.item.Segments,
			backupfmt.Segment{Index: index, Size: segmentSize},
		)
		snapshot.segmentSourceID = append(snapshot.segmentSourceID, sourceID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate backup segments: %w", err)
	}
	return nil
}

func inferredLegacyPartSize(size int64, count, index int, maxPart int64) int64 {
	if count == 0 && size == 0 {
		return 0
	}
	if count <= 0 || index < 0 || index >= count || maxPart <= 0 {
		return -1
	}
	if index < count-1 {
		return maxPart
	}
	return size - int64(count-1)*maxPart
}

func readBackupCompletedParts(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) ([]backupfmt.CompletedPart, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT part_number, part_size, checksum_state, checksum_algorithm,
checksum_value, ctime, mtime FROM tg_s3_completed_part_tab
WHERE file_id = ? ORDER BY part_number`,
		fileID,
	)
	if err != nil {
		return nil, fmt.Errorf("query completed part manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]backupfmt.CompletedPart, 0)
	for rows.Next() {
		var part backupfmt.CompletedPart
		if err := rows.Scan(
			&part.PartNumber, &part.PartSize, &part.ChecksumState,
			&part.ChecksumAlgorithm, &part.ChecksumValue, &part.Ctime, &part.Mtime,
		); err != nil {
			return nil, fmt.Errorf("scan completed part manifest: %w", err)
		}
		result = append(result, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed part manifest: %w", err)
	}
	return result, nil
}

func backupPathUsesS3Bucket(value string, buckets []backupfmt.RequiredBucket) bool {
	for _, bucket := range buckets {
		if strings.HasPrefix(value, "/"+bucket.Name+"/") {
			return true
		}
	}
	return false
}

func backupS3Object(path string, metadata *entity.S3ObjectMetadata) backupfmt.S3Object {
	return backupfmt.S3Object{
		Path: path, ETag: metadata.ETag, ChecksumSHA256: metadata.ChecksumSHA256,
		RequestChecksumAlgorithm: metadata.RequestChecksumAlgorithm,
		RequestChecksumValue:     metadata.RequestChecksumValue, ChecksumType: metadata.ChecksumType,
		ContentType: metadata.ContentType, CacheControl: metadata.CacheControl,
		ContentDisposition: metadata.ContentDisposition, ContentEncoding: metadata.ContentEncoding,
		ContentLanguage: metadata.ContentLanguage, Expires: metadata.Expires,
		UserMetadata: metadata.UserMetadata, Ctime: metadata.Ctime, Mtime: metadata.Mtime,
	}
}

func readBackupWebDAVProperties(
	ctx context.Context,
	queryer database.IQueryer,
	entries map[uint64]string,
) ([]backupfmt.WebDAVProperty, error) {
	result := make([]backupfmt.WebDAVProperty, 0)
	for entryID, entryPath := range entries {
		properties, err := readBackupEntryWebDAVProperties(ctx, queryer, entryID, entryPath)
		if err != nil {
			return nil, err
		}
		result = append(result, properties...)
	}
	return result, nil
}

func readBackupEntryWebDAVProperties(
	ctx context.Context,
	queryer database.IQueryer,
	entryID uint64,
	entryPath string,
) ([]backupfmt.WebDAVProperty, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT namespace_uri, local_name, value_xml, ctime, mtime
FROM tg_webdav_property_tab WHERE entry_id = ? ORDER BY namespace_uri, local_name`,
		entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("query WebDAV backup properties: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]backupfmt.WebDAVProperty, 0)
	for rows.Next() {
		property := backupfmt.WebDAVProperty{Path: entryPath}
		if err := rows.Scan(
			&property.NamespaceURI,
			&property.LocalName,
			&property.ValueXML,
			&property.Ctime,
			&property.Mtime,
		); err != nil {
			return nil, fmt.Errorf("scan WebDAV backup property: %w", err)
		}
		result = append(result, property)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate WebDAV backup properties: %w", err)
	}
	return result, nil
}

func sortBackupManifest(manifest *backupfmt.Manifest) {
	sort.Slice(manifest.RequiredBuckets, func(i, j int) bool {
		return manifest.RequiredBuckets[i].Name < manifest.RequiredBuckets[j].Name
	})
	sort.Slice(manifest.Directories, func(i, j int) bool {
		return manifest.Directories[i].Path < manifest.Directories[j].Path
	})
	sort.Slice(manifest.Mappings, func(i, j int) bool {
		return manifest.Mappings[i].Path < manifest.Mappings[j].Path
	})
	sort.Slice(manifest.S3Objects, func(i, j int) bool {
		return manifest.S3Objects[i].Path < manifest.S3Objects[j].Path
	})
	sort.Slice(manifest.WebDAVProperties, func(i, j int) bool {
		left, right := manifest.WebDAVProperties[i], manifest.WebDAVProperties[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.NamespaceURI != right.NamespaceURI {
			return left.NamespaceURI < right.NamespaceURI
		}
		return left.LocalName < right.LocalName
	})
	for fileIndex := range manifest.Files {
		for partIndex := range manifest.Files[fileIndex].Parts {
			manifest.Files[fileIndex].Parts[partIndex].Entry = fmt.Sprintf(
				"parts/%s/%08d.bin",
				manifest.Files[fileIndex].Ref,
				partIndex,
			)
		}
	}
}

func fillBackupSummary(manifest *backupfmt.Manifest) {
	manifest.Limits.MappingCount = int64(len(manifest.Mappings))
	manifest.Limits.DirectoryCount = int64(len(manifest.Directories))
	manifest.Limits.FileCount = int64(len(manifest.Files))
	for _, file := range manifest.Files {
		manifest.Limits.PartCount += int64(len(file.Parts))
		for _, part := range file.Parts {
			manifest.Limits.PhysicalBytes += part.Size
		}
	}
}

func (d *defaultFileManager) calculateBackupFileMD5(
	ctx context.Context,
	sourceFileID string,
) (string, error) {
	fileID, err := strconv.ParseUint(sourceFileID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse composite file id: %w", err)
	}
	stream, err := d.OpenFile(ctx, fileID)
	if err != nil {
		return "", fmt.Errorf("open composite file for backup digest: %w", err)
	}
	defer func() { _ = stream.Close() }()
	hash := md5.New() //nolint:gosec // Persisted compatibility digest.
	if _, err := io.Copy(hash, stream); err != nil {
		return "", fmt.Errorf("calculate composite backup digest: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (d *defaultFileManager) OpenBackupPart(
	ctx context.Context,
	sourceFileID string,
	partIndex int,
) (io.ReadCloser, error) {
	fileID, err := strconv.ParseUint(sourceFileID, 10, 64)
	if err != nil || partIndex < 0 {
		return nil, fmt.Errorf("open backup part: %w", ErrBackupState)
	}
	var key string
	if err := queryRow(
		ctx,
		d.dbc,
		`SELECT file_key FROM tg_file_part_tab WHERE file_id = ? AND file_part_id = ?`,
		fileID,
		partIndex,
	).Scan(&key); err != nil {
		return nil, fmt.Errorf("read backup part key: %w", err)
	}
	stream, err := d.bkio.Download(ctx, key, 0)
	if err != nil {
		return nil, fmt.Errorf("download backup part: %w", err)
	}
	return stream, nil
}

func (d *defaultFileManager) ReleaseBackupSnapshot(ctx context.Context, jobID string) error {
	if _, err := d.dbc.ExecContext(
		ctx,
		"DELETE FROM tg_backup_export_pin_tab WHERE job_id = ?",
		jobID,
	); err != nil {
		return fmt.Errorf("release backup snapshot pins: %w", err)
	}
	return nil
}

func (d *defaultFileManager) BeginBackupImport(
	ctx context.Context,
	jobID string,
	manifest *backupfmt.Manifest,
) error {
	if jobID == "" || manifest == nil {
		return ErrBackupState
	}
	now := time.Now().UnixMilli()
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		for _, file := range manifest.Files {
			var existing int
			err := queryRow(
				ctx,
				tx,
				"SELECT COUNT(*) FROM tg_backup_job_file_tab WHERE job_id = ? AND file_ref = ?",
				jobID,
				file.Ref,
			).Scan(&existing)
			if err != nil {
				return fmt.Errorf("check staged backup file: %w", err)
			}
			if existing != 0 {
				continue
			}
			targetID := idgen.NextId()
			extension, err := json.Marshal(entity.FileExtInfo{Md5: file.CompatibilityMD5})
			if err != nil {
				return fmt.Errorf("encode imported file metadata: %w", err)
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO tg_file_tab (
file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo, file_layout_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				targetID,
				file.Size,
				len(file.Parts),
				constant.FileStateInit,
				file.Ctime,
				file.Mtime,
				string(extension),
				file.LayoutVersion,
			); err != nil {
				return fmt.Errorf("create imported file draft: %w", err)
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO tg_backup_job_file_tab (
job_id, file_ref, source_file_id, target_file_id, layout_version,
stage_state, next_part_index, ctime, mtime
) VALUES (?, ?, ?, ?, ?, 'queued', 0, ?, ?)`,
				jobID,
				file.Ref,
				file.SourceFileID,
				targetID,
				file.LayoutVersion,
				now,
				now,
			); err != nil {
				return fmt.Errorf("record imported file draft: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("begin backup import transaction: %w", err)
	}
	return nil
}

func (d *defaultFileManager) ValidateBackupImport(
	ctx context.Context,
	manifest *backupfmt.Manifest,
	conflictPolicy string,
) error {
	if manifest == nil || (conflictPolicy != "fail" && conflictPolicy != "replace") {
		return ErrBackupState
	}
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		for _, item := range manifest.Directories {
			entry, exists, err := tx.Stat(ctx, item.Path)
			if err != nil {
				return fmt.Errorf("inspect backup import directory %s: %w", item.Path, err)
			}
			if exists && !entry.IsDir() {
				return fmt.Errorf("%s: %w", item.Path, ErrBackupConflict)
			}
		}
		for _, item := range manifest.Mappings {
			entry, exists, err := tx.Stat(ctx, item.Path)
			if err != nil {
				return fmt.Errorf("inspect backup import mapping %s: %w", item.Path, err)
			}
			if !exists {
				continue
			}
			if entry.IsDir() || conflictPolicy == "fail" {
				return fmt.Errorf("%s: %w", item.Path, ErrBackupConflict)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate backup import transaction: %w", err)
	}
	return nil
}

type stagedBackupPart struct {
	targetID  uint64
	fileRef   string
	index     int
	size      int64
	md5       string
	sha256    string
	fileKey   string
	deleteRef string
	uploaded  int64
}

func (d *defaultFileManager) StageBackupPart(
	ctx context.Context,
	jobID string,
	part backupfmt.Part,
	reader io.Reader,
) error {
	targetID, fileRef, nextPart, err := d.readBackupStageTarget(ctx, jobID, part.Entry)
	if err != nil {
		return err
	}
	if nextPart > part.Index {
		if _, err := io.CopyN(io.Discard, reader, part.Size); err != nil {
			return fmt.Errorf("discard already staged backup part: %w", err)
		}
		return nil
	}
	if nextPart != part.Index {
		return fmt.Errorf("stage part %d while expecting %d: %w", part.Index, nextPart, ErrBackupState)
	}
	staged, err := d.uploadBackupPart(ctx, targetID, fileRef, part, reader)
	if staged == nil {
		return err
	}
	if persistErr := d.persistBackupPart(ctx, jobID, staged); persistErr != nil {
		return errors.Join(err, persistErr, d.deleteUnpersistedBackupPart(ctx, staged))
	}
	if err != nil {
		return d.discardFailedBackupPart(ctx, staged.targetID, err)
	}
	if err := d.verifyBackupPartReadback(ctx, staged); err != nil {
		return d.discardFailedBackupPart(ctx, staged.targetID, err)
	}
	if err := d.advanceBackupPartCursor(ctx, jobID, staged); err != nil {
		return d.discardFailedBackupPart(ctx, staged.targetID, err)
	}
	return nil
}

func (d *defaultFileManager) deleteUnpersistedBackupPart(
	ctx context.Context,
	staged *stagedBackupPart,
) error {
	if err := d.persistBackupCompensationRecord(ctx, staged); err == nil {
		return nil
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()
	if err := d.bkio.DeleteBlocks(deleteCtx, []string{staged.deleteRef}); err != nil {
		return fmt.Errorf(
			"%w: compensate unpersisted backup part: %w",
			ErrBackupCompensation,
			err,
		)
	}
	return nil
}

func (d *defaultFileManager) persistBackupCompensationRecord(
	ctx context.Context,
	staged *stagedBackupPart,
) error {
	now := time.Now().UnixMilli()
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO tg_file_part_tab (
file_id, file_part_id, file_key, ctime, mtime, file_part_md5, file_part_size
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			staged.targetID,
			staged.index,
			staged.fileKey,
			now,
			now,
			staged.md5,
			staged.size,
		); err != nil {
			return fmt.Errorf("persist compensating backup part: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO tg_file_part_delete_state_tab (
file_id, file_part_id, backend_kind, delete_ref, uploaded_at, delete_state,
attempt_count, next_attempt_at, lease_until, last_attempt_at, last_error_code,
deleted_at, ctime, mtime
) VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, 0, 0, '', 0, ?, ?)`,
			staged.targetID,
			staged.index,
			d.bkio.Name(),
			staged.deleteRef,
			staged.uploaded,
			now,
			now,
			now,
		); err != nil {
			return fmt.Errorf("persist compensating backup delete state: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("persist backup compensation transaction: %w", err)
	}
	return nil
}

func (d *defaultFileManager) readBackupStageTarget(
	ctx context.Context,
	jobID, entry string,
) (uint64, string, int, error) {
	fileRef, err := backupPartFileRef(entry)
	if err != nil {
		return 0, "", 0, err
	}
	var targetID uint64
	var nextPart int
	if err := queryRow(
		ctx,
		d.dbc,
		`SELECT target_file_id, next_part_index FROM tg_backup_job_file_tab
WHERE job_id = ? AND file_ref = ? AND layout_version = 1`,
		jobID,
		fileRef,
	).Scan(&targetID, &nextPart); err != nil {
		return 0, "", 0, fmt.Errorf("read backup staging target: %w", err)
	}
	return targetID, fileRef, nextPart, nil
}

func (d *defaultFileManager) uploadBackupPart(
	ctx context.Context,
	targetID uint64,
	fileRef string,
	part backupfmt.Part,
	reader io.Reader,
) (*stagedBackupPart, error) {
	md5Hash := md5.New() //nolint:gosec // Archive compatibility checksum.
	shaHash := sha256.New()
	counted := &countingReader{reader: io.TeeReader(reader, io.MultiWriter(md5Hash, shaHash))}
	upload, err := d.bkio.Upload(ctx, counted)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBackupBackendUpload, err)
	}
	if upload == nil || upload.FileKey == "" || upload.DeleteRef == "" || upload.UploadedAt <= 0 {
		return nil, errInvalidUploadDeleteReference
	}
	staged := &stagedBackupPart{
		targetID:  targetID,
		fileRef:   fileRef,
		index:     part.Index,
		size:      part.Size,
		md5:       part.MD5,
		sha256:    part.SHA256,
		fileKey:   upload.FileKey,
		deleteRef: upload.DeleteRef,
		uploaded:  upload.UploadedAt,
	}
	if counted.count != part.Size ||
		hex.EncodeToString(md5Hash.Sum(nil)) != part.MD5 ||
		hex.EncodeToString(shaHash.Sum(nil)) != part.SHA256 {
		err := fmt.Errorf("imported part checksum or size differs: %w", backupfmt.ErrChecksum)
		return staged, err
	}
	return staged, nil
}

func (d *defaultFileManager) verifyBackupPartReadback(
	ctx context.Context,
	staged *stagedBackupPart,
) error {
	readback, err := d.bkio.Download(ctx, staged.fileKey, 0)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackupBackendReadback, err)
	}
	readbackHash := sha256.New()
	readbackSize, readbackErr := io.Copy(readbackHash, readback)
	closeErr := readback.Close()
	if err := errors.Join(readbackErr, closeErr); err != nil {
		return fmt.Errorf("%w: %w", ErrBackupBackendReadback, err)
	}
	if readbackSize != staged.size ||
		hex.EncodeToString(readbackHash.Sum(nil)) != staged.sha256 {
		return fmt.Errorf(
			"%w: imported part content differs: %w",
			ErrBackupBackendReadback,
			backupfmt.ErrChecksum,
		)
	}
	return nil
}

func (d *defaultFileManager) discardFailedBackupPart(
	ctx context.Context,
	fileID uint64,
	cause error,
) error {
	discardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()
	return errors.Join(cause, d.DiscardUnpublishedFile(discardCtx, fileID))
}

func (d *defaultFileManager) persistBackupPart(
	ctx context.Context,
	jobID string,
	staged *stagedBackupPart,
) error {
	now := time.Now().UnixMilli()
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO tg_file_part_tab (
file_id, file_part_id, file_key, ctime, mtime, file_part_md5, file_part_size
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			staged.targetID,
			staged.index,
			staged.fileKey,
			now,
			now,
			staged.md5,
			staged.size,
		); err != nil {
			return fmt.Errorf("insert imported file part: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO tg_file_part_delete_state_tab (
file_id, file_part_id, backend_kind, delete_ref, uploaded_at, delete_state,
attempt_count, next_attempt_at, lease_until, last_attempt_at, last_error_code,
deleted_at, ctime, mtime
) VALUES (?, ?, ?, ?, ?, 'live', 0, 0, 0, 0, '', 0, ?, ?)`,
			staged.targetID,
			staged.index,
			d.bkio.Name(),
			staged.deleteRef,
			staged.uploaded,
			now,
			now,
		); err != nil {
			return fmt.Errorf("insert imported block deletion reference: %w", err)
		}
		result, err := tx.ExecContext(
			ctx,
			`UPDATE tg_backup_job_file_tab
SET stage_state = 'verifying', mtime = ?
WHERE job_id = ? AND file_ref = ? AND next_part_index = ?`,
			now,
			jobID,
			staged.fileRef,
			staged.index,
		)
		if err != nil {
			return fmt.Errorf("advance imported part cursor: %w", err)
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return ErrBackupState
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("persist backup part transaction: %w", err)
	}
	return nil
}

func (d *defaultFileManager) advanceBackupPartCursor(
	ctx context.Context,
	jobID string,
	staged *stagedBackupPart,
) error {
	result, err := d.dbc.ExecContext(
		ctx,
		`UPDATE tg_backup_job_file_tab
SET stage_state = 'uploading', next_part_index = ?, mtime = ?
WHERE job_id = ? AND file_ref = ? AND next_part_index = ?`,
		staged.index+1,
		time.Now().UnixMilli(),
		jobID,
		staged.fileRef,
		staged.index,
	)
	if err != nil {
		return fmt.Errorf("advance imported part cursor: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrBackupState
	}
	return nil
}

func backupPartFileRef(entry string) (string, error) {
	parts := strings.Split(entry, "/")
	if len(parts) != 3 || parts[0] != "parts" || len(parts[1]) != 9 {
		return "", fmt.Errorf("parse backup part entry: %w", ErrBackupState)
	}
	return parts[1], nil
}

func (d *defaultFileManager) FinishBackupImportFiles(
	ctx context.Context,
	jobID string,
	manifest *backupfmt.Manifest,
) error {
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		targets, err := readBackupTargets(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if err := finishBackupPhysicalFiles(ctx, tx, jobID, manifest, targets); err != nil {
			return err
		}
		return finishBackupCompositeFiles(ctx, tx, jobID, manifest, targets)
	})
	if err != nil {
		return fmt.Errorf("finish backup import files transaction: %w", err)
	}
	return nil
}

func finishBackupPhysicalFiles(
	ctx context.Context,
	tx database.IQueryExecer,
	jobID string,
	manifest *backupfmt.Manifest,
	targets map[string]uint64,
) error {
	for _, file := range manifest.Files {
		if file.LayoutVersion != 1 {
			continue
		}
		targetID := targets[file.Ref]
		var count, size int64
		err := queryRow(
			ctx,
			tx,
			`SELECT COUNT(*), COALESCE(SUM(file_part_size), 0)
FROM tg_file_part_tab WHERE file_id = ?`,
			targetID,
		).Scan(&count, &size)
		if err != nil {
			return fmt.Errorf("validate staged physical file: %w", err)
		}
		if count != int64(len(file.Parts)) || size != file.Size {
			return fmt.Errorf("staged physical file %s is incomplete: %w", file.Ref, ErrBackupState)
		}
		if err := markBackupFileReady(ctx, tx, jobID, file, targetID); err != nil {
			return err
		}
	}
	return nil
}

func finishBackupCompositeFiles(
	ctx context.Context,
	tx database.IQueryExecer,
	jobID string,
	manifest *backupfmt.Manifest,
	targets map[string]uint64,
) error {
	for _, file := range manifest.Files {
		if file.LayoutVersion != 2 {
			continue
		}
		targetID := targets[file.Ref]
		ready, err := backupFileIsReady(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if ready {
			continue
		}
		if err := insertBackupSegments(ctx, tx, file, targetID, targets); err != nil {
			return err
		}
		if err := insertBackupCompletedParts(ctx, tx, file, targetID); err != nil {
			return err
		}
		if err := markBackupFileReady(ctx, tx, jobID, file, targetID); err != nil {
			return err
		}
	}
	return nil
}

func backupFileIsReady(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) (bool, error) {
	var state int
	if err := queryRow(
		ctx,
		queryer,
		"SELECT file_state FROM tg_file_tab WHERE file_id = ?",
		fileID,
	).Scan(&state); err != nil {
		return false, fmt.Errorf("read imported composite state: %w", err)
	}
	return state == constant.FileStateReady, nil
}

func insertBackupSegments(
	ctx context.Context,
	exec database.IExecer,
	file backupfmt.File,
	targetID uint64,
	targets map[string]uint64,
) error {
	for _, segment := range file.Segments {
		sourceID := targets[segment.SourceRef]
		if _, err := exec.ExecContext(
			ctx,
			`INSERT INTO tg_s3_file_segment_tab (
file_id, segment_index, source_file_id, segment_size, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?)`,
			targetID,
			segment.Index,
			sourceID,
			segment.Size,
			file.Ctime,
			file.Mtime,
		); err != nil {
			return fmt.Errorf("insert imported composite segment: %w", err)
		}
	}
	return nil
}

func insertBackupCompletedParts(
	ctx context.Context,
	exec database.IExecer,
	file backupfmt.File,
	targetID uint64,
) error {
	for _, part := range file.CompletedParts {
		if _, err := exec.ExecContext(
			ctx,
			`INSERT INTO tg_s3_completed_part_tab (
file_id, part_number, part_size, checksum_state, checksum_algorithm,
checksum_value, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			targetID,
			part.PartNumber,
			part.PartSize,
			part.ChecksumState,
			part.ChecksumAlgorithm,
			part.ChecksumValue,
			part.Ctime,
			part.Mtime,
		); err != nil {
			return fmt.Errorf("insert imported completed part: %w", err)
		}
	}
	return nil
}

func markBackupFileReady(
	ctx context.Context,
	exec database.IExecer,
	jobID string,
	file backupfmt.File,
	targetID uint64,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_file_tab SET file_state = ?, ctime = ?, mtime = ?
WHERE file_id = ? AND file_state = ?`,
		constant.FileStateReady,
		file.Ctime,
		file.Mtime,
		targetID,
		constant.FileStateInit,
	); err != nil {
		return fmt.Errorf("finish imported file: %w", err)
	}
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_backup_job_file_tab SET stage_state = 'ready', mtime = ?
WHERE job_id = ? AND file_ref = ?`,
		time.Now().UnixMilli(),
		jobID,
		file.Ref,
	); err != nil {
		return fmt.Errorf("finish imported file state: %w", err)
	}
	return nil
}

func readBackupTargets(
	ctx context.Context,
	queryer database.IQueryer,
	jobID string,
) (map[string]uint64, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT file_ref, target_file_id FROM tg_backup_job_file_tab
WHERE job_id = ? ORDER BY file_ref`,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("query backup staging targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]uint64)
	for rows.Next() {
		var ref string
		var fileID uint64
		if err := rows.Scan(&ref, &fileID); err != nil {
			return nil, fmt.Errorf("scan backup staging target: %w", err)
		}
		result[ref] = fileID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup staging targets: %w", err)
	}
	return result, nil
}

func (d *defaultFileManager) PublishBackupImport(
	ctx context.Context,
	jobID string,
	manifest *backupfmt.Manifest,
	conflictPolicy string,
) (*BackupPublishResult, error) {
	if conflictPolicy != "fail" && conflictPolicy != "replace" {
		return nil, ErrBackupState
	}
	// The directory engine treats the root row as infrastructure rather than a
	// logical imported path. Ensure it exists before the atomic publish
	// transaction so parent-first Mkdir can remain non-implicit.
	if err := d.objectDir.Mkdir(ctx, "/"); err != nil {
		return nil, fmt.Errorf("prepare backup import root: %w", err)
	}
	result := &BackupPublishResult{FilesCreated: int64(len(manifest.Files))}
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		publisher := backupImportPublisher{
			tx:             tx,
			jobID:          jobID,
			manifest:       manifest,
			conflictPolicy: conflictPolicy,
			result:         result,
			entryByPath:    make(map[string]directory.IDirectoryEntry),
			s3ByPath:       make(map[string]backupfmt.S3Object, len(manifest.S3Objects)),
			replacedIDs:    make([]uint64, 0),
		}
		return publisher.publish(ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBackupPublish, err)
	}
	return result, nil
}

type backupImportPublisher struct {
	tx             directory.ITransaction
	jobID          string
	manifest       *backupfmt.Manifest
	conflictPolicy string
	result         *BackupPublishResult
	targets        map[string]uint64
	entryByPath    map[string]directory.IDirectoryEntry
	s3ByPath       map[string]backupfmt.S3Object
	replacedIDs    []uint64
}

func (p *backupImportPublisher) publish(ctx context.Context) error {
	var state string
	if err := queryRow(
		ctx,
		p.tx.QueryExecer(),
		"SELECT job_state FROM tg_backup_job_tab WHERE job_id = ? AND job_kind = 'import'",
		p.jobID,
	).Scan(&state); err != nil {
		return fmt.Errorf("read imported backup job state: %w", err)
	}
	if state != "publishing" {
		return ErrBackupState
	}
	targets, err := readBackupTargets(ctx, p.tx.QueryExecer(), p.jobID)
	if err != nil {
		return err
	}
	if len(targets) != len(p.manifest.Files) {
		return ErrBackupState
	}
	p.targets = targets
	for _, item := range p.manifest.S3Objects {
		p.s3ByPath[item.Path] = item
	}
	if err := p.publishDirectories(ctx); err != nil {
		return err
	}
	if err := p.publishMappings(ctx); err != nil {
		return err
	}
	if err := p.publishWebDAVProperties(ctx); err != nil {
		return err
	}
	return p.complete(ctx)
}

func (p *backupImportPublisher) publishDirectories(ctx context.Context) error {
	for _, item := range p.manifest.Directories {
		entry, exists, err := p.tx.Stat(ctx, item.Path)
		if err != nil {
			return fmt.Errorf("inspect imported directory %s: %w", item.Path, err)
		}
		if exists && !entry.IsDir() {
			return fmt.Errorf("%s: %w", item.Path, ErrBackupConflict)
		}
		reused := exists
		if !exists {
			entry, err = p.tx.Mkdir(ctx, item.Path)
			if err != nil {
				return fmt.Errorf("create imported directory %s: %w", item.Path, err)
			}
		}
		if err := updateBackupEntryMetadata(
			ctx,
			p.tx.QueryExecer(),
			entry.EntryID(),
			item.Mode,
			item.Ctime,
			item.Mtime,
		); err != nil {
			return err
		}
		if reused {
			if err := p.tx.Touch(ctx, item.Path, item.Mtime); err != nil {
				return fmt.Errorf("record imported directory metadata change: %w", err)
			}
		}
		p.entryByPath[item.Path] = entry
	}
	return nil
}

func (p *backupImportPublisher) publishMappings(ctx context.Context) error {
	for _, item := range p.manifest.Mappings {
		targetID := p.targets[item.FileRef]
		if err := ensureFileTreeCanBeLinked(ctx, p.tx.QueryExecer(), targetID); err != nil {
			return err
		}
		entry, err := p.publishMapping(ctx, item, targetID)
		if err != nil {
			return err
		}
		if err := updateBackupEntryMetadata(
			ctx,
			p.tx.QueryExecer(),
			entry.EntryID(),
			item.Mode,
			item.Ctime,
			item.Mtime,
		); err != nil {
			return err
		}
		p.entryByPath[item.Path] = entry
		if metadata, exists := p.s3ByPath[item.Path]; exists {
			if err := insertS3Metadata(
				ctx,
				p.tx.QueryExecer(),
				restoreS3Metadata(entry.EntryID(), metadata),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *backupImportPublisher) publishMapping(
	ctx context.Context,
	item backupfmt.Mapping,
	targetID uint64,
) (directory.IDirectoryEntry, error) {
	current, exists, err := p.tx.Stat(ctx, item.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect imported mapping %s: %w", item.Path, err)
	}
	if !exists {
		entry, err := p.tx.Create(
			ctx,
			item.Path,
			item.Size,
			strconv.FormatUint(targetID, 10),
		)
		if err != nil {
			return nil, fmt.Errorf("create imported mapping %s: %w", item.Path, err)
		}
		p.result.MappingsCreated++
		return entry, nil
	}
	return p.replaceMapping(ctx, item, targetID, current)
}

func (p *backupImportPublisher) replaceMapping(
	ctx context.Context,
	item backupfmt.Mapping,
	targetID uint64,
	current directory.IDirectoryEntry,
) (directory.IDirectoryEntry, error) {
	if p.conflictPolicy != "replace" || current.IsDir() {
		return nil, fmt.Errorf("%s: %w", item.Path, ErrBackupConflict)
	}
	oldID, err := strconv.ParseUint(current.RefData(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse replaced file id: %w", err)
	}
	if err := deleteS3Metadata(ctx, p.tx.QueryExecer(), current.EntryID()); err != nil {
		return nil, err
	}
	if _, err := p.tx.QueryExecer().ExecContext(
		ctx,
		"DELETE FROM tg_webdav_property_tab WHERE entry_id = ?",
		current.EntryID(),
	); err != nil {
		return nil, fmt.Errorf("delete replaced WebDAV properties: %w", err)
	}
	entry, err := p.tx.Replace(
		ctx,
		item.Path,
		item.Size,
		strconv.FormatUint(targetID, 10),
		item.Mtime,
	)
	if err != nil {
		return nil, fmt.Errorf("replace imported mapping %s: %w", item.Path, err)
	}
	p.replacedIDs = append(p.replacedIDs, oldID)
	p.result.MappingsReplaced++
	return entry, nil
}

func (p *backupImportPublisher) publishWebDAVProperties(ctx context.Context) error {
	changedPaths := make(map[string]int64)
	for _, property := range p.manifest.WebDAVProperties {
		entry, exists := p.entryByPath[property.Path]
		if !exists {
			return fmt.Errorf("property target %s is missing: %w", property.Path, ErrBackupState)
		}
		if _, err := p.tx.QueryExecer().ExecContext(
			ctx,
			`INSERT INTO tg_webdav_property_tab (
entry_id, namespace_uri, local_name, value_xml, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(entry_id, namespace_uri, local_name)
DO UPDATE SET value_xml = excluded.value_xml, ctime = excluded.ctime, mtime = excluded.mtime`,
			entry.EntryID(),
			property.NamespaceURI,
			property.LocalName,
			property.ValueXML,
			property.Ctime,
			property.Mtime,
		); err != nil {
			return fmt.Errorf("insert imported WebDAV property: %w", err)
		}
		changedPaths[property.Path] = property.Mtime
	}
	for propertyPath, mtime := range changedPaths {
		if err := p.tx.Touch(ctx, propertyPath, mtime); err != nil {
			return fmt.Errorf("record imported WebDAV property change: %w", err)
		}
	}
	return nil
}

func (p *backupImportPublisher) complete(ctx context.Context) error {
	now := time.Now().UnixMilli()
	for _, fileID := range p.replacedIDs {
		if err := markFileTreePendingIfUnreferenced(
			ctx,
			p.tx.QueryExecer(),
			fileID,
			now,
		); err != nil {
			return err
		}
	}
	result, err := p.tx.QueryExecer().ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET job_state = 'succeeded', mappings_created = ?, mappings_replaced = ?,
files_created = ?, files_completed = files_total, parts_completed = parts_total,
bytes_completed = bytes_total, updated_at = ?, completed_at = ?
WHERE job_id = ? AND job_kind = 'import' AND job_state = 'publishing'`,
		p.result.MappingsCreated,
		p.result.MappingsReplaced,
		p.result.FilesCreated,
		now,
		now,
		p.jobID,
	)
	if err != nil {
		return fmt.Errorf("complete imported backup job: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrBackupState
	}
	return nil
}

func updateBackupEntryMetadata(
	ctx context.Context,
	exec database.IExecer,
	entryID uint64,
	mode uint32,
	ctime, mtime int64,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_file_mapping_tab
SET file_mode = ?, ctime = ?, mtime = ? WHERE entry_id = ?`,
		mode,
		ctime,
		mtime,
		entryID,
	); err != nil {
		return fmt.Errorf("restore mapping metadata: %w", err)
	}
	return nil
}

func restoreS3Metadata(entryID uint64, input backupfmt.S3Object) *entity.S3ObjectMetadata {
	return &entity.S3ObjectMetadata{
		EntryID: entryID, ETag: input.ETag, ChecksumSHA256: input.ChecksumSHA256,
		RequestChecksumAlgorithm: input.RequestChecksumAlgorithm,
		RequestChecksumValue:     input.RequestChecksumValue, ChecksumType: input.ChecksumType,
		ContentType: input.ContentType, CacheControl: input.CacheControl,
		ContentDisposition: input.ContentDisposition, ContentEncoding: input.ContentEncoding,
		ContentLanguage: input.ContentLanguage, Expires: input.Expires,
		UserMetadata: input.UserMetadata, Ctime: input.Ctime, Mtime: input.Mtime,
	}
}

func (d *defaultFileManager) DiscardBackupImport(ctx context.Context, jobID string) error {
	targets, err := readBackupTargets(ctx, d.dbc, jobID)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	err = d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		for _, targetID := range targets {
			if err := markFileTreePendingIfUnreferenced(ctx, tx, targetID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("discard backup import transaction: %w", err)
	}
	return nil
}
