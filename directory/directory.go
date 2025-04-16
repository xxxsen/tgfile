package directory

import "context"

type DirectoryScanCallbackFunc func(ctx context.Context, res []IDirectoryEntry) (bool, error)

type IDirectoryEntry interface {
	RefData() string
	Name() string
	Ctime() int64
	Mtime() int64
	Mode() uint32
	Size() int64
	IsDir() bool
}

type IDirectory interface {
	Mkdir(ctx context.Context, dir string) error
	Copy(ctx context.Context, src, dst string, overwrite bool) error
	Move(ctx context.Context, src, dst string, overwrite bool) error
	Create(ctx context.Context, filename string, size int64, refdata string) error
	List(ctx context.Context, dir string) ([]IDirectoryEntry, error)
	Stat(ctx context.Context, filename string) (IDirectoryEntry, error)
	Remove(ctx context.Context, filename string) error
	Scan(ctx context.Context, batch int64, cb DirectoryScanCallbackFunc) error
}
