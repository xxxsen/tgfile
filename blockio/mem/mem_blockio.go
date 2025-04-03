package mem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/xxxsen/common/utils"
	"github.com/xxxsen/tgfile/blockio"

	"github.com/google/uuid"
)

type memBlockIO struct {
	bksize int64
	m      sync.Map
}

func (m *memBlockIO) MaxFileSize() int64 {
	return m.bksize
}

func (m *memBlockIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	key := uuid.NewString()
	m.m.Store(key, raw)
	return key, nil
}

func (m *memBlockIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	raw, ok := m.m.Load(filekey)
	if !ok {
		return nil, fmt.Errorf("key:%s not found", filekey)
	}
	data := raw.([]byte)
	if pos > int64(len(data)) {
		pos = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[pos:])), nil
}

func (m *memBlockIO) Name() string {
	return "mem"
}

func New(bksize int64) (blockio.IBlockIO, error) {
	if bksize == 0 {
		bksize = 4 * 1024
	}
	return &memBlockIO{bksize: bksize}, nil
}

func create(args interface{}) (blockio.IBlockIO, error) {
	c := &config{}
	if err := utils.ConvStructJson(args, c); err != nil {
		return nil, err
	}
	return New(c.BlockSize)
}

func init() {
	blockio.Register("mem", create)
}
