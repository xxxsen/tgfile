package filemgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"

	"github.com/xxxsen/tgfile/blockio"
	"github.com/xxxsen/tgfile/db"
)

type captureBlockIO struct {
	maxSize       int64
	mutex         sync.Mutex
	parts         map[string][]byte
	order         []string
	downloadCount int
}

func (b *captureBlockIO) Name() string {
	return "capture"
}

func (b *captureBlockIO) MaxFileSize() int64 {
	return b.maxSize
}

func (b *captureBlockIO) Upload(_ context.Context, reader io.Reader) (*blockio.UploadResult, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	key := fmt.Sprintf("part-%d", len(b.order))
	b.parts[key] = append([]byte(nil), raw...)
	b.order = append(b.order, key)
	return &blockio.UploadResult{FileKey: key, DeleteRef: key, UploadedAt: time.Now().UnixMilli()}, nil
}

func (b *captureBlockIO) DeleteBlocks(_ context.Context, deleteRefs []string) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	for _, ref := range deleteRefs {
		delete(b.parts, ref)
	}
	return nil
}

func (b *captureBlockIO) Download(_ context.Context, key string, position int64) (io.ReadCloser, error) {
	b.mutex.Lock()
	raw, ok := b.parts[key]
	b.downloadCount++
	b.mutex.Unlock()
	if !ok {
		return nil, errors.New("part not found")
	}
	if position > int64(len(raw)) {
		position = int64(len(raw))
	}
	return io.NopCloser(bytes.NewReader(raw[position:])), nil
}

func newCreateFileTestManager(t *testing.T, blockSize int64) (IFileManager, *captureBlockIO, database.IDatabase) {
	t.Helper()
	databaseClient, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, databaseClient.Close())
	})
	cache, err := NewFileIOCache(&FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	block := &captureBlockIO{
		maxSize: blockSize,
		parts:   make(map[string][]byte),
	}
	return NewFileManager(databaseClient, block, cache), block, databaseClient
}

func queryCount(t *testing.T, databaseClient database.IDatabase, query string) int {
	t.Helper()
	rows, err := databaseClient.QueryContext(context.Background(), query)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var count int
	require.NoError(t, rows.Scan(&count))
	require.NoError(t, rows.Err())
	return count
}

func TestCreateFileValidatesSizeBeforeCreatingDraft(t *testing.T) {
	tests := []struct {
		name      string
		size      int64
		blockSize int64
		wantError error
	}{
		{name: "negative size", size: -1, blockSize: 4, wantError: ErrInvalidFileSize},
		{name: "invalid block size", size: 1, blockSize: 0, wantError: ErrInvalidBlockSize},
		{name: "too many parts", size: maxFilePartCount + 1, blockSize: 1, wantError: ErrTooManyFileParts},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, block, databaseClient := newCreateFileTestManager(t, test.blockSize)
			_, err := manager.CreateFile(context.Background(), test.size, bytes.NewReader(nil))
			require.ErrorIs(t, err, test.wantError)
			require.Empty(t, block.order)
			require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_tab;"))
		})
	}
}

func TestCreateFileUsesExactPartBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		content   []byte
		wantParts [][]byte
	}{
		{
			name:      "single byte",
			content:   []byte("a"),
			wantParts: [][]byte{[]byte("a")},
		},
		{
			name:      "partial final part",
			content:   []byte("abcde"),
			wantParts: [][]byte{[]byte("abcd"), []byte("e")},
		},
		{
			name:      "exact block multiple",
			content:   []byte("abcdefgh"),
			wantParts: [][]byte{[]byte("abcd"), []byte("efgh")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, block, _ := newCreateFileTestManager(t, 4)
			fileID, err := manager.CreateFile(
				context.Background(),
				int64(len(test.content)),
				bytes.NewReader(test.content),
			)
			require.NoError(t, err)
			require.Len(t, block.order, len(test.wantParts))
			for index, wantPart := range test.wantParts {
				require.Equal(t, wantPart, block.parts[block.order[index]])
			}

			meta, err := manager.StatFile(context.Background(), fileID)
			require.NoError(t, err)
			require.EqualValues(t, len(test.wantParts), meta.FilePartCount)
			reader, err := manager.OpenFile(context.Background(), fileID)
			require.NoError(t, err)
			defer reader.Close()
			actual, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, test.content, actual)
		})
	}
}

func TestCreateFileRejectsShortReaderWithoutReadyFile(t *testing.T) {
	manager, _, databaseClient := newCreateFileTestManager(t, 4)

	_, err := manager.CreateFile(context.Background(), 5, bytes.NewReader([]byte("abcd")))
	require.ErrorIs(t, err, ErrFileShortRead)

	require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_tab WHERE file_state = 2;"))
}

func TestCreateFileCompensatesUploadedBlockWhenDeleteStateCannotPersist(t *testing.T) {
	manager, block, databaseClient := newCreateFileTestManager(t, 4)
	_, err := databaseClient.ExecContext(t.Context(), `
CREATE TRIGGER reject_create_file_delete_state
BEFORE INSERT ON tg_file_part_delete_state_tab
BEGIN
    SELECT RAISE(ABORT, 'reject delete state');
END;`)
	require.NoError(t, err)

	_, err = manager.CreateFile(t.Context(), 1, bytes.NewBufferString("x"))

	require.Error(t, err)
	require.Empty(t, block.parts)
	require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_part_tab"))
}

func TestCreateEmptyFile(t *testing.T) {
	manager, block, _ := newCreateFileTestManager(t, 4)
	fileID, err := manager.CreateFile(context.Background(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	require.Empty(t, block.order)
	meta, err := manager.StatFile(context.Background(), fileID)
	require.NoError(t, err)
	require.Zero(t, meta.FilePartCount)
	require.Zero(t, meta.FileSize)
}
