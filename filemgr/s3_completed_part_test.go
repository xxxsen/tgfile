package filemgr

import (
	"bytes"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLayoutV1IsOneS3PartAcrossPhysicalBlocks(t *testing.T) {
	managerInterface, block, databaseClient := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	content := []byte("nine-byte")
	fileID, err := manager.CreateFile(t.Context(), int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)

	part, err := manager.StatS3ObjectPart(t.Context(), fileID, int64(len(content)), 1)
	require.NoError(t, err)
	require.False(t, part.IsMultipart)
	require.Equal(t, int64(len(content)), part.PartSize)
	require.Equal(t, fileID, part.SourceFileID)
	require.Zero(t, block.downloadCount)

	reader, err := manager.OpenS3ObjectPart(t.Context(), part)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.Equal(t, content, actual)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, 3, block.downloadCount)

	_, err = manager.StatS3ObjectPart(t.Context(), fileID, int64(len(content)), 2)
	var numberError *S3PartNumberError
	require.ErrorAs(t, err, &numberError)
	require.Equal(t, 2, numberError.Requested)
	require.Equal(t, 1, numberError.Actual)

	page, err := manager.ListS3ObjectParts(t.Context(), fileID, int64(len(content)), 0, 1000)
	require.NoError(t, err)
	require.False(t, page.IsMultipart)
	require.Empty(t, page.Parts)
	require.Zero(t, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab",
	))
}

func TestCompletedMultipartPartReadPaginationAndManifestValidation(t *testing.T) {
	managerInterface, block, databaseClient := newCreateFileTestManager(t, 4*1024*1024)
	manager := managerInterface.(*defaultFileManager)
	firstContent := bytes.Repeat([]byte("a"), 5*1024*1024)
	secondContent := []byte("tail-part")
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "parts.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	first := createAndRegisterMultipartPart(t, manager, upload, 1, firstContent)
	second := createAndRegisterMultipartPart(t, manager, upload, 9, secondContent)
	result, err := manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []CompleteMultipartPart{{
			PartNumber: first.PartNumber,
			ETag:       first.ETag,
		}, {
			PartNumber: second.PartNumber,
			ETag:       second.ETag,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab",
	))

	downloadsBefore := block.downloadCount
	part, err := manager.StatS3ObjectPart(t.Context(), result.FileID, result.Size, 2)
	require.NoError(t, err)
	require.True(t, part.IsMultipart)
	require.Equal(t, 2, part.PartNumber)
	require.Equal(t, int64(len(firstContent)), part.StartOffset)
	require.Equal(t, int64(len(secondContent)), part.PartSize)
	require.Equal(t, second.FileID, part.SourceFileID)
	require.Equal(t, second.ChecksumValue, part.ChecksumValue)
	require.Equal(t, downloadsBefore, block.downloadCount)

	firstPage, err := manager.ListS3ObjectParts(t.Context(), result.FileID, result.Size, 0, 1)
	require.NoError(t, err)
	require.Len(t, firstPage.Parts, 1)
	require.Equal(t, 1, firstPage.Parts[0].PartNumber)
	require.Equal(t, 2, firstPage.PartsCount)
	require.True(t, firstPage.IsTruncated)
	require.Equal(t, 1, firstPage.NextPartNumberMarker)
	secondPage, err := manager.ListS3ObjectParts(
		t.Context(),
		result.FileID,
		result.Size,
		firstPage.NextPartNumberMarker,
		1,
	)
	require.NoError(t, err)
	require.Len(t, secondPage.Parts, 1)
	require.Equal(t, 2, secondPage.Parts[0].PartNumber)
	require.False(t, secondPage.IsTruncated)
	require.Equal(t, downloadsBefore, block.downloadCount)

	reader, err := manager.OpenS3ObjectPart(t.Context(), part)
	require.NoError(t, err)
	actual, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, secondContent, actual)
	require.Equal(t, downloadsBefore+1, block.downloadCount)

	_, err = manager.StatS3ObjectPart(t.Context(), result.FileID, result.Size, 3)
	var numberError *S3PartNumberError
	require.ErrorAs(t, err, &numberError)
	require.Equal(t, 2, numberError.Actual)

	_, err = databaseClient.ExecContext(
		t.Context(),
		`UPDATE tg_s3_completed_part_tab SET part_size = part_size + 1
WHERE file_id = ? AND part_number = 2`,
		result.FileID,
	)
	require.NoError(t, err)
	_, err = manager.StatS3ObjectPart(t.Context(), result.FileID, result.Size, 2)
	require.ErrorIs(t, err, ErrInvalidS3Part)

	_, err = databaseClient.ExecContext(
		t.Context(),
		`UPDATE tg_s3_completed_part_tab SET part_size = part_size - 1
WHERE file_id = ? AND part_number = 2`,
		result.FileID,
	)
	require.NoError(t, err)
	_, err = databaseClient.ExecContext(
		t.Context(),
		`UPDATE tg_s3_file_segment_tab SET source_file_id = 999999999
WHERE file_id = ? AND segment_index = 1`,
		result.FileID,
	)
	require.NoError(t, err)
	_, err = manager.StatS3ObjectPart(t.Context(), result.FileID, result.Size, 2)
	require.ErrorIs(t, err, ErrInvalidS3Part)
}

func TestBoundedPartReaderRejectsShortContentAndOutOfRangeSeek(t *testing.T) {
	reader := &boundedPartReader{source: newBytesStream([]byte("x")), size: 2, open: true}
	raw, err := io.ReadAll(reader)
	require.Equal(t, []byte("x"), raw)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	_, err = reader.Seek(3, io.SeekStart)
	require.ErrorIs(t, err, ErrSeekPastEnd)
	require.NoError(t, reader.Close())
	_, err = reader.Read(make([]byte, 1))
	require.ErrorIs(t, err, ErrFileNotOpen)
}

func TestCompletedPartManifestSurvivesMappingDelete(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 1024)
	manager := managerInterface.(*defaultFileManager)
	upload, err := manager.CreateMultipartUpload(t.Context(), &CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "retained.bin",
		Metadata:    testObjectMetadata(""),
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	part := createAndRegisterMultipartPart(t, manager, upload, 1, []byte("content"))
	result, err := manager.CompleteMultipartUpload(t.Context(), &CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts:    []CompleteMultipartPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)

	copied, err := manager.CopyS3Object(
		t.Context(),
		"/bucket/retained.bin",
		"/bucket/retained-copy.bin",
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, result.FileID, copied.Link.FileId)
	require.Equal(t, 2, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = "+
			strconv.FormatUint(result.FileID, 10),
	))

	deleted, err := manager.DeleteS3Object(t.Context(), "/bucket/retained.bin", nil)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab WHERE file_id = "+
			strconv.FormatUint(result.FileID, 10),
	))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE file_id = "+
			strconv.FormatUint(part.FileID, 10)+" AND delete_state = 'live'",
	))

	deleted, err = manager.DeleteS3Object(t.Context(), "/bucket/retained-copy.bin", nil)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab WHERE file_id = "+
			strconv.FormatUint(result.FileID, 10),
	))
	require.Equal(t, 1, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE file_id = "+
			strconv.FormatUint(part.FileID, 10)+" AND delete_state = 'pending'",
	))
}
