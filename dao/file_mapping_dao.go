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

type IterFileLinkFunc func(ctx context.Context, name string, ent *entity.FileLinkMeta) (bool, error)
type ScanFileLinkFunc func(ctx context.Context, res []*entity.FileLinkMeta) (bool, error)

type IFileMappingDao interface {
	GetFileLinkMeta(ctx context.Context, req *entity.GetFileLinkMetaRequest) (*entity.GetFileLinkMetaResponse, bool, error)
	CreateFileLink(ctx context.Context, req *entity.CreateFileLinkRequest) (*entity.CreateFileLinkResponse, error)
	IterFileLink(ctx context.Context, prefix string, cb IterFileLinkFunc) error
	RemoveFileLink(ctx context.Context, link string) error
	RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	ScanFileLink(ctx context.Context, batch int64, cb ScanFileLinkFunc) error
}

type fileMappingDaoImpl struct {
	dbc database.IDatabase
	dir directory.IDirectory
}

func NewFileMappingDao(dbc database.IDatabase) IFileMappingDao {
	d := &fileMappingDaoImpl{
		dbc: dbc,
	}
	dir, err := directory.NewDBDirectory(dbc, d.table(), idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
	d.dir = dir
	return d
}

func (f *fileMappingDaoImpl) table() string {
	return "tg_file_mapping_tab"
}

func (f *fileMappingDaoImpl) GetFileLinkMeta(ctx context.Context, req *entity.GetFileLinkMetaRequest) (*entity.GetFileLinkMetaResponse, bool, error) {
	ent, err := f.dir.Stat(ctx, req.FileName)
	if err != nil {
		return nil, false, err
	}
	item, err := f.directoryEntryToFileMappingItem(ent)
	if err != nil {
		return nil, false, err
	}
	return &entity.GetFileLinkMetaResponse{Item: item}, true, nil
}

func (f *fileMappingDaoImpl) CreateFileLink(ctx context.Context, req *entity.CreateFileLinkRequest) (*entity.CreateFileLinkResponse, error) {
	if req.IsDir {
		if err := f.dir.Mkdir(ctx, req.FileName); err != nil {
			return nil, err
		}
		return &entity.CreateFileLinkResponse{}, nil
	}
	if err := f.dir.Create(ctx, req.FileName, req.FileSize, fmt.Sprintf("%d", req.FileId)); err != nil {
		return nil, err
	}
	return &entity.CreateFileLinkResponse{}, nil
}

func (f *fileMappingDaoImpl) RemoveFileLink(ctx context.Context, link string) error {
	return f.dir.Remove(ctx, link)
}

func (f *fileMappingDaoImpl) RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return f.dir.Move(ctx, src, dst, isOverwrite)
}

func (f *fileMappingDaoImpl) CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return f.dir.Copy(ctx, src, dst, isOverwrite)
}

func (f *fileMappingDaoImpl) IterFileLink(ctx context.Context, prefix string, cb IterFileLinkFunc) error {
	ents, err := f.dir.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, item := range ents {
		cbitem, err := f.directoryEntryToFileMappingItem(item)
		if err != nil {
			return err
		}
		next, err := cb(ctx, path.Join(prefix, item.Name()), cbitem)
		if err != nil {
			return err
		}
		if !next {
			break
		}
	}
	return nil
}

func (f *fileMappingDaoImpl) directoryEntryToFileMappingItem(item directory.IDirectoryEntry) (*entity.FileLinkMeta, error) {
	rs := &entity.FileLinkMeta{
		FileName: item.Name(),
		FileId:   0,
		FileSize: item.Size(),
		Mode:     item.Mode(),
		Ctime:    item.Ctime(),
		Mtime:    item.Mtime(),
		IsDir:    item.IsDir(),
	}
	if !rs.IsDir {
		fid, err := strconv.ParseUint(item.RefData(), 10, 64)
		if err != nil {
			return nil, err
		}
		rs.FileId = fid
	}
	return rs, nil
}

func (f *fileMappingDaoImpl) ScanFileLink(ctx context.Context, batch int64, cb ScanFileLinkFunc) error {
	return f.dir.Scan(ctx, batch, func(ctx context.Context, res []directory.IDirectoryEntry) (bool, error) {
		cbitems := make([]*entity.FileLinkMeta, 0, len(res))
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
