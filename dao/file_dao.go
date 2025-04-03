package dao

import (
	"context"
	"time"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/entity"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database/dbkit"
	"github.com/xxxsen/common/idgen"
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

func (f *fileDaoImpl) CreateFileDraft(ctx context.Context, req *entity.CreateFileDraftRequest) (*entity.CreateFileDraftResponse, error) {
	fileid := idgen.NextId()
	now := time.Now().UnixMilli()
	data := []map[string]interface{}{
		{
			"file_id":         fileid,
			"file_size":       req.FileSize,
			"file_part_count": req.FilePartCount,
			"ctime":           now,
			"mtime":           now,
			"file_state":      constant.FileStateInit,
		},
	}
	sql, args, err := builder.BuildInsert(f.table(), data)
	if err != nil {
		return nil, err
	}
	if _, err := db.GetClient().ExecContext(ctx, sql, args...); err != nil {
		return nil, err
	}
	return &entity.CreateFileDraftResponse{
		FileId: fileid,
	}, nil
}

func (f *fileDaoImpl) MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error) {
	where := map[string]interface{}{
		"file_id": req.FileID,
	}
	update := map[string]interface{}{
		"file_state": constant.FileStateReady,
		"mtime":      time.Now().UnixMilli(),
	}
	sql, args, err := builder.BuildUpdate(f.table(), where, update)
	if err != nil {
		return nil, err
	}
	if _, err := db.GetClient().ExecContext(ctx, sql, args...); err != nil {
		return nil, err
	}
	return &entity.MarkFileReadyResponse{}, nil
}

func (f *fileDaoImpl) GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error) {
	where := map[string]interface{}{
		"file_id in": req.FileIds,
	}
	rs := make([]*entity.FileInfoItem, 0, len(req.FileIds))
	if err := dbkit.SimpleQuery(ctx, db.GetClient(), f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	rsp := &entity.GetFileInfoResponse{List: rs}
	return rsp, nil
}
