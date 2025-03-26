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
	"tgfile/entity"
	"time"
)

type fileSystemFileEntry struct {
	stream         io.ReadSeekCloser
	initErr        error
	streamInitOnce sync.Once
	ctx            context.Context
	ent            *entity.FileMappingItem
	fullName       string
}

func newFileSystemFileEntry(ctx context.Context, fullName string, ent *entity.FileMappingItem) *fileSystemFileEntry {
	return &fileSystemFileEntry{
		ctx:      ctx,
		fullName: fullName,
		ent:      ent,
	}
}

func (f *fileSystemFileEntry) Stat() (fs.FileInfo, error) {
	return &wrapFileMappingItem{ent: f.ent}, nil
}

func (f *fileSystemFileEntry) tryInitStream() {
	f.streamInitOnce.Do(func() {
		f.stream, f.initErr = Open(f.ctx, f.ent.FileId)
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
	ent      *entity.FileMappingItem
}

func newFileSystemDirEntry(ctx context.Context, fullName string, ent *entity.FileMappingItem) *fileSystemDirEntry {
	return &fileSystemDirEntry{
		ctx:      ctx,
		fullName: fullName,
		ent:      ent,
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
	ents := make([]os.DirEntry, 0, 16)

	err := defaultFileMgr.IterLink(ctx, root, func(ctx context.Context, link string, ent *entity.FileMappingItem) (bool, error) {
		if !ent.IsDir {
			ents = append(ents, newFileSystemFileEntry(ctx, link, ent))
			return true, nil
		}

		ents = append(ents, newFileSystemDirEntry(ctx, link, ent))

		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return ents, nil
}

type wrapFileMappingItem struct {
	ent *entity.FileMappingItem
}

func (w *wrapFileMappingItem) Name() string {
	return w.ent.FileName
}

func (w *wrapFileMappingItem) Size() int64 {
	return w.ent.FileSize
}

func (w *wrapFileMappingItem) Mode() fs.FileMode {
	return fs.FileMode(w.ent.Mode)
}

func (w *wrapFileMappingItem) ModTime() time.Time {
	return time.UnixMilli(w.ent.Mtime)
}

func (w *wrapFileMappingItem) IsDir() bool {
	return w.ent.IsDir
}

func (w *wrapFileMappingItem) Sys() any {
	return w.ent
}
