package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/xxxsen/tgfile/filemgr"
)

// observedFileManager exposes a deterministic barrier for cache preflight
// requests without changing the production cache implementation.
type observedFileManager struct {
	filemgr.IFileManager
	mu           sync.RWMutex
	openObserver chan<- struct{}
}

func newObservedFileManager(manager filemgr.IFileManager) *observedFileManager {
	return &observedFileManager{IFileManager: manager}
}

func (m *observedFileManager) OpenFile(
	ctx context.Context,
	fileID uint64,
) (io.ReadSeekCloser, error) {
	m.mu.RLock()
	observer := m.openObserver
	m.mu.RUnlock()
	if observer != nil {
		select {
		case observer <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("observe soak file open: %w", ctx.Err())
		}
	}
	reader, err := m.IFileManager.OpenFile(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("open observed soak file: %w", err)
	}
	return reader, nil
}

func (m *observedFileManager) setOpenObserver(observer chan<- struct{}) {
	m.mu.Lock()
	m.openObserver = observer
	m.mu.Unlock()
}
