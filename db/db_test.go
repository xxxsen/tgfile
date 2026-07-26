package db

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"

	schemamigrations "github.com/xxxsen/tgfile/migrations"
)

func TestOpenCreatesSchemaFromEmbeddedMigrations(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "fresh.db")
	client, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 12, queryInt(t, client, `
	SELECT COUNT(*) FROM sqlite_master
	WHERE type = 'table' AND name IN (
	    'tg_file_tab', 'tg_file_part_tab', 'tg_file_mapping_tab',
	    'tg_s3_object_metadata_tab', 'tg_file_part_delete_state_tab',
	    'tg_s3_file_segment_tab', 'tg_s3_multipart_upload_tab',
	    'tg_s3_multipart_part_tab', 'tg_s3_completed_part_tab',
	    'tg_webdav_property_tab', 'tg_webdav_lock_tab',
	    'tg_webdav_change_tab'
	)`))
	require.NoError(t, insertPart(t.Context(), client, "fresh-checksum"))
}

func TestMigrationPlanDryRunAndPreMD5UpgradePreserveData(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "legacy.db")
	legacy := openRawDatabase(t, dbFile)
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, legacy, files[:3])
	insertLegacyRows(t, legacy)

	plan, err := planMigrations(t.Context(), legacy, files)
	require.NoError(t, err)
	require.True(t, plan.needsLedger)
	require.Len(t, plan.baseline, 3)
	require.Equal(t, 3, plan.baseline[2].version)
	require.Len(t, plan.pending, 8)
	require.Equal(t, 4, plan.pending[0].version)
	require.False(t, tableExistsForTest(t, legacy, "schema_migrations"))
	require.NoError(t, legacy.Close())

	upgraded, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, upgraded.Close())
	})

	require.Equal(t, 11, queryInt(t, upgraded, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
	require.Equal(t, "", queryString(
		t,
		upgraded,
		`SELECT file_part_md5 FROM tg_file_part_tab WHERE file_id = 101 AND file_part_id = 0`,
	))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT file_layout_version FROM tg_file_tab WHERE file_id = 101`))
	require.Zero(t, queryInt(t, upgraded, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'`))
}

