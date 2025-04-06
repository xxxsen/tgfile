package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"

	"github.com/gin-gonic/gin"
)

func (h *webdavHandler) handleDelete(c *gin.Context) {
	ctx := c.Request.Context()
	root := h.buildSrcPath(c)
	if err := filemgr.RemoveLink(ctx, root); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("remove link failed, err:%w", err))
		return
	}
	c.Status(http.StatusNoContent)
}
