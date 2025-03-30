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

func (h *webdavHandler) handleGet(c *gin.Context) {
	ctx := c.Request.Context()
	file := h.buildSrcPath(c)
	item, err := filemgr.ResolveLink(ctx, file)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("read link info failed, err:%w", err))
		return
	}
	if item.IsDir {
		proxyutil.FailStatus(c, http.StatusMethodNotAllowed, fmt.Errorf("cant open stream on dir"))
		return
	}
	stream, err := filemgr.Open(ctx, item.FileId)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("open stream failed, err:%w", err))
		return
	}
	defer stream.Close()
	http.ServeContent(c.Writer, c.Request, strconv.Quote(item.FileName), time.UnixMilli(item.Mtime), stream)
}
