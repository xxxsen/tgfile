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
)

type fileSystemFileEntry struct {
	stream         io.ReadSeekCloser
	initErr        error
	streamInitOnce sync.Once
	ctx            context.Context
	fullName       string
	ent            *entity.FileMappingItem
}

func newFileSystemFileEntry(ctx context.Context, fullName string, ent *entity.FileMappingItem) *fileSystemFileEntry {
	return &fileSystemFileEntry{
		ctx:      ctx,
		fullName: fullName,
		ent:      ent,
	}
}

func (f *fileSystemFileEntry) Stat() (fs.FileInfo, error) {
	return f.ent, nil
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
	return f.ent, nil
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

func cbfn(root string, entries *[]os.DirEntry) IterLinkFunc {
	return func(ctx context.Context, link string, ent *entity.FileMappingItem) (bool, error) {
		if link == root { //目录本身, 直接跳过
			return true, nil
		}
		if !strings.HasPrefix(link, root) { //已经遍历到结尾了
			return false, nil
		}
		//在移除了父目录后, 如果还能找到'/', 那么必定是个目录, 或者该文件非当前目录的直接子级
		idx := strings.Index(link[len(root):], "/")
		//TODO: 处理下一级目录
		if idx > 0 && len(root)+idx+1 != len(link) {
			return true, nil
		}
		if ent.IsDir() {
			*entries = append(*entries, newFileSystemDirEntry(ctx, link, ent))
			return true, nil
		}
		*entries = append(*entries, newFileSystemFileEntry(ctx, link, ent))
		return true, nil
	}
}

func internalReadDir(ctx context.Context, root string) ([]os.DirEntry, error) {
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	entries := make([]os.DirEntry, 0, 16)
	err := defaultFileMgr.IterLink(ctx, root, cbfn(root, &entries))
	if err != nil {
		return nil, err
	}
	return entries, nil
}
