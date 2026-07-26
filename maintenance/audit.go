package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/s3checksum"
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
	QuickCheck                       string           `json:"quick_check"`
	FileCountByState                 map[string]int64 `json:"file_count_by_state"`
	FileSizeByState                  map[string]int64 `json:"file_size_by_state"`
	FilePartCount                    int64            `json:"file_part_count"`
	MappingCount                     int64            `json:"mapping_count"`
	MappingToMissingFile             []MappingIssue   `json:"mapping_to_missing_file"`
	MappingToNonReadyFile            []MappingIssue   `json:"mapping_to_non_ready_file"`
	ReadyFilePartCountMismatch       []PartCountIssue `json:"ready_file_part_count_mismatch"`
	UnreferencedFileCount            int64            `json:"unreferenced_file_count"`
	DefaultRootExists                bool             `json:"default_root_exists"`
	S3MetadataCount                  int64            `json:"s3_metadata_count"`
	S3MetadataWithoutMapping         int64            `json:"s3_metadata_without_mapping"`
	S3MappingWithoutMetadataByBucket map[string]int64 `json:"s3_mapping_without_metadata_by_bucket"`
	BlockDeleteCountByState          map[string]int64 `json:"block_delete_state_count"`
	OldestPendingAgeMillis           int64            `json:"block_delete_oldest_pending_ms"`
	ExpiredDeleteLeaseCount          int64            `json:"block_delete_expired_lease_count"`
	ProcessableDeleteCount           int64            `json:"block_delete_eligible_before_deadline_count"`
	DeleteStateWithoutPart           int64            `json:"delete_state_without_part"`
	BlockDeleteBackendMismatch       int64            `json:"block_delete_backend_mismatch_count"`
	PrivateFileWithPublicMapping     int64            `json:"private_bucket_file_with_public_mapping_count"`
	PrivateFileWithDefaultsMapping   int64            `json:"private_bucket_file_with_defaults_mapping_count"`
	MultipartUploadCountByState      map[string]int64 `json:"multipart_upload_state_count"`
	MultipartPartCountByState        map[string]int64 `json:"multipart_part_state_count"`
	InvalidMultipartStateCount       int64            `json:"invalid_multipart_state_count"`
	InvalidCompletedUploadCount      int64            `json:"invalid_completed_upload_count"`
	InvalidCompositeManifestCount    int64            `json:"invalid_composite_manifest_count"`
	OrphanMultipartPartCount         int64            `json:"orphan_multipart_part_count"`
	OrphanCompositeSegmentCount      int64            `json:"orphan_composite_segment_count"`
	MappedCompositeNonLiveSource     int64            `json:"mapped_composite_non_live_source_count"`
	ActiveMultipartNonLivePart       int64            `json:"active_multipart_non_live_part_count"`
	NonLiveReferencedFileCount       int64            `json:"non_live_referenced_file_count"`
	DiscardedUnreferencedLivePart    int64            `json:"discarded_unreferenced_live_part_count"`
	RetainedUnreferencedLiveFile     int64            `json:"retained_unreferenced_live_file_count"`
	CompletingUploadCount            int64            `json:"multipart_completing_upload_count"`
	ExpiredActiveUploadCount         int64            `json:"multipart_expired_active_upload_count"`
	OldestExpiredUploadAgeMillis     int64            `json:"multipart_oldest_expired_upload_age_ms"`
	InvalidMultipartChecksumPolicy   int64            `json:"invalid_multipart_checksum_policy_count"`
	LegacyMultipartChecksumData      int64            `json:"legacy_multipart_checksum_data_count"`
	InvalidActivePartChecksum        int64            `json:"invalid_active_multipart_part_checksum_count"`
	InvalidCompositeResultChecksum   int64            `json:"invalid_composite_result_checksum_count"`
	MissingCompletedResultChecksum   int64            `json:"missing_completed_result_checksum_count"`
	MultipartResultMetadataMismatch  int64            `json:"multipart_result_metadata_mismatch_count"`
	InvalidObjectChecksumMetadata    int64            `json:"invalid_object_checksum_metadata_count"`
	CompletedPartCount               int64            `json:"completed_part_count"`
	CompletedPartCountByState        map[string]int64 `json:"completed_part_checksum_state_count"`
	LayoutV2MissingCompletedPart     int64            `json:"layout_v2_missing_completed_part_count"`
	LayoutV1WithCompletedPart        int64            `json:"layout_v1_with_completed_part_count"`
	CompletedPartWithoutSegment      int64            `json:"completed_part_without_segment_count"`
	CompletedPartNumberMismatch      int64            `json:"completed_part_number_mismatch_count"`
	CompletedPartSizeMismatch        int64            `json:"completed_part_size_mismatch_count"`
	CompletedPartFinalSizeMismatch   int64            `json:"completed_part_final_size_mismatch_count"`
	InvalidCompletedPartChecksum     int64            `json:"invalid_completed_part_checksum_count"`
	CompletedControlManifestMismatch int64            `json:"completed_control_manifest_mismatch_count"`
	ObjectManifestAlgorithmMismatch  int64            `json:"object_manifest_algorithm_mismatch_count"`
	LegacyUnknownPartSizeCount       int64            `json:"legacy_unknown_part_size_count"`
	BackupJobCountByState            map[string]int64 `json:"backup_job_state_count"`
	BackupTerminalJobPinCount        int64            `json:"backup_terminal_job_pin_count"`
	BackupOrphanPinCount             int64            `json:"backup_orphan_pin_count"`
	BackupJobFileInvalidTargetCount  int64            `json:"backup_job_file_invalid_target_count"`
	BackupStagedFileMappedCount      int64            `json:"backup_staged_file_mapped_count"`
	BackupExpiredArtifactCount       int64            `json:"backup_expired_artifact_count"`
	BackupOrphanWorkFileCount        int64            `json:"backup_orphan_work_file_count"`
	BackupOrphanWorkFileBytes        int64            `json:"backup_orphan_work_file_bytes"`
	BackupMissingWorkFileCount       int64            `json:"backup_missing_work_file_count"`
	BackupActiveExportMissingPin     int64            `json:"backup_active_export_missing_pin_count"`
	BackupActiveJobMissingPath       int64            `json:"backup_active_job_missing_path_count"`
	BackupPartMissingLiveDeleteState int64            `json:"backup_part_missing_live_delete_state_count"`
	BackupDeleteRefTargetMismatch    int64            `json:"backup_delete_ref_target_mismatch_count"`
}

