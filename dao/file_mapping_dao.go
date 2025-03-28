package dao

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"tgfile/db"
	"tgfile/directory"
	"tgfile/entity"

	"github.com/xxxsen/common/idgen"
)

type IterFileMappingFunc func(ctx context.Context, name string, ent *entity.FileMappingItem) (bool, error)

type IFileMappingDao interface {
	GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error)
	CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error)
	IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error
	RemoveFileMapping(ctx context.Context, link string) error
	RenameFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error
}

type fileMappingDao struct {
	dir directory.IDirectory
}

func NewFileMappingDao() IFileMappingDao {
	d := &fileMappingDao{}
	dir, err := directory.NewDBDirectory(db.GetClient(), d.table(), idgen.Default().NextId)
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
	var fileid uint64
	fileid, err = f.retrieveFileId(ent)
	if err != nil {
		return nil, false, err
	}
	item := &entity.FileMappingItem{
		FileName: ent.Name,
		FileId:   fileid,
		Ctime:    ent.Ctime,
		Mtime:    ent.Mtime,
		FileSize: ent.Size,
		IsDir:    ent.IsDir,
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

func (f *fileMappingDao) retrieveFileId(ent *directory.DirectoryEntry) (uint64, error) {
	if ent.IsDir {
		return 0, nil
	}
	return strconv.ParseUint(ent.RefData, 10, 64)
}

func (f *fileMappingDao) RemoveFileMapping(ctx context.Context, link string) error {
	return f.dir.Remove(ctx, link)
}

func (f *fileMappingDao) RenameFileMapping(ctx context.Context, src, dst string, isOverwrite bool) error {
	return f.dir.Move(ctx, src, dst, isOverwrite)
}

func (f *fileMappingDao) IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error {
	ents, err := f.dir.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, item := range ents {
		fid, err := f.retrieveFileId(item)
		if err != nil {
			return err
		}
		next, err := cb(ctx, filepath.Join(prefix, item.Name), &entity.FileMappingItem{
			FileName: item.Name,
			FileId:   fid,
			FileSize: item.Size,
			Ctime:    item.Ctime,
			Mtime:    item.Mtime,
			IsDir:    item.IsDir,
		})
		if err != nil {
			return err
		}
		if !next {
			break
		}
	}
	return nil
}
