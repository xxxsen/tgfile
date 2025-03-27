package webdav

import (
	"net/http"
	"tgfile/filemgr"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func Put(c *gin.Context) {
	ctx := c.Request.Context()
	file := c.Request.URL.Path
	fileid, err := filemgr.Create(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		logutil.GetLogger(ctx).Error("create file failed", zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if err := filemgr.CreateLink(ctx, file, fileid, c.Request.ContentLength, false); err != nil {
		logutil.GetLogger(ctx).Error("create file link failed", zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusCreated)
}
