package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	links := []string{
		"/root/p1/p2/f1.jpg",
		"/root/p1/p2/p3/f2.jpg",
		"/root/p1/p2/f3.jpg",
		"/root/p1/f5.jpg",
		"/root/f4.jpg",
	}
	for idx, link := range links {
		next, err := cb(ctx, link, uint64(idx)+1)
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
		dirs, err := internalReadDir(ctx, top)
		assert.NoError(t, err)
		for _, dir := range dirs {
			t.Logf("level:%d, dir:%s, is dir:%t", level, dir.Name(), dir.IsDir())
			if dir.IsDir() {

				queue = append(queue, top+dir.Name())
			}
		}
	}
}

func TestOpen(t *testing.T) {
	dirs, err := os.ReadDir("/tmp/")
	assert.NoError(t, err)
	for _, dir := range dirs {
		t.Logf("name:%s, isdir:%t", dir.Name(), dir.IsDir())
	}
}

func TestBase(t *testing.T) {
	t.Logf("t1:%s", filepath.Base("/tmp/test/"))
	t.Logf("t1:%s", filepath.Base("/tmp/test"))
}