type AuditOptions struct {
	S3Buckets      []AuditBucket
	BackendKind    string
	BackupWorkDir  string
	TelegramBotID  int64
	TelegramChatID int64
}

type AuditBucket struct {
	Name string
	ACL  string
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
WITH actual_counts AS (
    SELECT file.file_id, file.file_part_count,
        CASE file.file_layout_version
            WHEN 1 THEN (
                SELECT COUNT(*) FROM tg_file_part_tab part
                WHERE part.file_id = file.file_id
            )
            WHEN 2 THEN (
                SELECT COALESCE(SUM(source.file_part_count), 0)
                FROM tg_s3_file_segment_tab segment
                JOIN tg_file_tab source ON source.file_id = segment.source_file_id
                WHERE segment.file_id = file.file_id
            )
            ELSE -1
        END AS actual_part_count
    FROM tg_file_tab file
    WHERE file.file_state = ?
)
SELECT file_id, file_part_count, actual_part_count
FROM actual_counts
WHERE file_part_count <> actual_part_count
ORDER BY file_id;`, constant.FileStateReady)
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
	return AuditWithOptions(ctx, databaseFile, AuditOptions{})
}

func AuditWithOptions(
	ctx context.Context,
	databaseFile string,
	options AuditOptions,
) (*AuditReport, error) {
	database, err := openDatabase(ctx, databaseFile, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = database.Close()
	}()

	report := &AuditReport{
		FileCountByState:                 make(map[string]int64),
		FileSizeByState:                  make(map[string]int64),
		MappingToMissingFile:             make([]MappingIssue, 0),
		MappingToNonReadyFile:            make([]MappingIssue, 0),
		S3MappingWithoutMetadataByBucket: make(map[string]int64, len(options.S3Buckets)),
		BlockDeleteCountByState:          make(map[string]int64),
		MultipartUploadCountByState:      make(map[string]int64),
		MultipartPartCountByState:        make(map[string]int64),
		CompletedPartCountByState:        make(map[string]int64),
		BackupJobCountByState:            make(map[string]int64),
	}
	if err := readCoreAudit(ctx, database, report); err != nil {
		return nil, err
	}
	if err := readS3Audit(ctx, database, report, options); err != nil {
		return nil, err
	}
	if err := readMultipartAudit(ctx, database, report); err != nil {
		return nil, err
	}
	if err := readBackupAudit(ctx, database, report, options); err != nil {
		return nil, err
	}
	return report, nil
}

func readCoreAudit(ctx context.Context, database *sql.DB, report *AuditReport) error {
	var err error
	if err := database.QueryRowContext(ctx, "PRAGMA quick_check;").Scan(&report.QuickCheck); err != nil {
		return fmt.Errorf("run quick_check: %w", err)
	}
	if err := readFileStateTotals(ctx, database, report); err != nil {
		return err
	}
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_file_part_tab;",
	).Scan(&report.FilePartCount); err != nil {
		return fmt.Errorf("count file parts: %w", err)
	}
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_file_part_tab WHERE file_part_size < 0;",
	).Scan(&report.LegacyUnknownPartSizeCount); err != nil {
		return fmt.Errorf("count legacy unknown part sizes: %w", err)
	}
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_file_mapping_tab;",
	).Scan(&report.MappingCount); err != nil {
		return fmt.Errorf("count mappings: %w", err)
	}
	report.MappingToMissingFile, err = readMappingIssues(
		ctx,
		database,
		missingFileMappingsSQL,
		false,
	)
	if err != nil {
		return err
	}
	report.MappingToNonReadyFile, err = readMappingIssues(
		ctx,
		database,
		nonReadyFileMappingsSQL,
		true,
		constant.FileStateReady,
	)
	if err != nil {
		return err
	}
	report.ReadyFilePartCountMismatch, err = readPartCountIssues(ctx, database)
	if err != nil {
		return err
	}

	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM tg_file_tab AS file
WHERE NOT EXISTS (
    SELECT 1
    FROM tg_file_mapping_tab AS mapping
    WHERE mapping.file_kind = 2
      AND mapping.ref_data = CAST(file.file_id AS TEXT)
)
AND NOT EXISTS (
    SELECT 1 FROM tg_s3_file_segment_tab segment
    WHERE segment.file_id = file.file_id OR segment.source_file_id = file.file_id
)
AND NOT EXISTS (
    SELECT 1
    FROM tg_s3_multipart_part_tab part
    JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
    WHERE part.file_id = file.file_id
      AND part.part_state = 'active'
      AND upload.upload_state = 'active'
);`).Scan(&report.UnreferencedFileCount); err != nil {
		return fmt.Errorf("count unreferenced files: %w", err)
	}

	defaultRoot := database.QueryRowContext(ctx, defaultRootExistsSQL, "defaults")
	if err := defaultRoot.Scan(&report.DefaultRootExists); err != nil {
		return fmt.Errorf("check default root: %w", err)
	}
	return nil
}

func readMultipartAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	if err := readStateCounts(
		ctx,
		database,
		"SELECT upload_state, COUNT(*) FROM tg_s3_multipart_upload_tab GROUP BY upload_state ORDER BY upload_state",
		report.MultipartUploadCountByState,
	); err != nil {
		return fmt.Errorf("read multipart upload state counts: %w", err)
	}
	if err := readStateCounts(
		ctx,
		database,
		"SELECT part_state, COUNT(*) FROM tg_s3_multipart_part_tab GROUP BY part_state ORDER BY part_state",
		report.MultipartPartCountByState,
	); err != nil {
		return fmt.Errorf("read multipart part state counts: %w", err)
	}
	for _, readGroup := range []func(context.Context, *sql.DB, *AuditReport) error{
		readMultipartAuditGroupOne,
		readMultipartAuditGroupTwo,
		readMultipartAuditGroupThree,
		readMultipartAuditGroupFour,
	} {
		if err := readGroup(ctx, database, report); err != nil {
			return err
		}
	}
	if err := readChecksumAudit(ctx, database, report); err != nil {
		return err
	}
	if err := readCompletedPartAudit(ctx, database, report); err != nil {
		return err
	}
	return readExpiredMultipartAudit(ctx, database, report)
}

func readCompletedPartAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_s3_completed_part_tab",
	).Scan(&report.CompletedPartCount); err != nil {
		return fmt.Errorf("count completed S3 parts: %w", err)
	}
	if err := readStateCounts(
		ctx,
		database,
		`SELECT checksum_state, COUNT(*) FROM tg_s3_completed_part_tab
GROUP BY checksum_state ORDER BY checksum_state`,
		report.CompletedPartCountByState,
	); err != nil {
		return fmt.Errorf("read completed S3 part checksum states: %w", err)
	}
	if err := readCompletedPartInvariantAudit(ctx, database, report); err != nil {
		return err
	}
	return readCompletedPartChecksumAudit(ctx, database, report)
}

func readCompletedPartInvariantAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	if err := readCompletedPartStructuralAudit(ctx, database, report); err != nil {
		return err
	}
	return readCompletedPartControlAudit(ctx, database, report)
}

func readCompletedPartStructuralAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.LayoutV2MissingCompletedPart,
			name:        "layout-v2 files missing completed parts",
			query: `SELECT COUNT(*) FROM (
    SELECT final.file_id
    FROM tg_file_tab final
    LEFT JOIN tg_s3_file_segment_tab segment ON segment.file_id = final.file_id
    LEFT JOIN tg_s3_completed_part_tab part
      ON part.file_id = final.file_id
     AND part.part_number = segment.segment_index + 1
    WHERE final.file_layout_version = 2
    GROUP BY final.file_id
    HAVING COUNT(segment.segment_index) = 0
       OR COUNT(part.part_number) != COUNT(segment.segment_index)
)`,
		},
		{
			destination: &report.LayoutV1WithCompletedPart,
			name:        "layout-v1 files with completed parts",
			query: `SELECT COUNT(DISTINCT part.file_id)
FROM tg_s3_completed_part_tab part
JOIN tg_file_tab file ON file.file_id = part.file_id
WHERE file.file_layout_version = 1`,
		},
		{
			destination: &report.CompletedPartWithoutSegment,
			name:        "completed parts without segments",
			query: `SELECT COUNT(*)
FROM tg_s3_completed_part_tab part
LEFT JOIN tg_s3_file_segment_tab segment
  ON segment.file_id = part.file_id
 AND segment.segment_index + 1 = part.part_number
WHERE segment.file_id IS NULL`,
		},
		{
			destination: &report.CompletedPartNumberMismatch,
			name:        "completed part number mismatches",
			query: `SELECT COUNT(*) FROM (
    SELECT file_id
    FROM tg_s3_completed_part_tab
    GROUP BY file_id
    HAVING MIN(part_number) != 1
       OR MAX(part_number) != COUNT(*)
)`,
		},
		{
			destination: &report.CompletedPartSizeMismatch,
			name:        "completed part size mismatches",
			query: `SELECT COUNT(*)
FROM tg_s3_completed_part_tab part
JOIN tg_s3_file_segment_tab segment
  ON segment.file_id = part.file_id
 AND segment.segment_index + 1 = part.part_number
WHERE part.part_size != segment.segment_size`,
		},
		{
			destination: &report.CompletedPartFinalSizeMismatch,
			name:        "completed part final size mismatches",
			query: `SELECT COUNT(*) FROM (
    SELECT final.file_id
    FROM tg_file_tab final
    LEFT JOIN tg_s3_completed_part_tab part ON part.file_id = final.file_id
    WHERE final.file_layout_version = 2
    GROUP BY final.file_id, final.file_size
	    HAVING COALESCE(SUM(part.part_size), 0) != final.file_size
)`,
		},
	})
}

func readCompletedPartControlAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.CompletedControlManifestMismatch,
			name:        "completed control and part manifest mismatches",
			query: `WITH ranked_selected_part AS (
    SELECT
        upload.result_file_id AS file_id,
        ROW_NUMBER() OVER (
            PARTITION BY upload.upload_id ORDER BY source.part_number
        ) AS final_part_number,
        upload.checksum_algorithm AS checksum_algorithm,
        source.checksum_value AS checksum_value
    FROM tg_s3_multipart_upload_tab upload
    JOIN tg_s3_multipart_part_tab source ON source.upload_id = upload.upload_id
    WHERE upload.upload_state = 'completed'
      AND upload.checksum_algorithm != ''
      AND source.part_state = 'selected'
)
SELECT COUNT(*)
FROM ranked_selected_part source
LEFT JOIN tg_s3_completed_part_tab completed
  ON completed.file_id = source.file_id
 AND completed.part_number = source.final_part_number
WHERE completed.file_id IS NULL
   OR completed.checksum_state != 'available'
   OR completed.checksum_algorithm != source.checksum_algorithm
   OR completed.checksum_value != source.checksum_value`,
		},
		{
			destination: &report.ObjectManifestAlgorithmMismatch,
			name:        "object metadata and part manifest algorithm mismatches",
			query: `SELECT COUNT(DISTINCT part.file_id)
FROM tg_s3_completed_part_tab part
JOIN tg_file_mapping_tab mapping ON mapping.ref_data = CAST(part.file_id AS TEXT)
JOIN tg_s3_object_metadata_tab metadata ON metadata.entry_id = mapping.entry_id
WHERE part.checksum_state = 'available'
  AND metadata.request_checksum_algorithm != part.checksum_algorithm`,
		},
	})
}

func readCompletedPartChecksumAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	rows, err := database.QueryContext(ctx, `
SELECT checksum_state, checksum_algorithm, checksum_value
FROM tg_s3_completed_part_tab
ORDER BY file_id, part_number`)
	if err != nil {
		return fmt.Errorf("query completed S3 part checksums: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var state, algorithmValue, checksumValue string
		if err := rows.Scan(&state, &algorithmValue, &checksumValue); err != nil {
			return fmt.Errorf("scan completed S3 part checksum: %w", err)
		}
		if state == "unavailable" {
			if algorithmValue != "" || checksumValue != "" {
				report.InvalidCompletedPartChecksum++
			}
			continue
		}
		algorithm, err := s3checksum.ParseAlgorithm(algorithmValue)
		if state != "available" || err != nil {
			report.InvalidCompletedPartChecksum++
			continue
		}
		if _, err := s3checksum.Decode(algorithm, checksumValue); err != nil {
			report.InvalidCompletedPartChecksum++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate completed S3 part checksums: %w", err)
	}
	return nil
}

func readChecksumAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	if err := runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.InvalidMultipartChecksumPolicy,
			name:        "invalid multipart checksum policies",
			query: `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab
WHERE checksum_algorithm NOT IN ('', 'CRC32', 'CRC32C', 'CRC64NVME', 'SHA1', 'SHA256')
   OR checksum_type NOT IN ('', 'FULL_OBJECT', 'COMPOSITE')
   OR (checksum_algorithm = '') != (checksum_type = '')
   OR checksum_algorithm = 'CRC64NVME' AND checksum_type != 'FULL_OBJECT'
   OR checksum_algorithm IN ('SHA1', 'SHA256') AND checksum_type != 'COMPOSITE'
   OR checksum_algorithm IN ('CRC32', 'CRC32C')
      AND checksum_type NOT IN ('FULL_OBJECT', 'COMPOSITE')`,
		},
		{
			destination: &report.LegacyMultipartChecksumData,
			name:        "legacy multipart checksum data",
			query: `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab upload
WHERE upload.checksum_algorithm = '' AND upload.checksum_type = ''
  AND (
      upload.result_checksum_value != ''
      OR EXISTS (
          SELECT 1 FROM tg_s3_multipart_part_tab part
          WHERE part.upload_id = upload.upload_id AND part.checksum_value != ''
      )
  )`,
		},
		{
			destination: &report.MissingCompletedResultChecksum,
			name:        "missing completed multipart result checksums",
			query: `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab
WHERE upload_state = 'completed'
  AND checksum_algorithm != ''
  AND result_checksum_value = ''`,
		},
		{
			destination: &report.MultipartResultMetadataMismatch,
			name:        "multipart result metadata mismatches",
			query: `SELECT COUNT(*) FROM tg_s3_multipart_upload_tab upload
WHERE upload.upload_state = 'completed'
  AND upload.checksum_algorithm != ''
  AND EXISTS (
      SELECT 1
      FROM tg_file_mapping_tab mapping
      JOIN tg_s3_object_metadata_tab metadata ON metadata.entry_id = mapping.entry_id
      WHERE mapping.file_kind = 2
        AND mapping.ref_data = CAST(upload.result_file_id AS TEXT)
  )
  AND NOT EXISTS (
      SELECT 1
      FROM tg_file_mapping_tab mapping
      JOIN tg_s3_object_metadata_tab metadata ON metadata.entry_id = mapping.entry_id
      WHERE mapping.file_kind = 2
        AND mapping.ref_data = CAST(upload.result_file_id AS TEXT)
        AND metadata.request_checksum_algorithm = upload.checksum_algorithm
        AND metadata.checksum_type = upload.checksum_type
        AND metadata.request_checksum_value = upload.result_checksum_value
  )`,
		},
	}); err != nil {
		return err
	}
	if err := readActivePartChecksumAudit(ctx, database, report); err != nil {
		return err
	}
	if err := readCompositeResultChecksumAudit(ctx, database, report); err != nil {
		return err
	}
	return readObjectChecksumMetadataAudit(ctx, database, report)
}

func readActivePartChecksumAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	rows, err := database.QueryContext(ctx, `
SELECT upload.checksum_algorithm, part.checksum_value
FROM tg_s3_multipart_part_tab part
JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
WHERE upload.upload_state = 'active'
  AND part.part_state = 'active'
  AND upload.checksum_algorithm != ''`)
	if err != nil {
		return fmt.Errorf("query active multipart part checksums: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var algorithmValue, checksumValue string
		if err := rows.Scan(&algorithmValue, &checksumValue); err != nil {
			return fmt.Errorf("scan active multipart part checksum: %w", err)
		}
		algorithm, parseErr := s3checksum.ParseAlgorithm(algorithmValue)
		if parseErr != nil {
			report.InvalidActivePartChecksum++
			continue
		}
		if _, decodeErr := s3checksum.Decode(algorithm, checksumValue); decodeErr != nil {
			report.InvalidActivePartChecksum++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active multipart part checksums: %w", err)
	}
	return nil
}

func readCompositeResultChecksumAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	rows, err := database.QueryContext(ctx, `
SELECT upload.checksum_algorithm, upload.result_checksum_value, COUNT(part.part_number)
FROM tg_s3_multipart_upload_tab upload
LEFT JOIN tg_s3_multipart_part_tab part
  ON part.upload_id = upload.upload_id AND part.part_state = 'selected'
WHERE upload.upload_state = 'completed' AND upload.checksum_type = 'COMPOSITE'
GROUP BY upload.upload_id, upload.checksum_algorithm, upload.result_checksum_value`)
	if err != nil {
		return fmt.Errorf("query composite multipart result checksums: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var algorithmValue, checksumValue string
		var partCount int
		if err := rows.Scan(&algorithmValue, &checksumValue, &partCount); err != nil {
			return fmt.Errorf("scan composite multipart result checksum: %w", err)
		}
		algorithm, parseErr := s3checksum.ParseAlgorithm(algorithmValue)
		if parseErr != nil {
			report.InvalidCompositeResultChecksum++
			continue
		}
		if _, parseErr = s3checksum.ParseCompositeStored(
			algorithm,
			checksumValue,
			partCount,
		); parseErr != nil {
			report.InvalidCompositeResultChecksum++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate composite multipart result checksums: %w", err)
	}
	return nil
}

func readObjectChecksumMetadataAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	rows, err := database.QueryContext(ctx, `
SELECT request_checksum_algorithm, checksum_type, request_checksum_value
FROM tg_s3_object_metadata_tab`)
	if err != nil {
		return fmt.Errorf("query object checksum metadata: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var algorithmValue, typeValue, checksumValue string
		if err := rows.Scan(&algorithmValue, &typeValue, &checksumValue); err != nil {
			return fmt.Errorf("scan object checksum metadata: %w", err)
		}
		if !validObjectChecksumMetadata(algorithmValue, typeValue, checksumValue) {
			report.InvalidObjectChecksumMetadata++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate object checksum metadata: %w", err)
	}
	return nil
}

func validObjectChecksumMetadata(algorithmValue, typeValue, checksumValue string) bool {
	presentFields := 0
	for _, value := range []string{algorithmValue, typeValue, checksumValue} {
		if value != "" {
			presentFields++
		}
	}
	if presentFields == 0 {
		return true
	}
	if presentFields != 3 {
		return false
	}
	algorithm, algorithmErr := s3checksum.ParseAlgorithm(algorithmValue)
	checksumType, typeErr := s3checksum.ParseType(typeValue)
	if algorithmErr != nil || typeErr != nil {
		return false
	}
	value, valid := objectChecksumDigestValue(algorithm, checksumType, checksumValue)
	if !valid {
		return false
	}
	_, err := s3checksum.Decode(algorithm, value)
	return err == nil
}

func objectChecksumDigestValue(
	algorithm s3checksum.Algorithm,
	checksumType s3checksum.Type,
	value string,
) (string, bool) {
	if checksumType != s3checksum.TypeComposite {
		return value, true
	}
	if s3checksum.ValidateCombination(algorithm, checksumType) != nil {
		return "", false
	}
	separator := strings.LastIndexByte(value, '-')
	if separator <= 0 || separator == len(value)-1 {
		return "", false
	}
	partCount, err := strconv.Atoi(value[separator+1:])
	if err != nil || partCount <= 0 {
		return "", false
	}
	return value[:separator], true
}

type multipartAuditCountQuery struct {
	destination *int64
	name        string
	query       string
}

func readMultipartAuditGroupOne(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.InvalidMultipartStateCount,
			name:        "invalid multipart state combinations",
			query: `SELECT COUNT(*)
FROM tg_s3_multipart_part_tab part
LEFT JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
WHERE upload.upload_id IS NULL
   OR part.part_state = 'active' AND upload.upload_state != 'active'
   OR part.part_state = 'selected' AND upload.upload_state != 'completed'
   OR part.part_state = 'discarded' AND upload.upload_state NOT IN ('completed', 'aborted')`,
		},
		{
			destination: &report.InvalidCompletedUploadCount,
			name:        "invalid completed multipart uploads",
			query: `SELECT COUNT(*)
FROM tg_s3_multipart_upload_tab upload
LEFT JOIN tg_file_tab result ON result.file_id = upload.result_file_id
WHERE upload.upload_state = 'completed'
  AND (
      upload.completion_fingerprint = ''
      OR upload.result_file_id = 0
      OR upload.result_etag = ''
      OR upload.completed_at = 0
      OR upload.cleanup_at <= upload.completed_at
      OR result.file_id IS NULL
      OR result.file_layout_version != 2
      OR result.file_state != 2
  )`,
		},
	})
}

