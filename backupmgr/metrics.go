package backupmgr

import (
	"context"
	"fmt"

	"github.com/xxxsen/common/database"
)

type JobResultMetric struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Value  int64  `json:"value"`
}

type ActiveJobMetric struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
	Value int64  `json:"value"`
}

type JobKindMetric struct {
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
}

type FailureMetric struct {
	Kind  string `json:"kind"`
	Code  string `json:"code"`
	Value int64  `json:"value"`
}

type MetricsSnapshot struct {
	JobsTotal       []JobResultMetric `json:"tgfile_backup_jobs_total"`
	ActiveJobs      []ActiveJobMetric `json:"tgfile_backup_active_jobs"`
	BytesTotal      []JobKindMetric   `json:"tgfile_backup_bytes_total"`
	DurationSeconds []JobKindMetric   `json:"tgfile_backup_duration_seconds"`
	FailuresTotal   []FailureMetric   `json:"tgfile_backup_failures_total"`
	ArtifactBytes   int64             `json:"tgfile_backup_artifact_bytes"`
	StagedFiles     int64             `json:"tgfile_backup_staged_files"`
}

func (m *Manager) Metrics(ctx context.Context) (*MetricsSnapshot, error) {
	snapshot := new(MetricsSnapshot)
	if err := readJobResultMetrics(ctx, m.db, snapshot); err != nil {
		return nil, err
	}
	if err := readActiveJobMetrics(ctx, m.db, snapshot); err != nil {
		return nil, err
	}
	if err := readJobKindMetrics(ctx, m.db, snapshot); err != nil {
		return nil, err
	}
	if err := readFailureMetrics(ctx, m.db, snapshot); err != nil {
		return nil, err
	}
	if err := readBackupGauges(ctx, m.db, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func readJobResultMetrics(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *MetricsSnapshot,
) error {
	return readGroupedMetrics(
		ctx,
		queryer,
		`SELECT job_kind, job_state, COUNT(*) FROM tg_backup_job_tab
WHERE completed_at > 0 GROUP BY job_kind, job_state ORDER BY job_kind, job_state`,
		func(kind, result string, value int64) {
			snapshot.JobsTotal = append(snapshot.JobsTotal, JobResultMetric{
				Kind: kind, Result: result, Value: value,
			})
		},
	)
}

func readActiveJobMetrics(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *MetricsSnapshot,
) error {
	return readGroupedMetrics(
		ctx,
		queryer,
		`SELECT job_kind, job_state, COUNT(*) FROM tg_backup_job_tab
WHERE completed_at = 0 GROUP BY job_kind, job_state ORDER BY job_kind, job_state`,
		func(kind, state string, value int64) {
			snapshot.ActiveJobs = append(snapshot.ActiveJobs, ActiveJobMetric{
				Kind: kind, State: state, Value: value,
			})
		},
	)
}

func readJobKindMetrics(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *MetricsSnapshot,
) error {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT job_kind, COALESCE(SUM(bytes_completed), 0),
COALESCE(SUM(CASE WHEN completed_at > 0
  THEN (completed_at - created_at) / 1000.0 ELSE 0 END), 0)
FROM tg_backup_job_tab GROUP BY job_kind ORDER BY job_kind`,
	)
	if err != nil {
		return fmt.Errorf("query backup byte and duration metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var bytes, duration JobKindMetric
		if err := rows.Scan(&bytes.Kind, &bytes.Value, &duration.Value); err != nil {
			return fmt.Errorf("scan backup byte and duration metric: %w", err)
		}
		duration.Kind = bytes.Kind
		snapshot.BytesTotal = append(snapshot.BytesTotal, bytes)
		snapshot.DurationSeconds = append(snapshot.DurationSeconds, duration)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate backup byte and duration metrics: %w", err)
	}
	return nil
}

func readFailureMetrics(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *MetricsSnapshot,
) error {
	return readGroupedMetrics(
		ctx,
		queryer,
		`SELECT job_kind, error_code, COUNT(*) FROM tg_backup_job_tab
WHERE job_state = 'failed' GROUP BY job_kind, error_code ORDER BY job_kind, error_code`,
		func(kind, code string, value int64) {
			snapshot.FailuresTotal = append(snapshot.FailuresTotal, FailureMetric{
				Kind: kind, Code: code, Value: value,
			})
		},
	)
}

func readGroupedMetrics(
	ctx context.Context,
	queryer database.IQueryer,
	query string,
	consume func(string, string, int64),
) error {
	rows, err := queryer.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query grouped backup metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var first, second string
		var value int64
		if err := rows.Scan(&first, &second, &value); err != nil {
			return fmt.Errorf("scan grouped backup metric: %w", err)
		}
		consume(first, second, value)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate grouped backup metrics: %w", err)
	}
	return nil
}

func readBackupGauges(
	ctx context.Context,
	queryer database.IQueryer,
	snapshot *MetricsSnapshot,
) error {
	if err := queryJobRow(
		ctx,
		queryer,
		`SELECT COALESCE(SUM(artifact_size), 0) FROM tg_backup_job_tab
WHERE job_kind = 'export' AND job_state = 'succeeded' AND artifact_path != ''`,
	).Scan(&snapshot.ArtifactBytes); err != nil {
		return fmt.Errorf("read backup artifact byte metric: %w", err)
	}
	if err := queryJobRow(
		ctx,
		queryer,
		`SELECT COUNT(*) FROM tg_backup_job_file_tab stage
JOIN tg_backup_job_tab job ON job.job_id = stage.job_id
WHERE job.completed_at = 0`,
	).Scan(&snapshot.StagedFiles); err != nil {
		return fmt.Errorf("read staged backup file metric: %w", err)
	}
	return nil
}
