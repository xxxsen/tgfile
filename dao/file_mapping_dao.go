package dao

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/tgfile/directory"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
)

type IterFileMappingFunc func(ctx context.Context, name string, ent *entity.FileMappingItem) (bool, error)
type ScanFileMappingFunc func(ctx context.Context, res []*entity.FileMappingItem) (bool, error)

type IFileMappingDao interface {
	GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error)
	CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error)
	IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error
	RemoveFileMapping(ctx context.Context, link string) error
	RenameFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error
	ScanFileMapping(ctx context.Context, batch int64, cb ScanFileMappingFunc) error
}

type fileMappingDao struct {
	dbc database.IDatabase
	dir directory.IDirectory
}

func NewFileMappingDao(dbc database.IDatabase) IFileMappingDao {
	d := &fileMappingDao{
		dbc: dbc,
	}
	dir, err := directory.NewDBDirectory(dbc, d.table(), idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
	d.dir = dir
	return d
}

func (f *fileMappingDao) table() string {
	return "tg_file_mapping_tab"
}

func (f *fileMappingDao) GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error) {
	ent, err := f.dir.Stat(ctx, req.FileName)
	if err != nil {
		return nil, false, err
	}
	item, err := f.directoryEntryToFileMappingItem(ent)
	if err != nil {
		return nil, false, err
	}
	return &entity.GetFileMappingResponse{Item: item}, true, nil
}

func (f *fileMappingDao) CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error) {
	if req.IsDir {
		if err := f.dir.Mkdir(ctx, req.FileName); err != nil {
			return nil, err
		}
		return &entity.CreateFileMappingResponse{}, nil
	}
	if err := f.dir.Create(ctx, req.FileName, req.FileSize, fmt.Sprintf("%d", req.FileId)); err != nil {
		return nil, err
	}
	return &entity.CreateFileMappingResponse{}, nil
}

func (f *fileMappingDao) RemoveFileMapping(ctx context.Context, link string) error {
	return f.dir.Remove(ctx, link)
}

func (f *fileMappingDao) RenameFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error {
	return f.dir.Move(ctx, src, dst, isOverwrite)
}

func (f *fileMappingDao) CopyFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error {
	return f.dir.Copy(ctx, src, dst, isOverwrite)
}

func (f *fileMappingDao) IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error {
	ents, err := f.dir.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, item := range ents {
		cbitem, err := f.directoryEntryToFileMappingItem(item)
		if err != nil {
			return err
		}
		next, err := cb(ctx, path.Join(prefix, item.Name), cbitem)
		if err != nil {
			return err
		}
		if !next {
			break
		}
	}
	return nil
}

func (f *fileMappingDao) directoryEntryToFileMappingItem(item *directory.DirectoryEntry) (*entity.FileMappingItem, error) {
	rs := &entity.FileMappingItem{
		FileName: item.Name,
		FileId:   0,
		FileSize: item.Size,
		Mode:     item.Mode,
		Ctime:    item.Ctime,
		Mtime:    item.Mtime,
		IsDir:    item.IsDir,
	}
	if !rs.IsDir {
		fid, err := strconv.ParseUint(item.RefData, 10, 64)
		if err != nil {
			return nil, err
		}
		rs.FileId = fid
	}
	return rs, nil
}

func (f *fileMappingDao) ScanFileMapping(ctx context.Context, batch int64, cb ScanFileMappingFunc) error {
	return f.dir.Scan(ctx, batch, func(ctx context.Context, res []*directory.DirectoryEntry) (bool, error) {
		cbitems := make([]*entity.FileMappingItem, 0, len(res))
		for _, item := range res {
			cbitem, err := f.directoryEntryToFileMappingItem(item)
			if err != nil {
				return false, err
			}
			cbitems = append(cbitems, cbitem)
		}
		return cb(ctx, cbitems)
	})
}
