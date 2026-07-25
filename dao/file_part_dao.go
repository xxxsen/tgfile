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
	ListFilePart(ctx context.Context, req *entity.ListFilePartRequest) (*entity.ListFilePartResponse, error)
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

func (f *filePartDaoImpl) CreateFilePart(
	ctx context.Context,
	req *entity.CreateFilePartRequest,
) (*entity.CreateFilePartResponse, error) {
	now := time.Now().UnixMilli()
	err := f.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		partData := []map[string]any{{
			"file_id":       req.FileId,
			"file_part_id":  req.FilePartId,
			"ctime":         now,
			"mtime":         now,
			"file_key":      req.FileKey,
			"file_part_md5": req.FilePartMd5,
		}}
		partSQL, partArgs, err := builder.BuildInsert(f.table(), partData)
		if err != nil {
			return fmt.Errorf("build file part insert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, partSQL, partArgs...); err != nil {
			return fmt.Errorf("insert file part: %w", err)
		}
		deleteData := []map[string]any{{
			"file_id":         req.FileId,
			"file_part_id":    req.FilePartId,
			"backend_kind":    req.BackendKind,
			"delete_ref":      req.DeleteRef,
			"uploaded_at":     req.UploadedAt,
			"delete_state":    "live",
			"attempt_count":   0,
			"next_attempt_at": 0,
			"lease_until":     0,
			"last_attempt_at": 0,
			"last_error_code": "",
			"deleted_at":      0,
			"ctime":           now,
			"mtime":           now,
		}}
		deleteSQL, deleteArgs, err := builder.BuildInsert("tg_file_part_delete_state_tab", deleteData)
		if err != nil {
			return fmt.Errorf("build block delete state insert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, deleteSQL, deleteArgs...); err != nil {
			return fmt.Errorf("insert block delete state: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create file part transaction: %w", err)
	}
	return &entity.CreateFilePartResponse{}, nil
}

func (f *filePartDaoImpl) GetFilePartInfo(
	ctx context.Context,
	req *entity.GetFilePartInfoRequest,
) (*entity.GetFilePartInfoResponse, error) {
	where := map[string]any{
		"file_id":      req.FileId,
		"file_part_id": req.FilePartId,
	}

	rs := make([]*entity.FilePartInfoItem, 0, len(req.FilePartId))
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, fmt.Errorf("query file part info: %w", err)
	}
	return &entity.GetFilePartInfoResponse{List: rs}, nil
}

func (f *filePartDaoImpl) DeleteFilePart(
	ctx context.Context,
	req *entity.DeleteFilePartRequest,
) (*entity.DeleteFilePartResponse, error) {
	where := map[string]any{
		"file_id in": req.FileId,
	}
	sql, args, err := builder.BuildDelete(f.table(), where)
	if err != nil {
		return nil, fmt.Errorf("build file part delete: %w", err)
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, fmt.Errorf("delete file parts: %w", err)
	}
	return &entity.DeleteFilePartResponse{}, nil
}

func (f *filePartDaoImpl) ListFilePart(
	ctx context.Context,
	req *entity.ListFilePartRequest,
) (*entity.ListFilePartResponse, error) {
	where := map[string]any{
		"file_id": req.FileId,
		// "_limit":   []uint{uint(req.Offset), uint(req.Limit)}, //不折腾, 简单做, 正常来说都不会有性能问题
		"_orderby": "file_part_id asc",
	}
	rs := make([]*entity.FilePartInfoItem, 0, 32)
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, fmt.Errorf("list file parts: %w", err)
	}
	return &entity.ListFilePartResponse{List: rs}, nil
}
