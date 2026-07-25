package filemgr

import (
	"context"
	"errors"
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
	fmgr           IFileManager
	stream         io.ReadSeekCloser
	initErr        error
	streamInitOnce sync.Once
	ctx            context.Context
	ent            *entity.FileLinkMeta
	fullName       string
}

func newFileSystemEntry(
	ctx context.Context,
	fmgr IFileManager,
	fullName string,
	ent *entity.FileLinkMeta,
) *fileSystemFileEntry {
	return &fileSystemFileEntry{
		fmgr:     fmgr,
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
		f.stream, f.initErr = f.fmgr.OpenFile(f.ctx, f.ent.FileId)
	})
}

func (f *fileSystemFileEntry) Seek(offset int64, whence int) (int64, error) {
	if f.ent.IsDir {
		return 0, ErrDirectoryIO
	}
	f.tryInitStream()
	if f.initErr != nil {
		return 0, fmt.Errorf("open file system entry: %w", f.initErr)
	}
	position, err := f.stream.Seek(offset, whence)
	if err != nil {
		return position, fmt.Errorf("seek file system entry: %w", err)
	}
	return position, nil
}

func (f *fileSystemFileEntry) Read(p0 []byte) (int, error) {
	if f.ent.IsDir {
		return 0, ErrDirectoryIO
	}
	f.tryInitStream()
	if f.initErr != nil {
		return 0, fmt.Errorf("open file system entry: %w", f.initErr)
	}
	read, err := f.stream.Read(p0)
	if err != nil && !errors.Is(err, io.EOF) {
		return read, fmt.Errorf("read file system entry: %w", err)
	}
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	return read, nil
}

func (f *fileSystemFileEntry) Close() error {
	if f.stream == nil {
		return nil
	}
	if err := f.stream.Close(); err != nil {
		return fmt.Errorf("close file system entry: %w", err)
	}
	return nil
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
		return nil, ErrNotDirectory
	}
	ents, err := internalReadDir(f.ctx, f.fmgr, f.fullName)
	if err != nil {
		return nil, fmt.Errorf("read directory %q: %w", f.fullName, err)
	}
	if n <= 0 || len(ents) < n {
		return ents, nil
	}
	return ents[:n], nil
}

func internalReadDir(ctx context.Context, fmgr IFileManager, root string) ([]os.DirEntry, error) {
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	ents := make([]os.DirEntry, 0, 16)

	err := fmgr.WalkFileLink(ctx, root, func(ctx context.Context, link string, ent *entity.FileLinkMeta) (bool, error) {
		ents = append(ents, newFileSystemEntry(ctx, fmgr, link, ent))
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk directory %q: %w", root, err)
	}
	return ents, nil
}

type wrapFileMappingItem struct {
	ent *entity.FileLinkMeta
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
