package webdav

import (
	"fmt"
	"net/http"
	"strconv"
	"tgfile/filemgr"
	"tgfile/proxyutil"
	"time"

	"github.com/gin-gonic/gin"
)

func handleHead(c *gin.Context) {
	ctx := c.Request.Context()
	file := c.Request.URL.Path
	item, err := filemgr.ResolveLink(ctx, file)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("decode link info failed, link:%s, err:%w", file, err))
		return
	}
	// if item.IsDir {
	// 	logutil.GetLogger(ctx).Error("cant open stream on dir", zap.Error(err))
	// 	c.AbortWithStatus(http.StatusMethodNotAllowed)
	// 	return
	// }
	if !item.IsDir {
		c.Writer.Header().Set("Content-Length", strconv.FormatInt(item.FileSize, 10))
	}
	if item.IsDir {
		c.Writer.Header().Set("Content-Type", "text/plain")
	}
	//TODO: try set etag...
	c.Writer.Header().Set("Last-Modified", time.UnixMilli(item.Mtime).UTC().Format(http.TimeFormat))
}
