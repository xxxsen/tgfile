package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"

	"github.com/gin-gonic/gin"
)

func (h *webdavHandler) handleCopy(c *gin.Context) {
	ctx := c.Request.Context()
	src := h.buildSrcPath(c)
	isOverWrite := c.GetHeader("Overwrite") != "F"
	dst, err := h.tryBuildDstPath(c)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("build dst path failed, err:%w", err))
		return
	}
	if err := filemgr.CopyFileLink(ctx, src, dst, isOverWrite); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("rename link failed, src:%s, dst:%s, err:%w", src, dst, err))
		return
	}
	c.Status(http.StatusCreated)
}
