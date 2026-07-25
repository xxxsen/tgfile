package filemgr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/xxxsen/tgfile/blockio"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

const (
	deleteScanInterval = 5 * time.Second
	deleteLease        = 60 * time.Second
	deleteTimeout      = 15 * time.Second
	deleteDeadline     = 47 * time.Hour
	deleteBatchSize    = 100
)

type blockDeleteWork struct {
	fileID       uint64
	partID       int32
	deleteRef    string
	uploadedAt   int64
	attemptCount int
}

func (d *defaultFileManager) RunBlockDeleteWorker(ctx context.Context) error {
	if err := d.processBlockDeleteBatch(ctx); err != nil {
		logutil.GetLogger(ctx).Error("block delete worker scan failed", zap.String("error_code", "database"))
	}
	ticker := time.NewTicker(deleteScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := d.processBlockDeleteBatch(ctx); err != nil {
				logutil.GetLogger(ctx).Error(
					"block delete worker scan failed",
					zap.String("error_code", "database"),
				)
			}
		}
	}
}

func (d *defaultFileManager) processBlockDeleteBatch(ctx context.Context) error {
	now := time.Now()
	claimed, err := d.claimBlockDeleteWork(ctx, now)
	if err != nil {
		return err
	}
	if len(claimed) == 0 {
		return nil
	}
	return d.executeBlockDeleteWork(ctx, claimed, now)
}

