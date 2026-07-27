package filemgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func testL2Config(t *testing.T, cacheSize, keyLimit int) *FileIOCacheConfig {
	t.Helper()
	return &FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    cacheSize,
		L2KeySizeLimit: keyLimit,
		L2CacheDir:     filepath.Join(t.TempDir(), "cache"),
	}
}

func TestFileIOCachePersistsValidatedL2EntryAcrossRestart(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	data := []byte("persistent-data")
	identity := testCacheIdentity(101, int64(len(data)))
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	reader, err := first.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	_, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	closeTestCache(t, first)

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	var loaderCalls atomic.Int32
	reader, err = second.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream([]byte("wrong")), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Zero(t, loaderCalls.Load())
	require.Equal(t, data, actual)
}

func TestFileIOCacheL2ChargesMinimumAllocationAcrossRestart(t *testing.T) {
	config := testL2Config(t, int(2*diskCacheAllocationUnit), 1024)
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	implementation := first.(*fileIOCacheImpl)

	identities := []FileCacheIdentity{
		testCacheIdentity(110, 0),
		testCacheIdentity(111, 1),
		testCacheIdentity(112, 2),
	}
	contents := [][]byte{nil, {'a'}, {'b', 'c'}}
	paths := make([]string, len(identities))
	for index := range 2 {
		reader, loadErr := first.Load(
			t.Context(),
			identities[index],
			func(context.Context) (io.ReadSeekCloser, error) {
				return newBytesStream(contents[index]), nil
			},
		)
		require.NoError(t, loadErr)
		require.NoError(t, reader.Close())
		key := buildFileCacheKey(identities[index])
		var found bool
		paths[index], found = implementation.l2.entryPath(key)
		require.True(t, found)
	}
	require.Equal(t, 2*diskCacheAllocationUnit, implementation.l2.cost())

	reader, err := first.Load(
		t.Context(),
		identities[2],
		func(context.Context) (io.ReadSeekCloser, error) {
			return newBytesStream(contents[2]), nil
		},
	)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	thirdKey := buildFileCacheKey(identities[2])
	var thirdFound bool
	paths[2], thirdFound = implementation.l2.entryPath(thirdKey)
	require.True(t, thirdFound)
	require.Equal(t, 2*diskCacheAllocationUnit, implementation.l2.cost())
	require.NoFileExists(t, paths[0])
	_, firstFound := implementation.l2.entryPath(buildFileCacheKey(identities[0]))
	require.False(t, firstFound)
	closeTestCache(t, first)

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	recovered := second.(*fileIOCacheImpl)
	require.Equal(t, 2*diskCacheAllocationUnit, recovered.l2.cost())
	_, secondFound := recovered.l2.entryPath(buildFileCacheKey(identities[1]))
	_, recoveredThirdFound := recovered.l2.entryPath(thirdKey)
	require.True(t, secondFound)
	require.True(t, recoveredThirdFound)
}

func TestFileIOCacheEnablingL1RemovesRedundantPersistedL2Entry(t *testing.T) {
	config := testL2Config(t, int(2*diskCacheAllocationUnit), 16)
	identity := testCacheIdentity(113, 4)
	data := []byte("tiny")
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	reader, err := first.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	key := buildFileCacheKey(identity)
	oldPath, found := first.(*fileIOCacheImpl).l2.entryPath(key)
	require.True(t, found)
	closeTestCache(t, first)

	withL1 := *config
	withL1.DisableL1Cache = false
	withL1.L1CacheSize = 1024
	withL1.L1KeySizeLimit = 4
	second, err := NewFileIOCache(&withL1)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	implementation := second.(*fileIOCacheImpl)
	require.NoFileExists(t, oldPath)
	require.Zero(t, implementation.l2.cost())

	var loaderCalls atomic.Int32
	for range 2 {
		reader, loadErr := second.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
			loaderCalls.Add(1)
			return newBytesStream(data), nil
		})
		require.NoError(t, loadErr)
		require.NoError(t, reader.Close())
	}
	require.Equal(t, int32(1), loaderCalls.Load())
	_, l1Found := implementation.readL1(key, identity.Size)
	require.True(t, l1Found)
	_, l2Found := implementation.l2.entryPath(key)
	require.False(t, l2Found)
}

