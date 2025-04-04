package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/xxxsen/tgfile/entity"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
)

type IFilePartDao interface {
	CreateFilePart(ctx context.Context, req *entity.CreateFilePartRequest) (*entity.CreateFilePartResponse, error)
	GetFilePartInfo(ctx context.Context, req *entity.GetFilePartInfoRequest) (*entity.GetFilePartInfoResponse, error)
	DeleteFilePart(ctx context.Context, req *entity.DeleteFilePartRequest) (*entity.DeleteFilePartResponse, error)
}

type filePartDaoImpl struct {
	dbc database.IDatabase
}

func NewFilePartDao(dbc database.IDatabase) IFilePartDao {
	return &filePartDaoImpl{
		dbc: dbc,
	}
}

func (f *filePartDaoImpl) table() string {
	return "tg_file_part_tab"
}

func (f *filePartDaoImpl) CreateFilePart(ctx context.Context, req *entity.CreateFilePartRequest) (*entity.CreateFilePartResponse, error) {
	now := time.Now().UnixMilli()
	data := []map[string]interface{}{
		{
			"file_id":      req.FileId,
			"file_part_id": req.FilePartId,
			"ctime":        now,
			"mtime":        now,
			"file_key":     req.FileKey,
		},
	}
	sql, args, err := builder.BuildInsert(f.table(), data)
	if err != nil {
		return nil, err
	}
	_, insertErr := f.dbc.ExecContext(ctx, sql, args...)
	if insertErr == nil {
		return nil, insertErr
	}
	where := map[string]interface{}{
		"file_id":      req.FileId,
		"file_part_id": req.FilePartId,
	}
	update := map[string]interface{}{
		"file_key": req.FileKey,
		"mtime":    now,
	}
	sql, args, err = builder.BuildUpdate(f.table(), where, update)
	if err != nil {
		return nil, err
	}
	rs, err := f.dbc.ExecContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	affect, err := rs.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affect == 0 {
		return nil, fmt.Errorf("insert on duplicate key update no affect rows, insert err:%w", insertErr)
	}
	return &entity.CreateFilePartResponse{}, nil
}

func (f *filePartDaoImpl) GetFilePartInfo(ctx context.Context, req *entity.GetFilePartInfoRequest) (*entity.GetFilePartInfoResponse, error) {
	where := map[string]interface{}{
		"file_id":      req.FileId,
		"file_part_id": req.FilePartId,
	}

	rs := make([]*entity.FilePartInfoItem, 0, len(req.FilePartId))
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	return &entity.GetFilePartInfoResponse{List: rs}, nil

}

func (f *filePartDaoImpl) DeleteFilePart(ctx context.Context, req *entity.DeleteFilePartRequest) (*entity.DeleteFilePartResponse, error) {
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
	return &entity.DeleteFilePartResponse{}, nil
}
