package backupmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xxxsen/common/database"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/filemgr"
)

var (
	ErrInvalidRequest      = errors.New("invalid backup request")
	ErrIdempotencyConflict = errors.New("backup idempotency key has different parameters")
	ErrJobNotFound         = errors.New("backup job does not exist")
	ErrJobNotCancelable    = errors.New("backup job cannot be canceled")
	ErrArtifactUnavailable = errors.New("backup artifact is unavailable")
	ErrJobFailed           = errors.New("backup job failed")
	errJobCanceled         = errors.New("backup job was canceled")
	errJobClaimed          = errors.New("backup job is already being processed")
	errSourceUnreadable    = errors.New("backup source is unreadable")
)

const (
	pollInterval       = 250 * time.Millisecond
	cleanupInterval    = time.Minute
	spaceCheckInterval = 30 * time.Second
	spaceCheckBytes    = int64(1024 * 1024 * 1024)
)

type Options struct {
	WorkDir           string
	Limits            backupfmt.Limits
	RequiredBuckets   []backupfmt.RequiredBucket
	SchemaVersion     int
	MaxPartSize       int64
	ArtifactRetention time.Duration
	JobRetention      time.Duration
}

type Manager struct {
	db              database.IDatabase
	files           filemgr.IFileManager
	options         Options
	executionErrors sync.Map
}

type CreateExportRequest struct {
	Owner          string
	IdempotencyKey string
	Scope          string
}

type CreateImportRequest struct {
	Owner          string
	IdempotencyKey string
	ConflictPolicy string
	DryRun         bool
	ContentLength  int64
	ArtifactSHA256 string
	Body           io.Reader
}

type CreateImportUploadRequest struct {
	Owner          string
	IdempotencyKey string
	ConflictPolicy string
	DryRun         bool
	ContentLength  int64
	Body           io.Reader
}

type JobCursor struct {
	CreatedAt int64
	JobID     string
}

type ListJobsRequest struct {
	Owner  string
	Kind   string
	State  string
	Cursor *JobCursor
	Limit  int
}

type JobPage struct {
	Jobs       []*Job
	NextCursor *JobCursor
}

type Progress struct {
	FilesTotal     int64 `json:"files_total"`
	FilesCompleted int64 `json:"files_completed"`
	PartsTotal     int64 `json:"parts_total"`
	PartsCompleted int64 `json:"parts_completed"`
	BytesTotal     int64 `json:"bytes_total"`
	BytesCompleted int64 `json:"bytes_completed"`
}

type Result struct {
	MappingsCreated  int64 `json:"mappings_created"`
	MappingsReplaced int64 `json:"mappings_replaced"`
	FilesCreated     int64 `json:"files_created"`
}

type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Job struct {
	JobID             string   `json:"job_id"`
	Kind              string   `json:"kind"`
	Owner             string   `json:"-"`
	State             string   `json:"state"`
	DryRun            bool     `json:"dry_run"`
	Conflict          string   `json:"conflict"`
	Scope             string   `json:"scope"`
	ArtifactSHA256    string   `json:"artifact_sha256"`
	ArtifactAvailable bool     `json:"artifact_available"`
	Progress          Progress `json:"progress"`
	Result            Result   `json:"result"`
	Error             JobError `json:"error"`
	CreatedAt         int64    `json:"created_at"`
	UpdatedAt         int64    `json:"updated_at"`
	CompletedAt       int64    `json:"completed_at"`
	artifactPath      string
	fingerprint       string
	cancelRequested   bool
	artifactExpiresAt int64
}

type failureReport struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	FailedAt  int64  `json:"failed_at"`
	Retryable bool   `json:"retryable"`
}

func New(
	db database.IDatabase,
	files filemgr.IFileManager,
	options Options,
) (*Manager, error) {
	if db == nil || files == nil || !filepath.IsAbs(options.WorkDir) {
		return nil, ErrInvalidRequest
	}
	if err := os.MkdirAll(options.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("create backup work directory: %w", err)
	}
	if err := os.Chmod(options.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure backup work directory: %w", err)
	}
	return &Manager{db: db, files: files, options: options}, nil
}

func (m *Manager) CreateExport(
	ctx context.Context,
	request CreateExportRequest,
) (*Job, error) {
	scope := path.Clean(request.Scope)
	if request.Scope == "" {
		scope = "/"
	}
	if request.Owner == "" || !validIdempotencyKey(request.IdempotencyKey) ||
		!validScope(scope, m.options.Limits.MaxPathBytes) ||
		scope != request.Scope && request.Scope != "" {
		return nil, ErrInvalidRequest
	}
	fingerprint := requestFingerprint("export", scope)
	return m.createQueuedJob(ctx, "export", request.Owner, request.IdempotencyKey, fingerprint, scope, false, "fail")
}

func (m *Manager) CreateImport(
	ctx context.Context,
	request CreateImportRequest,
) (*Job, error) {
	if !validSHA256(request.ArtifactSHA256) {
		return nil, ErrInvalidRequest
	}
	return m.createImport(ctx, importReceiveRequest{
		Owner:          request.Owner,
		IdempotencyKey: request.IdempotencyKey,
		ConflictPolicy: request.ConflictPolicy,
		DryRun:         request.DryRun,
		ContentLength:  request.ContentLength,
		ExpectedSHA256: request.ArtifactSHA256,
		Body:           request.Body,
	})
}

func (m *Manager) CreateImportUpload(
	ctx context.Context,
	request CreateImportUploadRequest,
) (*Job, error) {
	return m.createImport(ctx, importReceiveRequest{
		Owner:          request.Owner,
		IdempotencyKey: request.IdempotencyKey,
		ConflictPolicy: request.ConflictPolicy,
		DryRun:         request.DryRun,
		ContentLength:  request.ContentLength,
		Body:           request.Body,
	})
}

type importReceiveRequest struct {
	Owner          string
	IdempotencyKey string
	ConflictPolicy string
	DryRun         bool
	ContentLength  int64
	ExpectedSHA256 string
	Body           io.Reader
}

func (m *Manager) createImport(
	ctx context.Context,
	request importReceiveRequest,
) (*Job, error) {
	fingerprint, err := m.prepareImportRequest(&request)
	if err != nil {
		return nil, err
	}
	job, created, err := m.insertJob(
		ctx,
		"import",
		request.Owner,
		request.IdempotencyKey,
		fingerprint,
		"/",
		request.DryRun,
		request.ConflictPolicy,
		"receiving",
	)
	if err != nil || !created {
		return job, err
	}
	if err := m.receiveAndQueueImport(ctx, job, request); err != nil {
		return nil, err
	}
	return m.GetJob(ctx, job.JobID)
}

