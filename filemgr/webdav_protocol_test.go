package filemgr

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/entity"
)

func TestWebDAVOverwriteAndDeleteQueueEveryUnreferencedFile(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 32)
	manager := managerInterface.(*defaultFileManager)
	oldContent := []byte("old")
	oldFileID, err := manager.CreateFile(t.Context(), int64(len(oldContent)), bytes.NewReader(oldContent))
	require.NoError(t, err)
	_, err = manager.PublishS3Object(
		t.Context(),
		"/bucket/object.bin",
		oldFileID,
		int64(len(oldContent)),
		testObjectMetadata(`"old"`),
		nil,
	)
	require.NoError(t, err)

	newContent := []byte("replacement")
	newFileID, err := manager.CreateFile(t.Context(), int64(len(newContent)), bytes.NewReader(newContent))
	require.NoError(t, err)
	_, err = manager.PublishWebDAVFile(
		t.Context(),
		"/bucket/object.bin",
		newFileID,
		int64(len(newContent)),
		WebDAVMutationOptions{Principal: "editor"},
	)
	require.NoError(t, err)
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = `+strconv.FormatUint(oldFileID, 10)+` AND delete_state = 'pending'`,
	))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = `+strconv.FormatUint(newFileID, 10)+` AND delete_state = 'live'`,
	))

	require.NoError(t, manager.DeleteWebDAVResource(
		t.Context(),
		"/bucket/object.bin",
		WebDAVMutationOptions{Principal: "editor"},
	))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = `+strconv.FormatUint(newFileID, 10)+` AND delete_state = 'pending'`,
	))
}

func TestWebDAVOverwriteWithEmptyFileDeletesUnreferencedOldBlocks(t *testing.T) {
	managerInterface, block, databaseClient := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	oldContent := []byte("stored data")
	oldFileID, err := manager.CreateFile(
		t.Context(),
		int64(len(oldContent)),
		bytes.NewReader(oldContent),
	)
	require.NoError(t, err)
	_, err = manager.PublishS3Object(
		t.Context(),
		"/bucket/object.bin",
		oldFileID,
		int64(len(oldContent)),
		testObjectMetadata(`"old"`),
		nil,
	)
	require.NoError(t, err)

	emptyFileID, err := manager.CreateFile(t.Context(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	_, err = manager.PublishWebDAVFile(
		t.Context(),
		"/bucket/object.bin",
		emptyFileID,
		0,
		WebDAVMutationOptions{Principal: "editor"},
	)
	require.NoError(t, err)

	link, err := manager.StatFileLink(t.Context(), "/bucket/object.bin")
	require.NoError(t, err)
	require.Equal(t, emptyFileID, link.FileId)
	require.Zero(t, link.FileSize)
	meta, err := manager.StatFile(t.Context(), emptyFileID)
	require.NoError(t, err)
	require.Equal(t, entity.EmptyFileMD5Sum, meta.Md5Sum)
	stream, err := manager.OpenFile(t.Context(), emptyFileID)
	require.NoError(t, err)
	content, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Empty(t, content)

	oldFileIDText := strconv.FormatUint(oldFileID, 10)
	require.Equal(t, 3, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = `+oldFileIDText+` AND delete_state = 'pending'`,
	))
	require.Len(t, block.parts, 3)
	require.NoError(t, manager.processBlockDeleteBatch(t.Context()))
	require.Empty(t, block.parts)
	require.Equal(t, 3, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = `+oldFileIDText+` AND delete_state = 'deleted'`,
	))
	require.Equal(t, 3, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = `+oldFileIDText,
	))
	require.Zero(t, queryCount(
		t,
		databaseClient,
		`SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = `+strconv.FormatUint(emptyFileID, 10),
	))
}
