package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/xxxsen/tgfile/constant"
)

type MappingIssue struct {
	EntryID   uint64 `json:"entry_id"`
	FileID    string `json:"file_id"`
	FileState *int64 `json:"file_state,omitempty"`
}

type PartCountIssue struct {
	FileID            uint64 `json:"file_id"`
	DeclaredPartCount int64  `json:"declared_part_count"`
	ActualPartCount   int64  `json:"actual_part_count"`
}

type AuditReport struct {
	QuickCheck                 string           `json:"quick_check"`
	FileCountByState           map[string]int64 `json:"file_count_by_state"`
	FileSizeByState            map[string]int64 `json:"file_size_by_state"`
	FilePartCount              int64            `json:"file_part_count"`
	MappingCount               int64            `json:"mapping_count"`
	MappingToMissingFile       []MappingIssue   `json:"mapping_to_missing_file"`
	MappingToNonReadyFile      []MappingIssue   `json:"mapping_to_non_ready_file"`
	ReadyFilePartCountMismatch []PartCountIssue `json:"ready_file_part_count_mismatch"`
	UnreferencedFileCount      int64            `json:"unreferenced_file_count"`
	LegacyDefaultRootExists    bool             `json:"legacy_default_root_exists"`
	CorrectDefaultRootExists   bool             `json:"correct_default_root_exists"`
}

