package s3

import (
	"fmt"
	"net/http"
	"time"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
	"github.com/xxxsen/tgfile/server/httpkit"

	"github.com/gin-gonic/gin"
)

func DownloadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	finfo, err := filemgr.ResolveLink(ctx, filename)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("get mapping info fail, err:%w", err))
		return
	}
	file, err := filemgr.Open(ctx, finfo.FileId)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer file.Close()
	httpkit.SetDefaultDownloadHeader(c, finfo)
	http.ServeContent(c.Writer, c.Request, finfo.FileName, time.UnixMilli(finfo.Mtime), file)
}

func UploadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	fileid, err := filemgr.Create(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("do file upload fail, err:%w", err))
		return
	}
	if err := filemgr.CreateLink(ctx, filename, fileid, c.Request.ContentLength, false); err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("create mapping fail, err:%w", err))
		return
	}
	//TODO: 确认下, 不写etag是否会有问题
	c.Writer.WriteHeader(http.StatusOK)
}