func TestProductionLegacySchemaIsBaselinedAndNormalized(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "production.db")
	legacy := openRawDatabase(t, dbFile)
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, legacy, files[:4])
	insertLegacyRows(t, legacy)

	plan, err := planMigrations(t.Context(), legacy, files)
	require.NoError(t, err)
	require.Len(t, plan.baseline, 4)
	require.Len(t, plan.pending, 7)
	require.Equal(t, 5, plan.pending[0].version)
	require.False(t, tableExistsForTest(t, legacy, "schema_migrations"))
	require.NoError(t, legacy.Close())

	upgraded, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, upgraded.Close())
	})

	require.Equal(t, 11, queryInt(t, upgraded, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
	require.True(t, columnIsNotNull(t, upgraded, "tg_file_mapping_tab", "ref_data"))

	_, err = upgraded.ExecContext(t.Context(), `
INSERT INTO tg_file_tab(
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo
) VALUES (102, 0, 0, 2, 2, 2, '{}')`)
	require.NoError(t, err)
	require.Equal(t, 2, queryInt(t, upgraded, `SELECT id FROM tg_file_tab WHERE file_id = 102`))
}

func TestVersionFivePlanContainsOnlyNewS3MigrationsAndIsReadOnly(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "version-five.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, client, files[:5])
	require.NoError(t, initializeMigrationLedger(t.Context(), client, files[:5]))
	insertLegacyRows(t, client)

	plan, err := planMigrations(t.Context(), client, files)
	require.NoError(t, err)
	require.False(t, plan.needsLedger)
	require.Len(t, plan.current, 5)
	require.Len(t, plan.pending, 6)
	require.Equal(t, "0006_add_s3_object_metadata.sql", plan.pending[0].filename)
	require.Equal(t, "0007_add_block_delete_state.sql", plan.pending[1].filename)
	require.Equal(t, "0008_add_s3_multipart_upload.sql", plan.pending[2].filename)
	require.Equal(t, "0009_add_s3_multipart_checksum.sql", plan.pending[3].filename)
	require.Equal(t, "0010_add_s3_completed_part_manifest.sql", plan.pending[4].filename)
	require.Equal(t, "0011_add_webdav_protocol_state.sql", plan.pending[5].filename)

	require.Equal(t, 5, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
	require.False(t, tableExistsForTest(t, client, "tg_s3_object_metadata_tab"))
	require.False(t, tableExistsForTest(t, client, "tg_file_part_delete_state_tab"))
}

func TestStrictLegacySchemaUsesCompatibilityProfile(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "current.db")
	legacy := openRawDatabase(t, dbFile)
	strictSchema, err := fs.ReadFile(schemamigrations.FS, "legacy/strict_current_schema.sql")
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), string(strictSchema))
	require.NoError(t, err)
	insertLegacyRows(t, legacy)
	_, err = legacy.ExecContext(
		t.Context(),
		`UPDATE tg_file_part_tab SET file_part_md5 = 'persisted-md5'
		 WHERE file_id = 101 AND file_part_id = 0`,
	)
	require.NoError(t, err)

	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	plan, err := planMigrations(t.Context(), legacy, files)
	require.NoError(t, err)
	require.Len(t, plan.baseline, 4)
	require.Len(t, plan.pending, 7)
	require.NoError(t, validateCurrentSchemaFingerprint(t.Context(), legacy, plan.current))
	require.NoError(t, legacy.Close())

	upgraded, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, upgraded.Close())
	})

	require.Equal(t, 11, queryInt(t, upgraded, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, "persisted-md5", queryString(
		t,
		upgraded,
		`SELECT file_part_md5 FROM tg_file_part_tab WHERE file_id = 101 AND file_part_id = 0`,
	))
	require.Equal(t, 1, queryInt(t, upgraded, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
}

func TestStrictLegacyNormalizationCanRetryAfterFailure(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "retry.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	strictSchema, err := fs.ReadFile(schemamigrations.FS, "legacy/strict_current_schema.sql")
	require.NoError(t, err)
	_, err = client.ExecContext(t.Context(), string(strictSchema))
	require.NoError(t, err)
	insertLegacyRows(t, client)
	_, err = client.ExecContext(t.Context(), `
INSERT INTO tg_file_tab(
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo
) VALUES (102, 0, 0, 2, 2, 2, '{}')`)
	require.NoError(t, err)

	original := getMigrationFS()
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0005_normalize_constraints.sql"] = &fstest.MapFile{Data: []byte(`
CREATE UNIQUE INDEX migration_should_rollback ON tg_file_tab(file_state);
`)}
	setMigrationFS(migrationSet)
	t.Cleanup(func() {
		setMigrationFS(original)
	})

	err = migrate(t.Context(), client)
	require.Error(t, err)
	require.Equal(t, 4, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 2, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab`))
	require.Equal(t, 0, queryInt(t, client, `
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'index' AND name = 'migration_should_rollback'`))

	setMigrationFS(original)
	require.NoError(t, migrate(t.Context(), client))
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 2, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
}

