package webdav

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/server/httpkit"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handleGet(c *gin.Context) {
	ctx := c.Request.Context()
	file := h.buildSrcPath(c)
	item, err := h.fmgr.ResolveFileLink(ctx, file)
	if errors.Is(err, os.ErrNotExist) {
		proxyutil.FailStatus(c, http.StatusNotFound, err)
		return
	}
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("read link info failed, err:%w", err))
		return
	}
	if item.IsDir {
		proxyutil.FailStatus(c, http.StatusMethodNotAllowed, fmt.Errorf("cant open stream on dir"))
		return
	}
	stream, err := h.fmgr.OpenFile(ctx, item.FileId)
	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("open stream failed, err:%w", err))
		return
	}
	defer stream.Close()
	httpkit.SetDefaultDownloadHeader(c, item)
	http.ServeContent(c.Writer, c.Request, strconv.Quote(item.FileName), time.UnixMilli(item.Mtime), stream)
}
