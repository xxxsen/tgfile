package filemgr

import (
	"context"
	"io"
	"testing"
	"time"

	lru "github.com/hnlq715/golang-lru"
	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/common/logger"
)

func TestFileIOCache(t *testing.T) {
	logger.Init("", "debug", 0, 0, 0, true)
	cc, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    10,
		L1KeySizeLimit: 5,
		DisableL2Cache: false,
		L2CacheSize:    30,
		L2KeySizeLimit: 10,
		L2CacheDir:     "/tmp/tgfile-cache",
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
		val, err := impl.l1.Get(ctx, uint64(1))
		assert.NoError(t, err)
		assert.Len(t, val, 1)
		_, err = impl.l2.Get(ctx, uint64(1))
		assert.NoError(t, err)
	}
	{ //内存无, 文件有
		_, err = cc.Load(ctx, 2, 10, dataReader(10))
		assert.NoError(t, err)
		_, err := impl.l1.Get(ctx, uint64(2))
		assert.Error(t, err)
		_, err = impl.l2.Get(ctx, uint64(2))
		assert.NoError(t, err)
	}
	{ // 内存无, 文件无, 直接从数据源加载
		_, err = cc.Load(ctx, 3, 100, dataReader(100))
		assert.NoError(t, err)
		_, err := impl.l1.Get(ctx, uint64(3))
		assert.Error(t, err)
		_, err = impl.l2.Get(ctx, uint64(3))
		assert.Error(t, err)
	}
	{ //测试l2缓存淘汰
		for i := 0; i < 40; i++ {
			_, err = cc.Load(ctx, uint64(i+4), 10, dataReader(10))
			assert.NoError(t, err)
		}
	}
	{ //测试l1缓存淘汰
		for i := 0; i < 20; i++ {
			_, err = cc.Load(ctx, uint64(i+4), 2, dataReader(2))
			assert.NoError(t, err)
		}
	}
}

func TestEvitTwice(t *testing.T) {
	evited := false
	cc, err := lru.NewWithEvict(5,
		func(key, value interface{}) {
			evited = true
		},
	)
	assert.NoError(t, err)
	cc.Add(1, "hello")
	cc.Add(1, "world") //对同一个数据的修改, 不会触发淘汰
	assert.False(t, evited)
}

func TestEvit(t *testing.T) {
	logger.Init("", "debug", 0, 0, 0, true)
	cc, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    10,
		L1KeySizeLimit: 5,
		DisableL2Cache: true,
	})
	assert.NoError(t, err)
	dataReader := func(sz int) func(ctx context.Context) (io.ReadSeekCloser, error) {
		return func(ctx context.Context) (io.ReadSeekCloser, error) {
			buf := make([]byte, sz)
			for i := 0; i < sz; i++ {
				buf[i] = byte(i % 256) // 填充一些数据
			}
			return newBytesStream(buf), nil
		}
	}

	for i := 0; i < 30; i++ {
		_, err := cc.Load(context.Background(), uint64(i), 3, dataReader(3))
		assert.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}
	impl := cc.(*fileIOCacheImpl)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_, err := impl.l1.Get(ctx, uint64(i))
		t.Logf("%d=>err:%v", i, err)
	}
}
