package filemgr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileIOCacheCoalescesConcurrentColdLoads(t *testing.T) {
	const workers = 32
	data := bytes.Repeat([]byte("cache-race"), 512)
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    1024 * 1024,
		L2KeySizeLimit: len(data),
		L2CacheDir:     filepath.Join(t.TempDir(), "cache"),
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	identity := testCacheIdentity(42, int64(len(data)))

	start := make(chan struct{})
	loaderEntered := make(chan struct{})
	releaseLoader := make(chan struct{})
	var loaderCalls atomic.Int32
	loader := func(ctx context.Context) (io.ReadSeekCloser, error) {
		if loaderCalls.Add(1) == 1 {
			close(loaderEntered)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-releaseLoader:
			return newBytesStream(data), nil
		}
	}

	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			<-start
			reader, loadErr := cache.Load(t.Context(), identity, loader)
			if loadErr != nil {
				errorsByWorker <- loadErr
				return
			}
			actual, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				errorsByWorker <- readErr
				return
			}
			if closeErr != nil {
				errorsByWorker <- closeErr
				return
			}
			if !bytes.Equal(data, actual) {
				errorsByWorker <- io.ErrUnexpectedEOF
			}
		}()
	}
	close(start)
	<-loaderEntered
	deadline := time.Now().Add(5 * time.Second)
	for implementation.stats.fillFollower.Load() != workers-1 {
		if time.Now().After(deadline) {
			t.Fatalf("followers did not join fill: got %d", implementation.stats.fillFollower.Load())
		}
		runtime.Gosched()
	}
	close(releaseLoader)
	group.Wait()
	close(errorsByWorker)
	for workerErr := range errorsByWorker {
		require.NoError(t, workerErr)
	}
	require.Equal(t, int32(1), loaderCalls.Load())
}

func TestFileIOCacheRejectsStaleFileIDWithDifferentRevision(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	config := &FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    1024,
		L2KeySizeLimit: 1024,
		L2CacheDir:     cacheDir,
	}
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	oldData := []byte("old")
	oldIdentity := testCacheIdentity(7, int64(len(oldData)))
	reader, err := first.Load(t.Context(), oldIdentity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(oldData), nil
	})
	require.NoError(t, err)
	_, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	closeTestCache(t, first)

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	newData := []byte("new-content")
	newIdentity := testCacheIdentity(7, int64(len(newData)))
	newIdentity.Mtime++
	var loaderCalls atomic.Int32
	reader, err = second.Load(t.Context(), newIdentity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(newData), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, int32(1), loaderCalls.Load())
	require.Equal(t, newData, actual)
}

func TestFileIOCacheRebuildsTruncatedL2Entry(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    1024,
		L2KeySizeLimit: 1024,
		L2CacheDir:     filepath.Join(t.TempDir(), "cache"),
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	expected := []byte("complete-content")
	identity := testCacheIdentity(8, int64(len(expected)))
	reader, err := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(expected), nil
	})
	require.NoError(t, err)
	_, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	implementation := cache.(*fileIOCacheImpl)
	key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
	location, found := implementation.l2.entryPath(key)
	require.True(t, found)
	require.NoError(t, os.WriteFile(location, []byte("short"), 0o600))
	var loaderCalls atomic.Int32
	reader, err = cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(expected), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, int32(1), loaderCalls.Load())
	require.Equal(t, expected, actual)
}

func TestFileIOCacheIgnoresDisabledL2LimitForL1(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    100,
		L1KeySizeLimit: 10,
		DisableL2Cache: true,
		L2KeySizeLimit: 5,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	data := []byte("12345678")
	identity := testCacheIdentity(9, int64(len(data)))
	var loaderCalls atomic.Int32
	loader := func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(data), nil
	}
	for range 2 {
		reader, loadErr := cache.Load(t.Context(), identity, loader)
		require.NoError(t, loadErr)
		actual, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		require.Equal(t, data, actual)
	}
	require.Equal(t, int32(1), loaderCalls.Load())
}

func TestFileIOCacheFillsDifferentKeysConcurrently(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    32,
		L1KeySizeLimit: 16,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)

	entered := make(chan uint64, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	for fileID := uint64(1); fileID <= 2; fileID++ {
		go func() {
			data := []byte{1}
			reader, loadErr := cache.Load(
				t.Context(),
				testCacheIdentity(fileID, int64(len(data))),
				func(context.Context) (io.ReadSeekCloser, error) {
					entered <- fileID
					<-release
					return newBytesStream(data), nil
				},
			)
			if loadErr == nil {
				_, loadErr = io.ReadAll(reader)
				loadErr = errors.Join(loadErr, reader.Close())
			}
			results <- loadErr
		}()
	}
	requireSignal(t, entered)
	requireSignal(t, entered)
	close(release)
	require.NoError(t, requireSignal(t, results))
	require.NoError(t, requireSignal(t, results))
}