func (m *Manager) prepareImportRequest(request *importReceiveRequest) (string, error) {
	if request.ConflictPolicy == "" {
		request.ConflictPolicy = "fail"
	}
	if request.ContentLength > m.options.Limits.MaxArchiveBytes {
		return "", backupfmt.ErrLimitExceeded
	}
	if request.Owner == "" || !validIdempotencyKey(request.IdempotencyKey) {
		return "", ErrInvalidRequest
	}
	if request.ConflictPolicy != "fail" && request.ConflictPolicy != "replace" {
		return "", ErrInvalidRequest
	}
	if request.ContentLength < 1 || request.Body == nil {
		return "", ErrInvalidRequest
	}
	if err := ensureFreeSpace(m.options.WorkDir, request.ContentLength); err != nil {
		return "", err
	}
	fingerprintValues := []string{
		"import",
		request.ConflictPolicy,
		strconv.FormatBool(request.DryRun),
		strconv.FormatInt(request.ContentLength, 10),
	}
	if request.ExpectedSHA256 != "" {
		fingerprintValues = append(fingerprintValues, request.ExpectedSHA256)
	}
	return requestFingerprint(fingerprintValues...), nil
}

func (m *Manager) receiveAndQueueImport(
	ctx context.Context,
	job *Job,
	request importReceiveRequest,
) error {
	partialName := job.JobID + ".receive.partial"
	artifactName := job.JobID + ".tgfb"
	partialPath := filepath.Join(m.options.WorkDir, partialName)
	artifactPath := filepath.Join(m.options.WorkDir, artifactName)
	artifactSHA256, err := receiveArtifact(
		ctx,
		partialPath,
		artifactPath,
		request.ContentLength,
		request.Body,
		request.ExpectedSHA256,
	)
	if err != nil {
		_ = os.Remove(partialPath)
		_ = os.Remove(artifactPath)
		_ = m.finishJob(context.WithoutCancel(ctx), job.JobID, "canceled", "canceled", "artifact receive failed")
		return err
	}
	now := time.Now().UnixMilli()
	result, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET job_state = 'queued', artifact_path = ?, artifact_size = ?,
artifact_sha256 = ?, updated_at = ?
WHERE job_id = ? AND job_state = 'receiving'`,
		artifactName,
		request.ContentLength,
		artifactSHA256,
		now,
		job.JobID,
	)
	if err != nil {
		_ = os.Remove(artifactPath)
		_ = m.finishJob(
			context.WithoutCancel(ctx),
			job.JobID,
			"canceled",
			"canceled",
			"artifact queue failed",
		)
		return fmt.Errorf("queue received backup import: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		_ = os.Remove(artifactPath)
		_ = m.finishJob(
			context.WithoutCancel(ctx),
			job.JobID,
			"canceled",
			"canceled",
			"artifact queue state changed",
		)
		return fmt.Errorf("queue received backup import: %w", errJobClaimed)
	}
	return nil
}

func (m *Manager) createQueuedJob(
	ctx context.Context,
	kind, owner, idempotencyKey, fingerprint, scope string,
	dryRun bool,
	conflict string,
) (*Job, error) {
	job, _, err := m.insertJob(
		ctx,
		kind,
		owner,
		idempotencyKey,
		fingerprint,
		scope,
		dryRun,
		conflict,
		"queued",
	)
	return job, err
}

func (m *Manager) insertJob(
	ctx context.Context,
	kind, owner, idempotencyKey, fingerprint, scope string,
	dryRun bool,
	conflict, state string,
) (*Job, bool, error) {
	jobID, err := randomJobID()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UnixMilli()
	_, insertErr := m.db.ExecContext(
		ctx,
		`INSERT INTO tg_backup_job_tab (
job_id, job_kind, owner, job_state, scope, dry_run, conflict_policy,
idempotency_key, request_fingerprint, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID,
		kind,
		owner,
		state,
		scope,
		boolInt(dryRun),
		conflict,
		idempotencyKey,
		fingerprint,
		now,
		now,
	)
	if insertErr == nil {
		job, err := m.GetJob(ctx, jobID)
		return job, true, err
	}
	existing, err := m.getJobByIdempotency(ctx, owner, kind, idempotencyKey)
	if err != nil {
		return nil, false, fmt.Errorf("create backup job: %w", insertErr)
	}
	if existing.fingerprint != fingerprint {
		return nil, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

func receiveArtifact(
	ctx context.Context,
	partialPath, artifactPath string,
	contentLength int64,
	body io.Reader,
	expectedSHA256 string,
) (string, error) {
	file, err := os.OpenFile(partialPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create import partial artifact: %w", err)
	}
	hash := sha256.New()
	spaceChecked := &spaceCheckingReader{
		reader:    body,
		workDir:   filepath.Dir(partialPath),
		total:     contentLength,
		lastCheck: time.Now(),
	}
	counted := &contextReader{ctx: ctx, reader: spaceChecked}
	written, copyErr := io.CopyN(io.MultiWriter(file, hash), counted, contentLength)
	var extra [1]byte
	extraCount, extraErr := counted.Read(extra[:])
	if extraErr != nil && !errors.Is(extraErr, io.EOF) {
		copyErr = errors.Join(copyErr, extraErr)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	syncErr := file.Sync()
	closeErr := file.Close()
	var checksumErr error
	if expectedSHA256 != "" &&
		subtle.ConstantTimeCompare([]byte(sum), []byte(expectedSHA256)) != 1 {
		checksumErr = backupfmt.ErrChecksum
	}
	receiveErr := errors.Join(copyErr, syncErr, closeErr, checksumErr)
	if written != contentLength || extraCount != 0 {
		receiveErr = errors.Join(receiveErr, backupfmt.ErrChecksum)
	}
	if receiveErr != nil {
		return "", fmt.Errorf(
			"receive import artifact: %w",
			receiveErr,
		)
	}
	if err := os.Rename(partialPath, artifactPath); err != nil {
		return "", fmt.Errorf("publish received import artifact: %w", err)
	}
	if err := syncDirectory(filepath.Dir(artifactPath)); err != nil {
		return "", err
	}
	return sum, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read import request context: %w", err)
	}
	count, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read import request body: %w", err)
	}
	return count, nil
}

const jobSelectColumns = `job_id, job_kind, owner, job_state, dry_run,
conflict_policy, scope, artifact_sha256, artifact_path, artifact_expires_at,
files_total, files_completed, parts_total, parts_completed, bytes_total,
bytes_completed, mappings_created, mappings_replaced, files_created,
error_code, error_message, cancel_requested, created_at, updated_at, completed_at,
request_fingerprint`

func (m *Manager) GetJob(ctx context.Context, jobID string) (*Job, error) {
	if !validJobID(jobID) {
		return nil, ErrJobNotFound
	}
	query := `SELECT ` + jobSelectColumns + `
FROM tg_backup_job_tab WHERE job_id = ?`
	job, err := scanJob(queryJobRow(ctx, m.db, query, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read backup job: %w", err)
	}
	m.setArtifactAvailability(job)
	return job, nil
}

func scanJob(scanner rowScanner) (*Job, error) {
	var job Job
	var dryRun, cancelRequested int
	err := scanner.Scan(
		&job.JobID,
		&job.Kind,
		&job.Owner,
		&job.State,
		&dryRun,
		&job.Conflict,
		&job.Scope,
		&job.ArtifactSHA256,
		&job.artifactPath,
		&job.artifactExpiresAt,
		&job.Progress.FilesTotal,
		&job.Progress.FilesCompleted,
		&job.Progress.PartsTotal,
		&job.Progress.PartsCompleted,
		&job.Progress.BytesTotal,
		&job.Progress.BytesCompleted,
		&job.Result.MappingsCreated,
		&job.Result.MappingsReplaced,
		&job.Result.FilesCreated,
		&job.Error.Code,
		&job.Error.Message,
		&cancelRequested,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.CompletedAt,
		&job.fingerprint,
	)
	if err != nil {
		return nil, fmt.Errorf("scan backup job: %w", err)
	}
	job.DryRun = dryRun != 0
	job.cancelRequested = cancelRequested != 0
	return &job, nil
}

func (m *Manager) setArtifactAvailability(job *Job) {
	if job.State == "succeeded" && job.Kind == "export" && job.artifactPath != "" &&
		filepath.Base(job.artifactPath) == job.artifactPath &&
		(job.artifactExpiresAt == 0 || job.artifactExpiresAt > time.Now().UnixMilli()) {
		_, err := os.Stat(filepath.Join(m.options.WorkDir, job.artifactPath))
		job.ArtifactAvailable = err == nil
	}
}

func (m *Manager) ListJobs(
	ctx context.Context,
	request ListJobsRequest,
) (*JobPage, error) {
	if !validListJobsRequest(request) {
		return nil, ErrInvalidRequest
	}
	cursorCreatedAt := int64(0)
	cursorJobID := ""
	if request.Cursor != nil {
		cursorCreatedAt = request.Cursor.CreatedAt
		cursorJobID = request.Cursor.JobID
	}
	rows, err := m.queryJobPage(
		ctx,
		request,
		cursorCreatedAt,
		cursorJobID,
		request.Limit+1,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobs := make([]*Job, 0, request.Limit+1)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup job page: %w", err)
		}
		m.setArtifactAvailability(job)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backup job page: %w", err)
	}
	page := &JobPage{}
	if len(jobs) > request.Limit {
		jobs = jobs[:request.Limit]
		last := jobs[len(jobs)-1]
		page.NextCursor = &JobCursor{CreatedAt: last.CreatedAt, JobID: last.JobID}
	}
	page.Jobs = jobs
	return page, nil
}

func validListJobsRequest(request ListJobsRequest) bool {
	if request.Limit < 1 || request.Limit > 200 {
		return false
	}
	if request.Owner != "" && !validOwner(request.Owner) {
		return false
	}
	if request.Kind != "" && request.Kind != "export" && request.Kind != "import" {
		return false
	}
	if request.State != "" && !validJobState(request.State) {
		return false
	}
	return request.Cursor == nil ||
		request.Cursor.CreatedAt >= 1 && validJobID(request.Cursor.JobID)
}

func (m *Manager) queryJobPage(
	ctx context.Context,
	request ListJobsRequest,
	cursorCreatedAt int64,
	cursorJobID string,
	limit int,
) (*sql.Rows, error) {
	const allJobsQuery = `SELECT ` + jobSelectColumns + `
FROM tg_backup_job_tab
WHERE (? = '' OR job_kind = ?)
  AND (? = '' OR job_state = ?)
  AND (? = 0 OR created_at < ? OR (created_at = ? AND job_id < ?))
ORDER BY created_at DESC, job_id DESC
LIMIT ?`
	const ownerJobsQuery = `SELECT ` + jobSelectColumns + `
FROM tg_backup_job_tab
WHERE owner = ?
  AND (? = '' OR job_kind = ?)
  AND (? = '' OR job_state = ?)
  AND (? = 0 OR created_at < ? OR (created_at = ? AND job_id < ?))
ORDER BY created_at DESC, job_id DESC
LIMIT ?`
	var rows *sql.Rows
	var err error
	if request.Owner == "" {
		rows, err = m.db.QueryContext(
			ctx,
			allJobsQuery,
			request.Kind,
			request.Kind,
			request.State,
			request.State,
			cursorCreatedAt,
			cursorCreatedAt,
			cursorCreatedAt,
			cursorJobID,
			limit,
		)
	} else {
		rows, err = m.db.QueryContext(
			ctx,
			ownerJobsQuery,
			request.Owner,
			request.Kind,
			request.Kind,
			request.State,
			request.State,
			cursorCreatedAt,
			cursorCreatedAt,
			cursorCreatedAt,
			cursorJobID,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list backup jobs: %w", err)
	}
	return rows, nil
}

func (m *Manager) getJobByIdempotency(
	ctx context.Context,
	owner, kind, key string,
) (*Job, error) {
	var jobID string
	err := queryJobRow(
		ctx,
		m.db,
		`SELECT job_id FROM tg_backup_job_tab
WHERE owner = ? AND job_kind = ? AND idempotency_key = ?`,
		owner,
		kind,
		key,
	).Scan(&jobID)
	if err != nil {
		return nil, fmt.Errorf("read idempotent backup job: %w", err)
	}
	return m.GetJob(ctx, jobID)
}

func (m *Manager) Cancel(ctx context.Context, jobID string) (*Job, error) {
	job, err := m.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if isTerminal(job.State) || job.State == "publishing" {
		return nil, ErrJobNotCancelable
	}
	now := time.Now().UnixMilli()
	result, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET cancel_requested = 1, job_state = 'canceling', updated_at = ?
WHERE job_id = ? AND completed_at = 0 AND job_state != 'publishing'`,
		now,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("request backup cancellation: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, ErrJobNotCancelable
	}
	return m.GetJob(ctx, jobID)
}

func (m *Manager) Artifact(ctx context.Context, jobID string) (string, *Job, error) {
	job, err := m.GetJob(ctx, jobID)
	if err != nil {
		return "", nil, err
	}
	if job.Kind != "export" || job.State != "succeeded" || !job.ArtifactAvailable {
		return "", job, ErrArtifactUnavailable
	}
	if filepath.Base(job.artifactPath) != job.artifactPath {
		return "", job, ErrArtifactUnavailable
	}
	return filepath.Join(m.options.WorkDir, job.artifactPath), job, nil
}

func (m *Manager) Run(ctx context.Context) error {
	if err := m.recover(ctx); err != nil {
		return err
	}
	if err := m.cleanup(ctx); err != nil {
		return err
	}
	exportDone := make(chan error, 1)
	importDone := make(chan error, 1)
	cleanupDone := make(chan error, 1)
	go func() { exportDone <- m.worker(ctx, "export") }()
	go func() { importDone <- m.worker(ctx, "import") }()
	go func() { cleanupDone <- m.cleanupWorker(ctx) }()
	select {
	case <-ctx.Done():
		<-exportDone
		<-importDone
		<-cleanupDone
		return fmt.Errorf("run backup workers: %w", ctx.Err())
	case err := <-exportDone:
		return err
	case err := <-importDone:
		return err
	case err := <-cleanupDone:
		return err
	}
}

func (m *Manager) cleanupWorker(ctx context.Context) error {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("run backup cleanup worker: %w", ctx.Err())
		case <-ticker.C:
			if err := m.cleanup(ctx); err != nil {
				return err
			}
		}
	}
}

func (m *Manager) worker(ctx context.Context, kind string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		worked, err := m.processNext(ctx, kind)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("run %s backup worker: %w", kind, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) ProcessUntilTerminal(ctx context.Context, jobID string) (*Job, error) {
	for {
		job, err := m.GetJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if isTerminal(job.State) {
			return m.terminalJobResult(job)
		}
		if err := m.processJob(ctx, job); err != nil {
			if errors.Is(err, errJobClaimed) {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("wait for claimed backup job: %w", ctx.Err())
				case <-time.After(pollInterval):
					continue
				}
			}
			return nil, err
		}
	}
}

func (m *Manager) terminalJobResult(job *Job) (*Job, error) {
	if job.State == "succeeded" {
		return job, nil
	}
	if cause, exists := m.executionErrors.Load(job.JobID); exists {
		if executionError, ok := cause.(error); ok {
			return job, fmt.Errorf(
				"%w in state %s: %w",
				ErrJobFailed,
				job.State,
				executionError,
			)
		}
	}
	return job, fmt.Errorf("%w in state %s: %s", ErrJobFailed, job.State, job.Error.Message)
}

func (m *Manager) processNext(ctx context.Context, kind string) (bool, error) {
	var jobID string
	err := queryJobRow(
		ctx,
		m.db,
		`SELECT job_id FROM tg_backup_job_tab
WHERE job_kind = ? AND job_state IN ('queued', 'canceling', 'publishing')
ORDER BY created_at LIMIT 1`,
		kind,
	).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim backup job: %w", err)
	}
	job, err := m.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	err = m.processJob(ctx, job)
	if errors.Is(err, errJobClaimed) {
		return true, nil
	}
	return true, err
}

func (m *Manager) processJob(ctx context.Context, job *Job) error {
	if job.State == "canceling" || job.cancelRequested {
		return m.cancelJob(ctx, job)
	}
	err := m.executeJob(ctx, job)
	if err == nil {
		if job.Kind == "import" {
			_ = m.releaseImportArtifact(context.WithoutCancel(ctx), job)
		}
		return nil
	}
	return m.handleJobFailure(ctx, job, err)
}

func (m *Manager) executeJob(ctx context.Context, job *Job) error {
	switch job.Kind {
	case "export":
		return m.processExport(ctx, job)
	case "import":
		return m.processImport(ctx, job)
	default:
		return ErrInvalidRequest
	}
}

func (m *Manager) handleJobFailure(ctx context.Context, job *Job, err error) error {
	if errors.Is(err, errJobClaimed) {
		return err
	}
	if errors.Is(err, errJobCanceled) || errors.Is(err, context.Canceled) && job.cancelRequested {
		return m.cancelJob(context.WithoutCancel(ctx), job)
	}
	if errors.Is(err, filemgr.ErrBackupCompensation) {
		return err
	}
	current, currentErr := m.GetJob(context.WithoutCancel(ctx), job.JobID)
	if currentErr == nil && isTerminal(current.State) {
		return nil
	}
	if cleanupErr := m.cleanupFailedJob(context.WithoutCancel(ctx), job); cleanupErr != nil {
		return errors.Join(err, cleanupErr)
	}
	code := classifyError(err)
	reportErr := m.writeFailureReport(
		context.WithoutCancel(ctx),
		job,
		code,
		safeErrorMessage(err),
	)
	m.executionErrors.Store(job.JobID, errors.Join(err, reportErr))
	if finishErr := m.finishJob(
		context.WithoutCancel(ctx),
		job.JobID,
		"failed",
		code,
		safeErrorMessage(err),
	); finishErr != nil {
		return errors.Join(err, finishErr)
	}
	return nil
}

func (m *Manager) cleanupFailedJob(ctx context.Context, job *Job) error {
	if job.Kind == "export" {
		if err := m.files.ReleaseBackupSnapshot(ctx, job.JobID); err != nil {
			return fmt.Errorf("release failed backup snapshot: %w", err)
		}
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".partial"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".snapshot.json"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".tgfb"))
		return nil
	}
	if err := m.files.DiscardBackupImport(ctx, job.JobID); err != nil {
		return fmt.Errorf("discard failed backup import: %w", err)
	}
	_ = m.releaseImportArtifact(ctx, job)
	return nil
}

func (m *Manager) processExport(ctx context.Context, job *Job) error {
	manifest, err := m.prepareExportSnapshot(ctx, job)
	if err != nil {
		return err
	}
	artifactName, report, err := m.buildExportArtifact(ctx, job.JobID, manifest)
	if err != nil {
		return err
	}
	return m.completeExport(ctx, job.JobID, artifactName, report)
}

func (m *Manager) prepareExportSnapshot(
	ctx context.Context,
	job *Job,
) (*backupfmt.Manifest, error) {
	if err := m.transitionState(ctx, job.JobID, "queued", "snapshotting"); err != nil {
		return nil, err
	}
	manifest, err := m.files.CreateBackupSnapshot(ctx, filemgr.BackupSnapshotRequest{
		JobID: job.JobID, Scope: job.Scope, SchemaVersion: m.options.SchemaVersion,
		RequiredBuckets: m.options.RequiredBuckets,
	})
	if err != nil {
		return nil, fmt.Errorf("create backup snapshot: %w", err)
	}
	if err := ensureFreeSpace(m.options.WorkDir, manifest.Limits.PhysicalBytes); err != nil {
		return nil, err
	}
	snapshotName := job.JobID + ".snapshot.json"
	if err := writeJSONFile(filepath.Join(m.options.WorkDir, snapshotName), manifest); err != nil {
		return nil, err
	}
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET snapshot_path = ?, files_total = ?, parts_total = ?, bytes_total = ?,
job_state = 'building', updated_at = ? WHERE job_id = ?`,
		snapshotName,
		manifest.Limits.FileCount,
		manifest.Limits.PartCount,
		manifest.Limits.PhysicalBytes,
		time.Now().UnixMilli(),
		job.JobID,
	); err != nil {
		return nil, fmt.Errorf("start backup artifact build: %w", err)
	}
	return manifest, nil
}

func (m *Manager) buildExportArtifact(
	ctx context.Context,
	jobID string,
	manifest *backupfmt.Manifest,
) (string, *backupfmt.Report, error) {
	partialPath := filepath.Join(m.options.WorkDir, jobID+".partial")
	file, err := os.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("create backup artifact partial: %w", err)
	}
	fileByRef := make(map[string]backupfmt.File, len(manifest.Files))
	for _, item := range manifest.Files {
		fileByRef[item.Ref] = item
	}
	checkedWriter := &spaceCheckingWriter{
		writer:    file,
		workDir:   m.options.WorkDir,
		total:     manifest.Limits.PhysicalBytes,
		lastCheck: time.Now(),
	}
	buildErr := backupfmt.Build(ctx, checkedWriter, manifest, m.options.Limits, func(
		ctx context.Context,
		fileRef string,
		partIndex int,
	) (io.ReadCloser, error) {
		canceled, cancelErr := m.isCanceled(ctx, jobID)
		if cancelErr != nil {
			return nil, cancelErr
		}
		if canceled {
			return nil, errJobCanceled
		}
		reader, openErr := m.files.OpenBackupPart(
			ctx,
			fileByRef[fileRef].SourceFileID,
			partIndex,
		)
		if openErr != nil {
			return nil, fmt.Errorf("%w: %w", errSourceUnreadable, openErr)
		}
		return reader, nil
	})
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(buildErr, syncErr, closeErr); err != nil {
		return "", nil, fmt.Errorf("build backup artifact: %w", err)
	}
	_, report, err := backupfmt.VerifyFile(ctx, partialPath, m.options.Limits, m.options.MaxPartSize)
	if err != nil {
		return "", nil, fmt.Errorf("verify built backup artifact: %w", err)
	}
	artifactName := jobID + ".tgfb"
	if err := os.Rename(partialPath, filepath.Join(m.options.WorkDir, artifactName)); err != nil {
		return "", nil, fmt.Errorf("publish backup artifact: %w", err)
	}
	if err := syncDirectory(m.options.WorkDir); err != nil {
		return "", nil, err
	}
	return artifactName, report, nil
}

