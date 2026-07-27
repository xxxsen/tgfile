package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInspectL2CacheDirectory(t *testing.T) {
	t.Run("valid entry", func(t *testing.T) {
		cacheDir := t.TempDir()
		content := []byte("valid soak cache entry")
		writeSoakCacheEntry(t, cacheDir, content)

		result, err := inspectL2CacheDirectory(cacheDir, int64(len(content)))

		require.NoError(t, err)
		require.Equal(t, int64(1), result.files)
		require.Equal(t, int64(len(content)), result.bytes)
		require.Zero(t, result.tempFiles)
	})

	t.Run("digest mismatch", func(t *testing.T) {
		cacheDir := t.TempDir()
		entry := writeSoakCacheEntry(t, cacheDir, []byte("original"))
		require.NoError(t, os.WriteFile(entry, []byte("tampered"), 0o600))

		_, err := inspectL2CacheDirectory(cacheDir, 1024)

		require.ErrorIs(t, err, errAuditInvariant)
		require.ErrorContains(t, err, "digest mismatch")
	})

	t.Run("temporary file", func(t *testing.T) {
		cacheDir := t.TempDir()
		entry := writeSoakCacheEntry(t, cacheDir, []byte("entry"))
		temporary := filepath.Join(filepath.Dir(entry), ".retired.temp")
		require.NoError(t, os.WriteFile(temporary, nil, 0o600))

		result, err := inspectL2CacheDirectory(cacheDir, 1024)

		require.ErrorIs(t, err, errAuditInvariant)
		require.Equal(t, int64(1), result.tempFiles)
	})

	t.Run("capacity boundary", func(t *testing.T) {
		cacheDir := t.TempDir()
		content := []byte("capacity")
		writeSoakCacheEntry(t, cacheDir, content)

		_, err := inspectL2CacheDirectory(cacheDir, int64(len(content)-1))

		require.ErrorIs(t, err, errAuditInvariant)
		require.ErrorContains(t, err, "limit")
	})
}

func writeSoakCacheEntry(t *testing.T, cacheDir string, content []byte) string {
	t.Helper()
	key := strings.Repeat("ab", sha256.Size)
	root := filepath.Join(cacheDir, "v2")
	shard := filepath.Join(root, key[:2])
	require.NoError(t, os.MkdirAll(shard, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".manifest"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".lock"), nil, 0o600))
	digest := sha256.Sum256(content)
	name := fmt.Sprintf(
		"%s.00000000-0000-4000-8000-000000000001.%d.%s.cache",
		key,
		len(content),
		hex.EncodeToString(digest[:]),
	)
	entry := filepath.Join(shard, name)
	require.NoError(t, os.WriteFile(entry, content, 0o600))
	return entry
}