func TestFileIOCacheCanceledFollowerDoesNotCancelLeader(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    32,
		L1KeySizeLimit: 16,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	identity := testCacheIdentity(71, 4)
	data := []byte("data")
	leaderEntered := make(chan struct{})
	releaseLeader := make(chan struct{})
	leaderResult := make(chan error, 1)
	var loaderCalls atomic.Int32
	loader := func(ctx context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		close(leaderEntered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-releaseLeader:
			return newBytesStream(data), nil
		}
	}
	go func() {
		reader, loadErr := cache.Load(t.Context(), identity, loader)
		if loadErr == nil {
			_, loadErr = io.ReadAll(reader)
			loadErr = errors.Join(loadErr, reader.Close())
		}
		leaderResult <- loadErr
	}()
	<-leaderEntered

	followerContext, cancelFollower := context.WithCancel(t.Context())
	followerResult := make(chan error, 1)
	go func() {
		_, loadErr := cache.Load(followerContext, identity, loader)
		followerResult <- loadErr
	}()
	waitForAtomicValue(t, &implementation.stats.fillFollower, 1)
	cancelFollower()
	require.ErrorIs(t, requireSignal(t, followerResult), context.Canceled)
	require.Equal(t, int32(1), loaderCalls.Load())

	close(releaseLeader)
	require.NoError(t, requireSignal(t, leaderResult))
}

func TestFileIOCacheFollowerRetriesAfterFailedLeader(t *testing.T) {
	const followerCount = 8
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    64,
		L1KeySizeLimit: 16,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	identity := testCacheIdentity(72, 4)
	data := []byte("data")
	sentinel := errors.New("first source failure")
	leaderEntered := make(chan struct{})
	releaseLeader := make(chan struct{})
	var loaderCalls atomic.Int32
	loader := func(context.Context) (io.ReadSeekCloser, error) {
		if loaderCalls.Add(1) == 1 {
			close(leaderEntered)
			<-releaseLeader
			return nil, sentinel
		}
		return newBytesStream(data), nil
	}
	leaderResult := make(chan error, 1)
	go func() {
		_, loadErr := cache.Load(t.Context(), identity, loader)
		leaderResult <- loadErr
	}()
	<-leaderEntered

	followerResults := make(chan error, followerCount)
	for range followerCount {
		go func() {
			reader, loadErr := cache.Load(t.Context(), identity, loader)
			if loadErr == nil {
				actual, readErr := io.ReadAll(reader)
				loadErr = errors.Join(readErr, reader.Close())
				if !bytes.Equal(actual, data) {
					loadErr = errors.Join(loadErr, io.ErrUnexpectedEOF)
				}
			}
			followerResults <- loadErr
		}()
	}
	waitForAtomicValue(t, &implementation.stats.fillFollower, followerCount)
	close(releaseLeader)
	require.ErrorIs(t, requireSignal(t, leaderResult), sentinel)
	for range followerCount {
		require.NoError(t, requireSignal(t, followerResults))
	}
	require.Equal(t, int32(2), loaderCalls.Load())
}

