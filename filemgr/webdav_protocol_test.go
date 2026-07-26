package filemgr

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
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
