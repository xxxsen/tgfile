package filemgr

import (
	"context"
	"io/fs"
	"strings"
)

type fileSystemWrap struct {
	ctx context.Context
}

func AsFileSystem(ctx context.Context) fs.FS {
	return &fileSystemWrap{ctx: ctx}
}

func (f *fileSystemWrap) Open(name string) (fs.File, error) {
	//重建名字, 必须以"/"开头
	if strings.HasPrefix(name, ".") {
		name = strings.TrimLeft(name, ".")
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	item, err := ResolveFileLink(f.ctx, name)
	if err != nil {
		return nil, err
	}
	return newFileSystemEntry(f.ctx, name, item), nil
}

func (f *fileSystemWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	return internalReadDir(f.ctx, name)
}
