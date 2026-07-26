package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/trace"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/filemgr"
)

type mappedAdminError struct {
	target  error
	status  int
	code    string
	message string
}

var mappedAdminErrors = []mappedAdminError{
	{errMalformedJSON, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{backupmgr.ErrInvalidRequest, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{backupfmt.ErrChecksum, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{filemgr.ErrInvalidFileSize, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{filemgr.ErrFileShortRead, http.StatusBadRequest, "invalid_request", "请求体短于声明长度"},
	{filemgr.ErrInvalidFileLinkPage, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{directory.ErrInvalidPath, http.StatusBadRequest, "invalid_request", "请求参数无效"},
	{backupmgr.ErrJobNotFound, http.StatusNotFound, "job_not_found", "备份任务不存在"},
	{os.ErrNotExist, http.StatusNotFound, "not_found", "资源不存在"},
	{directory.ErrSourceNotFound, http.StatusNotFound, "not_found", "资源不存在"},
	{filemgr.ErrNotDirectory, http.StatusConflict, "not_directory", "目标不是目录"},
	{directory.ErrPathComponentNotDirectory, http.StatusConflict, "not_directory", "目标不是目录"},
	{filemgr.ErrDirectoryIO, http.StatusConflict, "target_is_directory", "目标是目录"},
	{directory.ErrEntryNotFile, http.StatusConflict, "target_is_directory", "目标是目录"},
	{filemgr.ErrFileLinkCursorStale, http.StatusConflict, "cursor_stale", "目录已变化，请刷新"},
	{
		filemgr.ErrWebDAVPrecondition,
		http.StatusPreconditionFailed,
		"precondition_failed",
		"目标已发生变化",
	},
	{filemgr.ErrWebDAVLocked, http.StatusLocked, "locked", "目标已被 WebDAV 锁定"},
	{filemgr.ErrWebDAVQuota, http.StatusInsufficientStorage, "quota_exceeded", "操作超过服务限制"},
	{filemgr.ErrWebDAVTooManyItems, http.StatusInsufficientStorage, "quota_exceeded", "操作超过服务限制"},
	{backupmgr.ErrIdempotencyConflict, http.StatusConflict, "job_conflict", "幂等键与已有任务冲突"},
	{backupmgr.ErrJobNotCancelable, http.StatusConflict, "job_not_ready", "任务当前不能执行该操作"},
	{backupmgr.ErrArtifactUnavailable, http.StatusConflict, "job_not_ready", "任务当前不能执行该操作"},
	{backupfmt.ErrLimitExceeded, http.StatusRequestEntityTooLarge, "payload_too_large", "数据超过配置限制"},
	{filemgr.ErrTooManyFileParts, http.StatusRequestEntityTooLarge, "payload_too_large", "数据超过配置限制"},
	{syscall.ENOSPC, http.StatusInsufficientStorage, "insufficient_storage", "存储空间不足"},
	{errSessionCapacity, http.StatusServiceUnavailable, "session_capacity", "会话容量已满"},
	{context.Canceled, http.StatusServiceUnavailable, "backend_unavailable", "请求已取消"},
	{context.DeadlineExceeded, http.StatusServiceUnavailable, "backend_unavailable", "后端暂不可用"},
}

func (h *Handler) writeData(c *gin.Context, status int, data any) {
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Cache-Control", "no-store")
	c.JSON(status, responseEnvelope{Data: data})
}

func (h *Handler) writeMappedError(c *gin.Context, err error) {
	for _, mapping := range mappedAdminErrors {
		if errors.Is(err, mapping.target) {
			h.writePublicError(c, mapping.status, mapping.code, mapping.message, err)
			return
		}
	}
	h.writePublicError(c, http.StatusInternalServerError, "internal_error", "服务内部错误", err)
}

func (h *Handler) writePublicError(
	c *gin.Context,
	status int,
	code, message string,
	cause error,
) {
	requestID, exists := trace.GetTraceId(c.Request.Context())
	if !exists || requestID == "" || len(requestID) > 128 {
		token, err := randomURLToken(16)
		if err == nil {
			requestID = token
		} else {
			requestID = "unavailable"
		}
	}
	c.Header("X-Request-ID", requestID)
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Cache-Control", "no-store")
	if cause != nil {
		fields := []zap.Field{
			zap.String("request_id", requestID),
			zap.String("admin_error_code", code),
			zap.Int("status_code", status),
			zap.String("cause_type", safeAdminErrorType(cause)),
		}
		if user, ok := h.principal(c); ok {
			fields = append(fields, zap.String("username", user.Username))
		}
		logutil.GetLogger(c.Request.Context()).Warn("admin request failed", fields...)
	}
	c.AbortWithStatusJSON(status, errorEnvelope{
		Error: publicError{Code: code, Message: message, RequestID: requestID},
	})
}

func safeAdminErrorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
