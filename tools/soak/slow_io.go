package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxxsen/tgfile/blockio"
)

type slowBlockIO struct {
	delegate        blockio.IBlockIO
	delay           time.Duration
	chunkSize       int
	uploadCalls     atomic.Uint64
	downloadCalls   atomic.Uint64
	deleteCalls     atomic.Uint64
	deleteRefs      atomic.Uint64
	downloadGateMu  sync.RWMutex
	downloadGate    <-chan struct{}
	downloadStarted chan struct{}
}

type blockIOCounts struct {
	uploads     uint64
	downloads   uint64
	deleteCalls uint64
	deleteRefs  uint64
}

func newSlowBlockIO(
	delegate blockio.IBlockIO,
	delay time.Duration,
	chunkSize int,
) *slowBlockIO {
	return &slowBlockIO{
		delegate:        delegate,
		delay:           delay,
		chunkSize:       chunkSize,
		downloadStarted: make(chan struct{}, 1024),
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
	s.uploadCalls.Add(1)
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
	s.downloadCalls.Add(1)
	s.downloadGateMu.RLock()
	gate := s.downloadGate
	s.downloadGateMu.RUnlock()
	select {
	case s.downloadStarted <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for slow mock download gate: %w", ctx.Err())
		case <-gate:
		}
	}
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
	s.deleteCalls.Add(1)
	s.deleteRefs.Add(uint64(len(deleteRefs)))
	if err := waitForDelay(ctx, s.delay); err != nil {
		return err
	}
	if err := s.delegate.DeleteBlocks(ctx, deleteRefs); err != nil {
		return fmt.Errorf("slow mock delete: %w", err)
	}
	return nil
}

func (s *slowBlockIO) counts() blockIOCounts {
	return blockIOCounts{
		uploads:     s.uploadCalls.Load(),
		downloads:   s.downloadCalls.Load(),
		deleteCalls: s.deleteCalls.Load(),
		deleteRefs:  s.deleteRefs.Load(),
	}
}

func (s *slowBlockIO) setDownloadGate(gate <-chan struct{}) {
	s.downloadGateMu.Lock()
	s.downloadGate = gate
	s.downloadGateMu.Unlock()
}

func (s *slowBlockIO) drainDownloadStarts() {
	for {
		select {
		case <-s.downloadStarted:
		default:
			return
		}
	}
}

func (s *slowBlockIO) waitForDownloadStart(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for slow mock download start: %w", ctx.Err())
	case <-s.downloadStarted:
		return nil
	}
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
