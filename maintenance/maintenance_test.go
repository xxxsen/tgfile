package maintenance

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/db"
)

func createMaintenanceDatabase(t *testing.T) string {
	t.Helper()
	databaseFile := filepath.Join(t.TempDir(), "data.db")
	database, err := db.Open(databaseFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})

	ctx := context.Background()
	insertMapping := func(entryID, parentID uint64, refData string, kind int, name string) {
		t.Helper()
		_, err := database.ExecContext(ctx, `
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind,
    ctime, mtime, file_size, file_mode, file_name
) VALUES (?, ?, ?, ?, 100, 200, 0, 420, ?);`,
			entryID, parentID, refData, kind, name)
		require.NoError(t, err)
	}
	insertFile := func(fileID uint64, partCount, state int) {
		t.Helper()
		_, err := database.ExecContext(ctx, `
INSERT INTO tg_file_tab (
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo
) VALUES (?, 4, ?, ?, 100, 200, '{}');`, fileID, partCount, state)
		require.NoError(t, err)
	}

	insertMapping(1, 0, "", 1, "/")
	insertMapping(2, 1, "", 1, "defaults")
	insertMapping(3, 2, "", 1, "01")
	insertMapping(4, 3, "100", 2, "0123456789abcdef-file")
	insertMapping(5, 1, "101", 2, "draft")
	insertMapping(6, 1, "999", 2, "missing")

	insertFile(100, 1, 2)
	insertFile(101, 0, 1)
	insertFile(102, 2, 2)
	insertFile(103, 0, 2)
	_, err = database.ExecContext(ctx, `
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES
    (100, 0, 'telegram-key-100', 'md5-100', 100, 200),
    (102, 0, 'telegram-key-102', 'md5-102', 100, 200);`)
	require.NoError(t, err)
	return databaseFile
}

func fileDigest(t *testing.T, file string) [sha256.Size]byte {
	t.Helper()
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	return sha256.Sum256(raw)
}

func TestAuditIsReadOnlyAndReportsAnomalies(t *testing.T) {
	databaseFile := createMaintenanceDatabase(t)
	before := fileDigest(t, databaseFile)

	report, err := Audit(context.Background(), databaseFile)
	require.NoError(t, err)
	require.Equal(t, "ok", report.QuickCheck)
	require.Equal(t, int64(3), report.FileCountByState["2"])
	require.Equal(t, int64(1), report.FileCountByState["1"])
	require.Equal(t, int64(2), report.FilePartCount)
	require.Len(t, report.MappingToMissingFile, 1)
	require.Equal(t, "999", report.MappingToMissingFile[0].FileID)
	require.Len(t, report.MappingToNonReadyFile, 1)
	require.Equal(t, "101", report.MappingToNonReadyFile[0].FileID)
	require.Len(t, report.ReadyFilePartCountMismatch, 1)
	require.Equal(t, uint64(102), report.ReadyFilePartCountMismatch[0].FileID)
	require.Equal(t, int64(2), report.UnreferencedFileCount)
	require.True(t, report.DefaultRootExists)
	require.Equal(t, before, fileDigest(t, databaseFile))

	readOnly, err := openDatabase(context.Background(), databaseFile, true)
	require.NoError(t, err)
	defer readOnly.Close()
	_, err = readOnly.ExecContext(context.Background(), "UPDATE tg_file_tab SET file_size = 0;")
	require.Error(t, err)
}

func TestAuditReportsMissingDefaultRoot(t *testing.T) {
	databaseFile := filepath.Join(t.TempDir(), "data.db")
	database, err := db.Open(databaseFile)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind,
    ctime, mtime, file_size, file_mode, file_name
) VALUES (1, 0, '', 1, 100, 200, 0, 420, '/');`)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	report, err := Audit(t.Context(), databaseFile)
	require.NoError(t, err)
	require.False(t, report.DefaultRootExists)
}

func TestAuditReportsS3DeleteAndPrivateSharingMetrics(t *testing.T) {
	databaseFile := filepath.Join(t.TempDir(), "s3-audit.db")
	database, err := db.Open(databaseFile)
	require.NoError(t, err)
	now := time.Now().UnixMilli()
	_, err = database.ExecContext(t.Context(), `
