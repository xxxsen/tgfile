package filemgr

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/logger"

	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
)

func newPurgeTestManager(t *testing.T) IFileManager {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/data.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	blkio, err := mem.New(1024)
	require.NoError(t, err)
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	logger.Init("", "debug", 0, 0, 0, true)
	return NewFileManager(database, blkio, cache)
}

func TestPurge(t *testing.T) {
	testManager := newPurgeTestManager(t)
	ctx := context.Background()
	protectedFileID, err := testManager.CreateFile(ctx, 1, bytes.NewBufferString("x"))
	require.NoError(t, err)
	{
		_, err = testManager.CreateFile(ctx, 0, &bytes.Buffer{})
		require.NoError(t, err)
	}
	{
		fid, err := testManager.CreateFile(ctx, 0, &bytes.Buffer{})
		require.NoError(t, err)
		err = testManager.CreateFileLink(ctx, "/1.txt", fid, 0, false)
		require.NoError(t, err)
	}
	time.Sleep(1 * time.Second)
	now := time.Now().UnixMilli()
	cnt, err := testManager.PurgeFile(ctx, &now)
	require.NoError(t, err)
	require.Equal(t, 1, int(cnt))
	protected, err := testManager.OpenFile(ctx, protectedFileID)
	require.NoError(t, err)
	require.NoError(t, protected.Close())
}
