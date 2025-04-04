package filemgr

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/common/logger"
)

func TestFileIOCache(t *testing.T) {
	logger.Init("", "debug", 0, 0, 0, true)
	cc, err := NewFileIOCache(&FileIOCacheConfig{
		DisableMemCache:  false,
		MemKeyCount:      10,
		MemKeySizeLimit:  5,
		MemCacheTime:     5,
		DisableFileCache: false,
		FileKeyCount:     30,
		FileKeySizeLimit: 20,
		FileCacheTime:    10,
		FileCacheDir:     "/tmp/tgfile_cache",
	})
	assert.NoError(t, err)
	ctx := context.Background()

	dataReader := func(sz int) func(ctx context.Context) (io.ReadSeekCloser, error) {
		return func(ctx context.Context) (io.ReadSeekCloser, error) {
			buf := make([]byte, sz)
			for i := 0; i < sz; i++ {
				buf[i] = byte(i % 256) // 填充一些数据
			}
			return newBytesStream(buf), nil
		}
	}
	impl := cc.(*fileIOCacheImpl)
	{ // 内存有, 文件有
		_, err = cc.Load(ctx, 1, 1, dataReader(1))
		assert.NoError(t, err)
		val, ok := impl.l1.Get(uint64(1))
		assert.True(t, ok)
		assert.Len(t, val, 1)
		_, ok = impl.l2.Get(uint64(1))
		assert.True(t, ok)
	}
	{ //内存无, 文件有
		_, err = cc.Load(ctx, 2, 10, dataReader(10))
		assert.NoError(t, err)
		_, ok := impl.l1.Get(uint64(2))
		assert.False(t, ok)
		_, ok = impl.l2.Get(uint64(2))
		assert.True(t, ok)
	}
	{ // 内存无, 文件无, 直接从数据源加载
		_, err = cc.Load(ctx, 3, 100, dataReader(100))
		assert.NoError(t, err)
		_, ok := impl.l1.Get(uint64(3))
		assert.False(t, ok)
		_, ok = impl.l2.Get(uint64(3))
		assert.False(t, ok)
	}
	{ //测试l2缓存淘汰
		for i := 0; i < 40; i++ {
			_, err = cc.Load(ctx, uint64(i+4), 10, dataReader(10))
			assert.NoError(t, err)
		}
	}
}
