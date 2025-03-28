package webdav

import (
	"fmt"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handlePut(c *gin.Context) {
	ctx := c.Request.Context()
	file := c.Request.URL.Path
	fileid, err := filemgr.Create(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create file failed, err:%w", err))
		return
	}
	if err := filemgr.CreateLink(ctx, file, fileid, c.Request.ContentLength, false); err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}
	c.Status(http.StatusCreated)
}