func TestDiskCacheEntryCost(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		maxCost int64
		want    int64
	}{
		{name: "negative", size: -1, maxCost: 8192},
		{name: "zero", size: 0, maxCost: 8192, want: 4096},
		{name: "one byte", size: 1, maxCost: 8192, want: 4096},
		{name: "allocation boundary", size: 4096, maxCost: 8192, want: 4096},
		{name: "round up", size: 4097, maxCost: 8192, want: 8192},
		{name: "round over configured capacity", size: 4097, maxCost: 5000, want: 8192},
		{name: "small configured capacity", size: 1, maxCost: 1024, want: 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, diskCacheEntryCost(test.size, test.maxCost))
		})
	}
}

func TestFileIOCacheBindingMismatchInvalidatesPersistedEntries(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	data := []byte("old-binding")
	identity := testCacheIdentity(102, int64(len(data)))
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	reader, err := first.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	implementation := first.(*fileIOCacheImpl)
	oldKey := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
	oldPath, found := implementation.l2.entryPath(oldKey)
	require.True(t, found)
	closeTestCache(t, first)

	changed := *config
	changed.StorageBinding[0] = 1
	second, err := NewFileIOCache(&changed)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	_, err = os.Stat(oldPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	var loaderCalls atomic.Int32
	reader, err = second.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, int32(1), loaderCalls.Load())
}

func TestFileIOCacheRejectsConcurrentL2DirectoryOwner(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	_, err = NewFileIOCache(config)
	require.ErrorIs(t, err, ErrCacheDirInUse)
	closeTestCache(t, first)
	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	closeTestCache(t, second)
}

func TestFileIOCacheDefersEvictedFileRemovalUntilReaderClose(t *testing.T) {
	config := testL2Config(t, 8, 8)
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	firstData := []byte("12345678")
	firstIdentity := testCacheIdentity(201, int64(len(firstData)))
	firstReader, err := cache.Load(t.Context(), firstIdentity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(firstData), nil
	})
	require.NoError(t, err)
	firstKey := buildFileCacheKey(withCacheBinding(firstIdentity, implementation.c.StorageBinding))
	firstPath, found := implementation.l2.entryPath(firstKey)
	require.True(t, found)

	secondData := []byte("abcdefgh")
	secondIdentity := testCacheIdentity(202, int64(len(secondData)))
	secondReader, err := cache.Load(t.Context(), secondIdentity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(secondData), nil
	})
	require.NoError(t, err)
	require.NoError(t, secondReader.Close())
	_, found = implementation.l2.entryPath(firstKey)
	require.False(t, found)
	_, err = os.Stat(firstPath)
	require.NoError(t, err)

	actual, err := io.ReadAll(firstReader)
	require.NoError(t, err)
	require.Equal(t, firstData, actual)
	require.NoError(t, firstReader.Close())
	_, err = os.Stat(firstPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, int64(8), implementation.l2.cost())
}

func TestFileIOCacheCloseWaitsForActiveL2Reader(t *testing.T) {
	cache, err := NewFileIOCache(testL2Config(t, 16, 16))
	require.NoError(t, err)
	data := []byte("active")
	reader, err := cache.Load(t.Context(), testCacheIdentity(301, int64(len(data))), func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	closeContext, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, cache.Close(closeContext), context.DeadlineExceeded)
	require.NoError(t, reader.Close())
	closeTestCache(t, cache)
}

func TestFileIOCacheRecoveryChoosesLexicallyNewestGeneration(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	closeTestCache(t, first)

	data := []byte("duplicate")
	identity := testCacheIdentity(402, int64(len(data)))
	key := buildFileCacheKey(identity)
	digest := sha256.Sum256(data)
	shard := filepath.Join(config.L2CacheDir, diskCacheDirName, key.hex()[:2])
	require.NoError(t, os.MkdirAll(shard, 0o700))
	older := filepath.Join(shard, testDiskCacheFileName(
		key,
		"00000000-0000-4000-8000-000000000001",
		int64(len(data)),
		digest,
	))
	newer := filepath.Join(shard, testDiskCacheFileName(
		key,
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
		int64(len(data)),
		digest,
	))
	require.NoError(t, os.WriteFile(older, data, 0o600))
	require.NoError(t, os.WriteFile(newer, data, 0o600))

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	actualPath, found := second.(*fileIOCacheImpl).l2.entryPath(key)
	require.True(t, found)
	require.Equal(t, newer, actualPath)
	require.NoFileExists(t, older)
	require.FileExists(t, newer)
}

