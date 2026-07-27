package backupmgr_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Test fixture uses the S3-compatible ETag algorithm.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
)

func TestExportVerifyAndImportRoundTrip(t *testing.T) {
	sourceDB, sourceFiles := newBackupTestStorage(t, 4)
	content := []byte("0123456789")
	fileID, err := sourceFiles.CreateFile(t.Context(), int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)
	_, err = sourceFiles.PublishS3Object(
		t.Context(),
		"/bucket/path/object.bin",
		fileID,
		int64(len(content)),
		&entity.S3ObjectMetadata{
			ETag: `"persisted-etag"`, ContentType: "application/custom",
			UserMetadata: `{"owner":"backup-test"}`,
		},
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, sourceFiles.CreateFileLink(
		t.Context(),
		"/shared/object.bin",
		fileID,
		int64(len(content)),
		false,
	))
	emptyID, err := sourceFiles.CreateFile(t.Context(), 0, bytes.NewReader(nil))
	require.NoError(t, err)
	require.NoError(t, sourceFiles.CreateFileLink(t.Context(), "/empty.bin", emptyID, 0, false))
	require.NoError(t, sourceFiles.PatchWebDAVProperties(
		t.Context(),
		"/shared/object.bin",
		[]filemgr.WebDAVPropertyPatch{{
			Set: true,
			Property: filemgr.WebDAVProperty{
				Name:     filemgr.WebDAVPropertyName{Namespace: "urn:test", LocalName: "label"},
				ValueXML: "<x>stable</x>",
			},
		}},
		filemgr.WebDAVMutationOptions{Principal: "tester"},
	))

	sourceWork := filepath.Join(t.TempDir(), "source-work")
	sourceManager := newBackupTestManager(t, sourceDB, sourceFiles, sourceWork)
	exportJob, err := sourceManager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "export-round-trip", Scope: "/",
	})
	require.NoError(t, err)
	exportJob, err = sourceManager.ProcessUntilTerminal(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	require.True(t, exportJob.ArtifactAvailable)
	artifact, _, err := sourceManager.Artifact(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	manifest, report, err := backupfmt.VerifyFile(
		t.Context(),
		artifact,
		backupfmt.DefaultLimits(),
		4,
	)
	require.NoError(t, err)
	require.Equal(t, int64(3), manifest.Limits.MappingCount)
	require.Equal(t, int64(2), manifest.Limits.FileCount)
	require.Equal(t, int64(3), manifest.Limits.PartCount)
	require.Equal(t, exportJob.ArtifactSHA256, report.ArtifactSHA256)

	targetDB, targetFiles := newBackupTestStorage(t, 4)
	targetWork := filepath.Join(t.TempDir(), "target-work")
	targetManager := newBackupTestManager(t, targetDB, targetFiles, targetWork)
	input, err := os.Open(artifact)
	require.NoError(t, err)
	defer input.Close()
	stat, err := input.Stat()
	require.NoError(t, err)
	importJob, err := targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "import-round-trip", ConflictPolicy: "fail",
		ContentLength: stat.Size(), ArtifactSHA256: fileSHA256(t, artifact), Body: input,
	})
	require.NoError(t, err)
	importJob, err = targetManager.ProcessUntilTerminal(t.Context(), importJob.JobID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", importJob.State)
	require.Equal(t, int64(3), importJob.Result.MappingsCreated)
	require.Equal(t, int64(2), importJob.Result.FilesCreated)

	first, err := targetFiles.StatFileLink(t.Context(), "/bucket/path/object.bin")
	require.NoError(t, err)
	second, err := targetFiles.StatFileLink(t.Context(), "/shared/object.bin")
	require.NoError(t, err)
	require.Equal(t, first.FileId, second.FileId)
	stream, err := targetFiles.OpenFile(t.Context(), first.FileId)
	require.NoError(t, err)
	restored, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, content, restored)
	info, err := targetFiles.StatS3Object(t.Context(), "/bucket/path/object.bin")
	require.NoError(t, err)
	require.Equal(t, `"persisted-etag"`, info.Metadata.ETag)
	require.Equal(t, "application/custom", info.Metadata.ContentType)
	properties, err := targetFiles.ReadWebDAVProperties(t.Context(), second.EntryID)
	require.NoError(t, err)
	require.Equal(t, []filemgr.WebDAVProperty{{
		Name:     filemgr.WebDAVPropertyName{Namespace: "urn:test", LocalName: "label"},
		ValueXML: "<x>stable</x>",
	}}, properties)
	require.Equal(t, 3, queryInt(t, targetDB, `
SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = ? AND file_part_size >= 0`, first.FileId))
	require.Equal(t, 3, queryInt(t, targetDB, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND delete_state = 'live'`, first.FileId))
	empty, err := targetFiles.StatFileLink(t.Context(), "/empty.bin")
	require.NoError(t, err)
	require.Zero(t, empty.FileSize)
	require.Equal(
		t,
		"d41d8cd98f00b204e9800998ecf8427e",
		queryString(t, targetDB, "SELECT json_extract(extinfo, '$.md5') FROM tg_file_tab WHERE file_id = ?", empty.FileId),
	)

	artifactRaw, err := os.ReadFile(artifact)
	require.NoError(t, err)
	filesBeforeConflict := queryInt(t, targetDB, "SELECT COUNT(*) FROM tg_file_tab")
	conflicting, err := targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "import-conflict", ConflictPolicy: "fail",
		ContentLength: int64(len(artifactRaw)), ArtifactSHA256: fileSHA256(t, artifact),
		Body: bytes.NewReader(artifactRaw),
	})
	require.NoError(t, err)
	conflicting, err = targetManager.ProcessUntilTerminal(t.Context(), conflicting.JobID)
	require.ErrorIs(t, err, backupmgr.ErrJobFailed)
	require.Equal(t, "path_conflict", conflicting.Error.Code)
	require.Equal(t, filesBeforeConflict, queryInt(t, targetDB, "SELECT COUNT(*) FROM tg_file_tab"))

	replacing, err := targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "import-replace", ConflictPolicy: "replace",
		ContentLength: int64(len(artifactRaw)), ArtifactSHA256: fileSHA256(t, artifact),
		Body: bytes.NewReader(artifactRaw),
	})
	require.NoError(t, err)
	replacing, err = targetManager.ProcessUntilTerminal(t.Context(), replacing.JobID)
	require.NoError(t, err)
	require.Equal(t, int64(3), replacing.Result.MappingsReplaced)
	replaced, err := targetFiles.StatFileLink(t.Context(), "/bucket/path/object.bin")
	require.NoError(t, err)
	require.NotEqual(t, first.FileId, replaced.FileId)
	require.Equal(t, 3, queryInt(t, targetDB, `
SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND delete_state = 'pending'`, first.FileId))
}

func TestExportLegacyPhysicalFileWithoutDeleteStateRoundTrips(t *testing.T) {
	sourceDB, sourceFiles := newBackupTestStorage(t, 4)
	content := []byte("legacy-data")
	fileID, err := sourceFiles.CreateFile(
		t.Context(),
		int64(len(content)),
		bytes.NewReader(content),
	)
	require.NoError(t, err)
	require.NoError(t, sourceFiles.CreateFileLink(
		t.Context(),
		"/bucket/legacy.bin",
		fileID,
		int64(len(content)),
		false,
	))
	_, err = sourceDB.ExecContext(
		t.Context(),
		"DELETE FROM tg_file_part_delete_state_tab WHERE file_id = ?",
		fileID,
	)
	require.NoError(t, err)
	_, err = sourceDB.ExecContext(
		t.Context(),
		"UPDATE tg_file_part_tab SET file_part_size = -1 WHERE file_id = ?",
		fileID,
	)
	require.NoError(t, err)

	sourceManager := newBackupTestManager(
		t,
		sourceDB,
		sourceFiles,
		filepath.Join(t.TempDir(), "source-work"),
	)
	exportJob, err := sourceManager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "legacy-export", Scope: "/bucket",
	})
	require.NoError(t, err)
	exportJob, err = sourceManager.ProcessUntilTerminal(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", exportJob.State)
	artifact, _, err := sourceManager.Artifact(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	manifest, _, err := backupfmt.VerifyFile(
		t.Context(),
		artifact,
		backupfmt.DefaultLimits(),
		sourceFiles.BackupMaxPartSize(),
	)
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), manifest.Limits.PhysicalBytes)
	require.Zero(t, queryInt(
		t,
		sourceDB,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE file_id = ?",
		fileID,
	))
	require.Equal(t, 3, queryInt(
		t,
		sourceDB,
		"SELECT COUNT(*) FROM tg_file_part_tab WHERE file_id = ? AND file_part_size >= 0",
		fileID,
	))

	targetDB, targetFiles := newBackupTestStorage(t, 4)
	targetManager := newBackupTestManager(
		t,
		targetDB,
		targetFiles,
		filepath.Join(t.TempDir(), "target-work"),
	)
	raw, err := os.ReadFile(artifact)
	require.NoError(t, err)
	importJob, err := targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner:          "operator",
		IdempotencyKey: "legacy-import",
		ConflictPolicy: "fail",
		ContentLength:  int64(len(raw)),
		ArtifactSHA256: fileSHA256(t, artifact),
		Body:           bytes.NewReader(raw),
	})
	require.NoError(t, err)
	importJob, err = targetManager.ProcessUntilTerminal(t.Context(), importJob.JobID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", importJob.State)
	restored, err := targetFiles.StatFileLink(t.Context(), "/bucket/legacy.bin")
	require.NoError(t, err)
	stream, err := targetFiles.OpenFile(t.Context(), restored.FileId)
	require.NoError(t, err)
	restoredContent, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, content, restoredContent)
}

func TestExportRejectsPartialOrInvalidManagedDeleteState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, database.IDatabase, uint64)
	}{
		{
			name: "partial delete state coverage",
			mutate: func(t *testing.T, databaseClient database.IDatabase, fileID uint64) {
				_, err := databaseClient.ExecContext(
					t.Context(),
					`DELETE FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND file_part_id = 2`,
					fileID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "non-live delete state",
			mutate: func(t *testing.T, databaseClient database.IDatabase, fileID uint64) {
				_, err := databaseClient.ExecContext(
					t.Context(),
					`UPDATE tg_file_part_delete_state_tab SET delete_state = 'pending'
WHERE file_id = ? AND file_part_id = 0`,
					fileID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "incomplete live delete reference",
			mutate: func(t *testing.T, databaseClient database.IDatabase, fileID uint64) {
				_, err := databaseClient.ExecContext(
					t.Context(),
					`UPDATE tg_file_part_delete_state_tab SET delete_ref = ''
WHERE file_id = ? AND file_part_id = 0`,
					fileID,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "invalid live upload timestamp",
			mutate: func(t *testing.T, databaseClient database.IDatabase, fileID uint64) {
				_, err := databaseClient.ExecContext(
					t.Context(),
					`UPDATE tg_file_part_delete_state_tab SET uploaded_at = 0
WHERE file_id = ? AND file_part_id = 0`,
					fileID,
				)
				require.NoError(t, err)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databaseClient, files := newBackupTestStorage(t, 4)
			content := []byte("managed-data")
			fileID, err := files.CreateFile(
				t.Context(),
				int64(len(content)),
				bytes.NewReader(content),
			)
			require.NoError(t, err)
			require.NoError(t, files.CreateFileLink(
				t.Context(),
				"/bucket/managed.bin",
				fileID,
				int64(len(content)),
				false,
			))
			test.mutate(t, databaseClient, fileID)
			manager := newBackupTestManager(
				t,
				databaseClient,
				files,
				filepath.Join(t.TempDir(), "work"),
			)
			job, err := manager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
				Owner: "operator", IdempotencyKey: test.name, Scope: "/bucket",
			})
			require.NoError(t, err)
			failed, err := manager.ProcessUntilTerminal(t.Context(), job.JobID)
			require.ErrorIs(t, err, backupmgr.ErrJobFailed)
			require.Equal(t, "failed", failed.State)
			require.Equal(t, "target_incompatible", failed.Error.Code)
			_, _, err = manager.Artifact(t.Context(), job.JobID)
			require.ErrorIs(t, err, backupmgr.ErrArtifactUnavailable)
		})
	}
}

func TestImportDryRunAndIdempotencyDoNotWriteBusinessData(t *testing.T) {
	sourceDB, sourceFiles := newBackupTestStorage(t, 8)
	fileID, err := sourceFiles.CreateFile(t.Context(), 3, bytes.NewBufferString("abc"))
	require.NoError(t, err)
	require.NoError(t, sourceFiles.CreateFileLink(t.Context(), "/data.bin", fileID, 3, false))
	sourceManager := newBackupTestManager(t, sourceDB, sourceFiles, filepath.Join(t.TempDir(), "source"))
	exportJob, err := sourceManager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "reader", IdempotencyKey: "dry-export", Scope: "/",
	})
	require.NoError(t, err)
	_, err = sourceManager.ProcessUntilTerminal(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	artifact, _, err := sourceManager.Artifact(t.Context(), exportJob.JobID)
	require.NoError(t, err)

	targetDB, targetFiles := newBackupTestStorage(t, 8)
	targetManager := newBackupTestManager(t, targetDB, targetFiles, filepath.Join(t.TempDir(), "target"))
	raw, err := os.ReadFile(artifact)
	require.NoError(t, err)
	request := backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "dry-import", ConflictPolicy: "fail", DryRun: true,
		ContentLength: int64(len(raw)), ArtifactSHA256: fileSHA256(t, artifact),
		Body: bytes.NewReader(raw),
	}
	first, err := targetManager.CreateImport(t.Context(), request)
	require.NoError(t, err)
	secondRequest := request
	secondRequest.Body = bytes.NewReader(raw)
	second, err := targetManager.CreateImport(t.Context(), secondRequest)
	require.NoError(t, err)
	require.Equal(t, first.JobID, second.JobID)
	_, err = targetManager.ProcessUntilTerminal(t.Context(), first.JobID)
	require.NoError(t, err)
	require.Zero(t, queryInt(t, targetDB, "SELECT COUNT(*) FROM tg_file_tab"))
	require.Zero(t, queryInt(t, targetDB, "SELECT COUNT(*) FROM tg_file_mapping_tab"))
}

func TestCreateImportUploadComputesChecksumAndDirectImportStillVerifiesIt(t *testing.T) {
	sourceDB, sourceFiles := newBackupTestStorage(t, 4)
	fileID, err := sourceFiles.CreateFile(t.Context(), 3, bytes.NewBufferString("abc"))
	require.NoError(t, err)
	require.NoError(t, sourceFiles.CreateFileLink(t.Context(), "/data.bin", fileID, 3, false))
	sourceManager := newBackupTestManager(
		t,
		sourceDB,
		sourceFiles,
		filepath.Join(t.TempDir(), "source"),
	)
	exportJob, err := sourceManager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "server-hash-export", Scope: "/",
	})
	require.NoError(t, err)
	_, err = sourceManager.ProcessUntilTerminal(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	artifact, _, err := sourceManager.Artifact(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	raw, err := os.ReadFile(artifact)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	expected := hex.EncodeToString(sum[:])

	targetDB, targetFiles := newBackupTestStorage(t, 4)
	targetManager := newBackupTestManager(
		t,
		targetDB,
		targetFiles,
		filepath.Join(t.TempDir(), "target"),
	)
	importJob, err := targetManager.CreateImportUpload(
		t.Context(),
		backupmgr.CreateImportUploadRequest{
			Owner: "operator", IdempotencyKey: "server-hash-import",
			ConflictPolicy: "fail", DryRun: true,
			ContentLength: int64(len(raw)), Body: bytes.NewReader(raw),
		},
	)
	require.NoError(t, err)
	require.Equal(t, expected, importJob.ArtifactSHA256)
	importJob, err = targetManager.ProcessUntilTerminal(t.Context(), importJob.JobID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", importJob.State)
	require.Zero(t, queryInt(t, targetDB, "SELECT COUNT(*) FROM tg_file_tab"))

	_, err = targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "bad-client-hash",
		ConflictPolicy: "fail", DryRun: true,
		ContentLength:  int64(len(raw)),
		ArtifactSHA256: strings.Repeat("0", sha256.Size*2),
		Body:           bytes.NewReader(raw),
	})
	require.ErrorIs(t, err, backupfmt.ErrChecksum)
}

func TestListJobsUsesOwnerFiltersAndKeysetPagination(t *testing.T) {
	databaseClient, files := newBackupTestStorage(t, 8)
	manager := newBackupTestManager(
		t,
		databaseClient,
		files,
		filepath.Join(t.TempDir(), "work"),
	)
	for index, owner := range []string{"reader", "operator", "reader", "operator", "reader"} {
		_, err := manager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
			Owner: owner, IdempotencyKey: fmt.Sprintf("list-export-%d", index), Scope: "/",
		})
		require.NoError(t, err)
	}

	var (
		cursor *backupmgr.JobCursor
		all    []*backupmgr.Job
	)
	for {
		page, err := manager.ListJobs(t.Context(), backupmgr.ListJobsRequest{
			Cursor: cursor, Limit: 2,
		})
		require.NoError(t, err)
		all = append(all, page.Jobs...)
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
	}
	require.Len(t, all, 5)
	seen := make(map[string]struct{}, len(all))
	for _, job := range all {
		_, duplicate := seen[job.JobID]
		require.False(t, duplicate)
		seen[job.JobID] = struct{}{}
	}
	require.True(t, sort.SliceIsSorted(all, func(left, right int) bool {
		if all[left].CreatedAt != all[right].CreatedAt {
			return all[left].CreatedAt > all[right].CreatedAt
		}
		return all[left].JobID > all[right].JobID
	}))

	reader, err := manager.ListJobs(t.Context(), backupmgr.ListJobsRequest{
		Owner: "reader", Kind: "export", State: "queued", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, reader.Jobs, 3)
	for _, job := range reader.Jobs {
		require.Equal(t, "reader", job.Owner)
		require.Equal(t, "export", job.Kind)
		require.Equal(t, "queued", job.State)
	}

	_, err = manager.ListJobs(t.Context(), backupmgr.ListJobsRequest{Limit: 201})
	require.ErrorIs(t, err, backupmgr.ErrInvalidRequest)
	_, err = manager.ListJobs(t.Context(), backupmgr.ListJobsRequest{
		State: "unknown", Limit: 1,
	})
	require.ErrorIs(t, err, backupmgr.ErrInvalidRequest)
}

func TestInvalidImportWritesSafeReportAndRemovesReceivedArtifact(t *testing.T) {
	databaseClient, files := newBackupTestStorage(t, 8)
	workDir := filepath.Join(t.TempDir(), "work")
	manager := newBackupTestManager(t, databaseClient, files, workDir)
	raw := []byte("not a tgfile archive")
	sum := sha256.Sum256(raw)
	job, err := manager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner:          "operator",
		IdempotencyKey: "invalid-import",
		ConflictPolicy: "fail",
		ContentLength:  int64(len(raw)),
		ArtifactSHA256: hex.EncodeToString(sum[:]),
		Body:           &terminalEOFReader{raw: raw},
	})
	require.NoError(t, err)
	failed, err := manager.ProcessUntilTerminal(t.Context(), job.JobID)
	require.ErrorIs(t, err, backupmgr.ErrJobFailed)
	require.Equal(t, "failed", failed.State)
	require.Equal(t, "invalid_archive", failed.Error.Code)
	require.Zero(t, queryInt(t, databaseClient, "SELECT COUNT(*) FROM tg_file_tab"))

	reportName := queryString(
		t,
		databaseClient,
		"SELECT report_path FROM tg_backup_job_tab WHERE job_id = ?",
		job.JobID,
	)
	require.Equal(t, job.JobID+".report.json", reportName)
	reportInfo, err := os.Stat(filepath.Join(workDir, reportName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), reportInfo.Mode().Perm())
	require.Empty(t, queryString(
		t,
		databaseClient,
		"SELECT artifact_path FROM tg_backup_job_tab WHERE job_id = ?",
		job.JobID,
	))
}

type terminalEOFReader struct {
	raw    []byte
	offset int
}

func (r *terminalEOFReader) Read(output []byte) (int, error) {
	if r.offset == len(r.raw) {
		return 0, io.EOF
	}
	count := copy(output, r.raw[r.offset:])
	r.offset += count
	if r.offset == len(r.raw) {
		return count, io.EOF
	}
	return count, nil
}

func TestImportDoesNotBecomeTerminalBeforeDurableCompensation(t *testing.T) {
	databaseClient, files := newBackupTestStorage(t, 8)
	compensationErr := errors.New("injected discard failure")
	manager, err := backupmgr.New(
		databaseClient,
		&discardFailureFileManager{
			IFileManager: files,
			err:          compensationErr,
		},
		backupmgr.Options{
			WorkDir: filepath.Join(t.TempDir(), "work"),
			Limits:  backupfmt.DefaultLimits(), SchemaVersion: 13,
			MaxPartSize:       files.BackupMaxPartSize(),
			ArtifactRetention: time.Hour, JobRetention: 24 * time.Hour,
		},
	)
	require.NoError(t, err)
	raw := []byte("not a tgfile archive")
	sum := sha256.Sum256(raw)
	job, err := manager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner:          "operator",
		IdempotencyKey: "compensation-failure",
		ConflictPolicy: "fail",
		ContentLength:  int64(len(raw)),
		ArtifactSHA256: hex.EncodeToString(sum[:]),
		Body:           bytes.NewReader(raw),
	})
	require.NoError(t, err)

	_, err = manager.ProcessUntilTerminal(t.Context(), job.JobID)

	require.ErrorIs(t, err, compensationErr)
	pending, err := manager.GetJob(t.Context(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, "validating", pending.State)
	require.Zero(t, pending.CompletedAt)
}

type discardFailureFileManager struct {
	filemgr.IFileManager
	err error
}

func (m *discardFailureFileManager) DiscardBackupImport(context.Context, string) error {
	return m.err
}

func TestCanceledExportReleasesPinsAndPartialFiles(t *testing.T) {
	databaseClient, files := newBackupTestStorage(t, 8)
	fileID, err := files.CreateFile(t.Context(), 3, bytes.NewBufferString("abc"))
	require.NoError(t, err)
	require.NoError(t, files.CreateFileLink(t.Context(), "/data.bin", fileID, 3, false))
	workDir := filepath.Join(t.TempDir(), "work")
	manager := newBackupTestManager(t, databaseClient, files, workDir)
	job, err := manager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "cancel-export", Scope: "/",
	})
	require.NoError(t, err)
	requested, err := manager.Cancel(t.Context(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, "canceling", requested.State)
	canceled, err := manager.ProcessUntilTerminal(t.Context(), job.JobID)
	require.ErrorIs(t, err, backupmgr.ErrJobFailed)
	require.Equal(t, "canceled", canceled.State)
	require.Zero(t, queryInt(
		t,
		databaseClient,
		"SELECT COUNT(*) FROM tg_backup_export_pin_tab WHERE job_id = ?",
		job.JobID,
	))
	entries, err := os.ReadDir(workDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestExportEmptyNestedDirectoryIncludesParentChain(t *testing.T) {
	databaseClient, files := newBackupTestStorage(t, 8)
	require.NoError(t, files.CreateFileLink(t.Context(), "/parent", 0, 0, true))
	require.NoError(t, files.CreateFileLink(t.Context(), "/parent/empty", 0, 0, true))
	manager := newBackupTestManager(
		t,
		databaseClient,
		files,
		filepath.Join(t.TempDir(), "work"),
	)
	job, err := manager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "empty-directory", Scope: "/parent/empty",
	})
	require.NoError(t, err)
	_, err = manager.ProcessUntilTerminal(t.Context(), job.JobID)
	require.NoError(t, err)
	artifact, _, err := manager.Artifact(t.Context(), job.JobID)
	require.NoError(t, err)
	manifest, _, err := backupfmt.VerifyFile(
		t.Context(),
		artifact,
		backupfmt.DefaultLimits(),
		files.BackupMaxPartSize(),
	)
	require.NoError(t, err)
	require.Len(t, manifest.Directories, 2)
	require.Equal(t, "/parent", manifest.Directories[0].Path)
	require.Equal(t, "/parent/empty", manifest.Directories[1].Path)
	require.Equal(t, uint32(0o755), manifest.Directories[0].Mode)
	require.Equal(t, uint32(0o755), manifest.Directories[1].Mode)
	require.Empty(t, manifest.Files)
	require.Empty(t, manifest.Mappings)
}

func TestCompositeBackupRoundTripPreservesManifestAndChecksum(t *testing.T) {
	const blockSize = int64(2 * 1024 * 1024)
	sourceDB, sourceFiles := newBackupTestStorage(t, blockSize)
	firstContent := bytes.Repeat([]byte("a"), 5*1024*1024)
	secondContent := []byte("tail")
	upload, err := sourceFiles.CreateMultipartUpload(t.Context(), &filemgr.CreateMultipartRequest{
		Bucket:      "bucket",
		Key:         "multipart.bin",
		Metadata:    &entity.S3ObjectMetadata{UserMetadata: "{}"},
		ExpireAfter: time.Hour,
	})
	require.NoError(t, err)
	first := createBackupMultipartPart(t, sourceFiles, upload, 1, firstContent)
	second := createBackupMultipartPart(t, sourceFiles, upload, 2, secondContent)
	allContent := append(bytes.Clone(firstContent), secondContent...)
	fullHash, err := s3checksum.NewHash(upload.Algorithm)
	require.NoError(t, err)
	_, err = fullHash.Write(allContent)
	require.NoError(t, err)
	finalChecksum := s3checksum.SumBase64(fullHash)
	expectedSize := int64(len(allContent))
	completed, err := sourceFiles.CompleteMultipartUpload(t.Context(), &filemgr.CompleteMultipartRequest{
		UploadID: upload.UploadID,
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		Parts: []filemgr.CompleteMultipartPart{
			{
				PartNumber:        1,
				ETag:              first.ETag,
				ChecksumAlgorithm: upload.Algorithm,
				ChecksumValue:     first.ChecksumValue,
			},
			{
				PartNumber:        2,
				ETag:              second.ETag,
				ChecksumAlgorithm: upload.Algorithm,
				ChecksumValue:     second.ChecksumValue,
			},
		},
		FinalChecksumAlgorithm: upload.Algorithm,
		FinalChecksum:          finalChecksum,
		ChecksumType:           upload.ChecksumType,
		ExpectedSize:           &expectedSize,
	})
	require.NoError(t, err)

	sourceManager := newBackupTestManager(
		t,
		sourceDB,
		sourceFiles,
		filepath.Join(t.TempDir(), "source"),
	)
	exportJob, err := sourceManager.CreateExport(t.Context(), backupmgr.CreateExportRequest{
		Owner: "operator", IdempotencyKey: "composite-export", Scope: "/",
	})
	require.NoError(t, err)
	_, err = sourceManager.ProcessUntilTerminal(t.Context(), exportJob.JobID)
	require.NoError(t, err)
	artifact, _, err := sourceManager.Artifact(t.Context(), exportJob.JobID)
	require.NoError(t, err)

	targetDB, targetFiles := newBackupTestStorage(t, blockSize)
	targetManager := newBackupTestManager(
		t,
		targetDB,
		targetFiles,
		filepath.Join(t.TempDir(), "target"),
	)
	raw, err := os.ReadFile(artifact)
	require.NoError(t, err)
	importJob, err := targetManager.CreateImport(t.Context(), backupmgr.CreateImportRequest{
		Owner: "operator", IdempotencyKey: "composite-import", ConflictPolicy: "fail",
		ContentLength: int64(len(raw)), ArtifactSHA256: fileSHA256(t, artifact),
		Body: bytes.NewReader(raw),
	})
	require.NoError(t, err)
	_, err = targetManager.ProcessUntilTerminal(t.Context(), importJob.JobID)
	require.NoError(t, err)

	restoredObject, err := targetFiles.StatS3Object(t.Context(), "/bucket/multipart.bin")
	require.NoError(t, err)
	restoredFile, err := targetFiles.StatFile(t.Context(), restoredObject.Link.FileId)
	require.NoError(t, err)
	require.Equal(t, int32(2), restoredFile.LayoutVersion)
	require.Equal(t, completed.ETag, restoredObject.Metadata.ETag)
	require.Equal(t, finalChecksum, restoredObject.Metadata.RequestChecksumValue)
	require.Equal(t, string(upload.ChecksumType), restoredObject.Metadata.ChecksumType)
	require.Equal(t, 2, queryInt(t, targetDB, `
SELECT COUNT(*) FROM tg_s3_file_segment_tab WHERE file_id = ?`, restoredObject.Link.FileId))
	require.Equal(t, 2, queryInt(t, targetDB, `
SELECT COUNT(*) FROM tg_s3_completed_part_tab WHERE file_id = ?`, restoredObject.Link.FileId))
	reader, err := targetFiles.OpenFile(t.Context(), restoredObject.Link.FileId)
	require.NoError(t, err)
	restored, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, allContent, restored)
}

func createBackupMultipartPart(
	t *testing.T,
	files filemgr.IFileManager,
	upload *filemgr.MultipartUpload,
	partNumber int,
	content []byte,
) *filemgr.MultipartPart {
	t.Helper()
	fileID, err := files.CreateFile(t.Context(), int64(len(content)), bytes.NewReader(content))
	require.NoError(t, err)
	etagSum := md5.Sum(content) //nolint:gosec // Test fixture uses the S3-compatible ETag algorithm.
	hash, err := s3checksum.NewHash(upload.Algorithm)
	require.NoError(t, err)
	_, err = hash.Write(content)
	require.NoError(t, err)
	part, err := files.PutMultipartPart(t.Context(), &filemgr.PutMultipartPartRequest{
		UploadID: upload.UploadID, Bucket: upload.Bucket, Key: upload.Key,
		PartNumber: partNumber, FileID: fileID, Size: int64(len(content)),
		ETag:          hex.EncodeToString(etagSum[:]),
		ChecksumValue: s3checksum.SumBase64(hash), MaxObjectSize: 10 * 1024 * 1024,
	})
	require.NoError(t, err)
	return part
}

func newBackupTestStorage(
	t *testing.T,
	blockSize int64,
) (database.IDatabase, filemgr.IFileManager) {
	t.Helper()
	databaseClient, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, databaseClient.Close()) })
	block, err := mem.New(blockSize)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true, DisableL2Cache: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, cache.Close(closeContext))
	})
	return databaseClient, filemgr.NewFileManager(databaseClient, block, cache)
}

func newBackupTestManager(
	t *testing.T,
	databaseClient database.IDatabase,
	files filemgr.IFileManager,
	workDir string,
) *backupmgr.Manager {
	t.Helper()
	manager, err := backupmgr.New(databaseClient, files, backupmgr.Options{
		WorkDir: workDir, Limits: backupfmt.DefaultLimits(),
		RequiredBuckets: []backupfmt.RequiredBucket{{Name: "bucket", ACL: "private"}},
		SchemaVersion:   13, MaxPartSize: files.BackupMaxPartSize(),
		ArtifactRetention: time.Hour, JobRetention: 24 * time.Hour,
	})
	require.NoError(t, err)
	return manager
}

func fileSHA256(t *testing.T, filename string) string {
	t.Helper()
	raw, err := os.ReadFile(filename)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func queryInt(
	t *testing.T,
	databaseClient database.IDatabase,
	query string,
	args ...any,
) int {
	t.Helper()
	rows, err := databaseClient.QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var result int
	require.NoError(t, rows.Scan(&result))
	return result
}

func queryString(
	t *testing.T,
	databaseClient database.IDatabase,
	query string,
	args ...any,
) string {
	t.Helper()
	rows, err := databaseClient.QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var result string
	require.NoError(t, rows.Scan(&result))
	return result
}
