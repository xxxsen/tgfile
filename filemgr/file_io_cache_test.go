package filemgr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testCacheIdentity(fileID uint64, size int64) FileCacheIdentity {
	return FileCacheIdentity{
		FileID:        fileID,
		Size:          size,
		PartCount:     1,
		State:         1,
		LayoutVersion: 1,
		Ctime:         100,
		Mtime:         200,
		ExtInfo:       `{"md5":"test"}`,
	}
}

func closeTestCache(t *testing.T, cache IFileIOCache) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cache.Close(ctx))
}

func registerCacheCleanup(t *testing.T, cache IFileIOCache) {
	t.Helper()
	t.Cleanup(func() { closeTestCache(t, cache) })
}

func TestFileIOCacheTierSelectionAndHits(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    1024,
		L1KeySizeLimit: 4,
		L2CacheSize:    64,
		L2KeySizeLimit: 16,
		L2CacheDir:     filepath.Join(t.TempDir(), "cache"),
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)

	tests := []struct {
		name       string
		fileID     uint64
		data       []byte
		wantL1     bool
		wantL2     bool
		loaderHits int
	}{
		{name: "L1 only", fileID: 1, data: []byte("1234"), wantL1: true, loaderHits: 1},
		{name: "L2 only", fileID: 2, data: []byte("12345678"), wantL2: true, loaderHits: 1},
		{name: "uncacheable", fileID: 3, data: bytes.Repeat([]byte("x"), 17), loaderHits: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := testCacheIdentity(test.fileID, int64(len(test.data)))
			var calls int
			loader := func(context.Context) (io.ReadSeekCloser, error) {
				calls++
				return newBytesStream(test.data), nil
			}
			for range 2 {
				reader, loadErr := cache.Load(t.Context(), identity, loader)
				require.NoError(t, loadErr)
				actual, readErr := io.ReadAll(reader)
				require.NoError(t, readErr)
				require.NoError(t, reader.Close())
				require.True(t, bytes.Equal(test.data, actual))
			}
			require.Equal(t, test.loaderHits, calls)
			key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
			_, l1Found := implementation.readL1(key, identity.Size)
			_, l2Found := implementation.l2.entryPath(key)
			require.Equal(t, test.wantL1, l1Found)
			require.Equal(t, test.wantL2, l2Found)
		})
	}
}

func TestFileIOCacheExactSourceLength(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{name: "short", data: []byte("123"), wantErr: io.ErrUnexpectedEOF},
		{name: "long", data: []byte("123456"), wantErr: ErrCacheSourceSizeMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache, err := NewFileIOCache(&FileIOCacheConfig{
				DisableL1Cache: false,
				L1CacheSize:    1024,
				L1KeySizeLimit: 8,
				DisableL2Cache: true,
			})
			require.NoError(t, err)
			registerCacheCleanup(t, cache)
			identity := testCacheIdentity(10, 5)
			loader := func(context.Context) (io.ReadSeekCloser, error) {
				return newBytesStream(test.data), nil
			}
			for range 2 {
				_, loadErr := cache.Load(t.Context(), identity, loader)
				require.ErrorIs(t, loadErr, test.wantErr)
			}
			implementation := cache.(*fileIOCacheImpl)
			key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
			_, found := implementation.readL1(key, identity.Size)
			require.False(t, found)
		})
	}
}

func TestFileIOCacheConfigurationValidation(t *testing.T) {
	_, err := NewFileIOCache(nil)
	require.ErrorIs(t, err, ErrInvalidCache)

	_, err = NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    4,
		L1KeySizeLimit: 5,
		DisableL2Cache: true,
	})
	require.ErrorIs(t, err, ErrInvalidCache)

	_, err = NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    4,
		L2KeySizeLimit: 5,
		L2CacheDir:     t.TempDir(),
	})
	require.ErrorIs(t, err, ErrInvalidCache)
}

func TestFileIOCacheDisablesRedundantL2(t *testing.T) {
	tests := []struct {
		name    string
		l1Limit int
		l2Limit int
	}{
		{name: "equal limits", l1Limit: 8, l2Limit: 8},
		{name: "L1 limit greater", l1Limit: 16, l2Limit: 8},
	}
	fileID := uint64(300)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheDir := filepath.Join(t.TempDir(), "cache")
			cache, err := NewFileIOCache(&FileIOCacheConfig{
				L1CacheSize:    1024,
				L1KeySizeLimit: test.l1Limit,
				L2CacheSize:    64,
				L2KeySizeLimit: test.l2Limit,
				L2CacheDir:     cacheDir,
			})
			require.NoError(t, err)
			registerCacheCleanup(t, cache)
			implementation := cache.(*fileIOCacheImpl)
			require.True(t, implementation.c.DisableL2Cache)
			require.Nil(t, implementation.l2)
			require.NoDirExists(t, cacheDir)

			data := []byte("content")
			identity := testCacheIdentity(fileID, int64(len(data)))
			var loaderCalls int
			for range 2 {
				reader, loadErr := cache.Load(
					t.Context(),
					identity,
					func(context.Context) (io.ReadSeekCloser, error) {
						loaderCalls++
						return newBytesStream(data), nil
					},
				)
				require.NoError(t, loadErr)
				actual, readErr := io.ReadAll(reader)
				require.NoError(t, readErr)
				require.NoError(t, reader.Close())
				require.Equal(t, data, actual)
			}
			require.Equal(t, 1, loaderCalls)
		})
		fileID++
	}
}