func (m *Manager) completeExport(
	ctx context.Context,
	jobID, artifactName string,
	report *backupfmt.Report,
) error {
	snapshotPath := filepath.Join(m.options.WorkDir, jobID+".snapshot.json")
	if err := os.Remove(snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed backup snapshot: %w", err)
	}
	now := time.Now()
	err := m.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE tg_backup_job_tab
SET job_state = 'succeeded', artifact_path = ?, artifact_size = ?,
artifact_sha256 = ?, files_completed = files_total, parts_completed = parts_total,
bytes_completed = bytes_total, updated_at = ?, completed_at = ?, artifact_expires_at = ?,
snapshot_path = ''
WHERE job_id = ? AND job_state = 'building' AND cancel_requested = 0`,
			artifactName,
			report.ArtifactBytes,
			report.ArtifactSHA256,
			now.UnixMilli(),
			now.UnixMilli(),
			now.Add(m.options.ArtifactRetention).UnixMilli(),
			jobID,
		)
		if err != nil {
			return fmt.Errorf("complete backup export: %w", err)
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return errJobCanceled
		}
		if _, err := tx.ExecContext(
			ctx,
			"DELETE FROM tg_backup_export_pin_tab WHERE job_id = ?",
			jobID,
		); err != nil {
			return fmt.Errorf("release completed backup pins: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("commit completed backup export: %w", err)
	}
	return nil
}

func (m *Manager) processImport(ctx context.Context, job *Job) error {
	if job.artifactPath == "" || filepath.Base(job.artifactPath) != job.artifactPath {
		return fmt.Errorf("import artifact path is invalid: %w", filemgr.ErrBackupState)
	}
	artifactPath := filepath.Join(m.options.WorkDir, job.artifactPath)
	if job.State == "publishing" {
		return m.resumeImportPublish(ctx, job, artifactPath)
	}
	if err := m.transitionState(ctx, job.JobID, "queued", "validating"); err != nil {
		return err
	}
	manifest, err := m.validateImportArtifact(ctx, job, artifactPath)
	if err != nil {
		return err
	}
	if job.DryRun {
		return m.finishSuccessfulJob(ctx, job.JobID)
	}
	if err := m.stageImport(ctx, job, artifactPath, manifest); err != nil {
		return err
	}
	if err := m.ensureImportNotCanceled(ctx, job.JobID); err != nil {
		return err
	}
	if err := m.transitionState(ctx, job.JobID, "staging", "publishing"); err != nil {
		return err
	}
	return m.publishImport(ctx, job, manifest)
}

func (m *Manager) resumeImportPublish(
	ctx context.Context,
	job *Job,
	artifactPath string,
) error {
	manifest, report, err := backupfmt.VerifyFile(
		ctx,
		artifactPath,
		m.options.Limits,
		m.options.MaxPartSize,
	)
	if err != nil {
		return fmt.Errorf("verify import artifact for publish recovery: %w", err)
	}
	if report.ArtifactSHA256 != job.ArtifactSHA256 {
		return fmt.Errorf("import artifact changed before publish recovery: %w", backupfmt.ErrChecksum)
	}
	return m.publishImport(ctx, job, manifest)
}

func (m *Manager) validateImportArtifact(
	ctx context.Context,
	job *Job,
	artifactPath string,
) (*backupfmt.Manifest, error) {
	manifest, report, err := backupfmt.VerifyFile(
		ctx,
		artifactPath,
		m.options.Limits,
		m.options.MaxPartSize,
	)
	if err != nil {
		return nil, fmt.Errorf("verify import artifact: %w", err)
	}
	if report.ArtifactSHA256 != job.ArtifactSHA256 {
		return nil, fmt.Errorf("import artifact changed after receive: %w", backupfmt.ErrChecksum)
	}
	if err := ensureFreeSpace(m.options.WorkDir, manifest.Limits.PhysicalBytes); err != nil {
		return nil, err
	}
	if err := m.validateBuckets(manifest.RequiredBuckets); err != nil {
		return nil, err
	}
	if err := m.files.ValidateBackupImport(ctx, manifest, job.Conflict); err != nil {
		return nil, fmt.Errorf("validate backup import target: %w", err)
	}
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab SET files_total = ?, parts_total = ?,
bytes_total = ?, updated_at = ? WHERE job_id = ?`,
		manifest.Limits.FileCount,
		manifest.Limits.PartCount,
		manifest.Limits.PhysicalBytes,
		time.Now().UnixMilli(),
		job.JobID,
	); err != nil {
		return nil, fmt.Errorf("set backup import totals: %w", err)
	}
	return manifest, nil
}