func readMultipartAuditGroupTwo(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.InvalidCompositeManifestCount,
			name:        "invalid composite manifests",
			query: `SELECT COUNT(*) FROM (
    SELECT final.file_id
    FROM tg_file_tab final
    LEFT JOIN tg_s3_file_segment_tab segment ON segment.file_id = final.file_id
    LEFT JOIN tg_file_tab source ON source.file_id = segment.source_file_id
    WHERE final.file_layout_version = 2
    GROUP BY final.file_id, final.file_size, final.file_part_count, final.file_state
    HAVING final.file_state != 2
       OR COUNT(segment.segment_index) = 0
       OR MIN(segment.segment_index) != 0
       OR MAX(segment.segment_index) + 1 != COUNT(segment.segment_index)
       OR COALESCE(SUM(segment.segment_size), 0) != final.file_size
       OR COALESCE(SUM(source.file_part_count), 0) != final.file_part_count
       OR SUM(CASE
            WHEN source.file_id IS NULL
              OR source.file_state != 2
              OR source.file_layout_version != 1
              OR source.file_size != segment.segment_size
            THEN 1 ELSE 0 END) != 0
)`,
		},
		{
			destination: &report.OrphanMultipartPartCount,
			name:        "orphan multipart parts",
			query: `SELECT COUNT(*)
FROM tg_s3_multipart_part_tab part
LEFT JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
LEFT JOIN tg_file_tab file ON file.file_id = part.file_id
WHERE upload.upload_id IS NULL OR file.file_id IS NULL`,
		},
		{
			destination: &report.OrphanCompositeSegmentCount,
			name:        "orphan composite segments",
			query: `SELECT COUNT(*)
FROM tg_s3_file_segment_tab segment
LEFT JOIN tg_file_tab final ON final.file_id = segment.file_id
LEFT JOIN tg_file_tab source ON source.file_id = segment.source_file_id
WHERE final.file_id IS NULL OR final.file_layout_version != 2 OR source.file_id IS NULL`,
		},
	})
}

func readMultipartAuditGroupThree(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.MappedCompositeNonLiveSource,
			name:        "mapped composite non-live sources",
			query: `SELECT COUNT(DISTINCT segment.source_file_id)
	FROM tg_s3_file_segment_tab segment
	JOIN tg_file_mapping_tab mapping ON mapping.ref_data = CAST(segment.file_id AS TEXT)
	JOIN tg_file_tab source ON source.file_id = segment.source_file_id
	WHERE (
	    SELECT COUNT(*) FROM tg_file_part_tab physical
	    WHERE physical.file_id = source.file_id
	) != source.file_part_count
	   OR (
	    SELECT COUNT(*) FROM tg_file_part_delete_state_tab state
	    WHERE state.file_id = source.file_id AND state.delete_state = 'live'
	) != source.file_part_count`,
		},
		{
			destination: &report.ActiveMultipartNonLivePart,
			name:        "active multipart non-live parts",
			query: `SELECT COUNT(DISTINCT part.file_id)
	FROM tg_s3_multipart_part_tab part
	JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
	JOIN tg_file_tab file ON file.file_id = part.file_id
	WHERE upload.upload_state = 'active'
	  AND part.part_state = 'active'
	  AND (
	      (
	          SELECT COUNT(*) FROM tg_file_part_tab physical
	          WHERE physical.file_id = file.file_id
	      ) != file.file_part_count
	      OR (
	          SELECT COUNT(*) FROM tg_file_part_delete_state_tab state
	          WHERE state.file_id = file.file_id AND state.delete_state = 'live'
	      ) != file.file_part_count
	  )`,
		},
		{
			destination: &report.NonLiveReferencedFileCount,
			name:        "non-live referenced files",
			query: `SELECT COUNT(DISTINCT state.file_id)
FROM tg_file_part_delete_state_tab state
WHERE state.delete_state != 'live'
  AND (
      EXISTS (
          SELECT 1 FROM tg_file_mapping_tab mapping
          WHERE mapping.ref_data = CAST(state.file_id AS TEXT)
      )
      OR EXISTS (
          SELECT 1
          FROM tg_s3_file_segment_tab segment
          JOIN tg_file_mapping_tab mapping ON mapping.ref_data = CAST(segment.file_id AS TEXT)
          WHERE segment.source_file_id = state.file_id
      )
      OR EXISTS (
          SELECT 1
          FROM tg_s3_multipart_part_tab part
          JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
          WHERE part.file_id = state.file_id
            AND part.part_state = 'active'
            AND upload.upload_state = 'active'
      )
  )`,
		},
	})
}

