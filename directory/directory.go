package directory

import "context"

type DirectoryEntry struct {
	RefData string
	Name    string
	Ctime   int64
	Mtime   int64
	Mode    uint32
	Size    int64
	IsDir   bool
}

type IDirectory interface {
	Mkdir(ctx context.Context, dir string) error
	Copy(ctx context.Context, src, dst string, overwrite bool) error
	Move(ctx context.Context, src, dst string, overwrite bool) error
	Create(ctx context.Context, filename string, size int64, refdata string) error
	List(ctx context.Context, dir string) ([]*DirectoryEntry, error)
	Stat(ctx context.Context, filename string) (*DirectoryEntry, error)
	Remove(ctx context.Context, filename string) error
}
