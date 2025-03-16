package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeMgr struct {
}

func (f *fakeMgr) Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error) {
	return nil, fmt.Errorf("no impl")
}

func (f *fakeMgr) Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	return nil, fmt.Errorf("no impl")
}

func (f *fakeMgr) Create(ctx context.Context, size int64, r io.Reader) (uint64, error) {
	return 0, fmt.Errorf("no impl")
}

func (f *fakeMgr) CreateLink(ctx context.Context, link string, fileid uint64) error {
	return fmt.Errorf("no impl")
}

func (f *fakeMgr) ResolveLink(ctx context.Context, link string) (uint64, error) {
	return 0, fmt.Errorf("no impl")
}

func (f *fakeMgr) IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	filem := map[string]uint64{
		"/root/p1/p2/f1.jpg":    123,
		"/root/p1/p2/p3/f2.jpg": 234,
		"/root/p1/p2/f3.jpg":    2345,
		"/root/f4.jpg":          5555,
		"/root/p1/f5.jpg":       666,
	}
	for k, v := range filem {
		next, err := cb(ctx, k, v)
		if err != nil {
			return err
		}
		if !next {
			break
		}
	}
	return nil
}

func TestReadDir(t *testing.T) {
	impl := &fakeMgr{}
	SetFileManagerImpl(impl)
	ctx := context.Background()
	queue := make([]string, 0, 16)
	queue = append(queue, "/")
	level := 0

	for len(queue) > 0 {
		top := queue[0]
		queue = queue[1:]
		if !strings.HasSuffix(top, "/") {
			top += "/"
		}
		level++
		dirs, err := ReadDir(ctx, top)
		assert.NoError(t, err)
		for _, dir := range dirs {
			t.Logf("level:%d, dir:%s, is dir:%t", level, dir.Name(), dir.IsDir())
			if dir.IsDir() {

				queue = append(queue, top+dir.Name())
			}
		}
	}
}