func readMultipartAuditGroupFour(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	return runMultipartAuditCountQueries(ctx, database, []multipartAuditCountQuery{
		{
			destination: &report.DiscardedUnreferencedLivePart,
			name:        "discarded unreferenced live parts",
			query: `SELECT COUNT(DISTINCT part.file_id)
FROM tg_s3_multipart_part_tab part
WHERE part.part_state = 'discarded'
  AND EXISTS (
      SELECT 1 FROM tg_file_part_delete_state_tab state
      WHERE state.file_id = part.file_id AND state.delete_state = 'live'
  )
  AND NOT EXISTS (
      SELECT 1 FROM tg_file_mapping_tab mapping
      WHERE mapping.ref_data = CAST(part.file_id AS TEXT)
  )
  AND NOT EXISTS (
      SELECT 1
      FROM tg_s3_file_segment_tab segment
      JOIN tg_file_mapping_tab mapping ON mapping.ref_data = CAST(segment.file_id AS TEXT)
      WHERE segment.source_file_id = part.file_id
  )`,
		},
		{
			destination: &report.RetainedUnreferencedLiveFile,
			name:        "retained unreferenced live files",
			query: `SELECT COUNT(*) FROM tg_file_tab file
WHERE file.file_layout_version = 1
  AND EXISTS (
      SELECT 1 FROM tg_file_part_delete_state_tab state
      WHERE state.file_id = file.file_id AND state.delete_state = 'live'
  )
  AND NOT EXISTS (
      SELECT 1 FROM tg_file_mapping_tab mapping
      WHERE mapping.ref_data = CAST(file.file_id AS TEXT)
  )
  AND NOT EXISTS (
      SELECT 1 FROM tg_s3_file_segment_tab segment
      WHERE segment.source_file_id = file.file_id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM tg_s3_multipart_part_tab part
      JOIN tg_s3_multipart_upload_tab upload ON upload.upload_id = part.upload_id
      WHERE part.file_id = file.file_id
        AND part.part_state = 'active'
        AND upload.upload_state = 'active'
  )`,
		},
		{
			destination: &report.CompletingUploadCount,
			name:        "completing multipart uploads",
			query:       "SELECT COUNT(*) FROM tg_s3_multipart_upload_tab WHERE upload_state = 'completing'",
		},
	})
}

func runMultipartAuditCountQueries(
	ctx context.Context,
	database *sql.DB,
	queries []multipartAuditCountQuery,
) error {
	for _, item := range queries {
		if err := database.QueryRowContext(ctx, item.query).Scan(item.destination); err != nil {
			return fmt.Errorf("count %s: %w", item.name, err)
		}
	}
	return nil
}

func readStateCounts(
	ctx context.Context,
	database *sql.DB,
	query string,
	destination map[string]int64,
) error {
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query state counts: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return fmt.Errorf("scan state count: %w", err)
		}
		destination[state] = count
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate state counts: %w", err)
	}
	return nil
}

func readExpiredMultipartAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	now := time.Now().UnixMilli()
	var oldest sql.NullInt64
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*), MIN(expires_at)
FROM tg_s3_multipart_upload_tab
WHERE upload_state = 'active' AND expires_at <= ?`, now).Scan(
		&report.ExpiredActiveUploadCount,
		&oldest,
	); err != nil {
		return fmt.Errorf("count expired active multipart uploads: %w", err)
	}
	if oldest.Valid && oldest.Int64 < now {
		report.OldestExpiredUploadAgeMillis = now - oldest.Int64
	}
	return nil
}

func readS3Audit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
	options AuditOptions,
) error {
	if err := database.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM tg_s3_object_metadata_tab",
	).Scan(&report.S3MetadataCount); err != nil {
		return fmt.Errorf("count S3 metadata: %w", err)
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tg_s3_object_metadata_tab metadata
LEFT JOIN tg_file_mapping_tab mapping ON mapping.entry_id = metadata.entry_id
WHERE mapping.entry_id IS NULL`).Scan(&report.S3MetadataWithoutMapping); err != nil {
		return fmt.Errorf("count S3 metadata without mapping: %w", err)
	}
	for _, bucket := range options.S3Buckets {
		var count int64
		if err := database.QueryRowContext(ctx, `
WITH RECURSIVE bucket_tree(entry_id, file_kind) AS (
    SELECT bucket.entry_id, bucket.file_kind
    FROM tg_file_mapping_tab bucket
    JOIN tg_file_mapping_tab root ON root.entry_id = bucket.parent_entry_id
    WHERE root.parent_entry_id = 0 AND root.file_name = '/'
      AND bucket.file_name = ?
    UNION ALL
    SELECT child.entry_id, child.file_kind
    FROM tg_file_mapping_tab child
    JOIN bucket_tree parent ON child.parent_entry_id = parent.entry_id
)
SELECT COUNT(*) FROM bucket_tree item
LEFT JOIN tg_s3_object_metadata_tab metadata ON metadata.entry_id = item.entry_id
WHERE item.file_kind = 2 AND metadata.entry_id IS NULL`, bucket.Name).Scan(&count); err != nil {
			return fmt.Errorf("count S3 mapping without metadata for bucket %q: %w", bucket.Name, err)
		}
		report.S3MappingWithoutMetadataByBucket[bucket.Name] = count
	}
	if err := readBlockDeleteAudit(ctx, database, report, options.BackendKind); err != nil {
		return err
	}
	return readPrivateSharingAudit(ctx, database, report, options.S3Buckets)
}

func readBlockDeleteAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
	backendKind string,
) error {
	if err := readBlockDeleteStateCounts(ctx, database, report); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	var oldest sql.NullInt64
	if err := database.QueryRowContext(ctx, `
SELECT MIN(ctime) FROM tg_file_part_delete_state_tab
WHERE delete_state = 'pending'`).Scan(&oldest); err != nil {
		return fmt.Errorf("read oldest pending block delete: %w", err)
	}
	if oldest.Valid && oldest.Int64 < now {
		report.OldestPendingAgeMillis = now - oldest.Int64
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE delete_state = 'deleting' AND lease_until <= ?`, now).Scan(&report.ExpiredDeleteLeaseCount); err != nil {
		return fmt.Errorf("count expired block delete leases: %w", err)
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE delete_state IN ('pending', 'deleting') AND uploaded_at + ? > ?`,
		(48 * time.Hour).Milliseconds(),
		now,
	).Scan(&report.ProcessableDeleteCount); err != nil {
		return fmt.Errorf("count processable block deletes: %w", err)
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab state
LEFT JOIN tg_file_part_tab part
  ON part.file_id = state.file_id AND part.file_part_id = state.file_part_id
WHERE part.file_id IS NULL`).Scan(&report.DeleteStateWithoutPart); err != nil {
		return fmt.Errorf("count delete states without parts: %w", err)
	}
	if backendKind == "" {
		return nil
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE backend_kind <> ?`, backendKind).Scan(&report.BlockDeleteBackendMismatch); err != nil {
		return fmt.Errorf("count block delete backend mismatches: %w", err)
	}
	return nil
}

func readBlockDeleteStateCounts(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
) error {
	rows, err := database.QueryContext(ctx, `
SELECT delete_state, COUNT(*) FROM tg_file_part_delete_state_tab
GROUP BY delete_state ORDER BY delete_state`)
	if err != nil {
		return fmt.Errorf("count block delete states: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return fmt.Errorf("scan block delete state count: %w", err)
		}
		report.BlockDeleteCountByState[state] = count
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate block delete state counts: %w", err)
	}
	return nil
}

func readPrivateSharingAudit(
	ctx context.Context,
	database *sql.DB,
	report *AuditReport,
	buckets []AuditBucket,
) error {
	privateBuckets := make([]string, 0)
	publicBuckets := make([]string, 0)
	for _, bucket := range buckets {
		switch bucket.ACL {
		case "private":
			privateBuckets = append(privateBuckets, bucket.Name)
		case "public-read":
			publicBuckets = append(publicBuckets, bucket.Name)
		}
	}
	if len(privateBuckets) != 0 && len(publicBuckets) != 0 {
		count, err := countSharedTopLevelFiles(ctx, database, privateBuckets, publicBuckets)
		if err != nil {
			return err
		}
		report.PrivateFileWithPublicMapping = count
	}
	if len(privateBuckets) != 0 {
		count, err := countSharedTopLevelFiles(ctx, database, privateBuckets, []string{"defaults"})
		if err != nil {
			return err
		}
		report.PrivateFileWithDefaultsMapping = count
	}
	return nil
}

func countSharedTopLevelFiles(
	ctx context.Context,
	database *sql.DB,
	leftRoots, rightRoots []string,
) (int64, error) {
	if len(leftRoots) == 0 || len(rightRoots) == 0 {
		return 0, nil
	}
	leftJSON, err := json.Marshal(leftRoots)
	if err != nil {
		return 0, fmt.Errorf("encode left audit roots: %w", err)
	}
	rightJSON, err := json.Marshal(rightRoots)
	if err != nil {
		return 0, fmt.Errorf("encode right audit roots: %w", err)
	}
	const query = `
WITH RECURSIVE paths(entry_id, ref_data, file_kind, top_name) AS (
    SELECT child.entry_id, child.ref_data, child.file_kind, child.file_name
    FROM tg_file_mapping_tab child
    JOIN tg_file_mapping_tab root ON root.entry_id = child.parent_entry_id
    WHERE root.parent_entry_id = 0 AND root.file_name = '/'
    UNION ALL
    SELECT child.entry_id, child.ref_data, child.file_kind, parent.top_name
    FROM tg_file_mapping_tab child
    JOIN paths parent ON child.parent_entry_id = parent.entry_id
),
left_files AS (
    SELECT DISTINCT ref_data FROM paths
    WHERE file_kind = 2 AND top_name IN (SELECT value FROM json_each(?))
),
right_files AS (
    SELECT DISTINCT ref_data FROM paths
    WHERE file_kind = 2 AND top_name IN (SELECT value FROM json_each(?))
)
SELECT COUNT(*) FROM left_files
JOIN right_files USING (ref_data)`
	var count int64
	if err := database.QueryRowContext(ctx, query, string(leftJSON), string(rightJSON)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count shared top-level files: %w", err)
	}
	return count, nil
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
