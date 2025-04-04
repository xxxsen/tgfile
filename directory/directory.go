package directory

import "context"

type DirectoryScanCallbackFunc func(ctx context.Context, res []IDirectoryEntry) (bool, error)

type IDirectoryEntry interface {
	GetRefData() string
	GetName() string
	GetCtime() int64
	GetMtime() int64
	GetMode() uint32
	GetSize() int64
	GetIsDir() bool
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