func TestUnrecognizedLegacySchemaIsNotModified(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "partial.db")
	client := openRawDatabase(t, dbFile)
	_, err := client.ExecContext(t.Context(), `
CREATE TABLE tg_file_part_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    UNIQUE (file_id, file_part_id)
);
INSERT INTO tg_file_part_tab(file_id, file_part_id, file_key, ctime, mtime)
VALUES (101, 0, 'block-key', 1, 1);`)
	require.NoError(t, err)

	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	_, err = planMigrations(t.Context(), client, files)
	require.ErrorIs(t, err, errUnrecognizedLegacySchema)
	require.False(t, tableExistsForTest(t, client, "schema_migrations"))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_part_tab`))
	require.NoError(t, client.Close())

	opened, err := OpenContext(t.Context(), dbFile)
	require.Nil(t, opened)
	require.ErrorIs(t, err, errUnrecognizedLegacySchema)
	unchanged := openRawDatabase(t, dbFile)
	t.Cleanup(func() {
		require.NoError(t, unchanged.Close())
	})
	require.False(t, tableExistsForTest(t, unchanged, "schema_migrations"))
	require.Equal(t, 1, queryInt(t, unchanged, `SELECT COUNT(*) FROM tg_file_part_tab`))
}

func TestMigrationChecksumDriftIsRejected(t *testing.T) {
	client := openMigratedRawDatabase(t)
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0001_init_legacy_schema.sql"] = &fstest.MapFile{
		Data: append(migrationSet["0001_init_legacy_schema.sql"].Data, []byte("\n-- changed\n")...),
	}
	useMigrationFS(t, migrationSet)

	err := migrate(t.Context(), client)
	require.ErrorIs(t, err, errMigrationChanged)
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
}

func TestVersionEightChecksumMigrationPreservesMultipartData(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "version-eight.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, client, files[:8])
	require.NoError(t, initializeMigrationLedger(t.Context(), client, files[:8]))
	_, err = client.ExecContext(t.Context(), `
INSERT INTO tg_s3_multipart_upload_tab (
    upload_id, bucket_name, object_key, upload_state,
    completion_fingerprint, result_file_id, result_etag,
    initiated_at, expires_at, completed_at, cleanup_at, ctime, mtime
) VALUES
    ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
     'bucket', 'active', 'active', '', 0, '', 1, 100, 0, 0, 1, 1),
    ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
     'bucket', 'completed', 'completed', 'fingerprint', 10, '"etag"', 1, 100, 2, 100, 1, 2),
    ('cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
     'bucket', 'aborted', 'aborted', '', 0, '', 1, 100, 0, 100, 1, 2);
INSERT INTO tg_s3_multipart_part_tab (
    upload_id, part_number, part_state, file_id, part_size, part_etag,
    uploaded_at, ctime, mtime
) VALUES (
    'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    1, 'active', 11, 4, '0123456789abcdef0123456789abcdef', 1, 1, 1
);
INSERT INTO tg_s3_object_metadata_tab (
    entry_id, etag, checksum_sha256, request_checksum_algorithm,
    request_checksum_value, content_type, cache_control, content_disposition,
    content_encoding, content_language, expires, user_metadata, ctime, mtime
) VALUES
    (20, '"etag"', '', '', '', '', '', '', '', '', '', '{}', 1, 1),
    (21, '"etag-sha"', '', 'SHA256',
     'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=',
     '', '', '', '', '', '', '{}', 1, 1);`)
	require.NoError(t, err)

	require.NoError(t, migrate(t.Context(), client))
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 3, queryInt(t, client, `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_s3_multipart_part_tab`))
	require.Equal(t, "", queryString(
		t,
		client,
		`SELECT checksum_algorithm FROM tg_s3_multipart_upload_tab WHERE object_key = 'active'`,
	))
	require.Equal(t, "", queryString(
		t,
		client,
		`SELECT checksum_type FROM tg_s3_multipart_upload_tab WHERE object_key = 'completed'`,
	))
	require.Equal(t, "", queryString(
		t,
		client,
		`SELECT result_checksum_value FROM tg_s3_multipart_upload_tab WHERE object_key = 'aborted'`,
	))
	require.Equal(t, "", queryString(t, client, `SELECT checksum_value FROM tg_s3_multipart_part_tab`))
	require.Equal(t, "", queryString(
		t,
		client,
		`SELECT checksum_type FROM tg_s3_object_metadata_tab WHERE entry_id = 20`,
	))
	require.Equal(t, "FULL_OBJECT", queryString(
		t,
		client,
		`SELECT checksum_type FROM tg_s3_object_metadata_tab WHERE entry_id = 21`,
	))
}

func TestCompletedPartManifestMigrationBackfillsChecksumsAndPreservesData(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "version-nine.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, client, files[:9])
	require.NoError(t, initializeMigrationLedger(t.Context(), client, files[:9]))
	_, err = client.ExecContext(t.Context(), `
