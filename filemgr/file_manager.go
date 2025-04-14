package filemgr

import (
	"context"
	"io"

	"github.com/xxxsen/tgfile/entity"
)

type IterLinkFunc func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error)

var defaultFileMgr IFileManager

type IFileStorage interface {
	OpenOpen(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
	CreateFile(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error)
	CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error
	FinishFileCreate(ctx context.Context, fileid uint64) error
	PurgeFile(ctx context.Context, before *int64) (int64, error)
}

type ILinkManager interface {
	CreateFileLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error
	ResolveFileLink(ctx context.Context, link string) (*entity.FileMappingItem, error)
	WalkFileLink(ctx context.Context, prefix string, cb IterLinkFunc) error
	RemoveFileLink(ctx context.Context, link string) error
	RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
}

type IFileManager interface {
	IFileStorage
	ILinkManager
}

func SetFileManagerImpl(mgr IFileManager) {
	defaultFileMgr = mgr
}

func OpenFile(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	return defaultFileMgr.OpenOpen(ctx, fileid)
}

func CreateFile(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return defaultFileMgr.CreateFile(ctx, size, r)
}

func CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error) {
	return defaultFileMgr.CreateFileDraft(ctx, size)
}

func CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error {
	return defaultFileMgr.CreateFilePart(ctx, fileid, partid, r)
}

func FinishFileCreate(ctx context.Context, fileid uint64) error {
	return defaultFileMgr.FinishFileCreate(ctx, fileid)
}

func CreateFileLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error {
	return defaultFileMgr.CreateFileLink(ctx, link, fileid, size, isDir)
}

func ResolveFileLink(ctx context.Context, link string) (*entity.FileMappingItem, error) {
	return defaultFileMgr.ResolveFileLink(ctx, link)
}

func RenameFileLink(ctx context.Context, src string, dst string, isOverwrite bool) error {
	return defaultFileMgr.RenameFileLink(ctx, src, dst, isOverwrite)
}

func RemoveFileLink(ctx context.Context, link string) error {
	return defaultFileMgr.RemoveFileLink(ctx, link)
}

func CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return defaultFileMgr.CopyFileLink(ctx, src, dst, isOverwrite)
}

func WalkFileLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	return defaultFileMgr.WalkFileLink(ctx, prefix, cb)
}

func PurgeFile(ctx context.Context, before *int64) (int64, error) {
	return defaultFileMgr.PurgeFile(ctx, before)
}
