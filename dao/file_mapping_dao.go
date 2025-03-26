package dao

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"tgfile/db"
	"tgfile/entity"
	"tgfile/webdav"
)

type IterFileMappingFunc func(ctx context.Context, name string, ent *entity.FileMappingItem) (bool, error)

type IFileMappingDao interface {
	GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error)
	CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error)
	IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error
}

type fileMappingDao struct {
	dav webdav.IWebdav
}

func NewFileMappingDao() IFileMappingDao {
	d := &fileMappingDao{}
	inst, err := webdav.NewEnumWebdav(db.GetClient(), d.table())
	if err != nil {
		panic(err)
	}
	d.dav = inst
	return d
}

func (f *fileMappingDao) table() string {
	return "tg_file_mapping_tab"
}

func (f *fileMappingDao) buildKey(name string) string {
	return defaultFileMappingPrefix + name
}

func (f *fileMappingDao) GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error) {
	ent, err := f.dav.Stat(ctx, req.FileName)
	if err != nil {
		return nil, false, err
	}
	fileid, err := strconv.ParseUint(ent.RefData, 0, 64)
	if err != nil {
		return nil, false, err
	}
	item := &entity.FileMappingItem{
		FileName: req.FileName,
		FileId:   fileid,
		Ctime:    ent.Ctime,
		Mtime:    ent.Mtime,
		FileSize: ent.Size,
	}
	return &entity.GetFileMappingResponse{Item: item}, true, nil
}

func (f *fileMappingDao) CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error) {
	if err := f.dav.Create(ctx, req.FileName, req.FileSize, fmt.Sprintf("%d", req.FileId)); err != nil {
		return nil, err
	}
	return &entity.CreateFileMappingResponse{}, nil
}

func (f *fileMappingDao) IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error {
	ents, err := f.dav.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, item := range ents {
		fid, err := strconv.ParseUint(item.RefData, 10, 64)
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
