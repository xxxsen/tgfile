package directory

import "context"

type DirectoryScanCallbackFunc func(ctx context.Context, res []IDirectoryEntry) (bool, error)

type IDirectoryEntryIdentity interface {
	RefData() string
	Name() string
	IsDir() bool
}

type IDirectoryEntryMetadata interface {
	Ctime() int64
	Mtime() int64
	Mode() uint32
	Size() int64
}

type IDirectoryEntry interface {
	IDirectoryEntryIdentity
	IDirectoryEntryMetadata
}

type IDirectoryWriter interface {
	Mkdir(ctx context.Context, dir string) error
	Copy(ctx context.Context, src, dst string, overwrite bool) error
	Move(ctx context.Context, src, dst string, overwrite bool) error
	Create(ctx context.Context, filename string, size int64, refdata string) error
	Remove(ctx context.Context, filename string) error
}

type IDirectoryReader interface {
	List(ctx context.Context, dir string) ([]IDirectoryEntry, error)
	Stat(ctx context.Context, filename string) (IDirectoryEntry, error)
	Scan(ctx context.Context, batch uint, cb DirectoryScanCallbackFunc) error
}

type IDirectory interface {
	IDirectoryWriter
	IDirectoryReader
}