INSERT INTO tg_file_tab (
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo
) VALUES (200, 4, 1, 2, ?, ?, '{}');
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES (200, 0, 'file-key', 'checksum', ?, ?);
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind,
    ctime, mtime, file_size, file_mode, file_name
) VALUES
    (1, 0, '', 1, ?, ?, 0, 420, '/'),
    (2, 1, '', 1, ?, ?, 0, 420, 'private-data'),
    (3, 1, '', 1, ?, ?, 0, 420, 'public-data'),
    (4, 1, '', 1, ?, ?, 0, 420, 'defaults'),
    (5, 2, '200', 2, ?, ?, 4, 420, 'object'),
    (6, 3, '200', 2, ?, ?, 4, 420, 'object'),
    (7, 4, '200', 2, ?, ?, 4, 420, 'object');
INSERT INTO tg_s3_object_metadata_tab (
    entry_id, etag, checksum_sha256, content_type, cache_control,
    user_metadata, ctime, mtime
) VALUES (5, '"etag"', 'sha256', 'application/octet-stream', 'private', '{}', ?, ?);
INSERT INTO tg_file_part_delete_state_tab (
    file_id, file_part_id, backend_kind, delete_ref, uploaded_at, delete_state,
    attempt_count, next_attempt_at, lease_until, last_attempt_at, last_error_code,
    deleted_at, ctime, mtime
) VALUES (200, 0, 'old-backend', 'delete-ref', ?, 'pending', 0, 0, 0, 0, '', 0, ?, ?);`,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now,
		now-1000, now,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	report, err := AuditWithOptions(t.Context(), databaseFile, AuditOptions{
		S3Buckets: []AuditBucket{
			{Name: "private-data", ACL: "private"},
			{Name: "public-data", ACL: "public-read"},
		},
		BackendKind: "telegram",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.S3MetadataCount)
	require.Zero(t, report.S3MetadataWithoutMapping)
	require.Zero(t, report.S3MappingWithoutMetadataByBucket["private-data"])
	require.Equal(t, int64(1), report.S3MappingWithoutMetadataByBucket["public-data"])
	require.Equal(t, int64(1), report.BlockDeleteCountByState["pending"])
	require.Positive(t, report.OldestPendingAgeMillis)
	require.Equal(t, int64(1), report.ProcessableDeleteCount)
	require.Zero(t, report.DeleteStateWithoutPart)
	require.Equal(t, int64(1), report.BlockDeleteBackendMismatch)
	require.Equal(t, int64(1), report.PrivateFileWithPublicMapping)
	require.Equal(t, int64(1), report.PrivateFileWithDefaultsMapping)
}

func TestAuditUnderstandsCompositeAndMultipartReferences(t *testing.T) {
	databaseFile := filepath.Join(t.TempDir(), "multipart-audit.db")
	database, err := db.Open(databaseFile)
	require.NoError(t, err)
	now := time.Now().UnixMilli()
	_, err = database.ExecContext(t.Context(), `
