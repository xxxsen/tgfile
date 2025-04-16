package file

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func (h *FileHandler) BeginUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.BeginUploadRequest)
	if len(req.FileName) == 0 {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("no file name found"))
		return
	}
	fileid, blocksize, err := h.m.CreateFileDraft(ctx, req.FileSize)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create draft failed, err:%w", err))
		return
	}
	fctx := &model.MultiPartUploadContext{
		FileName:   req.FileName,
		CreateTime: time.Now().UnixMilli(),
		FileId:     fileid,
		FileSize:   req.FileSize,
		BlockSize:  blocksize,
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

func (h *FileHandler) PartUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.PartUploadRequest)
	logutil.GetLogger(ctx).Debug("recv file part upload request", zap.String("upload_key", req.UploadKey), zap.Int64("part_id", *req.PartId), zap.Int64("size", req.PartData.Size))
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
	if err := h.m.CreateFilePart(ctx, fctx.FileId, *req.PartId, f); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("upload part failed, err:%w", err))
		return
	}
	proxyutil.SuccessJson(c, &model.PartUploadResponse{})
}

func (h *FileHandler) FinishUpload(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.FinishUploadRequest)
	fctx := &model.MultiPartUploadContext{}
	if err := fctx.Decode(req.UploadKey); err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("decode file key failed, key:%s, err:%w", req.UploadKey, err))
		return
	}
	if err := h.m.FinishFileCreate(ctx, fctx.FileId); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("finish upload failed, err:%w", err))
		return
	}
	path, fileKey := h.buildFileKeyLink(fctx.FileName, fctx.FileId)
	if err := h.m.CreateFileLink(ctx, path, fctx.FileId, fctx.FileSize, false); err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("create link failed, err:%w", err))
		return
	}

	logutil.GetLogger(ctx).Info("finish big file upload", zap.Uint64("file_id", fctx.FileId), zap.Int64("file_size", fctx.FileSize))
	proxyutil.SuccessJson(c, &model.FinishUploadResponse{
		FileKey: fileKey,
	})
}
