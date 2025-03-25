package filemgr

import (
	"context"
	"os"
	"strings"
	"testing"
	"tgfile/entity"

	"github.com/stretchr/testify/assert"
)

type testReadDirPair struct {
	root  string
	names []string
}

func TestInternalReadDir(t *testing.T) {
	dataList := []*entity.FileMappingItem{
		{
			FileName:   "/",
			IsDirEntry: true,
		},
		{
			FileName:   "/a/",
			IsDirEntry: true,
		},
		{
			FileName:   "/a/b/",
			IsDirEntry: true,
		},
		{
			FileName:   "/a/b/1.txt",
			IsDirEntry: false,
		},
		{
			FileName:   "/a/2.txt",
			IsDirEntry: false,
		},
		{
			FileName:   "/a/3.txt",
			IsDirEntry: false,
		},
		{
			FileName:   "/4.txt",
			IsDirEntry: false,
		},
		{
			FileName:   "/5.txt",
			IsDirEntry: false,
		},
	}
	testList := []*testReadDirPair{
		{
			root: "/",
			names: []string{
				"a",
				"4.txt",
				"5.txt",
			},
		},
		{
			root: "/a/",
			names: []string{
				"b",
				"2.txt",
				"3.txt",
			},
		},
		{
			root: "/a/b/",
			names: []string{
				"1.txt",
			},
		},
	}
	for _, item := range testList {
		entries := make([]os.DirEntry, 0, 16)
		cb := cbfn(item.root, &entries)
		for _, dataItem := range dataList {
			if !strings.HasPrefix(dataItem.FileName, item.root) {
				continue
			}
			next, err := cb(context.Background(), dataItem.FileName, &entity.FileMappingItem{
				FileName: dataItem.FileName,
			})
			assert.NoError(t, err)
			if !next {
				break
			}
		}
		rs := make([]string, 0, len(entries))
		for _, ent := range entries {
			rs = append(rs, ent.Name())
		}
		assert.Equal(t, item.names, rs)
	}
}
