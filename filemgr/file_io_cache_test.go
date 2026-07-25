package filemgr

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/logger"

	"github.com/xxxsen/tgfile/cacheapi"
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
		L2CacheDir:     filepath.Join(t.TempDir(), "cache"),
	})
	require.NoError(t, err)
	ctx := context.Background()

	dataReader := func(sz int) func(ctx context.Context) (io.ReadSeekCloser, error) {
		return func(_ context.Context) (io.ReadSeekCloser, error) {
			buf := make([]byte, sz)
			for i := 0; i < sz; i++ {
				buf[i] = byte(i % 256) // 填充一些数据
			}
			return newBytesStream(buf), nil
		}
	}
	impl := cc.(*fileIOCacheImpl)
	{ // 内存有, 文件有
		reader, loadErr := cc.Load(ctx, 1, 1, dataReader(1))
		require.NoError(t, loadErr)
		require.NoError(t, reader.Close())
		val, err := impl.l1.Get(ctx, uint64(1))
		assert.NoError(t, err)
		assert.Len(t, val, 1)
		_, err = impl.l2.Get(ctx, uint64(1))
		assert.NoError(t, err)
	}
	{ // 内存无, 文件有
		reader, loadErr := cc.Load(ctx, 2, 10, dataReader(10))
		require.NoError(t, loadErr)
		require.NoError(t, reader.Close())
		_, err := impl.l1.Get(ctx, uint64(2))
		assert.Error(t, err)
		_, err = impl.l2.Get(ctx, uint64(2))
		assert.NoError(t, err)
	}
	{ // 内存无, 文件无, 直接从数据源加载
		reader, loadErr := cc.Load(ctx, 3, 100, dataReader(100))
		require.NoError(t, loadErr)
		require.NoError(t, reader.Close())
		_, err := impl.l1.Get(ctx, uint64(3))
		assert.Error(t, err)
		_, err = impl.l2.Get(ctx, uint64(3))
		assert.Error(t, err)
	}
	{ // 测试l2缓存淘汰
		for i := 0; i < 40; i++ {
			reader, loadErr := cc.Load(ctx, uint64(i+4), 10, dataReader(10))
			require.NoError(t, loadErr)
			require.NoError(t, reader.Close())
		}
	}
	{ // 测试l1缓存淘汰
		for i := 0; i < 20; i++ {
			reader, loadErr := cc.Load(ctx, uint64(i+4), 2, dataReader(2))
			require.NoError(t, loadErr)
			require.NoError(t, reader.Close())
		}
	}
}

func TestEvit(t *testing.T) {
	logger.Init("", "debug", 0, 0, 0, true)
	cc, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    10,
		L1KeySizeLimit: 5,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	dataReader := func(sz int) func(ctx context.Context) (io.ReadSeekCloser, error) {
		return func(_ context.Context) (io.ReadSeekCloser, error) {
			buf := make([]byte, sz)
			for i := 0; i < sz; i++ {
				buf[i] = byte(i % 256) // 填充一些数据
			}
			return newBytesStream(buf), nil
		}
	}

	for i := 0; i < 30; i++ {
		reader, err := cc.Load(context.Background(), uint64(i), 3, dataReader(3))
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		time.Sleep(100 * time.Millisecond)
	}
	impl := cc.(*fileIOCacheImpl)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_, err := impl.l1.Get(ctx, uint64(i))
		t.Logf("%d=>err:%v", i, err)
	}
}

func TestCacheRejectionDoesNotBreakCurrentRead(t *testing.T) {
	logger.Init("", "debug", 0, 0, 0, true)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: false,
		L2CacheSize:    5,
		L2KeySizeLimit: 10,
		L2CacheDir:     cacheDir,
	})
	require.NoError(t, err)
	expected := []byte("0123456789")

	reader, err := cache.Load(context.Background(), 99, int64(len(expected)), func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(expected), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	require.NoError(t, reader.Close())

	implementation := cache.(*fileIOCacheImpl)
	_, err = implementation.l2.Get(context.Background(), 99)
	require.ErrorIs(t, err, cacheapi.ErrCacheKeyNotExist)
	var cacheFiles []string
	require.NoError(t, filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			cacheFiles = append(cacheFiles, path)
		}
		return nil
	}))
	require.Empty(t, cacheFiles)
}
