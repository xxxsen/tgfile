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
	})
	require.NoError(t, err)
	require.NotNil(t, response)
}
