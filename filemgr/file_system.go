package filemgr

import (
	"context"
	"io/fs"
	"strings"
)

type fileSystemWrap struct {
	ctx  context.Context
	fmgr IFileManager
}

func ToFileSystem(ctx context.Context, fmgr IFileManager) fs.FS {
	return &fileSystemWrap{ctx: ctx, fmgr: fmgr}
}

func (f *fileSystemWrap) Open(name string) (fs.File, error) {
	//重建名字, 必须以"/"开头
	if strings.HasPrefix(name, ".") {
		name = strings.TrimLeft(name, ".")
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	item, err := f.fmgr.ResolveFileLink(f.ctx, name)
	if err != nil {
		return nil, err
	}
	return newFileSystemEntry(f.ctx, f.fmgr, name, item), nil
}

func (f *fileSystemWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	return internalReadDir(f.ctx, f.fmgr, name)
}
