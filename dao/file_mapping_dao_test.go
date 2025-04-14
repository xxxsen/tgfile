package dao

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/tgfile/entity"
)

func TestScanMapping(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, err := fileMappingDao.CreateFileLink(ctx, &entity.CreateFileLinkRequest{
			FileName: fmt.Sprintf("%d.txt", i),
			FileSize: int64(i),
			FileId:   uint64(i),
			IsDir:    false,
		})
		assert.NoError(t, err)
	}
	err := fileMappingDao.ScanFileLink(ctx, 1, func(ctx context.Context, res []*entity.FileLinkMeta) (bool, error) {
		if len(res) == 0 {
			return false, nil
		}
		t.Logf("recv scan item:%+v", *res[0])
		return true, nil
	})
	assert.NoError(t, err)
}
