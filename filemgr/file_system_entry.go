package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xxxsen/tgfile/entity"
)

type fileSystemFileEntry struct {
	stream         io.ReadSeekCloser
	initErr        error
	streamInitOnce sync.Once
	ctx            context.Context
	ent            *entity.FileMappingItem
	fullName       string
}

func newFileSystemEntry(ctx context.Context, fullName string, ent *entity.FileMappingItem) *fileSystemFileEntry {
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
	if f.ent.IsDir {
		return 0, fmt.Errorf("unable to seek on dir")
	}
	f.tryInitStream()
	if f.initErr != nil {
		return 0, f.initErr
	}
	return f.stream.Seek(offset, whence)
}

func (f *fileSystemFileEntry) Read(p0 []byte) (int, error) {
	if f.ent.IsDir {
		return 0, fmt.Errorf("unable to read on dir")
	}
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
	return path.Base(f.fullName)
}

func (f *fileSystemFileEntry) IsDir() bool {
	return f.ent.IsDir
}

func (f *fileSystemFileEntry) Type() fs.FileMode {
	return fs.FileMode(f.ent.Mode)
}

func (f *fileSystemFileEntry) Info() (fs.FileInfo, error) {
	return f.Stat()
}

func (f *fileSystemFileEntry) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.ent.IsDir {
		return nil, fmt.Errorf("unable to read dir on file")
	}
	ents, err := internalReadDir(f.ctx, f.fullName)
	if err != nil {
		return nil, err
	}
	if n <= 0 || len(ents) < n {
		return ents, nil
	}
	return ents[:n], nil
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
		ents = append(ents, newFileSystemEntry(ctx, link, ent))
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
