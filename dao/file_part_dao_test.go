package dao

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/entity"
)

func TestCreateFilePartReturnsResponseAfterInsert(t *testing.T) {
	database, err := db.Open(t.TempDir() + "/data.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})

	filePartDAO := NewFilePartDao(database)
	response, err := filePartDAO.CreateFilePart(t.Context(), &entity.CreateFilePartRequest{
		FileId:      100,
		FilePartId:  0,
		FileKey:     "file-key",
		FilePartMd5: "checksum",
		BackendKind: "mem",
		DeleteRef:   "file-key",
		UploadedAt:  1,
	})
	require.NoError(t, err)
	require.NotNil(t, response)
}

func TestCreateFilePartAndDeleteStateRollbackTogether(t *testing.T) {
	databaseClient, err := db.Open(t.TempDir() + "/data.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, databaseClient.Close())
	})
	_, err = databaseClient.ExecContext(t.Context(), `
CREATE TRIGGER reject_delete_state
BEFORE INSERT ON tg_file_part_delete_state_tab
BEGIN
    SELECT RAISE(ABORT, 'reject delete state');
END;`)
	require.NoError(t, err)
	filePartDAO := NewFilePartDao(databaseClient)

	_, err = filePartDAO.CreateFilePart(t.Context(), &entity.CreateFilePartRequest{
		FileId:      100,
		FilePartId:  0,
		FileKey:     "file-key",
		FilePartMd5: "checksum",
		BackendKind: "mem",
		DeleteRef:   "file-key",
		UploadedAt:  1,
	})

	require.Error(t, err)
	rows, err := databaseClient.QueryContext(
		t.Context(),
		"SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = 100",
	)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var count int
	require.NoError(t, rows.Scan(&count))
	require.Zero(t, count)
}
