package file

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func (h *FileHandler) FileUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.UploadFileRequest)
	header := req.File
	file, err := header.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer file.Close()
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
