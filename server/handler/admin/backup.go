package admin

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
)

type jobDTO struct {
	JobID             string             `json:"job_id"`
	Kind              string             `json:"kind"`
	Owner             string             `json:"owner,omitempty"`
	State             string             `json:"state"`
	DryRun            bool               `json:"dry_run"`
	Conflict          string             `json:"conflict"`
	Scope             string             `json:"scope"`
	ArtifactSHA256    string             `json:"artifact_sha256"`
	ArtifactAvailable bool               `json:"artifact_available"`
	Progress          backupmgr.Progress `json:"progress"`
	Result            backupmgr.Result   `json:"result"`
	Error             backupmgr.JobError `json:"error"`
	CreatedAt         int64              `json:"created_at"`
	UpdatedAt         int64              `json:"updated_at"`
	CompletedAt       int64              `json:"completed_at"`
	Cancelable        bool               `json:"cancelable"`
}

type jobCursor struct {
	Version   int    `json:"v"`
	CreatedAt int64  `json:"created_at"`
	JobID     string `json:"job_id"`
}

func (h *Handler) listJobs(c *gin.Context) {
	user, ok := h.principal(c)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		return
	}
	query, ok := h.parseQuery(c, "limit", "cursor", "kind", "state", "owner")
	if !ok {
		return
	}
	limit, err := parsePositiveInt(query.Get("limit"), 50, 200)
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "分页大小无效", err)
		return
	}
	decodedCursor, hasCursor, err := decodeJobCursor(query.Get("cursor"))
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_cursor", "分页游标无效", err)
		return
	}
	var cursor *backupmgr.JobCursor
	if hasCursor {
		cursor = &decodedCursor
	}
	owner := query.Get("owner")
	if user.Role == roleRead {
		if owner != "" && owner != user.Username {
			h.writePublicError(c, http.StatusForbidden, "forbidden", "不能查看其他用户任务", nil)
			return
		}
		owner = user.Username
	}
	page, err := h.backups.ListJobs(c.Request.Context(), backupmgr.ListJobsRequest{
		Owner:  owner,
		Kind:   query.Get("kind"),
		State:  query.Get("state"),
		Cursor: cursor,
		Limit:  limit,
	})
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	jobs := make([]jobDTO, 0, len(page.Jobs))
	for _, job := range page.Jobs {
		jobs = append(jobs, h.job(user, job))
	}
	next, err := encodeJobCursor(page.NextCursor)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusOK, map[string]any{
		"jobs":        jobs,
		"next_cursor": next,
	})
}

func (h *Handler) getJob(c *gin.Context) {
	user, ok := h.principal(c)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		return
	}
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	job, ok := h.authorizedJob(c, user, c.Param("job_id"))
	if !ok {
		return
	}
	h.writeData(c, http.StatusOK, h.job(user, job))
}

func (h *Handler) cancelJob(c *gin.Context) {
	user, ok := h.principal(c)
	if !ok || !h.requireMutation(c, user) {
		return
	}
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	job, ok := h.authorizedJob(c, user, c.Param("job_id"))
	if !ok {
		return
	}
	if user.Role != roleReadWrite && (job.Kind != "export" || job.Owner != user.Username) {
		h.writePublicError(c, http.StatusForbidden, "forbidden", "当前账号不能取消该任务", nil)
		return
	}
	job, err := h.backups.Cancel(c.Request.Context(), job.JobID)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusAccepted, h.job(user, job))
}

type exportRequest struct {
	Scope string `json:"scope"`
}

func (h *Handler) createExport(c *gin.Context) {
	user, ok := h.principal(c)
	if !ok || !h.requireMutation(c, user) {
		return
	}
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	if c.ContentType() != "application/json" {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "请求格式无效", nil)
		return
	}
	var request exportRequest
	if err := decodeStrictJSON(c.Request.Body, 64*1024, &request); err != nil {
		h.writeMappedError(c, err)
		return
	}
	if request.Scope == "" {
		request.Scope = "/"
	}
	scope, ok := h.parsePath(c, request.Scope)
	if !ok {
		return
	}
	setAuditPath(c, scope)
	if _, err := h.files.StatFileLink(c.Request.Context(), scope); err != nil {
		h.writeMappedError(c, err)
		return
	}
	idempotencyKey, ok := h.idempotencyKey(c)
	if !ok {
		return
	}
	job, err := h.backups.CreateExport(c.Request.Context(), backupmgr.CreateExportRequest{
		Owner:          user.Username,
		IdempotencyKey: idempotencyKey,
		Scope:          scope,
	})
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusAccepted, h.job(user, job))
}

func (h *Handler) createImport(c *gin.Context) {
	user, ok := h.requireWrite(c)
	if !ok || !h.requireMutation(c, user) {
		return
	}
	conflict, dryRun, ok := h.parseImportOptions(c)
	if !ok || !h.validateImportUpload(c, conflict, dryRun) {
		return
	}
	idempotencyKey, ok := h.idempotencyKey(c)
	if !ok {
		return
	}
	job, err := h.backups.CreateImportUpload(
		c.Request.Context(),
		backupmgr.CreateImportUploadRequest{
			Owner:          user.Username,
			IdempotencyKey: idempotencyKey,
			ConflictPolicy: conflict,
			DryRun:         dryRun,
			ContentLength:  requestContentLength(c.Request),
			Body:           c.Request.Body,
		},
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusAccepted, h.job(user, job))
}