func (m *Manager) stageImport(
	ctx context.Context,
	job *Job,
	artifactPath string,
	manifest *backupfmt.Manifest,
) error {
	if err := m.transitionState(ctx, job.JobID, "validating", "staging"); err != nil {
		return err
	}
	if err := m.files.BeginBackupImport(ctx, job.JobID, manifest); err != nil {
		return fmt.Errorf("begin backup import staging: %w", err)
	}
	partByEntry := indexManifestParts(manifest)
	err := backupfmt.WalkParts(ctx, artifactPath, m.options.Limits, func(
		ctx context.Context,
		observed backupfmt.Part,
		reader io.Reader,
	) error {
		if err := m.ensureImportNotCanceled(ctx, job.JobID); err != nil {
			return err
		}
		part := partByEntry[observed.Entry]
		if err := m.files.StageBackupPart(ctx, job.JobID, part, reader); err != nil {
			return fmt.Errorf("stage backup import part: %w", err)
		}
		if _, err := m.db.ExecContext(
			ctx,
			`UPDATE tg_backup_job_tab SET parts_completed = parts_completed + 1,
bytes_completed = bytes_completed + ?, updated_at = ? WHERE job_id = ?`,
			part.Size,
			time.Now().UnixMilli(),
			job.JobID,
		); err != nil {
			return fmt.Errorf("update backup import progress: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk backup import parts: %w", err)
	}
	if err := m.files.FinishBackupImportFiles(ctx, job.JobID, manifest); err != nil {
		return fmt.Errorf("finish staged backup files: %w", err)
	}
	return nil
}

func indexManifestParts(manifest *backupfmt.Manifest) map[string]backupfmt.Part {
	partByEntry := make(map[string]backupfmt.Part)
	for _, file := range manifest.Files {
		for _, part := range file.Parts {
			partByEntry[part.Entry] = part
		}
	}
	return partByEntry
}

func (m *Manager) ensureImportNotCanceled(ctx context.Context, jobID string) error {
	canceled, err := m.isCanceled(ctx, jobID)
	if err != nil {
		return err
	}
	if canceled {
		return errJobCanceled
	}
	return nil
}

func (m *Manager) publishImport(
	ctx context.Context,
	job *Job,
	manifest *backupfmt.Manifest,
) error {
	if _, err := m.files.PublishBackupImport(ctx, job.JobID, manifest, job.Conflict); err != nil {
		return fmt.Errorf("publish backup import: %w", err)
	}
	return nil
}

func (m *Manager) validateBuckets(required []backupfmt.RequiredBucket) error {
	configured := make(map[string]string, len(m.options.RequiredBuckets))
	for _, bucket := range m.options.RequiredBuckets {
		configured[bucket.Name] = bucket.ACL
	}
	for _, bucket := range required {
		if configured[bucket.Name] != bucket.ACL {
			return fmt.Errorf("required bucket %s is unavailable: %w", bucket.Name, filemgr.ErrBackupState)
		}
	}
	return nil
}

func (m *Manager) cancelJob(ctx context.Context, job *Job) error {
	if job.Kind == "export" {
		if err := m.files.ReleaseBackupSnapshot(ctx, job.JobID); err != nil {
			return fmt.Errorf("release canceled backup snapshot: %w", err)
		}
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".partial"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".snapshot.json"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".tgfb"))
	} else {
		if err := m.files.DiscardBackupImport(ctx, job.JobID); err != nil {
			return fmt.Errorf("discard canceled backup import: %w", err)
		}
		_ = os.Remove(filepath.Join(m.options.WorkDir, job.JobID+".receive.partial"))
		_ = m.releaseImportArtifact(ctx, job)
	}
	return m.finishJob(ctx, job.JobID, "canceled", "canceled", "backup job canceled")
}