func TestFileIOCacheRecoveryRemovesInvalidManagedNamesAndWrongShards(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	closeTestCache(t, first)

	data := []byte("invalid")
	key := buildFileCacheKey(testCacheIdentity(403, int64(len(data))))
	digest := sha256.Sum256(data)
	root := filepath.Join(config.L2CacheDir, diskCacheDirName)
	wrongPrefix := "00"
	if key.hex()[:2] == wrongPrefix {
		wrongPrefix = "ff"
	}
	wrongShard := filepath.Join(root, wrongPrefix, testDiskCacheFileName(
		key,
		"00000000-0000-4000-8000-000000000001",
		int64(len(data)),
		digest,
	))
	negativeSize := filepath.Join(
		root,
		key.hex()[:2],
		fmt.Sprintf(
			"%s.00000000-0000-4000-8000-000000000002.-1.%s%s",
			key.hex(),
			hex.EncodeToString(digest[:]),
			diskCacheFileSuffix,
		),
	)
	malformed := filepath.Join(root, "zz", "malformed.cache")
	managedTemp := filepath.Join(root, key.hex()[:2], ".00000000-0000-4000-8000-000000000003.temp")
	unrelated := filepath.Join(root, "zz", "keep.txt")
	for _, path := range []string{wrongShard, negativeSize, malformed, managedTemp, unrelated} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	require.NoFileExists(t, wrongShard)
	require.NoFileExists(t, negativeSize)
	require.NoFileExists(t, malformed)
	require.NoFileExists(t, managedTemp)
	require.FileExists(t, unrelated)
}

func TestFileIOCacheRejectedCandidateKeepsAdmittedGeneration(t *testing.T) {
	config := testL2Config(t, 8, 8)
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	data := []byte("12345678")
	identity := testCacheIdentity(407, int64(len(data)))
	reader, err := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	key := buildFileCacheKey(identity)
	admittedPath, found := implementation.l2.entryPath(key)
	require.True(t, found)

	rejected, err := implementation.l2.writeCandidate(
		t.Context(),
		key,
		9,
		bytes.NewReader([]byte("123456789")),
	)
	require.NoError(t, err)
	require.False(t, implementation.l2.admit(rejected))
	implementation.l2.cleanupPaths([]string{rejected.path})
	actualPath, found := implementation.l2.entryPath(key)
	require.True(t, found)
	require.Equal(t, admittedPath, actualPath)
	require.FileExists(t, admittedPath)
	require.NoFileExists(t, rejected.path)
}

func TestFileIOCacheInvalidManifestForcesColdCache(t *testing.T) {
	tests := []struct {
		name      string
		mutateRaw func([]byte) []byte
	}{
		{name: "missing", mutateRaw: func([]byte) []byte { return nil }},
		{name: "malformed", mutateRaw: func([]byte) []byte { return []byte(`{"version":2}`) }},
		{name: "trailing document", mutateRaw: func(raw []byte) []byte { return append(raw, []byte(` {}`)...) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testL2Config(t, 1024, 1024)
			data := []byte("manifest")
			identity := testCacheIdentity(404, int64(len(data)))
			first, err := NewFileIOCache(config)
			require.NoError(t, err)
			reader, err := first.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
				return newBytesStream(data), nil
			})
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			implementation := first.(*fileIOCacheImpl)
			key := buildFileCacheKey(identity)
			oldPath, found := implementation.l2.entryPath(key)
			require.True(t, found)
			closeTestCache(t, first)

			manifest := filepath.Join(config.L2CacheDir, diskCacheDirName, diskCacheManifestName)
			manifestRaw, err := os.ReadFile(manifest)
			require.NoError(t, err)
			mutated := test.mutateRaw(manifestRaw)
			if mutated == nil {
				require.NoError(t, os.Remove(manifest))
			} else {
				require.NoError(t, os.WriteFile(manifest, mutated, 0o600))
			}
			second, err := NewFileIOCache(config)
			require.NoError(t, err)
			registerCacheCleanup(t, second)
			require.NoFileExists(t, oldPath)
			var loaderCalls atomic.Int32
			reader, err = second.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
				loaderCalls.Add(1)
				return newBytesStream(data), nil
			})
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, int32(1), loaderCalls.Load())
		})
	}
}

