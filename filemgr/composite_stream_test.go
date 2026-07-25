package filemgr

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
)

func TestCompositeStreamReadsAndSeeksAcrossSourceAndBlockBoundaries(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	contents := [][]byte{
		nil,
		[]byte("abcdefghijklmno"),
		[]byte("pqrstuvwxyz0123"),
		[]byte("456789"),
	}
	sourceIDs := make([]uint64, 0, len(contents))
	var expected []byte
	for _, content := range contents {
		fileID, err := manager.CreateFile(t.Context(), int64(len(content)), bytes.NewReader(content))
		require.NoError(t, err)
		sourceIDs = append(sourceIDs, fileID)
		expected = append(expected, content...)
	}
	const finalFileID uint64 = 9_000_001
	insertCompositeForTest(t, databaseClient, finalFileID, sourceIDs, contents)

	reader, err := manager.OpenFile(t.Context(), finalFileID)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, expected, actual)

	for _, offset := range []int64{0, 1, 3, 4, 14, 15, 16, 29, 30, int64(len(expected))} {
		position, err := reader.Seek(offset, io.SeekStart)
		require.NoError(t, err)
		require.Equal(t, offset, position)
		buffer := make([]byte, 7)
		count, readErr := reader.Read(buffer)
		require.Equal(t, expected[offset:min(int64(len(expected)), offset+int64(len(buffer)))], buffer[:count])
		if offset == int64(len(expected)) {
			require.ErrorIs(t, readErr, io.EOF)
		}
	}
	position, err := reader.Seek(-5, io.SeekEnd)
	require.NoError(t, err)
	require.Equal(t, int64(len(expected)-5), position)
	tail, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, expected[len(expected)-5:], tail)
	require.NoError(t, reader.Close())
	_, err = reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, ErrFileNotOpen)
}

func TestCompositeStreamRejectsCorruptManifest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, databaseClient database.IDatabase, finalID, sourceID uint64)
	}{
		{
			name: "index gap",
			mutate: func(t *testing.T, databaseClient database.IDatabase, finalID, _ uint64) {
				t.Helper()
				_, err := databaseClient.ExecContext(
					t.Context(),
					"UPDATE tg_s3_file_segment_tab SET segment_index = 1 WHERE file_id = ?",
					finalID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "size mismatch",
			mutate: func(t *testing.T, databaseClient database.IDatabase, finalID, _ uint64) {
				t.Helper()
				_, err := databaseClient.ExecContext(
					t.Context(),
					"UPDATE tg_s3_file_segment_tab SET segment_size = 2 WHERE file_id = ?",
					finalID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "missing source",
			mutate: func(t *testing.T, databaseClient database.IDatabase, _, sourceID uint64) {
				t.Helper()
				_, err := databaseClient.ExecContext(
					t.Context(),
					"DELETE FROM tg_file_tab WHERE file_id = ?",
					sourceID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "recursive source",
			mutate: func(t *testing.T, databaseClient database.IDatabase, _, sourceID uint64) {
				t.Helper()
				_, err := databaseClient.ExecContext(
					t.Context(),
					"UPDATE tg_file_tab SET file_layout_version = 2 WHERE file_id = ?",
					sourceID,
				)
				require.NoError(t, err)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			managerInterface, _, databaseClient := newCreateFileTestManager(t, 4)
			manager := managerInterface.(*defaultFileManager)
			sourceID, err := manager.CreateFile(t.Context(), 1, bytes.NewReader([]byte("x")))
			require.NoError(t, err)
			const finalID uint64 = 9_000_002
			insertCompositeForTest(t, databaseClient, finalID, []uint64{sourceID}, [][]byte{[]byte("x")})
			test.mutate(t, databaseClient, finalID, sourceID)

			_, err = manager.OpenFile(t.Context(), finalID)
			require.ErrorIs(t, err, ErrInvalidComposite)
		})
	}
}

func insertCompositeForTest(
	t *testing.T,
	databaseClient database.IDatabase,
	finalFileID uint64,
	sourceIDs []uint64,
	contents [][]byte,
) {
	t.Helper()
	var size int64
	var partCount int64
	for index, sourceID := range sourceIDs {
		size += int64(len(contents[index]))
		var count int64
		require.NoError(t, queryRow(
			t.Context(),
			databaseClient,
			"SELECT file_part_count FROM tg_file_tab WHERE file_id = ?",
			sourceID,
		).Scan(&count))
		partCount += count
	}
	now := time.Now().UnixMilli()
	_, err := databaseClient.ExecContext(
		t.Context(),
		`INSERT INTO tg_file_tab (
file_id, file_size, file_part_count, file_state, ctime, mtime, extinfo, file_layout_version
) VALUES (?, ?, ?, 2, ?, ?, '{}', 2)`,
		finalFileID,
		size,
		partCount,
		now,
		now,
	)
	require.NoError(t, err)
	for index, sourceID := range sourceIDs {
		_, err := databaseClient.ExecContext(
			t.Context(),
			`INSERT INTO tg_s3_file_segment_tab (
file_id, segment_index, source_file_id, segment_size, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?)`,
			finalFileID,
			index,
			sourceID,
			len(contents[index]),
			now,
			now,
		)
		require.NoError(t, err)
	}
}
