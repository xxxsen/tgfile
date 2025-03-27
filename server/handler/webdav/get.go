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

func Get(c *gin.Context) {
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
	stream, err := filemgr.Open(ctx, item.FileId)
	if err != nil {
		logutil.GetLogger(ctx).Error("open stream failed", zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	http.ServeContent(c.Writer, c.Request, strconv.Quote(item.FileName), time.UnixMilli(item.Mtime), stream)
}
