package blockio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

var ErrTypeNotFound = errors.New("block io type not found")

// IBlockIO stores opaque blocks and deletes only the backend references
// returned by Upload. File and mapping semantics remain in FileManager.
type IBlockIO interface {
	Name() string
	MaxFileSize() int64
	Upload(ctx context.Context, r io.Reader) (*UploadResult, error)
	Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error)
	DeleteBlocks(ctx context.Context, deleteRefs []string) error
}

type UploadResult struct {
	FileKey    string
	DeleteRef  string
	UploadedAt int64
}

type DeleteFailure interface {
	error
	DeleteStatusCode() int
	DeleteRetryAfter() time.Duration
}

type CreateFunc func(args any) (IBlockIO, error)

var mp = make(map[string]CreateFunc)

func Register(name string, fn CreateFunc) {
	mp[name] = fn
}

func Create(name string, args any) (IBlockIO, error) {
	fn, ok := mp[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTypeNotFound, name)
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