func (h *Handler) parseImportOptions(c *gin.Context) (string, bool, bool) {
	query, ok := h.parseQuery(c, "conflict", "dry_run")
	if !ok {
		return "", false, false
	}
	conflict := query.Get("conflict")
	if conflict == "" {
		conflict = "fail"
	}
	if conflict != "fail" && conflict != "replace" {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "冲突策略无效", nil)
		return "", false, false
	}
	dryRun, err := parseStrictBool(query.Get("dry_run"), false)
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "dry_run 参数无效", err)
		return "", false, false
	}
	return conflict, dryRun, true
}

func (h *Handler) validateImportUpload(c *gin.Context, conflict string, dryRun bool) bool {
	if conflict == "replace" && !dryRun {
		confirm := c.Request.Header.Values("X-Tgfile-Confirm-Replace")
		if len(confirm) != 1 || confirm[0] != "true" {
			h.writePublicError(
				c,
				http.StatusBadRequest,
				"replace_confirmation_required",
				"正式覆盖导入需要再次确认",
				nil,
			)
			return false
		}
	}
	if c.ContentType() != backupfmt.MediaType {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "备份文件类型无效", nil)
		return false
	}
	if requestContentLength(c.Request) < 1 {
		h.writePublicError(c, http.StatusLengthRequired, "length_required", "必须提供备份文件大小", nil)
		return false
	}
	return true
}

func (h *Handler) artifact(c *gin.Context) {
	user, ok := h.principal(c)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		return
	}
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	authorized, ok := h.authorizedJob(c, user, c.Param("job_id"))
	if !ok {
		return
	}
	filename, job, err := h.backups.Artifact(c.Request.Context(), authorized.JobID)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	file, err := os.Open(filename)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			logutil.GetLogger(c.Request.Context()).Warn(
				"close admin backup artifact",
				zap.String("error_type", fmt.Sprintf("%T", err)),
			)
		}
	}()
	stat, err := file.Stat()
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Content-Type", backupfmt.MediaType)
	c.Header("Content-Disposition", safeDisposition(job.JobID+".tgfb"))
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", `"`+job.ArtifactSHA256+`"`)
	c.Header("X-Tgfile-Artifact-SHA256", job.ArtifactSHA256)
	http.ServeContent(c.Writer, c.Request, job.JobID+".tgfb", stat.ModTime(), file)
}

func (h *Handler) authorizedJob(
	c *gin.Context,
	user principal,
	jobID string,
) (*backupmgr.Job, bool) {
	job, err := h.backups.GetJob(c.Request.Context(), jobID)
	if err != nil {
		h.writeMappedError(c, err)
		return nil, false
	}
	if user.Role != roleReadWrite && job.Owner != user.Username {
		h.writePublicError(c, http.StatusForbidden, "forbidden", "不能访问其他用户任务", nil)
		return nil, false
	}
	return job, true
}

func (h *Handler) job(user principal, job *backupmgr.Job) jobDTO {
	owner := ""
	if user.Role == roleReadWrite {
		owner = job.Owner
	}
	cancelable := job.State != "succeeded" && job.State != "failed" &&
		job.State != "canceled" && job.State != "publishing"
	if user.Role != roleReadWrite {
		cancelable = cancelable && job.Kind == "export" && job.Owner == user.Username
	}
	return jobDTO{
		JobID:             job.JobID,
		Kind:              job.Kind,
		Owner:             owner,
		State:             job.State,
		DryRun:            job.DryRun,
		Conflict:          job.Conflict,
		Scope:             job.Scope,
		ArtifactSHA256:    job.ArtifactSHA256,
		ArtifactAvailable: job.ArtifactAvailable,
		Progress:          job.Progress,
		Result:            job.Result,
		Error:             job.Error,
		CreatedAt:         job.CreatedAt,
		UpdatedAt:         job.UpdatedAt,
		CompletedAt:       job.CompletedAt,
		Cancelable:        cancelable,
	}
}

func (h *Handler) idempotencyKey(c *gin.Context) (string, bool) {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) != 1 || len(values[0]) < 1 || len(values[0]) > 256 {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "幂等键无效", nil)
		return "", false
	}
	for _, character := range values[0] {
		if character < 0x20 || character == 0x7f {
			h.writePublicError(c, http.StatusBadRequest, "invalid_request", "幂等键无效", nil)
			return "", false
		}
	}
	return values[0], true
}

func parseStrictBool(value string, fallback bool) (bool, error) {
	if value == "" {
		return fallback, nil
	}
	if value != "true" && value != "false" {
		return false, errInvalidBooleanParam
	}
	result, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse boolean parameter: %w", err)
	}
	return result, nil
}

func decodeJobCursor(value string) (backupmgr.JobCursor, bool, error) {
	if value == "" {
		return backupmgr.JobCursor{}, false, nil
	}
	if len(value) > 2048 {
		return backupmgr.JobCursor{}, false, fmt.Errorf(
			"%w: too large",
			errInvalidBackupJobCursor,
		)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return backupmgr.JobCursor{}, false, fmt.Errorf("decode job cursor: %w", err)
	}
	var cursor jobCursor
	if err := decodeStrictJSON(strings.NewReader(string(raw)), 2048, &cursor); err != nil {
		return backupmgr.JobCursor{}, false, err
	}
	if cursor.Version != 1 || cursor.CreatedAt < 1 || len(cursor.JobID) != 64 {
		return backupmgr.JobCursor{}, false, fmt.Errorf(
			"%w: invalid fields",
			errInvalidBackupJobCursor,
		)
	}
	return backupmgr.JobCursor{
		CreatedAt: cursor.CreatedAt,
		JobID:     cursor.JobID,
	}, true, nil
}

func encodeJobCursor(cursor *backupmgr.JobCursor) (string, error) {
	if cursor == nil {
		return "", nil
	}
	raw, err := jsonMarshal(jobCursor{
		Version:   1,
		CreatedAt: cursor.CreatedAt,
		JobID:     cursor.JobID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
