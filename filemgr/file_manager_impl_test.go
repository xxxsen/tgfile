package filemgr

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/common/logger"
	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
)

var (
	dbfile  = "/tmp/sqlite_filemgr_test.db"
	testMgr IFileManager
)

func setup() {
	tearDown()
	if err := db.InitDB(dbfile); err != nil {
		panic(err)
	}
	blkio, err := mem.New(1024)
	if err != nil {
		panic(err)
	}
	cc, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	if err != nil {
		panic(err)
	}
	logger.Init("", "debug", 0, 0, 0, true)
	//cache.SetImpl(cache.MustNew(1000))
	mgr := NewFileManager(db.GetClient(), blkio, cc)
	testMgr = mgr
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

func TestPurge(t *testing.T) {
	ctx := context.Background()
	{
		_, err := testMgr.CreateFile(ctx, 0, &bytes.Buffer{})
		assert.NoError(t, err)
	}
	{
		fid, err := testMgr.CreateFile(ctx, 0, &bytes.Buffer{})
		assert.NoError(t, err)
		err = testMgr.CreateFileLink(ctx, "/1.txt", fid, 0, false)
		assert.NoError(t, err)
	}
	time.Sleep(1 * time.Second)
	now := time.Now().UnixMilli()
	cnt, err := testMgr.PurgeFile(ctx, &now)
	assert.NoError(t, err)
	assert.Equal(t, 1, int(cnt))
}
