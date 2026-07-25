package filemgr

import (
	"context"
	"errors"
	"io"
	"time"

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
	ErrS3Precondition   = errors.New("S3 object precondition failed")
	ErrS3ObjectConflict = errors.New("S3 object changed concurrently")
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
	IS3ObjectManager
	RunBlockDeleteWorker(ctx context.Context) error
}

type S3ObjectInfo struct {
	Link     *entity.FileLinkMeta
	Metadata *entity.S3ObjectMetadata
}

type S3ListRequest struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
	FetchOwner        bool
}

type S3ListItem struct {
	Key          string
	Size         int64
	LastModified int64
	ETag         string
}

type S3ListResult struct {
	Items          []S3ListItem
	CommonPrefixes []string
	IsTruncated    bool
	NextKey        string
}

type S3Condition struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   *time.Time
	IfUnmodifiedSince *time.Time
}

type IS3ObjectManager interface {
	StatS3Object(ctx context.Context, path string) (*S3ObjectInfo, error)
	ListS3Objects(ctx context.Context, req *S3ListRequest) (*S3ListResult, error)
	PublishS3Object(
		ctx context.Context,
		path string,
		fileID uint64,
		size int64,
		metadata *entity.S3ObjectMetadata,
		condition *S3Condition,
	) (*S3ObjectInfo, error)
	CopyS3Object(
		ctx context.Context,
		source string,
		destination string,
		metadata *entity.S3ObjectMetadata,
		sourceCondition *S3Condition,
		destinationCondition *S3Condition,
	) (*S3ObjectInfo, error)
	DeleteS3Object(ctx context.Context, path string, condition *S3Condition) (bool, error)
}
