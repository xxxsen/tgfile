package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *webdavHandler) handleMkcol(c *gin.Context) {
	ctx := c.Request.Context()
	if len(c.GetHeader("Content-Type")) != 0 || c.Request.ContentLength != 0 {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("could not mkdir on file"))
		return
	}
	file := h.buildSrcPath(c)
	if err := filemgr.CreateLink(ctx, file, 0, 0, true); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create link failed, link:%s, err:%w", file, err))
		return
	}
	c.Status(http.StatusCreated)
}
