package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type fileSystemFileEntry struct {
	stream         io.ReadSeekCloser
	initErr        error
	streamInitOnce sync.Once
	ctx            context.Context
	fileid         uint64
	fullName       string
}

func newFileSystemFileEntry(ctx context.Context, fullName string, fileid uint64) *fileSystemFileEntry {
	return &fileSystemFileEntry{
		ctx:      ctx,
		fileid:   fileid,
		fullName: fullName,
	}
}

func (f *fileSystemFileEntry) Stat() (fs.FileInfo, error) {
	st, err := Stat(f.ctx, f.fileid)
	if err != nil {
		return nil, err
	}
	return &defaultFileInfo{
		FieldSize:  st.Size(),
		FieldMtime: time.Time{},
		FieldName:  f.fullName,
		FieldMode:  06744,
		FieldIsDir: false,
		FieldSys:   nil,
	}, nil
}

func (f *fileSystemFileEntry) tryInitStream() {
	f.streamInitOnce.Do(func() {
		f.stream, f.initErr = Open(f.ctx, f.fileid)
	})
}

func (f *fileSystemFileEntry) Seek(offset int64, whence int) (int64, error) {
	f.tryInitStream()
	if f.initErr != nil {
		return 0, f.initErr
	}
	return f.stream.Seek(offset, whence)
}

func (f *fileSystemFileEntry) Read(p0 []byte) (int, error) {
	f.tryInitStream()
	if f.initErr != nil {
		return 0, f.initErr
	}
	return f.stream.Read(p0)
}

func (f *fileSystemFileEntry) Close() error {
	if f.stream == nil {
		return nil
	}
	return f.stream.Close()
}

func (f *fileSystemFileEntry) Name() string {
	return filepath.Base(f.fullName)
}

func (f *fileSystemFileEntry) IsDir() bool {
	return false
}

func (f *fileSystemFileEntry) Type() fs.FileMode {
	return 0644
}

func (f *fileSystemFileEntry) Info() (fs.FileInfo, error) {
	return f.Stat()
}

//====

type fileSystemDirEntry struct {
	ctx      context.Context
	fullName string
}

func newFileSystemDirEntry(ctx context.Context, fullName string) *fileSystemDirEntry {
	return &fileSystemDirEntry{
		ctx:      ctx,
		fullName: fullName,
	}
}

func (f *fileSystemDirEntry) ReadDir(n int) ([]fs.DirEntry, error) {
	ents, err := internalReadDir(f.ctx, f.fullName)
	if err != nil {
		return nil, err
	}
	if n <= 0 || len(ents) < n {
		return ents, nil
	}
	return ents[:n], nil
}

func (f *fileSystemDirEntry) Read(p0 []byte) (int, error) {
	return 0, fmt.Errorf("cant read on dir")
}

func (f *fileSystemDirEntry) Close() error {
	return nil
}

func (f *fileSystemDirEntry) Stat() (fs.FileInfo, error) {
	return &defaultFileInfo{
		FieldSize:  0,
		FieldMtime: time.Time{},
		FieldName:  filepath.Base(f.fullName),
		FieldMode:  0755,
		FieldIsDir: true,
		FieldSys:   nil,
	}, nil
}

func (f *fileSystemDirEntry) Name() string {
	return filepath.Base(f.fullName)
}

func (f *fileSystemDirEntry) IsDir() bool {
	return true
}

func (f *fileSystemDirEntry) Type() fs.FileMode {
	return 0755
}

func (f *fileSystemDirEntry) Info() (fs.FileInfo, error) {
	return f.Stat()
}

func internalReadDir(ctx context.Context, root string) ([]os.DirEntry, error) {
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	fileEntries := make([]os.DirEntry, 0, 16)
	dirEntries := make([]os.DirEntry, 0, 16)
	dirExists := make(map[string]struct{})

	err := defaultFileMgr.IterLink(ctx, root, func(ctx context.Context, link string, fileid uint64) (bool, error) {
		if link == root { //目录本身
			return true, nil
		}
		if !strings.HasPrefix(link, root) {
			return false, nil
		}
		relPath := link[len(root):]
		idx := strings.Index(relPath, "/")
		if idx < 0 { //没有额外的'/', 那么当前的这个item是文件, 否则是目录
			fileEntries = append(fileEntries, newFileSystemFileEntry(ctx, link, fileid))
			return true, nil
		}
		dirname := relPath[:idx]
		if _, ok := dirExists[dirname]; ok { //目录已经处理过了
			return true, nil
		}
		dirExists[dirname] = struct{}{}
		dirEntries = append(dirEntries, newFileSystemDirEntry(ctx, link[:len(root)+idx]))
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	rs := make([]os.DirEntry, 0, len(fileEntries)+len(dirEntries))
	rs = append(rs, dirEntries...)
	rs = append(rs, fileEntries...)
	return rs, nil
}
