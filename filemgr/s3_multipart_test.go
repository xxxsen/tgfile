package filemgr

import (
	"bytes"
	"encoding/hex"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMultipartCompleteCreatesReadableCompositeAndDeleteMarksSources(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 6*1024*1024)
	manager := managerInterface.(*defaultFileManager)
	firstContent := bytes.Repeat([]byte("a"), 5*1024*1024)
	secondContent := []byte("tail")
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "multipart.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)

	first := createAndRegisterMultipartPart(t, manager, upload, 1, firstContent)
	second := createAndRegisterMultipartPart(t, manager, upload, 2, secondContent)
	page, err := manager.ListMultipartParts(t.Context(), &ListMultipartPartsRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		MaxParts: 1000,
	})
	require.NoError(t, err)
	require.Equal(t, []int{1, 2}, []int{page.Parts[0].PartNumber, page.Parts[1].PartNumber})

	result, err := manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []CompleteMultipartPart{
			{PartNumber: 1, ETag: first.ETag},
			{PartNumber: 2, ETag: second.ETag},
		},
	})
	require.NoError(t, err)
	require.Equal(t, int64(len(firstContent)+len(secondContent)), result.Size)
	require.Equal(t, 2, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_file_segment_tab",
	))

	metadata, err := manager.StatFile(t.Context(), result.FileID)
	require.NoError(t, err)
	require.Equal(t, int32(2), metadata.LayoutVersion)
	reader, err := manager.OpenFile(t.Context(), result.FileID)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, append(firstContent, secondContent...), actual)

	retried, err := manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []CompleteMultipartPart{
			{PartNumber: 1, ETag: first.ETag},
			{PartNumber: 2, ETag: second.ETag},
		},
	})
	require.NoError(t, err)
	require.Equal(t, result, retried)

	require.NoError(t, manager.CreateFileLink(
		t.Context(),
		"/webdav-dir/first.bin",
		result.FileID,
		result.Size,
		false,
	))
	require.NoError(t, manager.CreateFileLink(
		t.Context(),
		"/webdav-dir/nested/second.bin",
		result.FileID,
		result.Size,
		false,
	))
	_, err = manager.DeleteS3Object(t.Context(), "/bucket/multipart.bin", nil)
	require.NoError(t, err)
	require.Zero(t, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))
	require.NoError(t, manager.RemoveFileLink(t.Context(), "/webdav-dir"))
	require.Equal(t, 2, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))
}

func TestMultipartAbortAndReplacementUseDurableCleanup(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 6*1024*1024)
	manager := managerInterface.(*defaultFileManager)
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "abort.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	oldPart := createAndRegisterMultipartPart(t, manager, upload, 1, []byte("old"))
	newPart := createAndRegisterMultipartPart(t, manager, upload, 1, []byte("new"))
	require.NotEqual(t, oldPart.FileID, newPart.FileID)
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))

	require.NoError(t, manager.AbortMultipartUpload(t.Context(), &AbortMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
	}))
	require.NoError(t, manager.AbortMultipartUpload(t.Context(), &AbortMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
	}))
	require.Equal(t, 2, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))
	_, err = manager.ListMultipartParts(t.Context(), &ListMultipartPartsRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		MaxParts: 1000,
	})
	require.ErrorIs(t, err, ErrNoSuchUpload)
}

func TestMultipartExpiryAndControlCleanupAreDurable(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "expired.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	createAndRegisterMultipartPart(t, manager, upload, 1, []byte("expired"))
	now := time.Now()
	_, err = databaseClient.ExecContext(
		t.Context(),
		"UPDATE tg_s3_multipart_upload_tab SET expires_at = ? WHERE upload_id = ?",
		now.Add(-time.Second).UnixMilli(),
		upload.UploadID,
	)
	require.NoError(t, err)

	require.NoError(t, manager.processExpiredMultipartUploads(t.Context(), now, 100))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_multipart_upload_tab WHERE upload_state = 'aborted'",
	))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))

	_, err = databaseClient.ExecContext(
		t.Context(),
		"UPDATE tg_s3_multipart_upload_tab SET cleanup_at = ? WHERE upload_id = ?",
		now.Add(-time.Second).UnixMilli(),
		upload.UploadID,
	)
	require.NoError(t, err)
	require.NoError(t, manager.purgeMultipartControlRows(t.Context(), now, 100))
	require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_s3_multipart_upload_tab"))
	require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_s3_multipart_part_tab"))
	require.Equal(t, 1, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_tab"))
	require.Equal(t, 1, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_part_delete_state_tab"))
}

func TestMultipartRequiresCompleteLiveDeletionReferences(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "strict-references.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	part := createAndRegisterMultipartPart(t, manager, upload, 1, []byte("content"))
	_, err = databaseClient.ExecContext(
		t.Context(),
		"DELETE FROM tg_file_part_delete_state_tab WHERE file_id = ?",
		part.FileID,
	)
	require.NoError(t, err)

	_, err = manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []CompleteMultipartPart{{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		}},
	})
	require.ErrorIs(t, err, ErrInvalidMultipartPart)
	require.Zero(t, queryCount(t, databaseClient, "SELECT COUNT(*) FROM tg_s3_file_segment_tab"))
	require.Zero(t, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE file_name = 'strict-references.bin'",
	))
}

