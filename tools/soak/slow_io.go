package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xxxsen/tgfile/blockio"
)

type slowBlockIO struct {
	delegate  blockio.IBlockIO
	delay     time.Duration
	chunkSize int
}

func newSlowBlockIO(
	delegate blockio.IBlockIO,
	delay time.Duration,
	chunkSize int,
) blockio.IBlockIO {
	return &slowBlockIO{
		delegate:  delegate,
		delay:     delay,
		chunkSize: chunkSize,
	}
}

func (s *slowBlockIO) Name() string {
	return s.delegate.Name()
}

func (s *slowBlockIO) MaxFileSize() int64 {
	return s.delegate.MaxFileSize()
}

func (s *slowBlockIO) Upload(
	ctx context.Context,
	reader io.Reader,
) (*blockio.UploadResult, error) {
	result, err := s.delegate.Upload(
		ctx,
		newDelayedReader(ctx, reader, s.delay, s.chunkSize),
	)
	if err != nil {
		return nil, fmt.Errorf("slow mock upload: %w", err)
	}
	return result, nil
}

func (s *slowBlockIO) Download(
	ctx context.Context,
	fileKey string,
	position int64,
) (io.ReadCloser, error) {
	stream, err := s.delegate.Download(ctx, fileKey, position)
	if err != nil {
		return nil, fmt.Errorf("slow mock download: %w", err)
	}
	return &delayedReadCloser{
		Reader: newDelayedReader(ctx, stream, s.delay, s.chunkSize),
		Closer: stream,
	}, nil
}

func (s *slowBlockIO) DeleteBlocks(
	ctx context.Context,
	deleteRefs []string,
) error {
	if err := waitForDelay(ctx, s.delay); err != nil {
		return err
	}
	if err := s.delegate.DeleteBlocks(ctx, deleteRefs); err != nil {
		return fmt.Errorf("slow mock delete: %w", err)
	}
	return nil
}

type delayedReader struct {
	context   context.Context
	reader    io.Reader
	delay     time.Duration
	chunkSize int
}

func newDelayedReader(
	ctx context.Context,
	reader io.Reader,
	delay time.Duration,
	chunkSize int,
) io.Reader {
	return &delayedReader{
		context:   ctx,
		reader:    reader,
		delay:     delay,
		chunkSize: chunkSize,
	}
}

func (r *delayedReader) Read(buffer []byte) (int, error) {
	if err := waitForDelay(r.context, r.delay); err != nil {
		return 0, err
	}
	if r.chunkSize > 0 && len(buffer) > r.chunkSize {
		buffer = buffer[:r.chunkSize]
	}
	read, err := r.reader.Read(buffer)
	if err == nil {
		return read, nil
	}
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	return read, fmt.Errorf("read delayed stream: %w", err)
}

type delayedReadCloser struct {
	io.Reader
	io.Closer
}

func waitForDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for delayed stream: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
