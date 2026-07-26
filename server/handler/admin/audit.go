package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/trace"
	"go.uber.org/zap"
)

const auditPathKey = "tgfile-admin-audit-path"

func (h *Handler) auditRequest(c *gin.Context) {
	started := time.Now()
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Cache-Control", "no-store")
	c.Next()

	action := c.Request.Method + " " + c.FullPath()
	if c.FullPath() == "" {
		action = c.Request.Method + " unmatched"
	}
	requestID, _ := trace.GetTraceId(c.Request.Context())
	result := "success"
	if c.Writer.Status() >= http.StatusBadRequest {
		result = "error"
	}
	responseBytes := c.Writer.Size()
	if responseBytes < 0 {
		responseBytes = 0
	}
	fields := []zap.Field{
		zap.String("request_id", requestID),
		zap.String("action", action),
		zap.String("result", result),
		zap.Int("status_code", c.Writer.Status()),
		zap.Int64("request_bytes", requestContentLength(c.Request)),
		zap.Int("response_bytes", responseBytes),
		zap.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if user, ok := h.principal(c); ok {
		fields = append(
			fields,
			zap.String("username", user.Username),
			zap.String("role", user.Role),
		)
	}
	if jobID := c.Param("job_id"); jobID != "" {
		fields = append(fields, zap.String("job_id", jobID))
	}
	if value, exists := c.Get(auditPathKey); exists {
		if resourcePath, valid := value.(string); valid {
			digest := sha256.Sum256([]byte(resourcePath))
			fields = append(
				fields,
				zap.String("path_sha256", hex.EncodeToString(digest[:])),
				zap.String("top_level_namespace", topLevelNamespace(resourcePath)),
			)
		}
	}
	logutil.GetLogger(c.Request.Context()).Info("admin request", fields...)
}

func setAuditPath(c *gin.Context, resourcePath string) {
	c.Set(auditPathKey, resourcePath)
}

func topLevelNamespace(resourcePath string) string {
	trimmed := strings.TrimPrefix(resourcePath, "/")
	if trimmed == "" {
		return "/"
	}
	namespace, _, _ := strings.Cut(trimmed, "/")
	return namespace
}
