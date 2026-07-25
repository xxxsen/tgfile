package dao

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/entity"
)

var (
	fileDao        IFileDao
	fileMappingDao IFileMappingDao
)

func setupDAOTest(t *testing.T) {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/data.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	fileDao = NewFileDao(database)
	fileMappingDao = NewFileMappingDao(database)
}

func TestScan(t *testing.T) {
	setupDAOTest(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		rsp, err := fileDao.CreateFileDraft(ctx, &entity.CreateFileDraftRequest{
			FileSize: int64(i),
		})
		require.NoError(t, err)
		_, err = fileDao.MarkFileReady(ctx, &entity.MarkFileReadyRequest{
			FileID: rsp.FileId,
		})
		require.NoError(t, err)
	}
	err := fileDao.ScanFile(ctx, 1, func(_ context.Context, res []*entity.FileInfoItem) (bool, error) {
		if len(res) == 0 {
			return false, nil
		}
		t.Logf("recv scan item:%+v", *res[0])
		return true, nil
	})
	require.NoError(t, err)
}
