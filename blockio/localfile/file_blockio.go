package localfile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/xxxsen/common/utils"

	"github.com/xxxsen/tgfile/blockio"

	"github.com/google/uuid"
)

type localFileBlockIO struct {
	baseDir string
	blksize int64
}

func (f *localFileBlockIO) MaxFileSize() int64 {
	return f.blksize
}

func (f *localFileBlockIO) Upload(_ context.Context, r io.Reader) (string, error) {
	key := uuid.NewString()
	filename := path.Join(f.baseDir, key)
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read block content: %w", err)
	}
	if err := os.WriteFile(filename, raw, 0o600); err != nil {
		return "", fmt.Errorf("write block file: %w", err)
	}
	return key, nil
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
