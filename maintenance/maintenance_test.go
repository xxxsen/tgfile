package maintenance

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

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
