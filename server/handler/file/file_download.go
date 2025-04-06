package file

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"

	"github.com/gin-gonic/gin"
)

func FileDownload(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	path, err := extractLinkFromFileKey(key)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("invalid fkey, err:%w", err))
		return
	}
	finfo, err := filemgr.ResolveLink(ctx, path)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("invalid down key, key:%s, err:%w", key, err))
		return
	}
	file, err := filemgr.Open(ctx, finfo.FileId)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("open file failed, err:%w", err))
		return
	}
	defer file.Close()
	//c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", strconv.Quote(finfo.Name())))
	http.ServeContent(c.Writer, c.Request, strconv.Quote(finfo.FileName), time.UnixMilli(finfo.Mtime), file)
}
