package webdav

import (
	"fmt"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handleDelete(c *gin.Context) {
	ctx := c.Request.Context()
	root := c.Request.URL.Path
	if err := filemgr.RemoveLink(ctx, root); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("remove link failed, err:%w", err))
		return
	}
	c.Status(http.StatusNoContent)
}
