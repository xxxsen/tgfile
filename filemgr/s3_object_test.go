package filemgr

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/xxxsen/tgfile/entity"

	"github.com/stretchr/testify/require"
)

func testObjectMetadata(etag string) *entity.S3ObjectMetadata {
	return &entity.S3ObjectMetadata{
		ETag:         etag,
		ContentType:  "application/octet-stream",
		CacheControl: defaultS3CacheControl,
		UserMetadata: "{}",
	}
}

func TestPublishS3ObjectRollsBackMappingWhenMetadataInsertFails(t *testing.T) {
	managerInterface, _, databaseClient := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	fileID, err := manager.CreateFile(t.Context(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	_, err = databaseClient.ExecContext(t.Context(), `
CREATE TRIGGER reject_s3_metadata
BEFORE INSERT ON tg_s3_object_metadata_tab
BEGIN
    SELECT RAISE(ABORT, 'reject metadata');
END;`)
	require.NoError(t, err)

	_, err = manager.PublishS3Object(
		t.Context(),
		"/bucket/object",
		fileID,
		0,
		testObjectMetadata(`"etag"`),
		nil,
	)

	require.Error(t, err)
	_, err = manager.StatFileLink(t.Context(), "/bucket/object")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, queryCount(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_s3_object_metadata_tab",
	))
}

func TestS3OverwriteWithEmptyFileDeletesUnreferencedOldBlocks(t *testing.T) {
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
		"/bucket/object",
		oldFileID,
		int64(len(oldContent)),
		testObjectMetadata(`"old"`),
		nil,
	)
	require.NoError(t, err)

	emptyFileID, err := manager.CreateFile(t.Context(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	_, err = manager.PublishS3Object(
		t.Context(),
		"/bucket/object",
		emptyFileID,
		0,
		testObjectMetadata(`"`+entity.EmptyFileMD5Sum+`"`),
		nil,
	)
	require.NoError(t, err)

	info, err := manager.StatS3Object(t.Context(), "/bucket/object")
	require.NoError(t, err)
	require.Equal(t, emptyFileID, info.Link.FileId)
	require.Zero(t, info.Link.FileSize)
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

func TestListS3ObjectsPaginatesProjectedCommonPrefixes(t *testing.T) {
	managerInterface, _, _ := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	for _, key := range []string{"a/one", "a/two", "b"} {
		fileID, err := manager.CreateFile(t.Context(), 0, bytes.NewReader(nil))
		require.NoError(t, err)
		_, err = manager.PublishS3Object(
			t.Context(),
			"/bucket/"+key,
			fileID,
			0,
			testObjectMetadata(`"`+key+`"`),
			nil,
		)
		require.NoError(t, err)
	}

	first, err := manager.ListS3Objects(t.Context(), &S3ListRequest{
		Bucket:    "bucket",
		Delimiter: "/",
		MaxKeys:   1,
	})
	require.NoError(t, err)
	require.Empty(t, first.Items)
	require.Equal(t, []string{"a/"}, first.CommonPrefixes)
	require.True(t, first.IsTruncated)
	require.Equal(t, "a/", first.NextKey)

	second, err := manager.ListS3Objects(t.Context(), &S3ListRequest{
		Bucket:            "bucket",
		Delimiter:         "/",
		ContinuationToken: first.NextKey,
		MaxKeys:           1,
	})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	require.Equal(t, "b", second.Items[0].Key)
	require.False(t, second.IsTruncated)
}

func TestDeleteS3ObjectConditionIsAtomic(t *testing.T) {
	managerInterface, _, _ := newCreateFileTestManager(t, 4)
	manager := managerInterface.(*defaultFileManager)
	fileID, err := manager.CreateFile(t.Context(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	_, err = manager.PublishS3Object(
		t.Context(),
		"/bucket/object",
		fileID,
		0,
		testObjectMetadata(`"current"`),
		nil,
	)
	require.NoError(t, err)

	_, err = manager.DeleteS3Object(t.Context(), "/bucket/object", &S3Condition{
		IfMatch: `"different"`,
	})

	require.ErrorIs(t, err, ErrS3Precondition)
	info, err := manager.StatS3Object(t.Context(), "/bucket/object")
	require.NoError(t, err)
	require.Equal(t, `"current"`, info.Metadata.ETag)

	_, err = manager.StatS3Object(t.Context(), "/bucket/missing")
	require.True(t, errors.Is(err, os.ErrNotExist))
}

func TestEvaluateS3ConditionSupportsETagLists(t *testing.T) {
	info := &S3ObjectInfo{
		Link:     &entity.FileLinkMeta{},
		Metadata: testObjectMetadata(`"current"`),
	}
	require.NoError(t, evaluateS3Condition(info, &S3Condition{
		IfMatch: `"other", "current"`,
	}))
	require.ErrorIs(t, evaluateS3Condition(info, &S3Condition{
		IfNoneMatch: `W/"other", W/"current"`,
	}), ErrS3Precondition)

	info.Metadata.ETag = `W/"current"`
	require.ErrorIs(t, evaluateS3Condition(info, &S3Condition{
		IfMatch: `"current"`,
	}), ErrS3Precondition)
	require.ErrorIs(t, evaluateS3Condition(info, &S3Condition{
		IfNoneMatch: `"current"`,
	}), ErrS3Precondition)
}

func TestEvaluateS3ConditionTruncatesModificationTimeToSeconds(t *testing.T) {
	base := time.Date(2026, time.July, 25, 12, 0, 1, 0, time.UTC)
	info := &S3ObjectInfo{
		Link:     &entity.FileLinkMeta{Mtime: base.UnixMilli()},
		Metadata: testObjectMetadata(`"current"`),
	}

	require.ErrorIs(t, evaluateS3Condition(info, &S3Condition{
		IfUnmodifiedSince: timePointer(base.Add(-time.Second)),
	}), ErrS3Precondition)
	require.NoError(t, evaluateS3Condition(info, &S3Condition{
		IfModifiedSince: timePointer(base.Add(-time.Second)),
	}))

	info.Link.Mtime = base.Add(999 * time.Millisecond).UnixMilli()
	info.Metadata.Mtime = base.Add(-time.Hour).UnixMilli()
	require.NoError(t, evaluateS3Condition(info, &S3Condition{
		IfUnmodifiedSince: timePointer(base),
	}))
	require.ErrorIs(t, evaluateS3Condition(info, &S3Condition{
		IfModifiedSince: timePointer(base),
	}), ErrS3Precondition)
}

func timePointer(value time.Time) *time.Time {
	return &value
}
