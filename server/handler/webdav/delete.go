package webdav

import (
	"net/http"
	"tgfile/filemgr"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func handleDelete(c *gin.Context) {
	ctx := c.Request.Context()
	root := c.Request.URL.Path
	if err := filemgr.RemoveLink(ctx, root); err != nil {
		logutil.GetLogger(ctx).Error("remove link failed", zap.Error(err), zap.String("link", root))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusNoContent)
}
