package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

type fsWrap struct {
	ctx context.Context
}

func AsFileSystem(ctx context.Context) fs.FS {
	return &fsWrap{ctx: ctx}
}

func (f *fsWrap) isDir(name string) (bool, error) {
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

func (f *fsWrap) Open(name string) (fs.File, error) {
	isDir, err := f.isDir(name)
	if err != nil {
		return nil, err
	}
	if isDir {
		return f.openDir(name)
	}
	return f.openFile(name)
}

func (f *fsWrap) openDir(name string) (fs.File, error) {
	return &fsDirWrap{name: filepath.Base(name)}, nil
}

func (f *fsWrap) openFile(name string) (fs.File, error) {
	fid, err := ResolveLink(f.ctx, name)
	if err != nil {
		return nil, err
	}
	//返回的名字, 只有最终的文件名而不能有路径
	return &fsFileWrap{ctx: f.ctx, fid: fid, name: filepath.Base(name)}, nil
}

func (f *fsWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	return ReadDir(f.ctx, name)
}

type fsDirWrap struct {
	name string
}

type fsFileWrap struct {
	ctx  context.Context
	name string
	fid  uint64
	rc   io.ReadCloser
}

func (f *fsDirWrap) Stat() (fs.FileInfo, error) {
	return &defaultFileInfo{
		FieldSize:  0,
		FieldMtime: time.Time{},
		FieldName:  f.name,
		FieldMode:  0644,
		FieldIsDir: true,
		FieldSys:   nil,
	}, nil
}

func (f *fsDirWrap) Read(p0 []byte) (int, error) {
	return 0, fmt.Errorf("cant read on dir")
}

func (f *fsDirWrap) Close() error {
	return nil
}

func (f *fsFileWrap) Stat() (fs.FileInfo, error) {
	info, err := Stat(f.ctx, f.fid)
	if err != nil {
		return nil, err
	}
	return &defaultFileInfo{
		FieldSize:  info.Size(),
		FieldMtime: info.ModTime(),
		FieldName:  f.name,
		FieldMode:  info.Mode(),
		FieldIsDir: false,
		FieldSys:   nil,
	}, nil
}

func (f *fsFileWrap) Read(p0 []byte) (int, error) {
	var err error
	if f.rc == nil {
		f.rc, err = Open(f.ctx, f.fid)
	}
	if err != nil {
		return 0, err
	}
	return f.rc.Read(p0)
}

func (f *fsFileWrap) Close() error {
	if f.rc != nil {
		return f.rc.Close()
	}
	return nil
}
