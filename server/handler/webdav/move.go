package webdav

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"tgfile/filemgr"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handleMove(c *gin.Context) {
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
	if src == dst {
		proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("src equal to dst"))
		return
	}
	if strings.HasPrefix(dst, src) {
		proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("src path should not be the prefix of dst"))
		return
	}
	if !checkSameWebdavRoot(src, dst) {
		proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("dst not in webdav root"))
		return
	}
	if err := filemgr.RenameLink(ctx, src, dst, isOverWrite); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("rename link failed, src:%s, dst:%s, err:%w", src, dst, err))
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
