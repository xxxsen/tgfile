package filemgr

import (
	"context"
	"io"
	"io/fs"
	"tgfile/entity"
)

type IterLinkFunc func(ctx context.Context, link string, ent *entity.FileMappingItem) (bool, error)

var defaultFileMgr IFileManager

type IFileManager interface {
	Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error)
	Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
	Create(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateLink(ctx context.Context, link string, fileid uint64, opt *entity.CreateLinkOption) error
	ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error)
	IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error
}

func SetFileManagerImpl(mgr IFileManager) {
	defaultFileMgr = mgr
}

func Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error) {
	return defaultFileMgr.Stat(ctx, fileid)
}

func Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	return defaultFileMgr.Open(ctx, fileid)
}

func Create(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return defaultFileMgr.Create(ctx, size, r)
}

func CreateLink(ctx context.Context, link string, fileid uint64, opt *entity.CreateLinkOption) error {
	return defaultFileMgr.CreateLink(ctx, link, fileid, opt)
}

func ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error) {
	return defaultFileMgr.ResolveLink(ctx, link)
}

func IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	return defaultFileMgr.IterLink(ctx, prefix, cb)
}
