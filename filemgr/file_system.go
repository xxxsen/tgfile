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

func (f *fileSystemWrap) checkIsDir(name string) (bool, error) {
	if strings.HasSuffix(name, "/") {
		return true, nil
	}
	return false, nil
}

func (f *fileSystemWrap) Open(name string) (fs.File, error) {
	//重建名字, 必须以"/"开头
	if strings.HasPrefix(name, ".") {
		name = strings.TrimLeft(name, ".")
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	item, err := ResolveLink(f.ctx, name)
	if err != nil {
		if strings.HasSuffix(name, "/") {
			return nil, err
		}
		name += "/"
		item, err = ResolveLink(f.ctx, name)
	}
	if err != nil {
		return nil, err
	}
	if !item.IsDirEntry {
		return newFileSystemFileEntry(f.ctx, name, item), nil
	}
	return newFileSystemDirEntry(f.ctx, name, item), nil

}

func (f *fileSystemWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	return internalReadDir(f.ctx, name)
}
