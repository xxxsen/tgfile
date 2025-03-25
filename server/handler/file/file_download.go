package file

import (
	"fmt"
	"net/http"
	"strconv"
	"tgfile/filemgr"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func FileDownload(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	path := defaultUploadPrefix + key
	minfo, err := filemgr.ResolveLink(ctx, path)
	if err != nil {
		proxyutil.Fail(c, http.StatusBadRequest, fmt.Errorf("invalid down key, key:%s, err:%w", key, err))
		return
	}
	file, err := filemgr.Open(ctx, minfo.FileId)
	if err != nil {
		proxyutil.Fail(c, http.StatusInternalServerError, fmt.Errorf("open file failed, err:%w", err))
		return
	}
	defer file.Close()
	//c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", strconv.Quote(finfo.Name())))
	http.ServeContent(c.Writer, c.Request, strconv.Quote(minfo.Name()), minfo.ModTime(), file)
}
