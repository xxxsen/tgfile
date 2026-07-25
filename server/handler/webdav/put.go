package webdav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handlePut(c *gin.Context) {
	ctx := c.Request.Context()
	file := h.buildSrcPath(c)
	length := c.Request.ContentLength
	reader := c.Request.Body
	fileid, err := h.fmgr.CreateFile(ctx, length, reader)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create file failed, err:%w", err))
		return
	}
	if err := h.fmgr.CreateFileLink(ctx, file, fileid, length, false); err != nil {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		cleanupErr := h.fmgr.DiscardUnpublishedFile(cleanupContext, fileid)
		cancel()
		proxyutil.FailStatus(
			c,
			http.StatusInternalServerError,
			fmt.Errorf("create link failed: %w", errors.Join(err, cleanupErr)),
		)
		return
	}
	c.Status(http.StatusCreated)
}
