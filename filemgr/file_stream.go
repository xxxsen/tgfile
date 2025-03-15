package filemgr

import (
	"context"
	"fmt"
	"io"
	"tgfile/blockio"

	"github.com/xxxsen/common/logutil"
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

func newFileStream(ctx context.Context, bkio blockio.IBlockIO, b2f BlockIdToFileKeyConvertFunc, fsize int64) io.ReadSeekCloser {
	return &defaultFsIO{
		ctx:    ctx,
		bkio:   bkio,
		b2f:    b2f,
		fsize:  fsize,
		isOpen: true,
	}
}

func (f *defaultFsIO) calcOffset(offset int64, whence int) int64 {
	cur := int64(f.cursor)
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
		return 0, fmt.Errorf("file not in open state")
	}
	if f.tmpReader != nil {
		_ = f.tmpReader.Close()
		f.tmpReader = nil
	}
	cur := f.calcOffset(offset, whence)
	if cur < 0 {
		return 0, fmt.Errorf("invalid offset, cur:%d", cur)
	}
	if cur > f.fsize {
		return f.fsize, fmt.Errorf("seek over file size, cur:%d, fsz:%d", cur, f.fsize)
	}
	f.cursor = cur
	return cur, nil
}

func (f *defaultFsIO) Read(b []byte) (n int, err error) {
	defer func() {
		if err != nil && err != io.EOF {
			logutil.GetLogger(f.ctx).Error("read file stream failed", zap.Error(err), zap.Int64("cursor", f.cursor), zap.Int64("fsize", f.fsize))
		}
	}()
	if !f.isOpen {
		return 0, fmt.Errorf("file not in open state")
	}
	if f.tmpReader == nil {
		//如果cursor已经到了文件末尾, 那么直接返回EOF
		if f.cursor == f.fsize {
			return 0, io.EOF
		}
		//重新计算当前的位置
		blkid := f.cursor / f.bkio.MaxFileSize()
		pos := f.cursor % f.bkio.MaxFileSize()
		filekey, err := f.b2f(f.ctx, int32(blkid))
		if err != nil {
			return 0, fmt.Errorf("unable to convert blockid:%d to fileid, err:%w", blkid, err)
		}
		rc, err := f.bkio.Download(f.ctx, filekey, pos)
		if err != nil {
			return 0, fmt.Errorf("open stream fail, err:%w", err)
		}
		f.tmpReader = rc
	}
	n, err = f.tmpReader.Read(b)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if n > 0 {
		f.cursor += int64(n)
	}
	if err == io.EOF {
		_ = f.tmpReader.Close()
		f.tmpReader = nil
	}
	return n, nil
}

func (f *defaultFsIO) Close() error {
	var err error
	if f.tmpReader != nil {
		err = f.tmpReader.Close()
		f.tmpReader = nil
	}
	f.isOpen = false
	return err
}
