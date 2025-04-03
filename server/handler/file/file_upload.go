package file

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/xxxsen/tgfile/server/model"
	"github.com/xxxsen/tgfile/utils"

	"github.com/gin-gonic/gin"
)

const (
	defaultUploadPrefix = "/defauls/"
)

func FileUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.UploadFileRequest)
	header := req.File
	file, err := header.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer file.Close()
	fileid, err := filemgr.Create(ctx, header.Size, file)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("upload file fail, err:%w", err))
		return
	}
	key := utils.EncodeFileId(fileid)

	path := defaultUploadPrefix + key
	if err := filemgr.CreateLink(ctx, path, fileid, header.Size, false); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}

	proxyutil.SuccessJson(c, &model.UploadFileResponse{
		Key: key,
	})
}
