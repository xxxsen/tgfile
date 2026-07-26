package filemgr

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/entity"
)

var (
	ErrInvalidFileSize       = errors.New("invalid file size")
	ErrInvalidBlockSize      = errors.New("invalid block size")
	ErrTooManyFileParts      = errors.New("too many file parts")
	ErrFileShortRead         = errors.New("file content shorter than declared size")
	ErrFilePartNotFound      = errors.New("file part not found")
	ErrInvalidFilePart       = errors.New("invalid file part id")
	ErrFileNotOpen           = errors.New("file is not open")
	ErrInvalidOffset         = errors.New("invalid file offset")
	ErrSeekPastEnd           = errors.New("seek exceeds file size")
	ErrDirectoryIO           = errors.New("operation is not supported on a directory")
	ErrNotDirectory          = errors.New("operation requires a directory")
	ErrInvalidCache          = errors.New("invalid file cache configuration")
	ErrInvalidCachePath      = errors.New("invalid file cache path")
	ErrS3Precondition        = errors.New("S3 object precondition failed")
	ErrS3ObjectConflict      = errors.New("S3 object changed concurrently")
	ErrInvalidS3Part         = errors.New("invalid completed S3 part manifest")
	ErrS3PartNotFound        = errors.New("S3 object part number exceeds the completed part count")
	ErrWebDAVPrecondition    = errors.New("WebDAV precondition failed")
	ErrWebDAVLocked          = errors.New("WebDAV resource is locked")
	ErrWebDAVLockToken       = errors.New("WebDAV lock token does not match")
	ErrWebDAVProperty        = errors.New("WebDAV property update is invalid")
	ErrWebDAVQuota           = errors.New("WebDAV logical quota exceeded")
	ErrWebDAVTooManyItems    = errors.New("WebDAV mutation exceeds the configured entry limit")
	ErrWebDAVSyncToken       = errors.New("WebDAV sync token is not valid for the current journal")
	ErrBackupBackendUpload   = errors.New("backup backend upload failed")
	ErrBackupBackendReadback = errors.New("backup backend readback failed")
	ErrBackupPublish         = errors.New("backup publish failed")
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

type IProtocolManager interface {
	ILinkManager
	IS3ObjectManager
	IS3MultipartManager
	IWebDAVManager
}

type IFileManager interface {
	IFileStorage
	IProtocolManager
	IBackupStorage
	IFileLifecycle
}

type IFileLifecycle interface {
	DiscardUnpublishedFile(ctx context.Context, fileid uint64) error
	RunBlockDeleteWorker(ctx context.Context) error
	RunMultipartCleanupWorker(context.Context) error
}

type BackupSnapshotRequest struct {
	JobID           string
	Scope           string
	SchemaVersion   int
	RequiredBuckets []backupfmt.RequiredBucket
}

type BackupPublishResult struct {
	MappingsCreated  int64
	MappingsReplaced int64
	FilesCreated     int64
}

// IBackupStorage keeps backup business-table mutations in FileManager so HTTP,
// CLI and the async task engine cannot bypass storage invariants.
type IBackupStorage interface {
	IBackupSnapshotStorage
	IBackupImportStorage
	IBackupPublishStorage
}

type IBackupSnapshotStorage interface {
	BackupMaxPartSize() int64
	CreateBackupSnapshot(context.Context, BackupSnapshotRequest) (*backupfmt.Manifest, error)
	OpenBackupPart(context.Context, string, int) (io.ReadCloser, error)
	ReleaseBackupSnapshot(context.Context, string) error
}

type IBackupImportStorage interface {
	ValidateBackupImport(context.Context, *backupfmt.Manifest, string) error
	BeginBackupImport(context.Context, string, *backupfmt.Manifest) error
	StageBackupPart(context.Context, string, backupfmt.Part, io.Reader) error
	FinishBackupImportFiles(context.Context, string, *backupfmt.Manifest) error
}

type IBackupPublishStorage interface {
	PublishBackupImport(
		context.Context,
		string,
		*backupfmt.Manifest,
		string,
	) (*BackupPublishResult, error)
	DiscardBackupImport(context.Context, string) error
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
	Key               string
	Size              int64
	LastModified      int64
	ETag              string
	ChecksumAlgorithm string
	ChecksumType      string
}

type S3ListResult struct {
	Items          []S3ListItem
	CommonPrefixes []string
	IsTruncated    bool
	NextKey        string
}

type S3PartNumberError struct {
	Requested int
	Actual    int
}

func (e *S3PartNumberError) Error() string {
	return ErrS3PartNotFound.Error()
}

func (e *S3PartNumberError) Unwrap() error {
	return ErrS3PartNotFound
}

type S3ObjectPartPage struct {
	Parts                []entity.S3CompletedPart
	PartsCount           int
	PartNumberMarker     int
	NextPartNumberMarker int
	MaxParts             int
	IsTruncated          bool
	IsMultipart          bool
}

type S3Condition struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   *time.Time
	IfUnmodifiedSince *time.Time
}

