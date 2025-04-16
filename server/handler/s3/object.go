package s3

import (
	"fmt"
	"net/http"
	"time"

	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
	"github.com/xxxsen/tgfile/server/httpkit"

	"github.com/gin-gonic/gin"
)

func (h *S3Handler) DownloadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	finfo, err := h.fmgr.StatFileLink(ctx, filename)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("get mapping info fail, err:%w", err))
		return
	}
	file, err := h.fmgr.OpenFile(ctx, finfo.FileId)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer file.Close()
	httpkit.SetDefaultDownloadHeader(c, finfo)
	http.ServeContent(c.Writer, c.Request, finfo.FileName, time.UnixMilli(finfo.Mtime), file)
}

func (h *S3Handler) UploadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	fileid, err := h.fmgr.CreateFile(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("do file upload fail, err:%w", err))
		return
	}
	if err := h.fmgr.CreateFileLink(ctx, filename, fileid, c.Request.ContentLength, false); err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("create mapping fail, err:%w", err))
		return
	}
	//TODO: 确认下, 不写etag是否会有问题
	c.Writer.WriteHeader(http.StatusOK)
}