func (m *Manager) finishJob(
	ctx context.Context,
	jobID, state, code, message string,
) error {
	now := time.Now().UnixMilli()
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET job_state = ?, error_code = ?, error_message = ?, updated_at = ?,
completed_at = ?, cancel_requested = CASE WHEN ? = 'canceled' THEN 1 ELSE cancel_requested END
WHERE job_id = ?`,
		state,
		code,
		message,
		now,
		now,
		state,
		jobID,
	); err != nil {
		return fmt.Errorf("finish backup job: %w", err)
	}
	return nil
}

func (m *Manager) finishSuccessfulJob(ctx context.Context, jobID string) error {
	now := time.Now().UnixMilli()
	result, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET job_state = 'succeeded', error_code = '', error_message = '',
updated_at = ?, completed_at = ?
WHERE job_id = ? AND completed_at = 0 AND cancel_requested = 0`,
		now,
		now,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("finish successful backup job: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return errJobCanceled
	}
	return nil
}

func (m *Manager) transitionState(ctx context.Context, jobID, from, to string) error {
	result, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab SET job_state = ?, updated_at = ?
WHERE job_id = ? AND job_state = ? AND completed_at = 0 AND cancel_requested = 0`,
		to,
		time.Now().UnixMilli(),
		jobID,
		from,
	)
	if err != nil {
		return fmt.Errorf("transition backup job from %s to %s: %w", from, to, err)
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		canceled, cancelErr := m.isCanceled(ctx, jobID)
		if cancelErr == nil && canceled {
			return errJobCanceled
		}
		return errJobClaimed
	}
	return nil
}

func (m *Manager) resetState(ctx context.Context, jobID, state string) error {
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab SET job_state = ?, updated_at = ?
WHERE job_id = ? AND completed_at = 0`,
		state,
		time.Now().UnixMilli(),
		jobID,
	); err != nil {
		return fmt.Errorf("reset backup job to %s: %w", state, err)
	}
	return nil
}

