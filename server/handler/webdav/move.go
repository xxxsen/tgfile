package webdav

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"tgfile/filemgr"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func handleMove(c *gin.Context) {
	ctx := c.Request.Context()
	src := path.Clean(c.Request.URL.Path)
	dstlink := c.GetHeader("Destination")
	isOverWrite := c.GetHeader("Overwrite") != "F"
	dsturi, err := url.Parse(dstlink)
	if err != nil {
		logutil.GetLogger(ctx).Error("parse dst link failed", zap.Error(err), zap.String("dstlink", dstlink))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	dst := path.Clean(dsturi.Path)
	if src == dst {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	if strings.HasPrefix(dst, src) {
		logutil.GetLogger(ctx).Error("src path should not be the prefix of dst")
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	if !checkSameWebdavRoot(src, dst) {
		logutil.GetLogger(ctx).Error("dst not in webdav root")
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if err := filemgr.RenameLink(ctx, src, dst, isOverWrite); err != nil {
		logutil.GetLogger(ctx).Error("rename link failed", zap.Error(err), zap.String("src", src), zap.String("dst", dst))
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	c.Status(http.StatusCreated)
}

func checkSameWebdavRoot(src string, dst string) bool {
	src = strings.TrimPrefix(src, "/")
	idx := strings.Index(src, "/")
	if idx < 0 {
		return false
	}
	root := src[:idx]
	dst = strings.TrimPrefix(dst, "/")
	return strings.HasPrefix(dst, root)
}
