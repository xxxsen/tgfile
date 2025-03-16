package filemgr

import (
	"context"
	"fmt"
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
	isFile := false
	isExist := false
	if err := IterLink(f.ctx, name, func(ctx context.Context, link string, fileid uint64) (bool, error) {
		if name == link { //完全匹配, 那必定为文件
			isFile = true
			isExist = true
			return false, nil
		}
		if !strings.HasSuffix(name, "/") {
			name += "/"
		}
		if strings.HasPrefix(link, name) { //name存在子路径, 那么必定为目录
			isExist = true
			return false, nil
		}
		return false, nil
	}); err != nil {
		return false, err
	}
	if !isExist {
		return false, fmt.Errorf("path:%s not found", name)
	}
	return !isFile, nil
}

func (f *fileSystemWrap) Open(name string) (fs.File, error) {
	//重建名字, 必须以"/"开头
	if strings.HasPrefix(name, ".") {
		name = strings.TrimLeft(name, ".")
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	isDir, err := f.checkIsDir(name)
	if err != nil {
		return nil, err
	}
	if isDir {
		return f.openDir(name)
	}
	return f.openFile(name)
}

func (f *fileSystemWrap) openDir(name string) (fs.File, error) {
	return newFileSystemDirEntry(f.ctx, name), nil
}

func (f *fileSystemWrap) openFile(name string) (fs.File, error) {
	fid, err := ResolveLink(f.ctx, name)
	if err != nil {
		return nil, err
	}
	return newFileSystemFileEntry(f.ctx, name, fid), nil
}

func (f *fileSystemWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	return internalReadDir(f.ctx, name)
}
