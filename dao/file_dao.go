package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xxxsen/tgfile/constant"
	"github.com/xxxsen/tgfile/entity"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
	"github.com/xxxsen/common/idgen"
)

var errInvalidFileScanBatch = errors.New("invalid file scan batch size")

type ScanFileCallbackFunc func(ctx context.Context, res []*entity.FileInfoItem) (bool, error)

type IFileDao interface {
	CreateFileDraft(ctx context.Context, req *entity.CreateFileDraftRequest) (*entity.CreateFileDraftResponse, error)
	MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error)
	GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error)
	ScanFile(ctx context.Context, batch uint, cb ScanFileCallbackFunc) error
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

func (f *fileDaoImpl) CreateFileDraft(
	ctx context.Context,
	req *entity.CreateFileDraftRequest,
) (*entity.CreateFileDraftResponse, error) {
	fileid := idgen.NextId()
	now := time.Now().UnixMilli()
	data := []map[string]any{
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
		return nil, fmt.Errorf("build file draft insert: %w", err)
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, fmt.Errorf("insert file draft: %w", err)
	}
	return &entity.CreateFileDraftResponse{
		FileId: fileid,
	}, nil
}

func (f *fileDaoImpl) MarkFileReady(
	ctx context.Context,
	req *entity.MarkFileReadyRequest,
) (*entity.MarkFileReadyResponse, error) {
	where := map[string]any{
		"file_id": req.FileID,
	}
	update := map[string]any{
		"file_state": constant.FileStateReady,
		"mtime":      time.Now().UnixMilli(),
		"extinfo":    req.Extinfo,
	}
	sql, args, err := builder.BuildUpdate(f.table(), where, update)
	if err != nil {
		return nil, fmt.Errorf("build ready file update: %w", err)
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, fmt.Errorf("mark file ready: %w", err)
	}
	return &entity.MarkFileReadyResponse{}, nil
}

func (f *fileDaoImpl) GetFileInfo(
	ctx context.Context,
	req *entity.GetFileInfoRequest,
) (*entity.GetFileInfoResponse, error) {
	where := map[string]any{
		"file_id in": req.FileIds,
	}
	rs := make([]*entity.FileInfoItem, 0, len(req.FileIds))
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, fmt.Errorf("query file info: %w", err)
	}
	rsp := &entity.GetFileInfoResponse{List: rs}
	return rsp, nil
}

func (f *fileDaoImpl) ScanFile(ctx context.Context, batch uint, cb ScanFileCallbackFunc) error {
	const maxScanBatch uint = 1_000_000
	if batch == 0 || batch > maxScanBatch {
		return fmt.Errorf("%w: %d", errInvalidFileScanBatch, batch)
	}
	var lastid uint64
	for {
		res, nextid, err := f.innerScan(ctx, lastid, batch)
		if err != nil {
			return fmt.Errorf("scan file batch: %w", err)
		}
		next, err := cb(ctx, res)
		if err != nil {
			return fmt.Errorf("process file batch: %w", err)
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

func (f *fileDaoImpl) innerScan(
	ctx context.Context,
	lastid uint64,
	limit uint,
) ([]*entity.FileInfoItem, uint64, error) {
	where := map[string]any{
		"id >":     lastid,
		"_orderby": "id asc",
		"_limit":   []uint{0, limit},
	}
	rs := make([]*entity.FileInfoItem, 0, limit)
	if err := dbkit.SimpleQuery(ctx, f.dbc, f.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, 0, fmt.Errorf("query file scan batch: %w", err)
	}
	var nextid uint64
	if len(rs) > 0 {
		nextid = rs[len(rs)-1].Id
	}
	return rs, nextid, nil
}

func (f *fileDaoImpl) DeleteFile(
	ctx context.Context,
	req *entity.DeleteFileRequest,
) (*entity.DeleteFileResponse, error) {
	where := map[string]any{
		"file_id in": req.FileId,
	}
	sql, args, err := builder.BuildDelete(f.table(), where)
	if err != nil {
		return nil, fmt.Errorf("build file delete: %w", err)
	}
	if _, err := f.dbc.ExecContext(ctx, sql, args...); err != nil {
		return nil, fmt.Errorf("delete file: %w", err)
	}
	return &entity.DeleteFileResponse{}, nil
}