INSERT INTO tg_file_tab (
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo,
    file_layout_version
) VALUES
    (500, 7, 0, 2, 1, 1, '{"kind":"final"}', 2),
    (501, 3, 1, 2, 1, 1, '{"kind":"source-1"}', 1),
    (502, 4, 1, 2, 1, 1, '{"kind":"source-2"}', 1),
    (510, 2, 0, 3, 1, 1, '{"kind":"orphan-final"}', 2),
    (511, 2, 1, 2, 1, 1, '{"kind":"orphan-source"}', 1);
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES
    (501, 0, 'source-one', 'md5-one', 1, 1),
    (502, 0, 'source-two', 'md5-two', 1, 1),
    (511, 0, 'orphan-source', 'md5-orphan', 1, 1);
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime,
    file_size, file_mode, file_name
) VALUES (600, 0, '500', 2, 1, 1, 7, 420, 'object.bin');
INSERT INTO tg_s3_object_metadata_tab (
    entry_id, etag, checksum_sha256, request_checksum_algorithm,
    request_checksum_value, content_type, cache_control, content_disposition,
    content_encoding, content_language, expires, user_metadata,
    ctime, mtime, checksum_type
) VALUES (
    600, '"object-etag"', 'object-sha256', 'SHA256',
    'b2JqZWN0LWNoZWNrc3Vt', 'application/octet-stream', '', '', '', '', '',
    '{"owner":"migration-test"}', 1, 1, 'COMPOSITE'
);
INSERT INTO tg_file_part_delete_state_tab (
    file_id, file_part_id, backend_kind, delete_ref, uploaded_at,
    delete_state, attempt_count, next_attempt_at, lease_until,
    last_attempt_at, last_error_code, deleted_at, ctime, mtime
) VALUES (
    501, 0, 'telegram', 'chat:message', 1,
    'pending', 2, 3, 4, 5, 'retry', 0, 1, 1
);
INSERT INTO tg_s3_file_segment_tab (
    file_id, segment_index, source_file_id, segment_size, ctime, mtime
) VALUES
    (500, 0, 501, 3, 10, 11),
    (500, 1, 502, 4, 12, 13),
    (510, 0, 511, 2, 14, 15);
INSERT INTO tg_s3_multipart_upload_tab (
    upload_id, bucket_name, object_key, upload_state,
    completion_fingerprint, result_file_id, result_etag,
    initiated_at, expires_at, completed_at, cleanup_at, ctime, mtime,
    checksum_algorithm, checksum_type, result_checksum_value
) VALUES (
    'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
    'bucket', 'object.bin', 'completed', 'fingerprint', 500, '"object-etag"',
    1, 100, 2, 100, 1, 2, 'SHA256', 'COMPOSITE', 'result-checksum'
);
INSERT INTO tg_s3_multipart_part_tab (
    upload_id, part_number, part_state, file_id, part_size, part_etag,
    uploaded_at, ctime, mtime, checksum_value
) VALUES
    ('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
     2, 'selected', 501, 3, '11111111111111111111111111111111',
     1, 1, 1, 'cGFydC1vbmUtY2hlY2tzdW0='),
    ('dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',
     9, 'selected', 502, 4, '22222222222222222222222222222222',
     1, 1, 1, 'cGFydC10d28tY2hlY2tzdW0=');`)
	require.NoError(t, err)

	require.NoError(t, migrate(t.Context(), client))
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 2, queryInt(t, client, `
SELECT COUNT(*) FROM tg_s3_completed_part_tab
WHERE file_id = 500 AND checksum_state = 'available'
  AND checksum_algorithm = 'SHA256'`))
	require.Equal(t, "cGFydC1vbmUtY2hlY2tzdW0=", queryString(
		t,
		client,
		`SELECT checksum_value FROM tg_s3_completed_part_tab
