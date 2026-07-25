package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/webapi/proxyutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

var errInvalidUploadRequest = errors.New("invalid upload request")

func (h *FileHandler) FileUpload(ctx context.Context, c *gin.Context, request any) {
	req, ok := request.(*model.UploadFileRequest)
	if !ok {
		proxyutil.FailJson(c, http.StatusInternalServerError, errInvalidUploadRequest)
		return
	}
	header := req.File
	file, err := header.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer logCloseError(ctx, file, "close uploaded file")
	fileid, err := h.m.CreateFile(ctx, header.Size, file)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("upload file fail, err:%w", err))
		return
	}
	path, key := h.buildFileKeyLink(header.Filename, fileid)
	if err := h.m.CreateFileLink(ctx, path, fileid, header.Size, false); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}

	proxyutil.SuccessJson(c, &model.UploadFileResponse{
		Key: key,
	})
}

func logCloseError(ctx context.Context, closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		logutil.GetLogger(ctx).Error(message, zap.Error(err))
	}
}
