package mem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/xxxsen/common/utils"

	"github.com/xxxsen/tgfile/blockio"

	"github.com/google/uuid"
)

var (
	errInvalidBlockValue          = errors.New("invalid in-memory block value")
	errEmptyMemoryDeleteReference = errors.New("empty in-memory delete reference")
)

type memBlockIO struct {
	bksize int64
	m      sync.Map
}

func (m *memBlockIO) MaxFileSize() int64 {
	return m.bksize
}

func (m *memBlockIO) Upload(_ context.Context, r io.Reader) (*blockio.UploadResult, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read block content: %w", err)
	}
	key := uuid.NewString()
	m.m.Store(key, raw)
	return &blockio.UploadResult{FileKey: key, DeleteRef: key, UploadedAt: time.Now().UnixMilli()}, nil
}

func (m *memBlockIO) Download(_ context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	raw, ok := m.m.Load(filekey)
	if !ok {
		return nil, fmt.Errorf("block key %q: %w", filekey, os.ErrNotExist)
	}
	data, ok := raw.([]byte)
	if !ok {
		return nil, errInvalidBlockValue
	}
	if pos > int64(len(data)) {
		pos = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[pos:])), nil
}

func (m *memBlockIO) Name() string {
	return "mem"
}

func (m *memBlockIO) DeleteBlocks(_ context.Context, deleteRefs []string) error {
	for _, ref := range deleteRefs {
		if ref == "" {
			return errEmptyMemoryDeleteReference
		}
		m.m.Delete(ref)
	}
	return nil
}

func New(bksize int64) (blockio.IBlockIO, error) {
	if bksize == 0 {
		bksize = 4 * 1024
	}
	return &memBlockIO{bksize: bksize}, nil
}

func create(args any) (blockio.IBlockIO, error) {
	c := &config{}
	if err := utils.ConvStructJson(args, c); err != nil {
		return nil, fmt.Errorf("decode memory block config: %w", err)
	}
	return New(c.BlockSize)
}

func init() {
	blockio.Register("mem", create)
}
