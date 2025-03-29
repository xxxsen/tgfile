package webdav

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"tgfile/filemgr"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handleCopy(c *gin.Context) {
	ctx := c.Request.Context()
	src := path.Clean(c.Request.URL.Path)
	dstlink := c.GetHeader("Destination")
	isOverWrite := c.GetHeader("Overwrite") != "F"
	dsturi, err := url.Parse(dstlink)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("parse dst failed, dst:%s, err:%w", dstlink, err))
		return
	}
	dst := path.Clean(dsturi.Path)
	if !checkSameWebdavRoot(src, dst) {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("dst not in webdav root"))
		return
	}
	if err := filemgr.CopyLink(ctx, src, dst, isOverWrite); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("rename link failed, src:%s, dst:%s, err:%w", src, dst, err))
		return
	}
	c.Status(http.StatusCreated)
}