func TestFileIOCacheRejectsLoadsAfterClose(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    1024,
		L1KeySizeLimit: 8,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	closeTestCache(t, cache)
	_, err = cache.Load(t.Context(), testCacheIdentity(1, 1), func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream([]byte("x")), nil
	})
	require.ErrorIs(t, err, ErrCacheClosed)
	require.NoError(t, cache.Close(t.Context()))
}

func TestFileIOCachePreservesErrorChains(t *testing.T) {
	sentinel := errors.New("source failed")
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    1024,
		L1KeySizeLimit: 8,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	_, err = cache.Load(t.Context(), testCacheIdentity(1, 1), func(context.Context) (io.ReadSeekCloser, error) {
		return nil, sentinel
	})
	require.ErrorIs(t, err, sentinel)
}

func TestFileIOCacheTierEligibilityBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		config     FileIOCacheConfig
		data       []byte
		loaderHits int32
		wantL1     bool
		wantL2     bool
		wantL2On   bool
	}{
		{
			name: "both disabled",
			config: FileIOCacheConfig{
				DisableL1Cache: true,
				DisableL2Cache: true,
			},
			data:       []byte("x"),
			loaderHits: 2,
		},
		{
			name: "L1 exact boundary",
			config: FileIOCacheConfig{
				L1CacheSize:    1024,
				L1KeySizeLimit: 4,
				DisableL2Cache: true,
			},
			data:       []byte("1234"),
			loaderHits: 1,
			wantL1:     true,
		},
		{
			name: "L2 exact boundary",
			config: FileIOCacheConfig{
				DisableL1Cache: true,
				L2CacheSize:    8,
				L2KeySizeLimit: 4,
			},
			data:       []byte("1234"),
			loaderHits: 1,
			wantL2:     true,
			wantL2On:   true,
		},
		{
			name: "zero byte in both tiers",
			config: FileIOCacheConfig{
				L1CacheSize:    1024,
				L1KeySizeLimit: 4,
				L2CacheSize:    8,
				L2KeySizeLimit: 4,
			},
			data:       nil,
			loaderHits: 1,
			wantL1:     true,
		},
		{
			name: "over both limits",
			config: FileIOCacheConfig{
				L1CacheSize:    1024,
				L1KeySizeLimit: 2,
				L2CacheSize:    8,
				L2KeySizeLimit: 3,
			},
			data:       []byte("1234"),
			loaderHits: 2,
			wantL2On:   true,
		},
	}
	fileID := uint64(100)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.config.DisableL2Cache {
				test.config.L2CacheDir = filepath.Join(t.TempDir(), "cache")
			}
			cache, err := NewFileIOCache(&test.config)
			require.NoError(t, err)
			registerCacheCleanup(t, cache)
			implementation := cache.(*fileIOCacheImpl)
			identity := testCacheIdentity(fileID, int64(len(test.data)))
			var loaderCalls int32
			for range 2 {
				reader, loadErr := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
					loaderCalls++
					return newBytesStream(test.data), nil
				})
				require.NoError(t, loadErr)
				actual, readErr := io.ReadAll(reader)
				require.NoError(t, readErr)
				require.NoError(t, reader.Close())
				require.True(t, bytes.Equal(test.data, actual))
			}
			require.Equal(t, test.loaderHits, loaderCalls)
			key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
			_, l1Found := implementation.readL1(key, identity.Size)
			require.Equal(t, test.wantL1, l1Found)
			require.Equal(t, test.wantL2On, implementation.l2 != nil)
			l2Found := false
			if implementation.l2 != nil {
				_, l2Found = implementation.l2.entryPath(key)
			}
			require.Equal(t, test.wantL2, l2Found)
		})
		fileID++
	}
}

func TestFileIOCacheCancellationWinsOverCompletedSourceRead(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    1024,
		L1KeySizeLimit: 4,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	ctx, cancel := context.WithCancel(t.Context())
	identity := testCacheIdentity(200, 4)
	_, err = cache.Load(ctx, identity, func(context.Context) (io.ReadSeekCloser, error) {
		return &cancelOnReadSeekCloser{
			ReadSeekCloser: newBytesStream([]byte("data")),
			cancel:         cancel,
		}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	key := buildFileCacheKey(identity)
	_, found := cache.(*fileIOCacheImpl).readL1(key, identity.Size)
	require.False(t, found)
}

type cancelOnReadSeekCloser struct {
	io.ReadSeekCloser
	cancel func()
	once   bool
}

func (r *cancelOnReadSeekCloser) Read(buffer []byte) (int, error) {
	count, err := r.ReadSeekCloser.Read(buffer)
	if !r.once {
		r.once = true
		r.cancel()
	}
	return count, err
}

func withCacheBinding(identity FileCacheIdentity, binding [32]byte) FileCacheIdentity {
	identity.StorageBinding = binding
	return identity
}