INSERT INTO tg_file_tab (
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo, file_layout_version
) VALUES
    (300, 4, 1, 2, ?, ?, '{}', 1),
    (301, 4, 1, 2, ?, ?, '{}', 2);
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES (300, 0, 'source-key', 'source-md5', ?, ?);
INSERT INTO tg_file_part_delete_state_tab (
    file_id, file_part_id, backend_kind, delete_ref, uploaded_at, delete_state,
    attempt_count, next_attempt_at, lease_until, last_attempt_at, last_error_code,
    deleted_at, ctime, mtime
) VALUES (300, 0, 'telegram', 'delete-ref', ?, 'live', 0, 0, 0, 0, '', 0, ?, ?);
INSERT INTO tg_s3_file_segment_tab (
    file_id, segment_index, source_file_id, segment_size, ctime, mtime
) VALUES (301, 0, 300, 4, ?, ?);
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind,
    ctime, mtime, file_size, file_mode, file_name
) VALUES
    (1, 0, '', 1, ?, ?, 0, 420, '/'),
    (2, 1, '', 1, ?, ?, 0, 420, 'private-data'),
    (3, 2, '301', 2, ?, ?, 4, 420, 'multipart.bin');
INSERT INTO tg_s3_multipart_upload_tab (
    upload_id, bucket_name, object_key, upload_state,
    completion_fingerprint, result_file_id, result_etag,
    initiated_at, expires_at, completed_at, cleanup_at, ctime, mtime
) VALUES (
    '0000000000000000000000000000000000000000000000000000000000000001',
    'private-data', 'multipart.bin', 'completed',
    'fingerprint', 301, '"etag-1"', ?, ?, ?, ?, ?, ?
);
INSERT INTO tg_s3_multipart_part_tab (
    upload_id, part_number, part_state, file_id, part_size, part_etag,
    uploaded_at, ctime, mtime
) VALUES (
    '0000000000000000000000000000000000000000000000000000000000000001',
    1, 'selected', 300, 4, '0123456789abcdef0123456789abcdef', ?, ?, ?
);`,
		now, now,
		now, now,
		now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now,
		now, now+time.Hour.Milliseconds(), now,
		now+time.Hour.Milliseconds(), now, now,
		now, now, now,
	)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `
UPDATE tg_s3_multipart_upload_tab
SET initiated_at = ?, expires_at = ?, completed_at = ?, cleanup_at = ?, ctime = ?, mtime = ?`,
		now,
		now+time.Hour.Milliseconds(),
		now,
		now+time.Hour.Milliseconds(),
		now,
		now,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	report, err := Audit(t.Context(), databaseFile)
	require.NoError(t, err)
	require.Empty(t, report.ReadyFilePartCountMismatch)
	require.Zero(t, report.UnreferencedFileCount)
	require.Zero(t, report.InvalidCompositeManifestCount)
	require.Zero(t, report.InvalidCompletedUploadCount)
	require.Zero(t, report.InvalidMultipartStateCount)
	require.Equal(t, int64(1), report.MultipartUploadCountByState["completed"])
	require.Equal(t, int64(1), report.MultipartPartCountByState["selected"])

	writable, err := openDatabase(t.Context(), databaseFile, false)
	require.NoError(t, err)
	_, err = writable.ExecContext(
		t.Context(),
		"DELETE FROM tg_file_part_delete_state_tab WHERE file_id = 300",
	)
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	report, err = Audit(t.Context(), databaseFile)
	require.NoError(t, err)
	require.Equal(t, int64(1), report.MappedCompositeNonLiveSource)

	writable, err = openDatabase(t.Context(), databaseFile, false)
	require.NoError(t, err)
	_, err = writable.ExecContext(
		t.Context(),
		"UPDATE tg_s3_file_segment_tab SET segment_size = 3 WHERE file_id = 301",
	)
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	report, err = Audit(t.Context(), databaseFile)
	require.NoError(t, err)
	require.Equal(t, int64(1), report.InvalidCompositeManifestCount)
}

func TestWriteAuditReportUsesPrivatePermissions(t *testing.T) {
	output := filepath.Join(t.TempDir(), "audit.json")
	require.NoError(t, os.WriteFile(output, []byte("old"), 0o600))
	require.NoError(t, WriteAuditReport(output, &AuditReport{QuickCheck: "ok"}))
	info, err := os.Stat(output)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	raw, err := os.ReadFile(output)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "telegram-key")
}
