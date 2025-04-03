package filemgr

import (
	"context"
	"io"

	"github.com/xxxsen/tgfile/entity"
)

type IterLinkFunc func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error)

var defaultFileMgr IFileManager

type IFileManager interface {
	Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
	Create(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateDraft(ctx context.Context, size int64) (uint64, int64, error)
	CreatePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error
	FinishCreate(ctx context.Context, fileid uint64) error
	CreateLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error
	ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error)
	IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error
	RemoveLink(ctx context.Context, link string) error
	RenameLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyLink(ctx context.Context, src, dst string, isOverwrite bool) error
}

func SetFileManagerImpl(mgr IFileManager) {
	defaultFileMgr = mgr
}

func Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	return defaultFileMgr.Open(ctx, fileid)
}

func Create(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return defaultFileMgr.Create(ctx, size, r)
}

func CreateDraft(ctx context.Context, size int64) (uint64, int64, error) {
	return defaultFileMgr.CreateDraft(ctx, size)
}

func CreatePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error {
	return defaultFileMgr.CreatePart(ctx, fileid, partid, r)
}

func FinishCreate(ctx context.Context, fileid uint64) error {
	return defaultFileMgr.FinishCreate(ctx, fileid)
}

func CreateLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error {
	return defaultFileMgr.CreateLink(ctx, link, fileid, size, isDir)
}

func ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error) {
	return defaultFileMgr.ResolveLink(ctx, link)
}

func RenameLink(ctx context.Context, src string, dst string, isOverwrite bool) error {
	return defaultFileMgr.RenameLink(ctx, src, dst, isOverwrite)
}

func RemoveLink(ctx context.Context, link string) error {
	return defaultFileMgr.RemoveLink(ctx, link)
}

func CopyLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return defaultFileMgr.CopyLink(ctx, src, dst, isOverwrite)
}

func IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	return defaultFileMgr.IterLink(ctx, prefix, cb)
}
