package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/xxxsen/common/logutil"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
	"github.com/xxxsen/tgfile/server/httpkit"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

var (
	errObjectIsDirectory    = errors.New("object is a directory")
	errContentLengthMissing = errors.New("content length is required")
	errObjectAlreadyExists  = errors.New("object already exists")
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
		s3base.WriteError(c, http.StatusNotFound, fmt.Errorf("%w: %s", errObjectIsDirectory, filename))
		return
	}
	file, err := h.fmgr.OpenFile(ctx, finfo.FileId)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer logCloseError(ctx, file, "close S3 download")
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
		s3base.WriteHeadError(c, http.StatusNotFound, fmt.Errorf("%w: %s", errObjectIsDirectory, filename))
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
	if c.Request.ContentLength < 0 {
		s3base.WriteError(c, http.StatusLengthRequired, errContentLengthMissing)
		return
	}
	filename := path.Clean(c.Request.URL.Path)
	unlock := h.locks.lock(filename)
	defer unlock()

	if _, err := h.fmgr.StatFileLink(ctx, filename); err == nil {
		s3base.WriteError(c, http.StatusConflict, fmt.Errorf("%w: %s", errObjectAlreadyExists, filename))
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("check object mapping fail, err:%w", err))
		return
	}

	fileid, err := h.fmgr.CreateFile(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, filemgr.ErrInvalidFileSize) ||
			errors.Is(err, filemgr.ErrTooManyFileParts) ||
			errors.Is(err, filemgr.ErrFileShortRead) {
			status = http.StatusBadRequest
		}
		s3base.WriteError(c, status, fmt.Errorf("do file upload fail, err:%w", err))
		return
	}
	if err := h.fmgr.CreateFileLink(ctx, filename, fileid, c.Request.ContentLength, false); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrExist) {
			status = http.StatusConflict
		}
		s3base.WriteError(c, status, fmt.Errorf("create mapping fail, err:%w", err))
		return
	}
	// TODO: 确认下, 不写etag是否会有问题
	c.Writer.WriteHeader(http.StatusOK)
}

func logCloseError(ctx context.Context, closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		logutil.GetLogger(ctx).Error(message, zap.Error(err))
	}
}
