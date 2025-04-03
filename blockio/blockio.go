package blockio

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// IBlockIO 文件系统仅做简单的上传下载操作, 指定位置下载等能力交给外部实现
// 这样后续扩展/调试都会相对容易
type IBlockIO interface {
	Name() string
	MaxFileSize() int64
	Upload(ctx context.Context, r io.Reader) (string, error)
	Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error)
}

type CreateFunc func(args interface{}) (IBlockIO, error)

var mp = make(map[string]CreateFunc)

func Register(name string, fn CreateFunc) {
	mp[name] = fn
}

func Create(name string, args interface{}) (IBlockIO, error) {
	fn, ok := mp[name]
	if !ok {
		return nil, fmt.Errorf("block io type not found, name:%s", name)
	}
	return fn(args)
}

func List() []string {
	rs := make([]string, 0, len(mp))
	for name := range mp {
		rs = append(rs, name)
	}
	sort.Strings(rs)
	return rs
}
