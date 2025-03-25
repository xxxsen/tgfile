package filemgr

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"tgfile/entity"
	"time"

	"github.com/stretchr/testify/assert"
)

type linkInfo struct {
	filename string
	size     int64
	data     []byte
	fileid   uint64
}

type fakeFsMgr struct {
	ls []*linkInfo
}

func (f *fakeFsMgr) searchFileid(fid uint64) (*linkInfo, error) {
	for _, item := range f.ls {
		if item.fileid == fid {
			return item, nil
		}
	}
	return nil, fmt.Errorf("fileid:%d not found", fid)
}

func (f *fakeFsMgr) searchFile(name string) (*linkInfo, error) {
	for _, item := range f.ls {
		if item.filename == name {
			return item, nil
		}
	}
	return nil, fmt.Errorf("file:%s not found", name)
}

func (f *fakeFsMgr) searchPrefix(prefix string) ([]*linkInfo, error) {
	rs := make([]*linkInfo, 0, 16)
	for _, item := range f.ls {
		if strings.HasPrefix(item.filename, prefix) {
			rs = append(rs, item)
		}
	}
	return rs, nil
}

type testRSC struct {
	io.ReadSeeker
}

func (t *testRSC) Close() error {
	return nil
}

func (f *fakeFsMgr) Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	finfo, err := f.searchFileid(fileid)
	if err != nil {
		return nil, err
	}
	return &testRSC{ReadSeeker: bytes.NewReader(finfo.data)}, nil
}

func (f *fakeFsMgr) Create(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return 0, fmt.Errorf("no impl")
}

func (f *fakeFsMgr) CreateLink(ctx context.Context, link string, fileid uint64, opt *entity.CreateLinkOption) error {
	return fmt.Errorf("no impl")
}

func (f *fakeFsMgr) ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error) {
	finfo, err := f.searchFile(link)
	if err != nil {
		return nil, err
	}
	return &entity.FileMappingItem{
		FileName:   finfo.filename,
		FileId:     finfo.fileid,
		FileSize:   finfo.size,
		IsDirEntry: false,
	}, nil
}

func (f *fakeFsMgr) IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	items, err := f.searchPrefix(prefix)
	if err != nil {
		return err
	}
	for _, item := range items {
		next, err := cb(ctx, item.filename, &entity.FileMappingItem{
			FileName:   item.filename,
			FileId:     item.fileid,
			FileSize:   int64(len(item.data)),
			IsDirEntry: false,
			Ctime:      uint64(time.Now().UnixMilli()),
			Mtime:      uint64(time.Now().UnixMilli()),
			FileMode:   0755,
		})
		if err != nil {
			return err
		}
		if !next {
			break
		}
	}
	return nil
}

type testFileSystemPair struct {
	filename string
	isDir    bool
	fileSize int64
	hasErr   bool
}

func TestFileSystemOpen(t *testing.T) {
	mgr := &fakeFsMgr{
		ls: []*linkInfo{
			{
				filename: "/root/d2/d3/f4.txt",
				size:     2,
				data:     []byte("ha"),
				fileid:   4567,
			},
		},
	}
	SetFileManagerImpl(mgr)
	testList := []*testFileSystemPair{
		{
			filename: "/",
			isDir:    true,
			fileSize: 0,
		},
		{
			filename: "/root",
			isDir:    true,
			fileSize: 0,
		},
		{
			filename: "/roo",
			hasErr:   true,
		},
		{
			filename: "/root/",
			isDir:    true,
			fileSize: 0,
		},
		{
			filename: "/root/d2",
			isDir:    true,
			fileSize: 0,
		},
		{
			filename: "/root/d2/d3",
			isDir:    true,
			fileSize: 0,
		},
		{
			filename: "/root/d2/d3/f4.txt",
			isDir:    false,
			fileSize: 2,
		},
		{
			filename: "/r",
			isDir:    false,
			hasErr:   true,
		},
	}
	fsys := AsFileSystem(context.Background())
	for _, item := range testList {
		finfo, err := fs.Stat(fsys, item.filename)
		//检查是否有错误
		assert.Equal(t, item.hasErr, err != nil)
		if err != nil {
			continue
		}
		//确认基础属性
		assert.Equal(t, item.isDir, finfo.IsDir())
		assert.Equal(t, item.fileSize, finfo.Size())
		if finfo.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, item.filename)
		assert.NoError(t, err)
		assert.Equal(t, int(item.fileSize), len(data))
		t.Logf("read file:%s, data:%s", item.filename, string(data))
	}
}

func TestFileSystemIter(t *testing.T) {
	mgr := &fakeFsMgr{
		ls: []*linkInfo{
			{
				filename: "/root/f1.txt",
				size:     5,
				data:     []byte("hello"),
				fileid:   1234,
			},
			{
				filename: "/root/f2.txt",
				size:     5,
				data:     []byte("world"),
				fileid:   2345,
			},
			{
				filename: "/root/d1/f3.txt",
				size:     6,
				data:     []byte("hahaha"),
				fileid:   3456,
			},
			{
				filename: "/root/d2/d3/f4.txt",
				size:     2,
				data:     []byte("ha"),
				fileid:   4567,
			},
			{
				filename: "/root/d2/d3/f5.txt",
				size:     4,
				data:     []byte("test"),
				fileid:   5678,
			},
		},
	}
	SetFileManagerImpl(mgr)
	ctx := context.Background()
	fsystem := AsFileSystem(ctx)
	err := fs.WalkDir(fsystem, "/", func(fullpath string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Logf("read dir err, path:%s, err:%v", fullpath, err)
			return err
		}

		stinfo, err := d.Info()
		if err != nil {
			t.Logf("stat info failed, fullpath:%s, err:%v", fullpath, err)
			return err
		}
		if d.IsDir() {
			t.Logf("-D- read dir succ, path:%s, name:%s, mod time:%d", fullpath, stinfo.Name(), stinfo.ModTime().UnixMilli())
			return nil
		}
		data, err := fs.ReadFile(fsystem, fullpath)
		if err != nil {
			t.Logf("read file failed, fullpath:%s, err:%v", fullpath, err)
		}
		t.Logf("-F- read file succ, filename:%s, fsize:%d, filemode:%d, mod time:%d, data:%s", fullpath, stinfo.Size(), stinfo.Mode(), stinfo.ModTime().UnixMilli(), string(data))
		return nil
	})
	assert.NoError(t, err)
}
