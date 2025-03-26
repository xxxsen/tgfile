package dao

import (
	"context"
	"fmt"
	"tgfile/constant"
	"tgfile/db"
	"tgfile/entity"
	"time"

	"github.com/xxxsen/common/database/kv"
	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

type IFileDao interface {
	CreateFileDraft(ctx context.Context, req *entity.CreateFileDraftRequest) (*entity.CreateFileDraftResponse, error)
	MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error)
	GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error)
}

type fileDaoImpl struct{}

func NewFileDao() IFileDao {
	return &fileDaoImpl{}
}

func (f *fileDaoImpl) table() string {
	return "tg_file_tab"
}

func (f *fileDaoImpl) buildKey(fileid uint64) string {
	return fmt.Sprintf("tgfile:file:%d", fileid)
}

func (f *fileDaoImpl) CreateFileDraft(ctx context.Context, req *entity.CreateFileDraftRequest) (*entity.CreateFileDraftResponse, error) {
	fileid := idgen.NextId()
	logutil.GetLogger(ctx).Debug("create file part", zap.Uint64("fileid", fileid), zap.Int64("size", req.FileSize))
	now := time.Now().UnixMilli()
	item := &entity.FileInfoItem{
		FileId:        fileid,
		FileSize:      req.FileSize,
		FilePartCount: req.FilePartCount,
		Ctime:         now,
		Mtime:         now,
		FileState:     constant.FileStateInit,
	}
	if err := kv.SetJsonObject(ctx, db.GetClient(), f.table(), f.buildKey(fileid), item); err != nil {
		return nil, err
	}
	return &entity.CreateFileDraftResponse{
		FileId: fileid,
	}, nil
}

func (f *fileDaoImpl) MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error) {
	if err := kv.OnGetJsonKeyForUpdate(ctx, db.GetClient(), f.table(), f.buildKey(req.FileID), 0, func(ctx context.Context, key string, val *entity.FileInfoItem) (*entity.FileInfoItem, bool, error) {
		if val.FileState != constant.FileStateInit {
			return nil, false, fmt.Errorf("file not in init state, current state:%d", val.FileState)
		}
		val.FileState = constant.FileStateReady
		val.Mtime = time.Now().UnixMilli()
		return val, true, nil
	}); err != nil {
		return nil, err
	}
	return &entity.MarkFileReadyResponse{}, nil
}

func (f *fileDaoImpl) GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error) {
	ks := make([]string, 0, len(req.FileIds))
	mapping := make(map[uint64]string, len(req.FileIds))
	for _, fileid := range req.FileIds {
		key := f.buildKey(fileid)
		ks = append(ks, key)
		mapping[fileid] = key
	}
	rs, err := kv.MultiGetJsonObject[entity.FileInfoItem](ctx, db.GetClient(), f.table(), ks)
	if err != nil {
		return nil, err
	}
	rsp := &entity.GetFileInfoResponse{List: make([]*entity.FileInfoItem, 0, len(rs))}
	for _, fileid := range req.FileIds {
		v, ok := rs[mapping[fileid]]
		if !ok {
			continue
		}
		rsp.List = append(rsp.List, v)
	}
	return rsp, nil
}
