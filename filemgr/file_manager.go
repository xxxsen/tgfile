package filemgr

import (
	"context"
	"errors"
	"io"

	"github.com/xxxsen/tgfile/entity"
)

var (
	ErrInvalidFileSize  = errors.New("invalid file size")
	ErrInvalidBlockSize = errors.New("invalid block size")
	ErrTooManyFileParts = errors.New("too many file parts")
	ErrFileShortRead    = errors.New("file content shorter than declared size")
	ErrFilePartNotFound = errors.New("file part not found")
	ErrInvalidFilePart  = errors.New("invalid file part id")
	ErrFileNotOpen      = errors.New("file is not open")
	ErrInvalidOffset    = errors.New("invalid file offset")
	ErrSeekPastEnd      = errors.New("seek exceeds file size")
	ErrDirectoryIO      = errors.New("operation is not supported on a directory")
	ErrNotDirectory     = errors.New("operation requires a directory")
	ErrInvalidCache     = errors.New("invalid file cache configuration")
	ErrInvalidCachePath = errors.New("invalid file cache path")
)

type WalkLinkFunc func(ctx context.Context, link string, item *entity.FileLinkMeta) (bool, error)

type IFileReader interface {
	StatFile(ctx context.Context, fileid uint64) (*entity.FileMeta, error)
	OpenFile(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
}

type IFileWriter interface {
	CreateFile(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error)
	CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error
	FinishFileCreate(ctx context.Context, fileid uint64) error
	PurgeFile(ctx context.Context, before *int64) (int64, error)
}

type IFileStorage interface {
	IFileReader
	IFileWriter
}

type ILinkReader interface {
	StatFileLink(ctx context.Context, link string) (*entity.FileLinkMeta, error)
	WalkFileLink(ctx context.Context, prefix string, cb WalkLinkFunc) error
}

type ILinkWriter interface {
	CreateFileLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error
	RemoveFileLink(ctx context.Context, link string) error
	RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
}

type ILinkManager interface {
	ILinkReader
	ILinkWriter
}

type IFileManager interface {
	IFileStorage
	ILinkManager
}
