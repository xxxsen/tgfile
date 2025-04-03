package file

import (
	"context"
	"fmt"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"
	"tgfile/server/model"
	"tgfile/utils"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func BeginUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.BeginUploadRequest)
	fileid, blocksize, err := filemgr.CreateDraft(ctx, req.FileSize)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create draft failed, err:%w", err))
		return
	}
	fctx := &model.MultiPartUploadContext{
		FileId:    fileid,
		FileSize:  req.FileSize,
		BlockSize: blocksize,
	}
	key, err := fctx.Encode()
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("encode file key failed, err:%w", err))
		return
	}
	proxyutil.SuccessJson(c, &model.BeginUploadResponse{
		UploadKey: key,
		BlockSize: blocksize,
	})
}

func PartUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.PartUploadRequest)
	f, err := req.PartData.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer f.Close()
	fctx := &model.MultiPartUploadContext{}
	if err := fctx.Decode(req.UploadKey); err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("decode file key failed, err:%w", err))
		return
	}
	if err := filemgr.CreatePart(ctx, fctx.FileId, *req.PartId, f); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("upload part failed, err:%w", err))
		return
	}
	proxyutil.SuccessJson(c, &model.PartUploadResponse{})
}

func FinishUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.FinishUploadRequest)
	fctx := &model.MultiPartUploadContext{}
	if err := fctx.Decode(req.UploadKey); err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("decode file key failed, err:%w", err))
		return
	}
	if err := filemgr.FinishCreate(ctx, fctx.FileId); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("finish upload failed, err:%w", err))
		return
	}
	fileKey := utils.EncodeFileId(fctx.FileId)
	path := defaultUploadPrefix + fileKey
	if err := filemgr.CreateLink(ctx, path, fctx.FileId, fctx.FileSize, false); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}

	logutil.GetLogger(ctx).Info("finish big file upload", zap.Uint64("file_id", fctx.FileId), zap.Int64("file_size", fctx.FileSize))
	proxyutil.SuccessJson(c, &model.FinishUploadResponse{
		FileKey: fileKey,
	})
}
