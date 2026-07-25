package localfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/xxxsen/common/utils"

	"github.com/xxxsen/tgfile/blockio"

	"github.com/google/uuid"
)

var errInvalidDeleteReference = errors.New("invalid local file delete reference")

type localFileBlockIO struct {
	baseDir string
	blksize int64
}

func (f *localFileBlockIO) MaxFileSize() int64 {
	return f.blksize
}

func (f *localFileBlockIO) Upload(_ context.Context, r io.Reader) (*blockio.UploadResult, error) {
	key := uuid.NewString()
	filename := path.Join(f.baseDir, key)
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read block content: %w", err)
	}
	if err := os.WriteFile(filename, raw, 0o600); err != nil {
		return nil, fmt.Errorf("write block file: %w", err)
	}
	return &blockio.UploadResult{FileKey: key, DeleteRef: key, UploadedAt: time.Now().UnixMilli()}, nil
}

func (f *localFileBlockIO) Download(_ context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	filename := path.Join(f.baseDir, filekey)
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open block file: %w", err)
	}
	if pos != 0 {
		if _, err := file.Seek(pos, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("seek block file: %w", err)
		}
	}
	return file, nil
}

func (f *localFileBlockIO) Name() string {
	return "localfile"
}

func (f *localFileBlockIO) DeleteBlocks(_ context.Context, deleteRefs []string) error {
	for _, ref := range deleteRefs {
		if ref == "" || path.Base(ref) != ref {
			return errInvalidDeleteReference
		}
		if err := os.Remove(path.Join(f.baseDir, ref)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete block file: %w", err)
		}
	}
	return nil
}

func New(dir string, blksize int64) (blockio.IBlockIO, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create block directory: %w", err)
	}
	return &localFileBlockIO{baseDir: dir, blksize: blksize}, nil
}

func create(args any) (blockio.IBlockIO, error) {
	c := &config{}
	if err := utils.ConvStructJson(args, c); err != nil {
		return nil, fmt.Errorf("decode localfile config: %w", err)
	}
	return New(c.Dir, c.BlockSize)
}

func init() {
	blockio.Register("localfile", create)
}
