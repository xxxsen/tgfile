package dao

import (
	"context"
	"time"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/entity"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
	"github.com/xxxsen/common/idgen"
)

type ScanFileCallbackFunc func(ctx context.Context, res []*entity.FileInfoItem) (bool, error)

type IFileDao interface {
	CreateFileDraft(ctx context.Context, req *entity.CreateFileDraftRequest) (*entity.CreateFileDraftResponse, error)
	MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error)
	GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error)
	ScanFile(ctx context.Context, batch int64, cb ScanFileCallbackFunc) error
	DeleteFile(ctx context.Context, req *entity.DeleteFileRequest) (*entity.DeleteFileResponse, error)
}

type fileDaoImpl struct {
	dbc database.IDatabase
}

func NewFileDao(dbc database.IDatabase) IFileDao {
	return &fileDaoImpl{
		dbc: dbc,
	}
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
			"extinfo":         "{}",
		},
	}
	sql, args, err := builder.BuildInsert(f.table(), data)
	if err != nil {
		return nil, err
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
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
		"extinfo":    req.Extinfo,
	}
	sql, args, err := builder.BuildUpdate(f.table(), where, update)
	if err != nil {
		return nil, err
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, err
	}
	return &entity.MarkFileReadyResponse{}, nil
}

func (f *fileDaoImpl) GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error) {
	where := map[string]interface{}{
		"file_id in": req.FileIds,
	}
	rs := make([]*entity.FileInfoItem, 0, len(req.FileIds))
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	rsp := &entity.GetFileInfoResponse{List: rs}
	return rsp, nil
}

func (f *fileDaoImpl) ScanFile(ctx context.Context, batch int64, cb ScanFileCallbackFunc) error {
	var lastid uint64
	for {
		res, nextid, err := f.innerScan(ctx, lastid, batch)
		if err != nil {
			return err
		}
		next, err := cb(ctx, res)
		if err != nil {
			return err
		}
		if !next {
			break
		}
		lastid = nextid
		if len(res) < int(batch) {
			break
		}
	}
	return nil
}

func (f *fileDaoImpl) innerScan(ctx context.Context, lastid uint64, limit int64) ([]*entity.FileInfoItem, uint64, error) {
	where := map[string]interface{}{
		"id >":     lastid,
		"_orderby": "id asc",
		"_limit":   []uint{0, uint(limit)},
	}
	rs := make([]*entity.FileInfoItem, 0, limit)
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, 0, err
	}
	var nextid uint64
	if len(rs) > 0 {
		nextid = rs[len(rs)-1].Id
	}
	return rs, nextid, nil
}

func (f *fileDaoImpl) DeleteFile(ctx context.Context, req *entity.DeleteFileRequest) (*entity.DeleteFileResponse, error) {
	where := map[string]interface{}{
		"file_id in": req.FileId,
	}
	sql, args, err := builder.BuildDelete(f.table(), where)
	if err != nil {
		return nil, err
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, err
	}
	return &entity.DeleteFileResponse{}, nil
}
