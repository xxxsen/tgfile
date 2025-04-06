package tgc

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/retry"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type TGFileClient struct {
	c *config
}

func New(opts ...Option) *TGFileClient {
	c := &config{
		Thread: 4,
	}
	for _, opt := range opts {
		opt(c)
	}
	return &TGFileClient{c: c}
}

func (c *TGFileClient) partUpload(ctx context.Context, src string, filekey string, partid int64, startAt int64, size int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := retry.RetryDo(ctx, 3, 2*time.Second, func(ctx context.Context) error {
		if _, err := f.Seek(startAt, io.SeekStart); err != nil {
			return err
		}
		r := io.LimitReader(f, size)
		if err := c.c.Client.CreatePart(ctx, filekey, partid, r); err != nil {
			logutil.GetLogger(ctx).Error("upload part failed, wait retry", zap.Error(err), zap.Int64("part_id", partid))
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (c *TGFileClient) UploadFile(ctx context.Context, src string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	uploadKey, blocksize, err := c.c.Client.CreateDraft(ctx, info.Name(), info.Size())
	if err != nil {
		logutil.GetLogger(ctx).Error("create file draft failed", zap.Error(err), zap.Int64("file_size", info.Size()))
		return "", err
	}
	if blocksize == 0 {
		return "", fmt.Errorf("zero block size from server")
	}
	blockcnt := int((info.Size() + blocksize - 1) / blocksize)
	eg, subctx := errgroup.WithContext(ctx)
	eg.SetLimit(c.c.Thread)
	logutil.GetLogger(ctx).Debug("start upload file", zap.Int64("block_size", blocksize), zap.Int("block_cnt", blockcnt))
	for i := 0; i < blockcnt; i++ {
		partid := int64(i)
		startAt := int64(i) * blocksize
		partSize := blocksize
		if i == blockcnt-1 {
			partSize = info.Size() - startAt
		}
		eg.Go(func() error {
			start := time.Now()
			defer func() {
				cost := time.Since(start)
				speed := "-"
				if cost > 0 {
					speed = humanize.IBytes(uint64(float64(blocksize) * 1000 / float64(int64(cost/time.Millisecond))))
				}
				logutil.GetLogger(ctx).Debug("part upload finish", zap.Int64("part_id", partid), zap.Duration("cost", cost), zap.String("speed", speed+"/s"))
			}()
			return c.partUpload(subctx, src, uploadKey, partid, startAt, partSize)
		})
	}
	if err := eg.Wait(); err != nil {
		logutil.GetLogger(ctx).Error("upload part failed", zap.Error(err))
		return "", err
	}
	fileKey, err := c.c.Client.FinishCreate(ctx, uploadKey)
	if err != nil {
		logutil.GetLogger(ctx).Error("finish create failed", zap.Error(err))
		return "", err
	}
	return fileKey, nil
}