func TestFileIOCacheStartupRejectsSameSizeDigestCorruption(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	data := []byte("original")
	identity := testCacheIdentity(401, int64(len(data)))
	first, err := NewFileIOCache(config)
	require.NoError(t, err)
	reader, err := first.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	implementation := first.(*fileIOCacheImpl)
	key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
	location, found := implementation.l2.entryPath(key)
	require.True(t, found)
	closeTestCache(t, first)
	require.NoError(t, os.WriteFile(location, []byte("tampered"), 0o600))

	second, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, second)
	var loaderCalls atomic.Int32
	reader, err = second.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, int32(1), loaderCalls.Load())
	require.Equal(t, data, actual)
}

func TestFileIOCacheRuntimeInvalidatesChangedL2Files(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func([]byte) []byte
	}{
		{name: "truncated", corrupt: func(data []byte) []byte { return data[:len(data)-1] }},
		{name: "expanded", corrupt: func(data []byte) []byte { return append(data, 'x') }},
		{name: "same size digest", corrupt: func(data []byte) []byte {
			changed := append([]byte(nil), data...)
			changed[0] ^= 0xff
			return changed
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testL2Config(t, 1024, 1024)
			cache, err := NewFileIOCache(config)
			require.NoError(t, err)
			registerCacheCleanup(t, cache)
			data := []byte("runtime-content")
			identity := testCacheIdentity(405, int64(len(data)))
			reader, err := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
				return newBytesStream(data), nil
			})
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			implementation := cache.(*fileIOCacheImpl)
			key := buildFileCacheKey(identity)
			location, found := implementation.l2.entryPath(key)
			require.True(t, found)
			require.NoError(t, os.WriteFile(location, test.corrupt(data), 0o600))
			changedTime := time.Now().Add(2 * time.Second)
			require.NoError(t, os.Chtimes(location, changedTime, changedTime))

			var loaderCalls atomic.Int32
			reader, err = cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
				loaderCalls.Add(1)
				return newBytesStream(data), nil
			})
			require.NoError(t, err)
			actual, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, data, actual)
			require.Equal(t, int32(1), loaderCalls.Load())
		})
	}
}

func TestFileIOCacheRuntimeDetectsReplacedFileIdentity(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	data := []byte("inode-content")
	identity := testCacheIdentity(406, int64(len(data)))
	reader, err := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	implementation := cache.(*fileIOCacheImpl)
	key := buildFileCacheKey(identity)
	location, found := implementation.l2.entryPath(key)
	require.True(t, found)
	originalInfo, err := os.Stat(location)
	require.NoError(t, err)
	replacement := filepath.Join(filepath.Dir(location), "replacement")
	tampered := append([]byte(nil), data...)
	tampered[0] ^= 0xff
	require.NoError(t, os.WriteFile(replacement, tampered, 0o600))
	require.NoError(t, os.Chtimes(replacement, originalInfo.ModTime(), originalInfo.ModTime()))
	require.NoError(t, os.Remove(location))
	require.NoError(t, os.Rename(replacement, location))
	replacementInfo, err := os.Stat(location)
	require.NoError(t, err)
	require.False(t, os.SameFile(originalInfo, replacementInfo))
	require.Equal(t, originalInfo.ModTime(), replacementInfo.ModTime())

	var loaderCalls atomic.Int32
	reader, err = cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, data, actual)
	require.Equal(t, int32(1), loaderCalls.Load())
}

func TestFileIOCacheCleansStrictLegacyEntriesOnly(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	shard := filepath.Join(base, "aa")
	require.NoError(t, os.MkdirAll(shard, 0o700))
	legacy := filepath.Join(shard, "3#7.cache")
	temporary := filepath.Join(shard, "3#8.cache."+uuid.NewString()+diskCacheTempSuffix)
	unrelated := filepath.Join(shard, "keep.txt")
	require.NoError(t, os.WriteFile(legacy, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(temporary, []byte("tmp"), 0o600))
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		L2CacheSize:    1024,
		L2KeySizeLimit: 1024,
		L2CacheDir:     base,
	})
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	_, err = os.Stat(legacy)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(temporary)
	require.ErrorIs(t, err, os.ErrNotExist)
	actual, err := os.ReadFile(unrelated)
	require.NoError(t, err)
	require.Equal(t, []byte("keep"), actual)
}