func readFileStateTotals(ctx context.Context, database *sql.DB, report *AuditReport) error {
	rows, err := database.QueryContext(ctx, `
SELECT file_state, COUNT(*), COALESCE(SUM(file_size), 0)
FROM tg_file_tab
GROUP BY file_state
ORDER BY file_state;`)
	if err != nil {
		return fmt.Errorf("count files by state: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var state, count, size int64
		if err := rows.Scan(&state, &count, &size); err != nil {
			return fmt.Errorf("scan file state totals: %w", err)
		}
		key := fmt.Sprintf("%d", state)
		report.FileCountByState[key] = count
		report.FileSizeByState[key] = size
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read file state totals: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close file state rows: %w", err)
	}
	return nil
}

func readMappingIssues(
	ctx context.Context,
	database *sql.DB,
	query string,
	hasState bool,
	args ...any,
) ([]MappingIssue, error) {
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query mapping issues: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	issues := make([]MappingIssue, 0)
	for rows.Next() {
		var issue MappingIssue
		var scanErr error
		if hasState {
			var state int64
			scanErr = rows.Scan(&issue.EntryID, &issue.FileID, &state)
			issue.FileState = &state
		} else {
			scanErr = rows.Scan(&issue.EntryID, &issue.FileID)
		}
		if scanErr != nil {
			return nil, fmt.Errorf("scan mapping issue: %w", scanErr)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mapping issues: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close mapping issue rows: %w", err)
	}
	return issues, nil
}

func readPartCountIssues(ctx context.Context, database *sql.DB) ([]PartCountIssue, error) {
	rows, err := database.QueryContext(ctx, `
SELECT file.file_id, file.file_part_count, COUNT(part.id)
FROM tg_file_tab AS file
LEFT JOIN tg_file_part_tab AS part ON part.file_id = file.file_id
WHERE file.file_state = ?
GROUP BY file.file_id, file.file_part_count
HAVING file.file_part_count <> COUNT(part.id)
ORDER BY file.file_id;`, constant.FileStateReady)
	if err != nil {
		return nil, fmt.Errorf("find part count mismatches: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	issues := make([]PartCountIssue, 0)
	for rows.Next() {
		var issue PartCountIssue
		if err := rows.Scan(
			&issue.FileID,
			&issue.DeclaredPartCount,
			&issue.ActualPartCount,
		); err != nil {
			return nil, fmt.Errorf("scan part count mismatch: %w", err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read part count mismatches: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close part mismatch rows: %w", err)
	}
	return issues, nil
}

const missingFileMappingsSQL = `
SELECT mapping.entry_id, mapping.ref_data
FROM tg_file_mapping_tab AS mapping
LEFT JOIN tg_file_tab AS file
  ON CAST(file.file_id AS TEXT) = mapping.ref_data
WHERE mapping.file_kind = 2
  AND file.file_id IS NULL
ORDER BY mapping.entry_id;`

const nonReadyFileMappingsSQL = `
SELECT mapping.entry_id, mapping.ref_data, file.file_state
FROM tg_file_mapping_tab AS mapping
JOIN tg_file_tab AS file
  ON CAST(file.file_id AS TEXT) = mapping.ref_data
WHERE mapping.file_kind = 2
  AND file.file_state <> ?
ORDER BY mapping.entry_id;`

func Audit(ctx context.Context, databaseFile string) (*AuditReport, error) {
	database, err := openDatabase(ctx, databaseFile, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = database.Close()
	}()

	report := &AuditReport{
		FileCountByState:      make(map[string]int64),
		FileSizeByState:       make(map[string]int64),
		MappingToMissingFile:  make([]MappingIssue, 0),
		MappingToNonReadyFile: make([]MappingIssue, 0),
	}

	if err := database.QueryRowContext(ctx, "PRAGMA quick_check;").Scan(&report.QuickCheck); err != nil {
		return nil, fmt.Errorf("run quick_check: %w", err)
	}
	if err := readFileStateTotals(ctx, database, report); err != nil {
		return nil, err
	}
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_file_part_tab;",
	).Scan(&report.FilePartCount); err != nil {
		return nil, fmt.Errorf("count file parts: %w", err)
	}
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_file_mapping_tab;",
	).Scan(&report.MappingCount); err != nil {
		return nil, fmt.Errorf("count mappings: %w", err)
	}
	report.MappingToMissingFile, err = readMappingIssues(
		ctx,
		database,
		missingFileMappingsSQL,
		false,
	)
	if err != nil {
		return nil, err
	}
	report.MappingToNonReadyFile, err = readMappingIssues(
		ctx,
		database,
		nonReadyFileMappingsSQL,
		true,
		constant.FileStateReady,
	)
	if err != nil {
		return nil, err
	}
	report.ReadyFilePartCountMismatch, err = readPartCountIssues(ctx, database)
	if err != nil {
		return nil, err
	}

	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM tg_file_tab AS file
WHERE NOT EXISTS (
    SELECT 1
    FROM tg_file_mapping_tab AS mapping
    WHERE mapping.file_kind = 2
      AND mapping.ref_data = CAST(file.file_id AS TEXT)
);`).Scan(&report.UnreferencedFileCount); err != nil {
		return nil, fmt.Errorf("count unreferenced files: %w", err)
	}

	legacyRoot := database.QueryRowContext(ctx, defaultRootExistsSQL, "defauls")
	if err := legacyRoot.Scan(&report.LegacyDefaultRootExists); err != nil {
		return nil, fmt.Errorf("check legacy default root: %w", err)
	}
	correctRoot := database.QueryRowContext(ctx, defaultRootExistsSQL, "defaults")
	if err := correctRoot.Scan(&report.CorrectDefaultRootExists); err != nil {
		return nil, fmt.Errorf("check correct default root: %w", err)
	}
	return report, nil
}

const defaultRootExistsSQL = `
SELECT EXISTS (
    SELECT 1
    FROM tg_file_mapping_tab AS child
    JOIN tg_file_mapping_tab AS root
      ON root.entry_id = child.parent_entry_id
    WHERE root.parent_entry_id = 0
      AND root.file_name = '/'
      AND child.file_name = ?
      AND child.file_kind = 1
);`

func WriteAuditReport(file string, report *AuditReport) error {
	output, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open audit output: %w", err)
	}
	if err := output.Chmod(0o600); err != nil {
		_ = output.Close()
		return fmt.Errorf("restrict audit output permissions: %w", err)
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_ = output.Close()
		return fmt.Errorf("encode audit report: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close audit output: %w", err)
	}
	return nil
}