func (d *defaultFileManager) claimBlockDeleteWork(
	ctx context.Context,
	now time.Time,
) ([]blockDeleteWork, error) {
	claimed := make([]blockDeleteWork, 0, deleteBatchSize)
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		nowMillis := now.UnixMilli()
		if err := restoreExpiredDeleteLeases(ctx, tx, d.bkio.Name(), nowMillis); err != nil {
			return err
		}
		candidates, err := queryPendingDeleteCandidates(ctx, tx, d.bkio.Name(), nowMillis)
		if err != nil {
			return err
		}
		for _, work := range candidates {
			wasClaimed, err := claimDeleteCandidate(ctx, tx, work, now)
			if err != nil {
				return err
			}
			if wasClaimed {
				work.attemptCount++
				claimed = append(claimed, work)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim block delete work transaction: %w", err)
	}
	return claimed, nil
}

func restoreExpiredDeleteLeases(
	ctx context.Context,
	exec database.IExecer,
	backendKind string,
	nowMillis int64,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'pending', next_attempt_at = ?, lease_until = 0, mtime = ?
WHERE backend_kind = ? AND delete_state = 'deleting' AND lease_until <= ?`,
		nowMillis,
		nowMillis,
		backendKind,
		nowMillis,
	); err != nil {
		return fmt.Errorf("restore expired block delete leases: %w", err)
	}
	return nil
}

func queryPendingDeleteCandidates(
	ctx context.Context,
	queryer database.IQueryer,
	backendKind string,
	nowMillis int64,
) ([]blockDeleteWork, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT file_id, file_part_id, delete_ref, uploaded_at, attempt_count
FROM tg_file_part_delete_state_tab
WHERE backend_kind = ? AND delete_state = 'pending' AND next_attempt_at <= ?
ORDER BY uploaded_at, file_id, file_part_id LIMIT ?`,
		backendKind,
		nowMillis,
		deleteBatchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending block deletes: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	candidates := make([]blockDeleteWork, 0, deleteBatchSize)
	for rows.Next() {
		var work blockDeleteWork
		if err := rows.Scan(
			&work.fileID,
			&work.partID,
			&work.deleteRef,
			&work.uploadedAt,
			&work.attemptCount,
		); err != nil {
			return nil, fmt.Errorf("scan pending block delete: %w", err)
		}
		candidates = append(candidates, work)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending block deletes: %w", err)
	}
	return candidates, nil
}

func claimDeleteCandidate(
	ctx context.Context,
	tx database.IQueryExecer,
	work blockDeleteWork,
	now time.Time,
) (bool, error) {
	nowMillis := now.UnixMilli()
	referenced, err := fileHasMapping(ctx, tx, work.fileID)
	if err != nil {
		return false, err
	}
	if referenced {
		if err := restoreFileDeleteState(ctx, tx, work.fileID, nowMillis); err != nil {
			return false, err
		}
		return false, nil
	}
	if nowMillis >= work.uploadedAt+deleteDeadline.Milliseconds() {
		if err := expireBlockDelete(ctx, tx, work, nowMillis); err != nil {
			return false, err
		}
		return false, nil
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'deleting', attempt_count = attempt_count + 1,
    last_attempt_at = ?, lease_until = ?, mtime = ?
WHERE file_id = ? AND file_part_id = ? AND delete_state = 'pending'`,
		nowMillis,
		now.Add(deleteLease).UnixMilli(),
		nowMillis,
		work.fileID,
		work.partID,
	)
	if err != nil {
		return false, fmt.Errorf("claim block delete: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read block delete claim count: %w", err)
	}
	return count == 1, nil
}

func fileHasMapping(ctx context.Context, queryer database.IQueryer, fileID uint64) (bool, error) {
	var count int64
	if err := queryRow(
		ctx,
		queryer,
		"SELECT COUNT(*) FROM tg_file_mapping_tab WHERE ref_data = ?",
		strconv.FormatUint(fileID, 10),
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count block delete file mappings: %w", err)
	}
	return count != 0, nil
}

func restoreFileDeleteState(
	ctx context.Context,
	exec database.IExecer,
	fileID uint64,
	now int64,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'live', next_attempt_at = 0, lease_until = 0, mtime = ?
WHERE file_id = ? AND delete_state IN ('pending', 'deleting')`,
		now,
		fileID,
	); err != nil {
		return fmt.Errorf("restore referenced block delete state: %w", err)
	}
	return nil
}

func expireBlockDelete(
	ctx context.Context,
	exec database.IExecer,
	work blockDeleteWork,
	now int64,
) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_file_part_delete_state_tab
SET delete_state = 'expired', last_error_code = 'deadline', lease_until = 0, mtime = ?
WHERE file_id = ? AND file_part_id = ? AND delete_state IN ('pending', 'deleting')`,
		now,
		work.fileID,
		work.partID,
	); err != nil {
		return fmt.Errorf("expire block delete: %w", err)
	}
	return nil
}

func (d *defaultFileManager) executeBlockDeleteWork(
	ctx context.Context,
	works []blockDeleteWork,
	now time.Time,
) error {
	deleteRefs := make([]string, 0, len(works))
	for _, work := range works {
		deleteRefs = append(deleteRefs, work.deleteRef)
	}
	deleteContext, cancel := context.WithTimeout(ctx, deleteTimeout)
	err := d.bkio.DeleteBlocks(deleteContext, deleteRefs)
	cancel()
	if err == nil {
		return d.finishBlockDeleteWork(ctx, works, "deleted", "", 0, now)
	}
	code, retry, delay := classifyBlockDeleteError(err, works[0].attemptCount)
	if !retry && len(works) > 1 {
		for _, work := range works {
			if err := d.executeBlockDeleteWork(ctx, []blockDeleteWork{work}, now); err != nil {
				return err
			}
		}
		return nil
	}
	state := "failed"
	nextAttemptAt := int64(0)
	if retry {
		next := now.Add(delay)
		for _, work := range works {
			workState := "pending"
			workCode := code
			workNextAttemptAt := next.UnixMilli()
			deadline := time.UnixMilli(work.uploadedAt).Add(deleteDeadline)
			if !next.Before(deadline) {
				workState = "expired"
				workCode = "deadline"
				workNextAttemptAt = 0
			}
			if err := d.finishBlockDeleteWork(
				ctx,
				[]blockDeleteWork{work},
				workState,
				workCode,
				workNextAttemptAt,
				now,
			); err != nil {
				return err
			}
		}
		return nil
	}
	return d.finishBlockDeleteWork(ctx, works, state, code, nextAttemptAt, now)
}

func classifyBlockDeleteError(err error, attempt int) (string, bool, time.Duration) {
	var failure blockio.DeleteFailure
	if errors.As(err, &failure) {
		return classifyDeleteFailure(failure, attempt)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return "network", true, deleteBackoff(attempt)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout", true, deleteBackoff(attempt)
	}
	return "invalid_reference", false, 0
}

func classifyDeleteFailure(failure blockio.DeleteFailure, attempt int) (string, bool, time.Duration) {
	status := failure.DeleteStatusCode()
	if status == http.StatusTooManyRequests {
		delay := failure.DeleteRetryAfter()
		if delay <= 0 {
			delay = time.Second
		}
		return "rate_limited", true, delay
	}
	if status >= http.StatusInternalServerError {
		return "server", true, deleteBackoff(attempt)
	}
	return "client", false, 0
}

func deleteBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	seconds := 1 << min(attempt-1, 4)
	return min(time.Duration(seconds)*time.Second, 30*time.Second)
}

func (d *defaultFileManager) finishBlockDeleteWork(
	ctx context.Context,
	works []blockDeleteWork,
	state, errorCode string,
	nextAttemptAt int64,
	now time.Time,
) error {
	if err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		for _, work := range works {
			deletedAt := int64(0)
			if state == "deleted" {
				deletedAt = now.UnixMilli()
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE tg_file_part_delete_state_tab
SET delete_state = ?, next_attempt_at = ?, lease_until = 0,
    last_error_code = ?, deleted_at = ?, mtime = ?
WHERE file_id = ? AND file_part_id = ? AND delete_state = 'deleting'`,
				state,
				nextAttemptAt,
				errorCode,
				deletedAt,
				now.UnixMilli(),
				work.fileID,
				work.partID,
			); err != nil {
				return fmt.Errorf("finish block delete work: %w", err)
			}
			logutil.GetLogger(ctx).Info(
				"telegram_message_delete",
				zap.Uint64("file_id", work.fileID),
				zap.Int32("part_id", work.partID),
				zap.String("state", state),
				zap.Int("attempt", work.attemptCount),
				zap.String("error_code", errorCode),
			)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("finish block delete work transaction: %w", err)
	}
	return nil
}