func TestFileIOCacheIgnoresManagedSymlink(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	external := filepath.Join(t.TempDir(), "external")
	require.NoError(t, os.WriteFile(external, []byte("external"), 0o600))
	managed := filepath.Join(config.L2CacheDir, diskCacheDirName)
	require.NoError(t, os.MkdirAll(filepath.Join(managed, "aa"), 0o700))
	link := filepath.Join(managed, "aa", "invalid.cache")
	require.NoError(t, os.Symlink(external, link))
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	actual, err := os.ReadFile(external)
	require.NoError(t, err)
	require.Equal(t, []byte("external"), actual)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestFileIOCacheFallsBackWhenL2CannotCreateShard(t *testing.T) {
	config := testL2Config(t, 1024, 1024)
	cache, err := NewFileIOCache(config)
	require.NoError(t, err)
	registerCacheCleanup(t, cache)
	implementation := cache.(*fileIOCacheImpl)
	data := []byte("fallback")
	identity := testCacheIdentity(501, int64(len(data)))
	key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
	shard := filepath.Join(implementation.l2.root, key.hex()[:2])
	require.NoError(t, os.WriteFile(shard, []byte("not-a-directory"), 0o600))
	var loaderCalls atomic.Int32
	reader, err := cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
		loaderCalls.Add(1)
		return newBytesStream(data), nil
	})
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, data, actual)
	require.Equal(t, int32(1), loaderCalls.Load())
	_, found := implementation.l2.entryPath(key)
	require.False(t, found)
}

func TestBuildStorageBindingChangesWithDatabaseAndBackend(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "data.db")
	require.NoError(t, os.WriteFile(database, []byte("db"), 0o600))
	first, err := BuildStorageBinding(database, "localfile", map[string]any{"dir": "/blocks-a"}, 0)
	require.NoError(t, err)
	backendChanged, err := BuildStorageBinding(database, "localfile", map[string]any{"dir": "/blocks-b"}, 0)
	require.NoError(t, err)
	rotateChanged, err := BuildStorageBinding(database, "localfile", map[string]any{"dir": "/blocks-a"}, 1)
	require.NoError(t, err)
	otherDatabase := filepath.Join(root, "other.db")
	require.NoError(t, os.WriteFile(otherDatabase, []byte("db"), 0o600))
	pathChanged, err := BuildStorageBinding(otherDatabase, "localfile", map[string]any{"dir": "/blocks-a"}, 0)
	require.NoError(t, err)
	replacement := filepath.Join(root, "replacement.db")
	require.NoError(t, os.WriteFile(replacement, []byte("replacement"), 0o600))
	require.NoError(t, os.Remove(database))
	require.NoError(t, os.Rename(replacement, database))
	identityChanged, err := BuildStorageBinding(database, "localfile", map[string]any{"dir": "/blocks-a"}, 0)
	require.NoError(t, err)
	require.NotEqual(t, first, backendChanged)
	require.NotEqual(t, first, rotateChanged)
	require.NotEqual(t, first, pathChanged)
	require.NotEqual(t, first, identityChanged)
	require.NotEqual(t, [sha256.Size]byte{}, first)
}

func TestFileIOCacheL2SourceSizeMismatchLeavesNoEntry(t *testing.T) {
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
			cache, err := NewFileIOCache(testL2Config(t, 16, 8))
			require.NoError(t, err)
			registerCacheCleanup(t, cache)
			identity := testCacheIdentity(601, 5)
			_, err = cache.Load(t.Context(), identity, func(context.Context) (io.ReadSeekCloser, error) {
				return newBytesStream(test.data), nil
			})
			require.ErrorIs(t, err, test.wantErr)
			implementation := cache.(*fileIOCacheImpl)
			key := buildFileCacheKey(withCacheBinding(identity, implementation.c.StorageBinding))
			_, found := implementation.l2.entryPath(key)
			require.False(t, found)
			cacheFiles, globErr := filepath.Glob(filepath.Join(implementation.l2.root, "*", "*"+diskCacheFileSuffix))
			require.NoError(t, globErr)
			require.Empty(t, cacheFiles)
		})
	}
}

func testDiskCacheFileName(
	key fileCacheKey,
	generation string,
	size int64,
	digest [sha256.Size]byte,
) string {
	return fmt.Sprintf(
		"%s.%s.%d.%s%s",
		key.hex(),
		generation,
		size,
		hex.EncodeToString(digest[:]),
		diskCacheFileSuffix,
	)
}
