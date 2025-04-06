package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"

	"github.com/gin-gonic/gin"
)

func (h *webdavHandler) handlePut(c *gin.Context) {
	ctx := c.Request.Context()
	file := h.buildSrcPath(c)
	length := c.Request.ContentLength
	reader := c.Request.Body
	fileid, err := filemgr.Create(ctx, length, reader)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create file failed, err:%w", err))
		return
	}
	if err := filemgr.CreateLink(ctx, file, fileid, length, false); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}
	c.Status(http.StatusCreated)
}