func (m *Manager) isCanceled(ctx context.Context, jobID string) (bool, error) {
	var requested int
	if err := queryJobRow(
		ctx,
		m.db,
		"SELECT cancel_requested FROM tg_backup_job_tab WHERE job_id = ?",
		jobID,
	).Scan(&requested); err != nil {
		return false, fmt.Errorf("read backup cancellation state: %w", err)
	}
	return requested != 0, nil
}

type recoverableJob struct {
	id, kind, state, artifact string
}

func (m *Manager) recover(ctx context.Context) error {
	items, err := m.readRecoverableJobs(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := m.recoverJob(ctx, item); err != nil {
			return err
		}
	}
	if _, err := m.db.ExecContext(
		ctx,
		`DELETE FROM tg_backup_export_pin_tab
WHERE job_id NOT IN (
  SELECT job_id FROM tg_backup_job_tab
  WHERE job_kind = 'export' AND completed_at = 0
)`,
	); err != nil {
		return fmt.Errorf("clean orphan backup pins: %w", err)
	}
	return nil
}

func (m *Manager) readRecoverableJobs(ctx context.Context) ([]recoverableJob, error) {
	rows, err := m.db.QueryContext(
		ctx,
		`SELECT job_id, job_kind, job_state, artifact_path
FROM tg_backup_job_tab WHERE completed_at = 0 ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("query recoverable backup jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]recoverableJob, 0)
	for rows.Next() {
		var item recoverableJob
		if err := rows.Scan(&item.id, &item.kind, &item.state, &item.artifact); err != nil {
			return nil, fmt.Errorf("scan recoverable backup job: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable backup jobs: %w", err)
	}
	return items, nil
}

func (m *Manager) recoverJob(ctx context.Context, item recoverableJob) error {
	switch {
	case item.state == "receiving":
		_ = os.Remove(filepath.Join(m.options.WorkDir, item.id+".receive.partial"))
		return m.finishJob(ctx, item.id, "canceled", "canceled", "interrupted artifact receive")
	case item.kind == "export":
		if err := m.files.ReleaseBackupSnapshot(ctx, item.id); err != nil {
			return fmt.Errorf("release recovered backup snapshot: %w", err)
		}
		_ = os.Remove(filepath.Join(m.options.WorkDir, item.id+".partial"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, item.id+".snapshot.json"))
		_ = os.Remove(filepath.Join(m.options.WorkDir, item.id+".tgfb"))
		if _, err := m.db.ExecContext(
			ctx,
			"UPDATE tg_backup_job_tab SET snapshot_path = '' WHERE job_id = ?",
			item.id,
		); err != nil {
			return fmt.Errorf("reset recovered backup snapshot path: %w", err)
		}
		return m.resetState(ctx, item.id, "queued")
	case item.state == "publishing":
		return nil
	case item.state == "staging":
		return m.resetStagedImport(ctx, item.id)
	default:
		return m.resetState(ctx, item.id, "queued")
	}
}

func (m *Manager) resetStagedImport(ctx context.Context, jobID string) error {
	if err := m.files.DiscardBackupImport(ctx, jobID); err != nil {
		return fmt.Errorf("discard interrupted backup import: %w", err)
	}
	if _, err := m.db.ExecContext(
		ctx,
		"DELETE FROM tg_backup_job_file_tab WHERE job_id = ?",
		jobID,
	); err != nil {
		return fmt.Errorf("remove interrupted backup staging records: %w", err)
	}
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab SET job_state = 'queued',
parts_completed = 0, bytes_completed = 0, updated_at = ? WHERE job_id = ?`,
		time.Now().UnixMilli(),
		jobID,
	); err != nil {
		return fmt.Errorf("queue interrupted backup import: %w", err)
	}
	return nil
}

