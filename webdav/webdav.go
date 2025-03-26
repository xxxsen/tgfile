package webdav

import "context"

type WebEntry struct {
	RefData string
	Name    string
	Ctime   int64
	Mtime   int64
	Mode    uint32
	Size    int64
	IsDir   bool
}

type IWebdav interface {
	Mkdir(ctx context.Context, dir string, mode uint32) error
	Copy(ctx context.Context, src, dst string, overwrite bool) error
	Move(ctx context.Context, src, dst string, overwrite bool) error
	Create(ctx context.Context, filename string, ent *WebEntry) error
	List(ctx context.Context, dir string) ([]*WebEntry, error)
	Stat(ctx context.Context, filename string) (*WebEntry, error)
}
