package webdav

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handleMove(c *gin.Context) {
	ctx := c.Request.Context()
	src := h.buildSrcPath(c)
	isOverWrite := c.GetHeader("Overwrite") != "F"
	dst, err := h.tryBuildDstPath(c)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("build dst path failed, err:%w", err))
		return
	}
	if err := h.fmgr.RenameFileLink(ctx, src, dst, isOverWrite); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("rename link failed, src:%s, dst:%s, err:%w", src, dst, err))
		return
	}
	c.Status(http.StatusCreated)
}

func (h *WebdavHandler) checkSameWebdavRoot(src string, dst string) bool {
	src = strings.TrimPrefix(src, "/")
	idx := strings.Index(src, "/")
	if idx < 0 {
		return false
	}
	root := src[:idx]
	dst = strings.TrimPrefix(dst, "/")
	return strings.HasPrefix(dst, root)
}
