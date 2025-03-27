package webdav

import (
	"net/http"
	"strconv"
	"tgfile/filemgr"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func handleHead(c *gin.Context) {
	ctx := c.Request.Context()
	file := c.Request.URL.Path
	item, err := filemgr.ResolveLink(ctx, file)
	if err != nil {
		logutil.GetLogger(ctx).Error("read link info failed", zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if item.IsDir {
		logutil.GetLogger(ctx).Error("cant open stream on dir", zap.Error(err))
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}
	c.Writer.Header().Set("Content-Length", strconv.FormatInt(item.FileSize, 10))
	//TODO: try set etag...
	c.Writer.Header().Set("Last-Modified", time.UnixMilli(item.Mtime).UTC().Format(http.TimeFormat))
}
