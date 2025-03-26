package dao

import (
	"context"
	"fmt"
	"tgfile/db"
	"tgfile/entity"
	"time"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database/dbkit"
)

type IFilePartDao interface {
	CreateFilePart(ctx context.Context, req *entity.CreateFilePartRequest) (*entity.CreateFilePartResponse, error)
	GetFilePartInfo(ctx context.Context, req *entity.GetFilePartInfoRequest) (*entity.GetFilePartInfoResponse, error)
}

type filePartDaoImpl struct {
}

func NewFilePartDao() IFilePartDao {
	return &filePartDaoImpl{}
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
	_, insertErr := db.GetClient().ExecContext(ctx, sql, args...)
	if err == nil {
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
	rs, err := db.GetClient().ExecContext(ctx, sql, args...)
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
	if err := dbkit.SimpleQuery(ctx, db.GetClient(), f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	return &entity.GetFilePartInfoResponse{List: rs}, nil

}
