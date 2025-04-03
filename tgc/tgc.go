package tgc

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

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
		return c.c.Client.CreatePart(ctx, filekey, partid, r)
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
	uploadKey, blocksize, err := c.c.Client.CreateDraft(ctx, info.Size())
	if err != nil {
		return "", err
	}
	if blocksize == 0 {
		return "", fmt.Errorf("zero block size from server")
	}
	blockcnt := int((info.Size() + blocksize - 1) / blocksize)
	eg, _ := errgroup.WithContext(ctx)
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
				logutil.GetLogger(ctx).Debug("part upload finish", zap.Int64("part_id", partid), zap.Duration("cost", time.Since(start)))
			}()
			return c.partUpload(ctx, src, uploadKey, partid, startAt, partSize)
		})
	}
	if err := eg.Wait(); err != nil {
		return "", err
	}
	fileKey, err := c.c.Client.FinishCreate(ctx, uploadKey)
	if err != nil {
		return "", err
	}
	return fileKey, nil
}
