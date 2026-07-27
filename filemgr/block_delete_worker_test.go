package filemgr

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/xxxsen/tgfile/blockio"
	"github.com/xxxsen/tgfile/db"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
)

type deleteTestBlockIO struct {
	delete func([]string) error
}

func (b *deleteTestBlockIO) Name() string {
	return "delete-test"
}

func (b *deleteTestBlockIO) MaxFileSize() int64 {
	return 1024
}

func (b *deleteTestBlockIO) Upload(_ context.Context, _ io.Reader) (*blockio.UploadResult, error) {
	return nil, errors.New("upload is not used")
}

func (b *deleteTestBlockIO) Download(
	_ context.Context,
	_ string,
	_ int64,
) (io.ReadCloser, error) {
	return nil, errors.New("download is not used")
}

func (b *deleteTestBlockIO) DeleteBlocks(_ context.Context, refs []string) error {
	return b.delete(refs)
}

type testDeleteFailure struct {
	status     int
	retryAfter time.Duration
}

func (e *testDeleteFailure) Error() string {
	return "delete failed"
}

func (e *testDeleteFailure) DeleteStatusCode() int {
	return e.status
}

func (e *testDeleteFailure) DeleteRetryAfter() time.Duration {
	return e.retryAfter
}

func newDeleteWorkerTestManager(
	t *testing.T,
	block *deleteTestBlockIO,
) (*defaultFileManager, database.IDatabase) {
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
	registerCacheCleanup(t, cache)
	manager, ok := NewFileManager(databaseClient, block, cache).(*defaultFileManager)
	require.True(t, ok)
	return manager, databaseClient
}

func insertDeleteWork(
	t *testing.T,
	databaseClient database.IDatabase,
	fileID uint64,
	partID int,
	ref, state string,
	uploadedAt, leaseUntil int64,
) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := databaseClient.ExecContext(t.Context(), `
INSERT INTO tg_file_part_delete_state_tab (
file_id, file_part_id, backend_kind, delete_ref, uploaded_at, delete_state,
attempt_count, next_attempt_at, lease_until, last_attempt_at, last_error_code,
deleted_at, ctime, mtime
) VALUES (?, ?, 'delete-test', ?, ?, ?, 0, 0, ?, 0, '', 0, ?, ?)`,
		fileID,
		partID,
		ref,
		uploadedAt,
		state,
		leaseUntil,
		now,
		now,
	)
	require.NoError(t, err)
}

func readDeleteState(
	t *testing.T,
	databaseClient database.IDatabase,
	fileID uint64,
	partID int,
) (string, string, int64) {
	t.Helper()
	rows, err := databaseClient.QueryContext(t.Context(), `
SELECT delete_state, last_error_code, next_attempt_at
FROM tg_file_part_delete_state_tab WHERE file_id = ? AND file_part_id = ?`,
		fileID,
		partID,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()
	require.True(t, rows.Next())
	var state, code string
	var nextAttempt int64
	require.NoError(t, rows.Scan(&state, &code, &nextAttempt))
	require.NoError(t, rows.Err())
	return state, code, nextAttempt
}

func TestDeleteWorkerHonorsRetryAfter(t *testing.T) {
	block := &deleteTestBlockIO{
		delete: func([]string) error {
			return &testDeleteFailure{status: 429, retryAfter: 10 * time.Second}
		},
	}
	manager, databaseClient := newDeleteWorkerTestManager(t, block)
	now := time.Now()
	insertDeleteWork(t, databaseClient, 1, 0, "ref", "pending", now.UnixMilli(), 0)

	require.NoError(t, manager.processBlockDeleteBatch(t.Context()))

	state, code, nextAttempt := readDeleteState(t, databaseClient, 1, 0)
	require.Equal(t, "pending", state)
	require.Equal(t, "rate_limited", code)
	require.GreaterOrEqual(t, nextAttempt, now.Add(9*time.Second).UnixMilli())
}

func TestDeleteWorkerSplitsPermanentBatchFailure(t *testing.T) {
	block := &deleteTestBlockIO{}
	block.delete = func(refs []string) error {
		if len(refs) > 1 || refs[0] == "bad" {
			return &testDeleteFailure{status: 400}
		}
		return nil
	}
	manager, databaseClient := newDeleteWorkerTestManager(t, block)
	now := time.Now().UnixMilli()
	insertDeleteWork(t, databaseClient, 2, 0, "good", "pending", now, 0)
	insertDeleteWork(t, databaseClient, 2, 1, "bad", "pending", now, 0)

	require.NoError(t, manager.processBlockDeleteBatch(t.Context()))

	state, _, _ := readDeleteState(t, databaseClient, 2, 0)
	require.Equal(t, "deleted", state)
	state, code, _ := readDeleteState(t, databaseClient, 2, 1)
	require.Equal(t, "failed", state)
	require.Equal(t, "client", code)
}

func TestDeleteWorkerRestoresLeaseAndExpiresOldWork(t *testing.T) {
	deleteCalls := 0
	block := &deleteTestBlockIO{
		delete: func([]string) error {
			deleteCalls++
			return nil
		},
	}
	manager, databaseClient := newDeleteWorkerTestManager(t, block)
	now := time.Now()
	insertDeleteWork(
		t,
		databaseClient,
		3,
		0,
		"expired",
		"deleting",
		now.Add(-48*time.Hour).UnixMilli(),
		now.Add(-time.Minute).UnixMilli(),
	)

	require.NoError(t, manager.processBlockDeleteBatch(t.Context()))

	state, code, _ := readDeleteState(t, databaseClient, 3, 0)
	require.Equal(t, "expired", state)
	require.Equal(t, "deadline", code)
	require.Zero(t, deleteCalls)
}

func TestDeleteWorkerAppliesRetryDeadlinePerPart(t *testing.T) {
	block := &deleteTestBlockIO{
		delete: func([]string) error {
			return &testDeleteFailure{status: 429, retryAfter: 10 * time.Second}
		},
	}
	manager, databaseClient := newDeleteWorkerTestManager(t, block)
	now := time.Now()
	insertDeleteWork(
		t,
		databaseClient,
		4,
		0,
		"near-deadline",
		"deleting",
		now.Add(-deleteDeadline+5*time.Second).UnixMilli(),
		now.Add(deleteLease).UnixMilli(),
	)
	insertDeleteWork(
		t,
		databaseClient,
		4,
		1,
		"new",
		"deleting",
		now.UnixMilli(),
		now.Add(deleteLease).UnixMilli(),
	)

	err := manager.executeBlockDeleteWork(t.Context(), []blockDeleteWork{
		{
			fileID:       4,
			partID:       0,
			deleteRef:    "near-deadline",
			uploadedAt:   now.Add(-deleteDeadline + 5*time.Second).UnixMilli(),
			attemptCount: 1,
		},
		{
			fileID:       4,
			partID:       1,
			deleteRef:    "new",
			uploadedAt:   now.UnixMilli(),
			attemptCount: 1,
		},
	}, now)
	require.NoError(t, err)

	state, code, nextAttempt := readDeleteState(t, databaseClient, 4, 0)
	require.Equal(t, "expired", state)
	require.Equal(t, "deadline", code)
	require.Zero(t, nextAttempt)
	state, code, nextAttempt = readDeleteState(t, databaseClient, 4, 1)
	require.Equal(t, "pending", state)
	require.Equal(t, "rate_limited", code)
	require.Equal(t, now.Add(10*time.Second).UnixMilli(), nextAttempt)
}
