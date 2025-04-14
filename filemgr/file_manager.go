package filemgr

import (
	"context"
	"io"

	"github.com/xxxsen/tgfile/entity"
)

type WalkLinkFunc func(ctx context.Context, link string, item *entity.FileLinkMeta) (bool, error)

type IFileStorage interface {
	//TODO: 增加一个StatFile 方法, 用于返回文件信息
	//TODO: 对于CreateFile接口, 增加FileMeta返回
	OpenFile(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
	CreateFile(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error)
	CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error
	FinishFileCreate(ctx context.Context, fileid uint64) error
	PurgeFile(ctx context.Context, before *int64) (int64, error)
}

type ILinkManager interface {
	CreateFileLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error
	StatFileLink(ctx context.Context, link string) (*entity.FileLinkMeta, error)
	WalkFileLink(ctx context.Context, prefix string, cb WalkLinkFunc) error
	RemoveFileLink(ctx context.Context, link string) error
	RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
}

type IFileManager interface {
	IFileStorage
	ILinkManager
}
