package filemgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xxxsen/tgfile/blockio"

	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/retry"
	"go.uber.org/zap"
)

// BlockIdToFileKeyConvertFunc 实现blockid到文件key的转换, 之后seeker会使用filesystem再去获取文件流
type BlockIdToFileKeyConvertFunc func(ctx context.Context, blkid int32) (string, error)

type defaultFsIO struct {
	ctx    context.Context
	bkio   blockio.IBlockIO
	b2f    BlockIdToFileKeyConvertFunc
	fsize  int64
	isOpen bool
	//
	cursor    int64
	tmpReader io.ReadCloser
}

func newFileStream(
	ctx context.Context,
	bkio blockio.IBlockIO,
	b2f BlockIdToFileKeyConvertFunc,
	fsize int64,
) io.ReadSeekCloser {
	return &defaultFsIO{
		ctx:    ctx,
		bkio:   bkio,
		b2f:    b2f,
		fsize:  fsize,
		isOpen: true,
	}
}

func (f *defaultFsIO) calcOffset(offset int64, whence int) int64 {
	cur := f.cursor
	switch whence {
	case io.SeekStart:
		cur = offset
	case io.SeekCurrent:
		cur += offset
	case io.SeekEnd:
		cur = f.fsize + offset
	}
	return cur
}

func (f *defaultFsIO) Seek(offset int64, whence int) (int64, error) {
	if !f.isOpen {
		return 0, ErrFileNotOpen
	}
	if f.tmpReader != nil {
		_ = f.tmpReader.Close()
		f.tmpReader = nil
	}
	cur := f.calcOffset(offset, whence)
	if cur < 0 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidOffset, cur)
	}
	if cur > f.fsize {
		return f.fsize, fmt.Errorf("%w: offset=%d size=%d", ErrSeekPastEnd, cur, f.fsize)
	}
	f.cursor = cur
	return cur, nil
}

func (f *defaultFsIO) retryGetDownloadStream(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	var rc io.ReadCloser
	if err := retry.RetryDo(ctx, 3, 1*time.Second, func(ctx context.Context) error {
		var err error
		rc, err = f.bkio.Download(ctx, filekey, pos)
		if err != nil {
			return fmt.Errorf("download file block: %w", err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("retry get stream final failed, err:%w", err)
	}
	return rc, nil
}

func (f *defaultFsIO) Read(b []byte) (int, error) {
	n, err := f.read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		logutil.GetLogger(f.ctx).Error(
			"read file stream failed",
			zap.Error(err),
			zap.Int64("cursor", f.cursor),
			zap.Int64("fsize", f.fsize),
		)
	}
	return n, err
}

func (f *defaultFsIO) read(b []byte) (int, error) {
	if !f.isOpen {
		return 0, ErrFileNotOpen
	}
	if f.tmpReader == nil {
		if err := f.openCurrentPart(); err != nil {
			return 0, err
		}
	}
	n, err := f.tmpReader.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read file part stream: %w", err)
	}
	if n > 0 {
		f.cursor += int64(n)
	}
	if errors.Is(err, io.EOF) {
		_ = f.tmpReader.Close()
		f.tmpReader = nil
	}
	return n, nil
}

func (f *defaultFsIO) openCurrentPart() error {
	if f.cursor == f.fsize {
		return io.EOF
	}
	blockSize := f.bkio.MaxFileSize()
	if blockSize <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidBlockSize, blockSize)
	}
	blockID := f.cursor / blockSize
	position := f.cursor % blockSize
	if blockID < 0 || blockID > maxFilePartCount {
		return fmt.Errorf("%w: %d", ErrInvalidFilePart, blockID)
	}
	fileKey, err := f.b2f(f.ctx, int32(blockID))
	if err != nil {
		return fmt.Errorf("convert block id %d to file key: %w", blockID, err)
	}
	reader, err := f.retryGetDownloadStream(f.ctx, fileKey, position)
	if err != nil {
		return fmt.Errorf("open file part stream: %w", err)
	}
	f.tmpReader = reader
	return nil
}

func (f *defaultFsIO) Close() error {
	var err error
	if f.tmpReader != nil {
		err = f.tmpReader.Close()
		f.tmpReader = nil
	}
	f.isOpen = false
	if err != nil {
		return fmt.Errorf("close file part stream: %w", err)
	}
	return nil
}

type bytesStream struct {
	*bytes.Reader
}

func (b *bytesStream) Close() error {
	return nil
}

func newBytesStream(raw []byte) io.ReadSeekCloser {
	return &bytesStream{Reader: bytes.NewReader(raw)}
}
