package s3

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
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
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		s3base.WriteError(c, status, fmt.Errorf("get mapping info fail, err:%w", err))
		return
	}
	if finfo.IsDir {
		s3base.WriteError(c, http.StatusNotFound, fmt.Errorf("object is a directory: %s", filename))
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

func (h *S3Handler) HeadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	finfo, err := h.fmgr.StatFileLink(ctx, filename)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		s3base.WriteHeadError(c, status, fmt.Errorf("get mapping info fail, err:%w", err))
		return
	}
	if finfo.IsDir {
		s3base.WriteHeadError(c, http.StatusNotFound, fmt.Errorf("object is a directory: %s", filename))
		return
	}

	httpkit.SetDefaultDownloadHeader(c, finfo)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Length", strconv.FormatInt(finfo.FileSize, 10))
	c.Header("Last-Modified", time.UnixMilli(finfo.Mtime).UTC().Format(http.TimeFormat))
	c.Status(http.StatusOK)
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
