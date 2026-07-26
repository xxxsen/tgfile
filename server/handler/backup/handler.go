package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
)

type Handler struct {
	manager *backupmgr.Manager
	users   map[string]string
}

var errTrailingJSON = errors.New("request contains trailing JSON")

func New(manager *backupmgr.Manager, users map[string]string) *Handler {
	return &Handler{manager: manager, users: users}
}

type exportRequest struct {
	Scope string `json:"scope"`
}

func (h *Handler) CreateExport(c *gin.Context) {
	owner, role, ok := h.authorize(c, false)
	if !ok {
		return
	}
	_ = role
	var request exportRequest
	if err := decodeJSON(c.Request.Body, &request); err != nil {
		writeError(c, err)
		return
	}
	if request.Scope == "" {
		request.Scope = "/"
	}
	job, err := h.manager.CreateExport(c.Request.Context(), backupmgr.CreateExportRequest{
		Owner: owner, IdempotencyKey: c.GetHeader("Idempotency-Key"), Scope: request.Scope,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *Handler) CreateImport(c *gin.Context) {
	owner, _, ok := h.authorize(c, true)
	if !ok {
		return
	}
	if c.ContentType() != backupfmt.MediaType {
		c.Status(http.StatusUnsupportedMediaType)
		return
	}
	if c.Request.ContentLength < 0 {
		c.Status(http.StatusLengthRequired)
		return
	}
	conflict := c.DefaultQuery("conflict", "fail")
	dryRunRaw := c.DefaultQuery("dry_run", "false")
	dryRun, err := strconv.ParseBool(dryRunRaw)
	if err != nil || (dryRunRaw != "true" && dryRunRaw != "false") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dry_run parameter"})
		return
	}
	job, err := h.manager.CreateImport(c.Request.Context(), backupmgr.CreateImportRequest{
		Owner: owner, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		ConflictPolicy: conflict, DryRun: dryRun, ContentLength: c.Request.ContentLength,
		ArtifactSHA256: c.GetHeader("X-Tgfile-Artifact-SHA256"), Body: c.Request.Body,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *Handler) GetJob(c *gin.Context) {
	owner, role, ok := h.authorize(c, false)
	if !ok {
		return
	}
	job, err := h.manager.GetJob(c.Request.Context(), c.Param("job_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	if role != "read-write" && job.Owner != owner {
		c.Status(http.StatusForbidden)
		return
	}
	c.JSON(http.StatusOK, job)
}

func (h *Handler) Cancel(c *gin.Context) {
	_, _, ok := h.authorize(c, true)
	if !ok {
		return
	}
	job, err := h.manager.Cancel(c.Request.Context(), c.Param("job_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *Handler) Artifact(c *gin.Context) {
	owner, role, ok := h.authorize(c, false)
	if !ok {
		return
	}
	filename, job, err := h.manager.Artifact(c.Request.Context(), c.Param("job_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	if role != "read-write" && job.Owner != owner {
		c.Status(http.StatusForbidden)
		return
	}
	file, err := os.Open(filename)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Type", backupfmt.MediaType)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tgfb"`, job.JobID))
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", `"`+job.ArtifactSHA256+`"`)
	c.Header("X-Tgfile-Artifact-SHA256", job.ArtifactSHA256)
	http.ServeContent(c.Writer, c.Request, path.Base(filename), stat.ModTime(), file)
}

func (h *Handler) Metrics(c *gin.Context) {
	_, _, ok := h.authorize(c, true)
	if !ok {
		return
	}
	snapshot, err := h.manager.Metrics(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.Header("Cache-Control", "private, no-store")
	c.String(http.StatusOK, renderMetrics(snapshot))
}

func renderMetrics(snapshot *backupmgr.MetricsSnapshot) string {
	var output strings.Builder
	for _, item := range snapshot.JobsTotal {
		writeMetric(
			&output,
			"tgfile_backup_jobs_total",
			"kind="+strconv.Quote(item.Kind)+",result="+strconv.Quote(item.Result),
			strconv.FormatInt(item.Value, 10),
		)
	}
	for _, item := range snapshot.ActiveJobs {
		writeMetric(
			&output,
			"tgfile_backup_active_jobs",
			"kind="+strconv.Quote(item.Kind)+",state="+strconv.Quote(item.State),
			strconv.FormatInt(item.Value, 10),
		)
	}
	for _, item := range snapshot.BytesTotal {
		writeMetric(
			&output,
			"tgfile_backup_bytes_total",
			"kind="+strconv.Quote(item.Kind),
			strconv.FormatFloat(item.Value, 'f', -1, 64),
		)
	}
	for _, item := range snapshot.DurationSeconds {
		writeMetric(
			&output,
			"tgfile_backup_duration_seconds",
			"kind="+strconv.Quote(item.Kind),
			strconv.FormatFloat(item.Value, 'f', -1, 64),
		)
	}
	for _, item := range snapshot.FailuresTotal {
		writeMetric(
			&output,
			"tgfile_backup_failures_total",
			"kind="+strconv.Quote(item.Kind)+",code="+strconv.Quote(item.Code),
			strconv.FormatInt(item.Value, 10),
		)
	}
	writeMetric(
		&output,
		"tgfile_backup_artifact_bytes",
		"",
		strconv.FormatInt(snapshot.ArtifactBytes, 10),
	)
	writeMetric(
		&output,
		"tgfile_backup_staged_files",
		"",
		strconv.FormatInt(snapshot.StagedFiles, 10),
	)
	return output.String()
}

func writeMetric(output *strings.Builder, name, labels, value string) {
	output.WriteString(name)
	if labels != "" {
		output.WriteByte('{')
		output.WriteString(labels)
		output.WriteByte('}')
	}
	output.WriteByte(' ')
	output.WriteString(value)
	output.WriteByte('\n')
}

func (h *Handler) authorize(c *gin.Context, write bool) (string, string, bool) {
	user, ok := proxyutil.GetUserInfo(c.Request.Context())
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="Restricted Area"`)
		c.Status(http.StatusUnauthorized)
		return "", "", false
	}
	role, exists := h.users[user.Username]
	if !exists || write && role != "read-write" {
		c.Status(http.StatusForbidden)
		return "", "", false
	}
	return user.Username, role, true
}

func decodeJSON(reader io.Reader, output any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errTrailingJSON
	}
	return nil
}

func writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "backup operation failed"
	switch {
	case errors.Is(err, backupmgr.ErrInvalidRequest):
		status, message = http.StatusBadRequest, "invalid backup request"
	case errors.Is(err, backupmgr.ErrIdempotencyConflict):
		status, message = http.StatusConflict, "idempotency key conflicts with an existing request"
	case errors.Is(err, backupmgr.ErrJobNotFound):
		status, message = http.StatusNotFound, "backup job not found"
	case errors.Is(err, backupmgr.ErrJobNotCancelable),
		errors.Is(err, backupmgr.ErrArtifactUnavailable):
		status, message = http.StatusConflict, "backup job is not ready for this operation"
	case errors.Is(err, backupfmt.ErrLimitExceeded):
		status, message = http.StatusRequestEntityTooLarge, "backup archive exceeds configured limits"
	case errors.Is(err, backupfmt.ErrChecksum):
		status, message = http.StatusBadRequest, "backup archive checksum does not match"
	}
	if errors.Is(err, syscall.ENOSPC) {
		status, message = http.StatusInsufficientStorage, "insufficient backup work space"
	}
	c.JSON(status, gin.H{"error": message})
}