WHERE file_id = 500 AND part_number = 1`,
	))
	require.Equal(t, "cGFydC10d28tY2hlY2tzdW0=", queryString(
		t,
		client,
		`SELECT checksum_value FROM tg_s3_completed_part_tab
WHERE file_id = 500 AND part_number = 2`,
	))
	require.Equal(t, 7, queryInt(t, client, `
SELECT SUM(part_size) FROM tg_s3_completed_part_tab WHERE file_id = 500`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_s3_completed_part_tab
WHERE file_id = 510 AND part_number = 1 AND part_size = 2
  AND checksum_state = 'unavailable'
  AND checksum_algorithm = '' AND checksum_value = ''`))

	require.Equal(t, 5, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab`))
	require.Equal(t, 3, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_part_tab`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_file_mapping_tab
WHERE entry_id = 600 AND ref_data = '500' AND file_name = 'object.bin'`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = 501 AND delete_state = 'pending'
  AND attempt_count = 2 AND last_error_code = 'retry'`))
	require.Equal(t, 3, queryInt(t, client, `SELECT COUNT(*) FROM tg_s3_file_segment_tab`))
	require.Equal(t, 2, queryInt(t, client, `
SELECT COUNT(*) FROM tg_s3_multipart_part_tab WHERE part_state = 'selected'`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_s3_multipart_upload_tab
WHERE result_file_id = 500 AND upload_state = 'completed'`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_s3_object_metadata_tab
WHERE entry_id = 600 AND checksum_type = 'COMPOSITE'
  AND user_metadata = '{"owner":"migration-test"}'`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'`))
	require.Zero(t, queryInt(t, client, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'deleting'`))
}

func TestCompletedPartManifestMigrationFailureRollsBackTableAndLedger(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "manifest-rollback.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, client, files[:9])
	require.NoError(t, initializeMigrationLedger(t.Context(), client, files[:9]))
	insertLegacyRows(t, client)
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0010_add_s3_completed_part_manifest.sql"] = &fstest.MapFile{Data: []byte(`
CREATE TABLE tg_s3_completed_part_tab (file_id INTEGER PRIMARY KEY);
INSERT INTO missing_completed_part_source(file_id) VALUES (1);
`)}
	useMigrationFS(t, migrationSet)

	err = migrate(t.Context(), client)
	require.Error(t, err)
	require.False(t, tableExistsForTest(t, client, "tg_s3_completed_part_tab"))
	require.Equal(t, 9, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, client, `
SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
}

func TestChecksumMigrationFailureLeavesNoPartialColumns(t *testing.T) {
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "checksum-rollback.db"))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	applyMigrationBodies(t, client, files[:8])
	require.NoError(t, initializeMigrationLedger(t.Context(), client, files[:8]))
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0009_add_s3_multipart_checksum.sql"] = &fstest.MapFile{Data: []byte(`
ALTER TABLE tg_s3_multipart_upload_tab
ADD COLUMN checksum_algorithm TEXT NOT NULL DEFAULT '';
ALTER TABLE missing_table ADD COLUMN checksum_type TEXT NOT NULL DEFAULT '';
`)}
	useMigrationFS(t, migrationSet)

	err = migrate(t.Context(), client)
	require.Error(t, err)
	require.False(t, columnExistsForTest(t, client, "tg_s3_multipart_upload_tab", "checksum_algorithm"))
	require.Equal(t, 8, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
}

func TestFailedMigrationRollsBackSchemaAndLedger(t *testing.T) {
	client := openMigratedRawDatabase(t)
	insertLegacyRows(t, client)
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0012_broken.sql"] = &fstest.MapFile{Data: []byte(`
CREATE TABLE migration_should_rollback (id INTEGER PRIMARY KEY);
CREATE TABLE migration_should_rollback (id INTEGER PRIMARY KEY);
`)}
	useMigrationFS(t, migrationSet)

	err := migrate(t.Context(), client)
	require.Error(t, err)
	require.False(t, tableExistsForTest(t, client, "migration_should_rollback"))
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
}

func TestMigrationBackupCanBeRestoredAfterFailure(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "data.db")
	backupFile := filepath.Join(dir, "data.db.backup")
	client, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	insertLegacyRows(t, client)
	require.NoError(t, client.Close())
	copyFile(t, dbFile, backupFile)

	migrationSet := embeddedMigrationMap(t)
	migrationSet["0012_broken.sql"] = &fstest.MapFile{Data: []byte(`
UPDATE tg_file_tab SET extinfo = 'changed';
CREATE TABLE tg_file_tab (id INTEGER);
`)}
	original := getMigrationFS()
	setMigrationFS(migrationSet)
	t.Cleanup(func() {
		setMigrationFS(original)
	})
	failed := openRawDatabase(t, dbFile)
	err = migrate(t.Context(), failed)
	require.Error(t, err)
	require.NoError(t, failed.Close())

	require.NoError(t, os.Remove(dbFile))
	copyFile(t, backupFile, dbFile)
	setMigrationFS(original)

	restored, err := OpenContext(t.Context(), dbFile)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, restored.Close())
	})
	require.Equal(t, "{}", queryString(t, restored, `SELECT extinfo FROM tg_file_tab WHERE file_id = 101`))
	require.Equal(t, 1, queryInt(t, restored, `SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = '101'`))
}

func TestSchemaDriftIsRejectedWithoutDataMutation(t *testing.T) {
	client := openMigratedRawDatabase(t)
	insertLegacyRows(t, client)
	_, err := client.ExecContext(t.Context(), `DROP INDEX idx_entry_id`)
	require.NoError(t, err)
	migrationSet := embeddedMigrationMap(t)
	migrationSet["0012_add_drift_probe.sql"] = &fstest.MapFile{
		Data: []byte(`CREATE TABLE drift_probe (id INTEGER PRIMARY KEY);`),
	}
	useMigrationFS(t, migrationSet)

	err = migrate(t.Context(), client)
	require.ErrorIs(t, err, errSchemaDrift)
	require.False(t, tableExistsForTest(t, client, "drift_probe"))
	require.Equal(t, 11, queryInt(t, client, `SELECT COUNT(*) FROM schema_migrations`))
	require.Equal(t, 1, queryInt(t, client, `SELECT COUNT(*) FROM tg_file_tab WHERE file_id = 101`))
}

func TestMigrationFilesUseVersionedNames(t *testing.T) {
	files, err := listMigrationFiles(schemamigrations.FS)
	require.NoError(t, err)
	require.Len(t, files, 11)
	require.Equal(t, "0001_init_legacy_schema.sql", files[0].filename)
	require.Equal(t, "0005_normalize_constraints.sql", files[4].filename)
	require.Equal(t, "0007_add_block_delete_state.sql", files[6].filename)
	require.Equal(t, "0008_add_s3_multipart_upload.sql", files[7].filename)
	require.Equal(t, "0009_add_s3_multipart_checksum.sql", files[8].filename)
	require.Equal(t, "0010_add_s3_completed_part_manifest.sql", files[9].filename)
	require.Equal(t, "0011_add_webdav_protocol_state.sql", files[10].filename)

	_, err = listMigrationFiles(fstest.MapFS{
		"1_invalid.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	})
	require.ErrorIs(t, err, errMigrationNameFormat)
}

func openRawDatabase(t *testing.T, path string) database.IDatabase {
	t.Helper()
	client, err := sqlite.New(path)
	require.NoError(t, err)
	return client
}

func openMigratedRawDatabase(t *testing.T) database.IDatabase {
	t.Helper()
	client := openRawDatabase(t, filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, migrate(t.Context(), client))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})
	return client
}

func applyMigrationBodies(
	t *testing.T,
	client database.IExecer,
	files []migrationFile,
) {
	t.Helper()
	for _, file := range files {
		_, err := client.ExecContext(t.Context(), string(file.body))
		require.NoError(t, err)
	}
}

func insertLegacyRows(t *testing.T, client database.IExecer) {
	t.Helper()
	_, err := client.ExecContext(t.Context(), `
INSERT INTO tg_file_tab(
    file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo
) VALUES (101, 4, 1, 2, 1, 1, '{}');
INSERT INTO tg_file_part_tab(
    file_id, file_part_id, file_key, ctime, mtime
) VALUES (101, 0, 'block-key', 1, 1);
INSERT INTO tg_file_mapping_tab(
    entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime,
    file_size, file_mode, file_name
) VALUES (201, 0, '101', 2, 1, 1, 4, 493, 'file.txt');`)
	require.NoError(t, err)
}

func insertPart(ctx context.Context, client database.IExecer, checksum string) error {
	_, err := client.ExecContext(ctx, `
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES (1, 0, 'block-key', ?, 1, 1);`, checksum)
	return err
}

func queryInt(t *testing.T, client database.IQueryer, query string) int {
	t.Helper()
	rows, err := client.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()
	require.True(t, rows.Next())
	var value int
	require.NoError(t, rows.Scan(&value))
	require.NoError(t, rows.Err())
	return value
}

func queryString(t *testing.T, client database.IQueryer, query string) string {
	t.Helper()
	rows, err := client.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()
	require.True(t, rows.Next())
	var value string
	require.NoError(t, rows.Scan(&value))
	require.NoError(t, rows.Err())
	return value
}

func tableExistsForTest(t *testing.T, client database.IQueryer, name string) bool {
	t.Helper()
	exists, err := tableExists(t.Context(), client, name)
	require.NoError(t, err)
	return exists
}

func columnIsNotNull(
	t *testing.T,
	client database.IQueryer,
	table string,
	column string,
) bool {
	t.Helper()
	rows, err := client.QueryContext(t.Context(), "PRAGMA table_info("+quoteIdentifier(table)+")")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()
	for rows.Next() {
		var (
			columnID    int
			name        string
			columnType  string
			notNull     int
			defaultText any
			primaryKey  int
		)
		require.NoError(t, rows.Scan(
			&columnID,
			&name,
			&columnType,
			&notNull,
			&defaultText,
			&primaryKey,
		))
		if name == column {
			return notNull == 1
		}
	}
	require.NoError(t, rows.Err())
	t.Fatalf("column %s.%s not found", table, column)
	return false
}

func columnExistsForTest(
	t *testing.T,
	client database.IQueryer,
	table string,
	column string,
) bool {
	t.Helper()
	rows, err := client.QueryContext(t.Context(), "PRAGMA table_info("+quoteIdentifier(table)+")")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()
	for rows.Next() {
		var (
			columnID    int
			name        string
			columnType  string
			notNull     int
			defaultText any
			primaryKey  int
		)
		require.NoError(t, rows.Scan(
			&columnID,
			&name,
			&columnType,
			&notNull,
			&defaultText,
			&primaryKey,
		))
		if name == column {
			return true
		}
	}
	require.NoError(t, rows.Err())
	return false
}

func embeddedMigrationMap(t *testing.T) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(schemamigrations.FS, ".")
	require.NoError(t, err)
	result := make(fstest.MapFS)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := fs.ReadFile(schemamigrations.FS, entry.Name())
		require.NoError(t, err)
		result[entry.Name()] = &fstest.MapFile{Data: body}
	}
	return result
}

func useMigrationFS(t *testing.T, source fs.FS) {
	t.Helper()
	original := getMigrationFS()
	setMigrationFS(source)
	t.Cleanup(func() {
		setMigrationFS(original)
	})
}

func copyFile(t *testing.T, source, target string) {
	t.Helper()
	input, err := os.Open(source)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, input.Close())
	}()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, output.Close())
	}()
	_, err = io.Copy(output, input)
	require.NoError(t, err)
	require.NoError(t, output.Sync())
}
