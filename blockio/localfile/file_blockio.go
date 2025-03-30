package localfile

import (
	"context"
	"io"
	"os"
	"path"
	"tgfile/blockio"

	"github.com/google/uuid"
)

type localFileBlockIO struct {
	baseDir string
	blksize int64
}

func (f *localFileBlockIO) MaxFileSize() int64 {
	return f.blksize
}

func (f *localFileBlockIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	key := uuid.NewString()
	filename := path.Join(f.baseDir, key)
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filename, raw, 0644); err != nil {
		return "", err
	}
	return key, nil
}

func (f *localFileBlockIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	filename := path.Join(f.baseDir, filekey)
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	if pos != 0 {
		if _, err := file.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return file, nil
}

func New(dir string, blksize int64) (blockio.IBlockIO, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &localFileBlockIO{baseDir: dir, blksize: blksize}, nil
}