type WebDAVCondition struct {
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   *time.Time
	IfUnmodifiedSince *time.Time
	IfHeader          *WebDAVIfHeader
	RequestPath       string
}

type WebDAVIfHeader struct {
	Lists []WebDAVIfList
}

type WebDAVIfList struct {
	Resource string
	Terms    []WebDAVIfTerm
}

type WebDAVIfTerm struct {
	Not       bool
	LockToken string
	ETag      string
}

type WebDAVMutationOptions struct {
	Principal  string
	Condition  *WebDAVCondition
	MaxEntries int
	QuotaRoot  string
	QuotaBytes int64
}

type WebDAVPropertyName struct {
	Namespace string
	LocalName string
}

type WebDAVProperty struct {
	Name     WebDAVPropertyName
	ValueXML string
}

type WebDAVPropertyPatch struct {
	Set      bool
	Property WebDAVProperty
}

type WebDAVPublishResult struct {
	Link    *entity.FileLinkMeta
	Created bool
}

type WebDAVMutationResult struct {
	Created bool
}

type WebDAVLock struct {
	Token       string
	RootPath    string
	RootEntryID uint64
	Depth       string
	OwnerXML    string
	Principal   string
	CreatedAt   int64
	ExpiresAt   int64
	LockNull    bool
}

type WebDAVLockRequest struct {
	Path       string
	Depth      string
	OwnerXML   string
	Principal  string
	Timeout    time.Duration
	IfHeader   *WebDAVIfHeader
	MaxEntries int
}

type WebDAVLockResult struct {
	Lock    WebDAVLock
	Created bool
}

type WebDAVChange struct {
	Revision int64
	Path     string
	Kind     string
}

type WebDAVChangePage struct {
	Changes      []WebDAVChange
	SyncRevision int64
	HasMore      bool
}

type IWebDAVResourceManager interface {
	CreateWebDAVCollection(
		ctx context.Context,
		path string,
		options WebDAVMutationOptions,
	) (*WebDAVMutationResult, error)
	PublishWebDAVFile(
		ctx context.Context,
		path string,
		fileID uint64,
		size int64,
		options WebDAVMutationOptions,
	) (*WebDAVPublishResult, error)
	DeleteWebDAVResource(
		ctx context.Context,
		path string,
		options WebDAVMutationOptions,
	) error
	CopyWebDAVResource(
		ctx context.Context,
		source, destination string,
		overwrite, recursive bool,
		options WebDAVMutationOptions,
	) (*WebDAVMutationResult, error)
	MoveWebDAVResource(
		ctx context.Context,
		source, destination string,
		overwrite bool,
		options WebDAVMutationOptions,
	) (*WebDAVMutationResult, error)
}

type IWebDAVPropertyManager interface {
	ReadWebDAVProperties(ctx context.Context, entryID uint64) ([]WebDAVProperty, error)
	PatchWebDAVProperties(
		ctx context.Context,
		path string,
		patches []WebDAVPropertyPatch,
		options WebDAVMutationOptions,
	) error
}

type IWebDAVLockManager interface {
	ListWebDAVLocks(ctx context.Context, path string) ([]WebDAVLock, error)
	LockWebDAVResource(ctx context.Context, request WebDAVLockRequest) (*WebDAVLockResult, error)
	RefreshWebDAVLock(
		ctx context.Context,
		path, token, principal string,
		timeout time.Duration,
		ifHeader *WebDAVIfHeader,
	) (*WebDAVLock, error)
	UnlockWebDAVResource(ctx context.Context, path, token, principal string) error
}

type IWebDAVDiscoveryManager interface {
	WebDAVQuota(ctx context.Context, root string, limit int64) (int64, int64, error)
	WebDAVChanges(
		ctx context.Context,
		root string,
		since int64,
		depth string,
		limit int,
	) (*WebDAVChangePage, error)
}

type IWebDAVManager interface {
	IWebDAVResourceManager
	IWebDAVPropertyManager
	IWebDAVLockManager
	IWebDAVDiscoveryManager
}

type IS3ObjectReader interface {
	StatS3Object(ctx context.Context, path string) (*S3ObjectInfo, error)
	StatS3ObjectPart(
		ctx context.Context,
		fileID uint64,
		objectSize int64,
		partNumber int,
	) (*entity.S3CompletedPart, error)
	OpenS3ObjectPart(
		ctx context.Context,
		part *entity.S3CompletedPart,
	) (io.ReadSeekCloser, error)
	ListS3ObjectParts(
		ctx context.Context,
		fileID uint64,
		objectSize int64,
		marker int,
		maxParts int,
	) (*S3ObjectPartPage, error)
	ListS3Objects(ctx context.Context, req *S3ListRequest) (*S3ListResult, error)
}

type IS3ObjectWriter interface {
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

type IS3ObjectManager interface {
	IS3ObjectReader
	IS3ObjectWriter
}