func (m *Manager) cleanup(ctx context.Context) error {
	if err := m.cleanupCompletedImportArtifacts(ctx); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	expired, err := m.readExpiredArtifacts(ctx, now)
	if err != nil {
		return err
	}
	if err := m.expireArtifacts(ctx, now, expired); err != nil {
		return err
	}
	return m.cleanupRetainedJobs(ctx)
}

type expiredArtifact struct {
	jobID, filename string
}

func (m *Manager) readExpiredArtifacts(
	ctx context.Context,
	now int64,
) ([]expiredArtifact, error) {
	rows, err := m.db.QueryContext(
		ctx,
		`SELECT job_id, artifact_path FROM tg_backup_job_tab
WHERE artifact_path != '' AND artifact_expires_at > 0 AND artifact_expires_at <= ?`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("query expired backup artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expired := make([]expiredArtifact, 0)
	for rows.Next() {
		var item expiredArtifact
		if err := rows.Scan(&item.jobID, &item.filename); err != nil {
			return nil, fmt.Errorf("scan expired backup artifact: %w", err)
		}
		expired = append(expired, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired backup artifacts: %w", err)
	}
	return expired, nil
}

func (m *Manager) expireArtifacts(
	ctx context.Context,
	now int64,
	expired []expiredArtifact,
) error {
	for _, item := range expired {
		if filepath.Base(item.filename) != item.filename {
			continue
		}
		if err := os.Remove(filepath.Join(m.options.WorkDir, item.filename)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired backup artifact: %w", err)
		}
		if _, err := m.db.ExecContext(
			ctx,
			`UPDATE tg_backup_job_tab
SET artifact_path = '', artifact_size = 0, updated_at = ? WHERE job_id = ?`,
			now,
			item.jobID,
		); err != nil {
			return fmt.Errorf("expire backup artifact record: %w", err)
		}
	}
	return nil
}

func (m *Manager) cleanupRetainedJobs(ctx context.Context) error {
	if m.options.JobRetention <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-m.options.JobRetention).UnixMilli()
	if err := m.cleanupRetainedJobFiles(ctx, cutoff); err != nil {
		return err
	}
	if err := m.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if _, err := tx.ExecContext(
			ctx,
			`DELETE FROM tg_backup_job_file_tab WHERE job_id IN (
  SELECT job_id FROM tg_backup_job_tab
  WHERE completed_at > 0 AND completed_at < ? AND artifact_path = ''
)`,
			cutoff,
		); err != nil {
			return fmt.Errorf("remove retained backup job files: %w", err)
		}
		_, err := tx.ExecContext(
			ctx,
			`DELETE FROM tg_backup_job_tab
WHERE completed_at > 0 AND completed_at < ? AND artifact_path = ''`,
			cutoff,
		)
		if err != nil {
			return fmt.Errorf("remove retained backup job: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("clean retained backup jobs: %w", err)
	}
	return nil
}

func (m *Manager) releaseImportArtifact(ctx context.Context, job *Job) error {
	if job.artifactPath == "" || filepath.Base(job.artifactPath) != job.artifactPath {
		return nil
	}
	if err := os.Remove(filepath.Join(m.options.WorkDir, job.artifactPath)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed import artifact: %w", err)
	}
	if _, err := m.db.ExecContext(
		ctx,
		`UPDATE tg_backup_job_tab
SET artifact_path = '', artifact_size = 0, updated_at = ? WHERE job_id = ?`,
		time.Now().UnixMilli(),
		job.JobID,
	); err != nil {
		return fmt.Errorf("clear completed import artifact: %w", err)
	}
	return nil
}

func (m *Manager) cleanupCompletedImportArtifacts(ctx context.Context) error {
	rows, err := m.db.QueryContext(
		ctx,
		`SELECT job_id, artifact_path FROM tg_backup_job_tab
WHERE job_kind = 'import' AND completed_at > 0 AND artifact_path != ''`,
	)
	if err != nil {
		return fmt.Errorf("query completed import artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	jobs := make([]Job, 0)
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.JobID, &job.artifactPath); err != nil {
			return fmt.Errorf("scan completed import artifact: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate completed import artifacts: %w", err)
	}
	for index := range jobs {
		if err := m.releaseImportArtifact(ctx, &jobs[index]); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) cleanupRetainedJobFiles(ctx context.Context, cutoff int64) error {
	rows, err := m.db.QueryContext(
		ctx,
		`SELECT report_path, snapshot_path FROM tg_backup_job_tab
WHERE completed_at > 0 AND completed_at < ? AND artifact_path = ''`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("query retained backup job files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	filenames := make([]string, 0)
	for rows.Next() {
		var reportPath, snapshotPath string
		if err := rows.Scan(&reportPath, &snapshotPath); err != nil {
			return fmt.Errorf("scan retained backup job files: %w", err)
		}
		filenames = append(filenames, reportPath, snapshotPath)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate retained backup job files: %w", err)
	}
	for _, filename := range filenames {
		if filename == "" || filepath.Base(filename) != filename {
			continue
		}
		if err := os.Remove(filepath.Join(m.options.WorkDir, filename)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove retained backup job file: %w", err)
		}
	}
	return nil
}

func (m *Manager) writeFailureReport(
	ctx context.Context,
	job *Job,
	code, message string,
) error {
	reportName := job.JobID + ".report.json"
	partialPath := filepath.Join(m.options.WorkDir, job.JobID+".report.partial")
	reportPath := filepath.Join(m.options.WorkDir, reportName)
	report := failureReport{
		Kind:      job.Kind,
		State:     "failed",
		Code:      code,
		Message:   message,
		FailedAt:  time.Now().UnixMilli(),
		Retryable: false,
	}
	if err := writeJSONAtomic(partialPath, reportPath, report); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(
		ctx,
		"UPDATE tg_backup_job_tab SET report_path = ? WHERE job_id = ?",
		reportName,
		job.JobID,
	); err != nil {
		return fmt.Errorf("record backup failure report: %w", err)
	}
	return nil
}

func writeJSONAtomic(partialPath, targetPath string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode backup report: %w", err)
	}
	file, err := os.OpenFile(partialPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create backup report partial: %w", err)
	}
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write backup report: %w", err)
	}
	if err := os.Rename(partialPath, targetPath); err != nil {
		return fmt.Errorf("publish backup report: %w", err)
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func ensureFreeSpace(workDir string, payloadBytes int64) error {
	if payloadBytes < 0 {
		return ErrInvalidRequest
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workDir, &stat); err != nil {
		return fmt.Errorf("inspect backup work space: %w", err)
	}
	const gib = int64(1024 * 1024 * 1024)
	margin := payloadBytes / 20
	if margin < gib {
		margin = gib
	}
	required := payloadBytes
	if required > math.MaxInt64-margin {
		return backupfmt.ErrLimitExceeded
	}
	required += margin
	available := int64(stat.Bavail) * stat.Bsize //nolint:gosec // Statfs values are non-negative.
	if available < required {
		return fmt.Errorf("backup work directory requires %d free bytes: %w", required, syscall.ENOSPC)
	}
	return nil
}

type spaceCheckingReader struct {
	reader       io.Reader
	workDir      string
	total        int64
	observed     int64
	lastObserved int64
	lastCheck    time.Time
}

func (r *spaceCheckingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.observed += int64(count)
	if checkErr := r.check(); checkErr != nil {
		return count, checkErr
	}
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read space-checked stream: %w", err)
	}
	return count, nil
}

func (r *spaceCheckingReader) check() error {
	if r.observed-r.lastObserved < spaceCheckBytes &&
		time.Since(r.lastCheck) < spaceCheckInterval {
		return nil
	}
	if err := ensureFreeSpace(r.workDir, max(r.total-r.observed, 0)); err != nil {
		return fmt.Errorf("recheck import receive space: %w", err)
	}
	r.lastObserved = r.observed
	r.lastCheck = time.Now()
	return nil
}

type spaceCheckingWriter struct {
	writer       io.Writer
	workDir      string
	total        int64
	observed     int64
	lastObserved int64
	lastCheck    time.Time
}

func (w *spaceCheckingWriter) Write(buffer []byte) (int, error) {
	count, err := w.writer.Write(buffer)
	w.observed += int64(count)
	if checkErr := w.check(); checkErr != nil {
		return count, checkErr
	}
	if err != nil {
		return count, fmt.Errorf("write space-checked stream: %w", err)
	}
	return count, nil
}

func (w *spaceCheckingWriter) check() error {
	if w.observed-w.lastObserved < spaceCheckBytes &&
		time.Since(w.lastCheck) < spaceCheckInterval {
		return nil
	}
	if err := ensureFreeSpace(w.workDir, max(w.total-w.observed, 0)); err != nil {
		return fmt.Errorf("recheck backup build space: %w", err)
	}
	w.lastObserved = w.observed
	w.lastCheck = time.Now()
	return nil
}

func writeJSONFile(filename string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode backup snapshot: %w", err)
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup snapshot: %w", err)
	}
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write backup snapshot: %w", err)
	}
	return nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open backup work directory: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup work directory: %w", err)
	}
	return nil
}

func requestFingerprint(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func randomJobID() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate backup job id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func validJobID(value string) bool {
	return len(value) == 64 && strings.ToLower(value) == value && validHex(value)
}

func validIdempotencyKey(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validScope(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes ||
		!strings.HasPrefix(value, "/") ||
		path.Clean(value) != value ||
		strings.Contains(value, "\\") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	return len(value) == 64 && strings.ToLower(value) == value && validHex(value)
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func isTerminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled"
}

func validOwner(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validJobState(state string) bool {
	switch state {
	case "receiving",
		"queued",
		"snapshotting",
		"building",
		"validating",
		"staging",
		"publishing",
		"canceling",
		"succeeded",
		"failed",
		"canceled":
		return true
	default:
		return false
	}
}

func classifyError(err error) string {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "insufficient_storage"
	case errors.Is(err, errSourceUnreadable):
		return "source_unreadable"
	case errors.Is(err, filemgr.ErrBackupBackendUpload):
		return "backend_upload_failed"
	case errors.Is(err, filemgr.ErrBackupBackendReadback):
		return "backend_readback_failed"
	case errors.Is(err, filemgr.ErrBackupPublish):
		return "publish_failed"
	case errors.Is(err, backupfmt.ErrInvalidArchive):
		return "invalid_archive"
	case errors.Is(err, backupfmt.ErrChecksum):
		return "archive_checksum_mismatch"
	case errors.Is(err, backupfmt.ErrLimitExceeded):
		return "archive_limit_exceeded"
	case errors.Is(err, filemgr.ErrBackupConflict):
		return "path_conflict"
	case errors.Is(err, filemgr.ErrBackupState):
		return "target_incompatible"
	default:
		return "internal"
	}
}

func safeErrorMessage(err error) string {
	switch classifyError(err) {
	case "invalid_archive":
		return "backup archive is invalid"
	case "archive_checksum_mismatch":
		return "backup archive checksum validation failed"
	case "archive_limit_exceeded":
		return "backup archive exceeds configured limits"
	case "insufficient_storage":
		return "backup work directory has insufficient free space"
	case "source_unreadable":
		return "backup source data could not be read"
	case "backend_upload_failed":
		return "backup data could not be uploaded to the backend"
	case "backend_readback_failed":
		return "uploaded backup data failed readback verification"
	case "publish_failed":
		return "backup data could not be published atomically"
	case "path_conflict":
		return "backup paths conflict with existing data"
	case "target_incompatible":
		return "backup is incompatible with the target configuration"
	default:
		return "backup operation failed"
	}
}

type rowScanner interface {
	Scan(...any) error
}

func queryJobRow(
	ctx context.Context,
	queryer database.IQueryer,
	query string,
	args ...any,
) rowScanner {
	if handle, ok := queryer.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}); ok {
		return handle.QueryRowContext(ctx, query, args...)
	}
	return &fallbackRow{ctx: ctx, queryer: queryer, query: query, args: args}
}

type fallbackRow struct {
	ctx     context.Context
	queryer database.IQueryer
	query   string
	args    []any
}

func (r *fallbackRow) Scan(dest ...any) error {
	rows, err := r.queryer.QueryContext(r.ctx, r.query, r.args...)
	if err != nil {
		return fmt.Errorf("query fallback row: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate fallback row: %w", err)
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return fmt.Errorf("scan fallback row: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate fallback row: %w", err)
	}
	return nil
}
