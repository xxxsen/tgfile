package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func readBackupAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
	options AuditOptions,
) error {
	if err := readBackupJobStateCounts(ctx, database, report); err != nil {
		return err
	}
	if err := readBackupAuditCounters(ctx, database, report); err != nil {
		return err
	}
	if err := readBackupDeleteRefTargets(ctx, database, report, options); err != nil {
		return err
	}
	if options.BackupWorkDir == "" {
		return nil
	}
	expected, err := readExpectedBackupWorkFiles(ctx, database)
	if err != nil {
		return err
	}
	return countOrphanBackupWorkFiles(options.BackupWorkDir, expected, report)
}

func readBackupJobStateCounts(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	if err := readStateCounts(
		ctx,
		database,
		`SELECT job_state, COUNT(*) FROM tg_backup_job_tab
GROUP BY job_state ORDER BY job_state`,
		report.BackupJobCountByState,
	); err != nil {
		return fmt.Errorf("read backup job state counts: %w", err)
	}
	return nil
}

func readBackupAuditCounters(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	queries := []struct {
		target *int64
		query  string
		args   []any
	}{
		{
			target: &report.BackupTerminalJobPinCount,
			query: `SELECT COUNT(*) FROM tg_backup_export_pin_tab pin
JOIN tg_backup_job_tab job ON job.job_id = pin.job_id
WHERE job.completed_at > 0`,
		},
		{
			target: &report.BackupOrphanPinCount,
			query: `SELECT COUNT(*) FROM tg_backup_export_pin_tab pin
LEFT JOIN tg_backup_job_tab job ON job.job_id = pin.job_id
WHERE job.job_id IS NULL`,
		},
		{
			target: &report.BackupJobFileInvalidTargetCount,
			query: `SELECT COUNT(*) FROM tg_backup_job_file_tab stage
LEFT JOIN tg_file_tab file ON file.file_id = stage.target_file_id
WHERE stage.target_file_id = 0 OR file.file_id IS NULL
   OR (stage.stage_state = 'ready' AND file.file_state != 2)`,
		},
		{
			target: &report.BackupStagedFileMappedCount,
			query: `SELECT COUNT(DISTINCT stage.target_file_id)
FROM tg_backup_job_file_tab stage
JOIN tg_backup_job_tab job ON job.job_id = stage.job_id
JOIN tg_file_mapping_tab mapping
  ON mapping.ref_data = CAST(stage.target_file_id AS TEXT)
WHERE job.job_kind = 'import' AND job.completed_at = 0`,
		},
		{
			target: &report.BackupExpiredArtifactCount,
			query: `SELECT COUNT(*) FROM tg_backup_job_tab
WHERE artifact_path != '' AND artifact_expires_at > 0 AND artifact_expires_at <= ?`,
			args: []any{time.Now().UnixMilli()},
		},
		{
			target: &report.BackupActiveExportMissingPin,
			query: `SELECT COUNT(*) FROM tg_backup_job_tab job
WHERE job.job_kind = 'export' AND job.completed_at = 0
  AND job.job_state = 'building' AND job.files_total > 0
  AND NOT EXISTS (
    SELECT 1 FROM tg_backup_export_pin_tab pin WHERE pin.job_id = job.job_id
  )`,
		},
		{
			target: &report.BackupActiveJobMissingPath,
			query: `SELECT COUNT(*) FROM tg_backup_job_tab
WHERE completed_at = 0 AND (
  (job_kind = 'export' AND job_state = 'building' AND snapshot_path = '')
  OR
  (job_kind = 'import' AND job_state NOT IN ('receiving') AND artifact_path = '')
)`,
		},
		{
			target: &report.BackupPartMissingLiveDeleteState,
			query: `SELECT COUNT(*) FROM tg_backup_job_file_tab stage
JOIN tg_backup_job_tab job ON job.job_id = stage.job_id
JOIN tg_file_part_tab part ON part.file_id = stage.target_file_id
LEFT JOIN tg_file_part_delete_state_tab state
  ON state.file_id = part.file_id AND state.file_part_id = part.file_part_id
WHERE stage.layout_version = 1 AND job.completed_at = 0
  AND (state.file_id IS NULL OR state.delete_state != 'live')`,
		},
	}
	for _, item := range queries {
		if err := database.QueryRowContext(ctx, item.query, item.args...).Scan(item.target); err != nil {
			return fmt.Errorf("read backup audit counter: %w", err)
		}
	}
	return nil
}

