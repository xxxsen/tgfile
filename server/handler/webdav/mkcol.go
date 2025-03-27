package webdav

import (
	"net/http"
	"tgfile/filemgr"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func Mkcol(c *gin.Context) {
	ctx := c.Request.Context()
	if len(c.GetHeader("Content-Type")) != 0 || c.Request.ContentLength != 0 {
		logutil.GetLogger(ctx).Error("could not mkcol on file")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	file := c.Request.URL.Path
	if err := filemgr.CreateLink(ctx, file, 0, 0, true); err != nil {
		logutil.GetLogger(ctx).Error("create link failed", zap.String("link", file), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusCreated)
}
