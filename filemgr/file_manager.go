package filemgr

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

type IterLinkFunc func(ctx context.Context, link string, fileid uint64) (bool, error)

var defaultFileMgr IFileManager

type IFileManager interface {
	Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error)
	Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error)
	Create(ctx context.Context, size int64, r io.Reader) (uint64, error)
	CreateLink(ctx context.Context, link string, fileid uint64) error
	ResolveLink(ctx context.Context, link string) (uint64, error)
	IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error
}

func SetFileManagerImpl(mgr IFileManager) {
	defaultFileMgr = mgr
}

func Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error) {
	return defaultFileMgr.Stat(ctx, fileid)
}

func Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	return defaultFileMgr.Open(ctx, fileid)
}

func Create(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return defaultFileMgr.Create(ctx, size, r)
}

func CreateLink(ctx context.Context, link string, fileid uint64) error {
	return defaultFileMgr.CreateLink(ctx, link, fileid)
}

func ResolveLink(ctx context.Context, link string) (uint64, error) {
	return defaultFileMgr.ResolveLink(ctx, link)
}

func IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	return defaultFileMgr.IterLink(ctx, prefix, cb)
}

func ReadDir(ctx context.Context, dir string) ([]os.DirEntry, error) {
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	filem := make(map[string]uint64, 16)
	dirm := make(map[string]struct{})
	err := defaultFileMgr.IterLink(ctx, dir, func(ctx context.Context, link string, fileid uint64) (bool, error) {
		if link == dir { //目录本身
			return true, nil
		}
		if !strings.HasPrefix(link, dir) {
			return false, nil
		}
		relPath := link[len(dir):]
		idx := strings.Index(relPath, "/")
		if idx < 0 { //没有额外的'/', 那么当前的这个item是文件, 否则是目录
			filem[relPath] = fileid
			return true, nil
		}
		dirm[relPath[:idx]] = struct{}{}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	//排序目录
	dirs := make([]string, 0, len(dirm))
	for k := range dirm {
		dirs = append(dirs, k)
	}
	sort.Strings(dirs)
	//排序文件
	files := make([]string, 0, len(filem))
	for k := range filem {
		files = append(files, k)
	}
	sort.Strings(files)
	rs := make([]os.DirEntry, 0, len(filem)+len(dirm))
	for _, dir := range dirs {
		rs = append(rs, &dirEntry{
			ctx:        ctx,
			FileId:     0,
			FieldIsDir: true,
			FieldName:  dir,
			FieldMode:  0644,
		})
	}
	for _, file := range files {
		rs = append(rs, &dirEntry{
			ctx:        ctx,
			FileId:     filem[file],
			FieldIsDir: false,
			FieldName:  file,
			FieldMode:  0644,
		})
	}
	return rs, nil
}