func readExpectedBackupWorkFiles(
	ctx context.Context,
	database *sql.DB,
) (map[string]struct{}, error) {
	expected := make(map[string]struct{})
	rows, err := database.QueryContext(
		ctx,
		`SELECT job_id, job_kind, job_state, artifact_path, snapshot_path, report_path
FROM tg_backup_job_tab`,
	)
	if err != nil {
		return nil, fmt.Errorf("query backup work files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var jobID, kind, state, artifact, snapshot, reportFile string
		if err := rows.Scan(
			&jobID,
			&kind,
			&state,
			&artifact,
			&snapshot,
			&reportFile,
		); err != nil {
			return nil, fmt.Errorf("scan backup work files: %w", err)
		}
		for _, filename := range []string{artifact, snapshot, reportFile} {
			if filename != "" && filepath.Base(filename) == filename {
				expected[filename] = struct{}{}
			}
		}
		switch {
		case kind == "import" && state == "receiving":
			expected[jobID+".receive.partial"] = struct{}{}
		case kind == "export" && state == "snapshotting":
			expected[jobID+".snapshot.json"] = struct{}{}
		case kind == "export" && state == "building":
			expected[jobID+".snapshot.json"] = struct{}{}
			expected[jobID+".partial"] = struct{}{}
			expected[jobID+".tgfb"] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup work files: %w", err)
	}
	return expected, nil
}

func countOrphanBackupWorkFiles(
	workDir string,
	expected map[string]struct{},
	report *AuditReport,
) error {
	for filename := range expected {
		if _, err := os.Stat(filepath.Join(workDir, filename)); os.IsNotExist(err) {
			report.BackupMissingWorkFileCount++
		} else if err != nil {
			return fmt.Errorf("stat referenced backup work file: %w", err)
		}
	}
	entries, err := os.ReadDir(workDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read backup work directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, exists := expected[entry.Name()]; exists {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat backup work file: %w", err)
		}
		report.BackupOrphanWorkFileCount++
		report.BackupOrphanWorkFileBytes += info.Size()
	}
	return nil
}

func readBackupDeleteRefTargets(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
	options AuditOptions,
) error {
	if options.BackendKind != "telegram" ||
		options.TelegramBotID <= 0 ||
		options.TelegramChatID == 0 {
		return nil
	}
	rows, err := database.QueryContext(
		ctx,
		`SELECT delete_ref FROM tg_file_part_delete_state_tab
WHERE backend_kind = 'telegram' AND delete_state IN ('live', 'pending', 'deleting')`,
	)
	if err != nil {
		return fmt.Errorf("query Telegram backup deletion references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan Telegram backup deletion reference: %w", err)
		}
		var reference struct {
			Version   int   `json:"v"`
			BotID     int64 `json:"bot_id"`
			ChatID    int64 `json:"chat_id"`
			MessageID int   `json:"message_id"`
		}
		if err := decodeBackupDeleteReference(raw, &reference); err != nil ||
			reference.Version != 1 ||
			reference.BotID != options.TelegramBotID ||
			reference.ChatID != options.TelegramChatID ||
			reference.MessageID <= 0 {
			report.BackupDeleteRefTargetMismatch++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Telegram backup deletion references: %w", err)
	}
	return nil
}

func decodeBackupDeleteReference(raw string, output any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("delete reference contains trailing JSON: %w", err)
	}
	return nil
}
