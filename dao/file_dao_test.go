package dao

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/entity"
)

var (
	dbfile         = "/tmp/sqlite_dao_test.db"
	fileDao        IFileDao
	fileMappingDao IFileMappingDao
)

func setup() {
	tearDown()
	if err := db.InitDB(dbfile); err != nil {
		panic(err)
	}
	fileDao = NewFileDao(db.GetClient())
	fileMappingDao = NewFileMappingDao(db.GetClient())
}

func tearDown() {
	_ = os.Remove(dbfile)
}

func TestMain(m *testing.M) {
	setup()
	code := m.Run()
	tearDown()
	if code != 0 {
		os.Exit(code)
	}
}

func TestScan(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		rsp, err := fileDao.CreateFileDraft(ctx, &entity.CreateFileDraftRequest{
			FileSize: int64(i),
		})
		assert.NoError(t, err)
		_, err = fileDao.MarkFileReady(ctx, &entity.MarkFileReadyRequest{
			FileID: rsp.FileId,
		})
		assert.NoError(t, err)
	}
	err := fileDao.ScanFile(ctx, 1, func(ctx context.Context, res []*entity.FileInfoItem) (bool, error) {
		if len(res) == 0 {
			return false, nil
		}
		t.Logf("recv scan item:%+v", *res[0])
		return true, nil
	})
	assert.NoError(t, err)
}