func TestFileIOCacheFollowerRetriesAfterCanceledLeader(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    64,
		L1KeySizeLimit: 16,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	identity := testCacheIdentity(74, 4)
	leaderEntered := make(chan struct{})
	var loaderCalls atomic.Int32
	loader := func(ctx context.Context) (io.ReadSeekCloser, error) {
		if loaderCalls.Add(1) == 1 {
			close(leaderEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return newBytesStream([]byte("data")), nil
	}
	leaderContext, cancelLeader := context.WithCancel(t.Context())
	leaderResult := make(chan error, 1)
	go func() {
		_, loadErr := cache.Load(leaderContext, identity, loader)
		leaderResult <- loadErr
	}()
	<-leaderEntered

	followerResult := make(chan error, 1)
	go func() {
		reader, loadErr := cache.Load(t.Context(), identity, loader)
		if loadErr == nil {
			actual, readErr := io.ReadAll(reader)
			loadErr = errors.Join(readErr, reader.Close())
			if !bytes.Equal(actual, []byte("data")) {
				loadErr = errors.Join(loadErr, io.ErrUnexpectedEOF)
			}
		}
		followerResult <- loadErr
	}()
	waitForAtomicValue(t, &implementation.stats.fillFollower, 1)
	cancelLeader()
	require.ErrorIs(t, requireSignal(t, leaderResult), context.Canceled)
	require.NoError(t, requireSignal(t, followerResult))
	require.Equal(t, int32(2), loaderCalls.Load())
}

func TestFileIOCacheLeaderDoubleChecksBeforeLoading(t *testing.T) {
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		L1CacheSize:    32,
		L1KeySizeLimit: 16,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	identity := testCacheIdentity(75, 4)
	key := buildFileCacheKey(identity)
	implementation.writeL1(key, []byte("data"))
	call, leader, err := implementation.beginFill(key)
	require.NoError(t, err)
	require.True(t, leader)
	var loaderCalls atomic.Int32
	reader, err := implementation.runFillLeader(
		t.Context(),
		key,
		identity.Size,
		true,
		false,
		func(context.Context) (io.ReadSeekCloser, error) {
			loaderCalls.Add(1)
			return newBytesStream([]byte("wrong")), nil
		},
		call,
	)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, []byte("data"), actual)
	require.Zero(t, loaderCalls.Load())
}

func TestFileIOCacheCloseTimesOutDuringFillAndCanRetry(t *testing.T) {
	config := testL2Config(t, 32, 16)
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	identity := testCacheIdentity(73, 4)
	loaderEntered := make(chan struct{})
	releaseLoader := make(chan struct{})
	loadResult := make(chan error, 1)
	go func() {
		reader, loadErr := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
			close(loaderEntered)
			<-releaseLoader
			return newBytesStream([]byte("data")), nil
		})
		if loadErr == nil {
			_, loadErr = io.ReadAll(reader)
			loadErr = errors.Join(loadErr, reader.Close())
		}
		loadResult <- loadErr
	}()
	<-loaderEntered

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, cache.Close(expired), context.DeadlineExceeded)
	_, err = cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream([]byte("data")), nil
	})
	require.ErrorIs(t, err, ErrCacheClosed)

	close(releaseLoader)
	require.NoError(t, requireSignal(t, loadResult))
	closeTestCache(t, cache)
}

func TestFileIOCacheConcurrentCloseHonorsWaitingContext(t *testing.T) {
	cache, err := NewFileIOCache(testL2Config(t, 32, 16))
	require.NoError(t, err)
	reader, err := cache.Load(t.Context(), testCacheIdentity(76, 4), func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream([]byte("data")), nil
	})
	require.NoError(t, err)
	implementation := cache.(*fileIOCacheImpl)
	firstClose := make(chan error, 1)
	go func() {
		firstClose <- cache.Close(context.Background())
	}()
	waitForDiskClosing(t, implementation.l2)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, cache.Close(expired), context.DeadlineExceeded)
	require.NoError(t, reader.Close())
	require.NoError(t, requireSignal(t, firstClose))
}

func TestBuildFileCacheKeyIncludesEveryIdentityField(t *testing.T) {
	base := testCacheIdentity(80, 4)
	base.StorageBinding[0] = 1
	tests := []struct {
		name   string
		mutate func(*FileCacheIdentity)
	}{
		{name: "storage binding", mutate: func(value *FileCacheIdentity) { value.StorageBinding[1]++ }},
		{name: "file ID", mutate: func(value *FileCacheIdentity) { value.FileID++ }},
		{name: "size", mutate: func(value *FileCacheIdentity) { value.Size++ }},
		{name: "part count", mutate: func(value *FileCacheIdentity) { value.PartCount++ }},
		{name: "state", mutate: func(value *FileCacheIdentity) { value.State++ }},
		{name: "layout", mutate: func(value *FileCacheIdentity) { value.LayoutVersion++ }},
		{name: "ctime", mutate: func(value *FileCacheIdentity) { value.Ctime++ }},
		{name: "mtime", mutate: func(value *FileCacheIdentity) { value.Mtime++ }},
		{name: "extinfo", mutate: func(value *FileCacheIdentity) { value.ExtInfo += "x" }},
	}
	baseKey := buildFileCacheKey(base)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.mutate(&changed)
			require.NotEqual(t, baseKey, buildFileCacheKey(changed))
		})
	}
}

func requireSignal[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for synchronized test event")
		var zero T
		return zero
	}
}

func waitForAtomicValue(t *testing.T, value *atomic.Uint64, expected uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for value.Load() < expected {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for atomic value: got %d, want %d", value.Load(), expected)
		}
		runtime.Gosched()
	}
}

func waitForDiskClosing(t *testing.T, cache *diskCache) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cache.mu.Lock()
		closing := cache.closing
		cache.mu.Unlock()
		if closing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for disk cache close")
		}
		runtime.Gosched()
	}
}