func TestCompositeCannotBeRelinkedWithMissingDeletionReference(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "composite.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	part := createAndRegisterMultipartPart(t, manager, upload, 1, []byte("content"))
	result, err := manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []CompleteMultipartPart{{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		}},
	})
	require.NoError(t, err)
	_, err = databaseClient.ExecContext(
		t.Context(),
		"DELETE FROM tg_file_part_delete_state_tab WHERE file_id = ?",
		part.FileID,
	)
	require.NoError(t, err)

	err = manager.CreateFileLink(
		t.Context(),
		"/webdav/composite-copy.bin",
		result.FileID,
		result.Size,
		false,
	)
	require.ErrorIs(t, err, ErrS3ObjectConflict)
	require.Zero(t, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE file_name = 'composite-copy.bin'",
	))
}

func TestListMultipartUploadsPrefixIsCaseSensitive(t *testing.T) {
	managerInterface, _, _ := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	for _, key := range []string{"Alpha.bin", "alpha.bin"} {
		_, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
			Bucket:      "bucket",
			Key:         key,
			Metadata:    testObjectMetadata(""),
			ExpireAfter: time.Hour,
		})
		require.NoError(t, err)
	}
	page, err := manager.ListMultipartUploads(t.Context(), &ListMultipartUploadsRequest{
		Bucket:     "bucket",
		Prefix:     "a",
		MaxUploads: 1000,
	})
	require.NoError(t, err)
	require.Len(t, page.Uploads, 1)
	require.Equal(t, "alpha.bin", page.Uploads[0].Key)
}

func TestListMultipartUploadsBoundsDelimiterAndMarkerPagination(t *testing.T) {
	managerInterface, _, _ := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	for _, key := range []string{"nested/first.bin", "nested/second.bin"} {
		_, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
			Bucket:      "bucket",
			Key:         key,
			Metadata:    testObjectMetadata(""),
			ExpireAfter: time.Hour,
		})
		require.NoError(t, err)
	}
	plainUploadIDs := make([]string, 0, 2)
	for range 2 {
		upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
			Bucket:      "bucket",
			Key:         "plain.bin",
			Metadata:    testObjectMetadata(""),
			ExpireAfter: time.Hour,
		})
		require.NoError(t, err)
		plainUploadIDs = append(plainUploadIDs, upload.UploadID)
	}
	slices.Sort(plainUploadIDs)

	first, err := manager.ListMultipartUploads(t.Context(), &ListMultipartUploadsRequest{
		Bucket:     "bucket",
		Delimiter:  "/",
		MaxUploads: 1,
	})
	require.NoError(t, err)
	require.Empty(t, first.Uploads)
	require.Equal(t, []string{"nested/"}, first.CommonPrefixes)
	require.True(t, first.IsTruncated)
	require.Equal(t, "nested/", first.NextKeyMarker)
	require.Empty(t, first.NextUploadIDMarker)

	second, err := manager.ListMultipartUploads(t.Context(), &ListMultipartUploadsRequest{
		Bucket:     "bucket",
		Delimiter:  "/",
		KeyMarker:  first.NextKeyMarker,
		MaxUploads: 1,
	})
	require.NoError(t, err)
	require.Len(t, second.Uploads, 1)
	require.Equal(t, "plain.bin", second.Uploads[0].Key)
	require.Equal(t, plainUploadIDs[0], second.Uploads[0].UploadID)
	require.True(t, second.IsTruncated)
	require.Equal(t, plainUploadIDs[0], second.NextUploadIDMarker)

	third, err := manager.ListMultipartUploads(t.Context(), &ListMultipartUploadsRequest{
		Bucket:         "bucket",
		Delimiter:      "/",
		KeyMarker:      second.NextKeyMarker,
		UploadIDMarker: second.NextUploadIDMarker,
		MaxUploads:     1,
	})
	require.NoError(t, err)
	require.Len(t, third.Uploads, 1)
	require.Equal(t, plainUploadIDs[1], third.Uploads[0].UploadID)
	require.False(t, third.IsTruncated)
	require.Empty(t, third.NextKeyMarker)
	require.Empty(t, third.NextUploadIDMarker)
}

func createAndRegisterMultipartPart(
	t *testing.T,
	manager *defaultFileManager,
	upload *MultipartUpload,
	partNumber int,
	content []byte,
) *MultipartPart {
	t.Helper()
	fileID, err := manager.CreateFile(t.Context(), int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)
	digest := NewMD5CompatibilityHash()
	_, err = digest.Write(content)
	require.NoError(t, err)
	part, err := manager.PutMultipartPart(t.Context(), &PutMultipartPartRequest{
		UploadID:   upload.UploadID,
		Bucket:     upload.Bucket,
		Key:        upload.Key,
		PartNumber: partNumber,
		FileID:     fileID,
		Size:       int64(len(content)),
		ETag:       hex.EncodeToString(digest.Sum(nil)),
	})
	require.NoError(t, err)
	return part
}
