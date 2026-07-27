package filemgr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

const (
	defaultMaxAllowKeySizeToL1 = 4 * 1024
	defaultMaxAllowKeySizeToL2 = 512 * 1024
	fileCacheKeyFormatVersion  = uint32(2)
	cacheWarningInterval       = time.Minute
)

type FileCacheIdentity struct {
	StorageBinding [sha256.Size]byte
	FileID         uint64
	Size           int64
	PartCount      int32
	State          uint32
	LayoutVersion  int32
	Ctime          int64
	Mtime          int64
	ExtInfo        string
}

type fileCacheKey [sha256.Size]byte

type IFileIOCache interface {
	Load(
		ctx context.Context,
		identity FileCacheIdentity,
		loader func(ctx context.Context) (io.ReadSeekCloser, error),
	) (io.ReadSeekCloser, error)
	Close(context.Context) error
}

type FileIOCacheConfig struct {
	DisableL1Cache bool
	L1CacheSize    int
	L1KeySizeLimit int
	DisableL2Cache bool
	L2CacheSize    int
	L2KeySizeLimit int
	L2CacheDir     string
	StorageBinding [sha256.Size]byte
}

type fillCall struct {
	done chan struct{}
}

type fileIOCacheStats struct {
	l1Hit            atomic.Uint64
	l2Hit            atomic.Uint64
	miss             atomic.Uint64
	fillLeader       atomic.Uint64
	fillFollower     atomic.Uint64
	sourceMismatch   atomic.Uint64
	invalidPersisted atomic.Uint64
	evict            atomic.Uint64
	reject           atomic.Uint64
	fallback         atomic.Uint64
	cleanupFailure   atomic.Uint64
}

type cacheWarningLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

type fileIOCacheImpl struct {
	ctx context.Context
	c   FileIOCacheConfig
	l1  *ristretto.Cache[string, []byte]
	l2  *diskCache

	fillMu   sync.Mutex
	fills    map[fileCacheKey]*fillCall
	loads    sync.WaitGroup
	closeCh  chan struct{}
	closing  bool
	closed   bool
	stats    fileIOCacheStats
	warnings cacheWarningLimiter
}

func NewDefaultFileIOCacheConfig() *FileIOCacheConfig {
	return &FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    16 * 1024 * 1024,
		L1KeySizeLimit: defaultMaxAllowKeySizeToL1,
		DisableL2Cache: false,
		L2CacheSize:    5 * 1024 * 1024 * 1024,
		L2KeySizeLimit: defaultMaxAllowKeySizeToL2,
		L2CacheDir:     filepath.Join(os.TempDir(), "tgfile-cache"),
	}
}

func NewFileIOCache(config *FileIOCacheConfig) (IFileIOCache, error) {
	return NewFileIOCacheWithContext(context.Background(), config)
}

func NewFileIOCacheWithContext(ctx context.Context, config *FileIOCacheConfig) (IFileIOCache, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", ErrInvalidCache)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidCache)
	}
	copied := *config
	if err := validateFileIOCacheConfig(&copied); err != nil {
		return nil, err
	}
	cache := &fileIOCacheImpl{
		ctx:     ctx,
		c:       copied,
		fills:   make(map[fileCacheKey]*fillCall),
		closeCh: make(chan struct{}, 1),
	}
	cache.closeCh <- struct{}{}
	if err := cache.buildL1Cache(); err != nil {
		return nil, err
	}
	if !copied.DisableL2Cache {
		disk, err := newDiskCache(
			ctx,
			copied.L2CacheDir,
			int64(copied.L2CacheSize),
			int64(copied.L2KeySizeLimit),
			copied.StorageBinding,
			&cache.stats,
			&cache.warnings,
		)
		if err != nil {
			if cache.l1 != nil {
				cache.l1.Close()
			}
			return nil, fmt.Errorf("initialize L2 cache: %w", err)
		}
		cache.l2 = disk
	}
	return cache, nil
}

func validateFileIOCacheConfig(config *FileIOCacheConfig) error {
	if !config.DisableL1Cache {
		if config.L1CacheSize <= 0 || config.L1KeySizeLimit <= 0 || config.L1KeySizeLimit > config.L1CacheSize {
			return fmt.Errorf("%w: L1 size and per-key limit are inconsistent", ErrInvalidCache)
		}
	}
	if config.DisableL2Cache {
		return nil
	}
	if config.L2CacheSize <= 0 || config.L2KeySizeLimit <= 0 || config.L2KeySizeLimit > config.L2CacheSize {
		return fmt.Errorf("%w: L2 size and per-key limit are inconsistent", ErrInvalidCache)
	}
	if config.L2CacheDir == "" {
		return fmt.Errorf("%w: L2 directory is empty", ErrInvalidCache)
	}
	absolute, err := filepath.Abs(config.L2CacheDir)
	if err != nil {
		return fmt.Errorf("%w: resolve L2 directory: %w", ErrInvalidCache, err)
	}
	config.L2CacheDir = filepath.Clean(absolute)
	return nil
}

func (f *fileIOCacheImpl) buildL1Cache() error {
	if f.c.DisableL1Cache {
		return nil
	}
	numCounters := max(int64(f.c.L1CacheSize/f.c.L1KeySizeLimit)*10, 10)
	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters:        numCounters,
		MaxCost:            int64(f.c.L1CacheSize),
		BufferItems:        64,
		IgnoreInternalCost: true,
		Cost: func(value []byte) int64 {
			return int64(len(value))
		},
		OnReject: func(item *ristretto.Item[[]byte]) {
			f.stats.reject.Add(1)
			logutil.GetLogger(f.ctx).Debug("L1 cache admission rejected", zap.Int("cost", len(item.Value)))
		},
		OnEvict: func(*ristretto.Item[[]byte]) {
			f.stats.evict.Add(1)
		},
	})
	if err != nil {
		return fmt.Errorf("initialize L1 cache: %w", err)
	}
	f.l1 = cache
	return nil
}

func (f *fileIOCacheImpl) Load(
	ctx context.Context,
	identity FileCacheIdentity,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidCache)
	}
	if loader == nil {
		return nil, fmt.Errorf("%w: loader is nil", ErrInvalidCache)
	}
	if identity.Size < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidFileSize, identity.Size)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load file cache: %w", err)
	}
	if err := f.beginLoad(); err != nil {
		return nil, err
	}
	defer f.loads.Done()

	l1Eligible := !f.c.DisableL1Cache && identity.Size <= int64(f.c.L1KeySizeLimit)
	l2Eligible := !f.c.DisableL2Cache && identity.Size <= int64(f.c.L2KeySizeLimit)
	if !l1Eligible && !l2Eligible {
		return loadUncacheable(ctx, loader)
	}
	identity.StorageBinding = f.c.StorageBinding
	return f.loadCacheable(ctx, buildFileCacheKey(identity), identity.Size, l1Eligible, l2Eligible, loader)
}

func (f *fileIOCacheImpl) loadCacheable(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	l1Eligible, l2Eligible bool,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for file cache: %w", err)
		}
		reader, found, err := f.lookup(ctx, key, size, l1Eligible, l2Eligible)
		if err != nil {
			return nil, err
		}
		if found {
			return reader, nil
		}

		f.stats.miss.Add(1)
		call, leader, err := f.beginFill(key)
		if err != nil {
			return nil, err
		}
		if !leader {
			f.stats.fillFollower.Add(1)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("wait for cache fill: %w", ctx.Err())
			case <-call.done:
				continue
			}
		}
		f.stats.fillLeader.Add(1)
		return f.runFillLeader(ctx, key, size, l1Eligible, l2Eligible, loader, call)
	}
}

func loadUncacheable(
	ctx context.Context,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	stream, err := loader(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(
			fmt.Errorf("load uncacheable file: %w", ctxErr),
			closeLoadedStream(stream),
		)
	}
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("load uncacheable file: %w", err),
			closeLoadedStream(stream),
		)
	}
	if stream == nil {
		return nil, fmt.Errorf("%w: uncacheable loader returned a nil stream", errCacheSourceRead)
	}
	return stream, nil
}

func (f *fileIOCacheImpl) lookup(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	l1Eligible, l2Eligible bool,
) (io.ReadSeekCloser, bool, error) {
	if l1Eligible {
		if raw, found := f.readL1(key, size); found {
			f.stats.l1Hit.Add(1)
			return newBytesStream(raw), true, nil
		}
	}
	if !l2Eligible {
		return nil, false, nil
	}
	reader, found, err := f.l2.open(ctx, key, size)
	if err != nil || !found {
		return nil, false, err
	}
	f.stats.l2Hit.Add(1)
	if !l1Eligible {
		return reader, true, nil
	}
	raw, readErr := readExactBytes(ctx, reader, size)
	closeErr := reader.Close()
	if readErr == nil && closeErr == nil {
		f.writeL1(key, raw)
		return newBytesStream(raw), true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, fmt.Errorf("read L2 cache entry: %w", ctxErr)
	}
	if diskReader, ok := reader.(*diskCacheReader); ok {
		f.l2.invalidate(diskReader.entry)
	}
	f.warn(ctx, "discard unreadable L2 cache entry", errors.Join(readErr, closeErr))
	return nil, false, nil
}

func (f *fileIOCacheImpl) runFillLeader(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	l1Eligible, l2Eligible bool,
	loader func(context.Context) (io.ReadSeekCloser, error),
	call *fillCall,
) (io.ReadSeekCloser, error) {
	defer f.finishFill(key, call)
	reader, found, err := f.lookup(ctx, key, size, l1Eligible, l2Eligible)
	if err != nil || found {
		return reader, err
	}
	return f.fill(ctx, key, size, l1Eligible, l2Eligible, loader)
}

func (f *fileIOCacheImpl) fill(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	l1Eligible, l2Eligible bool,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	if l1Eligible {
		return f.fillBuffered(ctx, key, size, l2Eligible, loader)
	}
	return f.fillDisk(ctx, key, size, loader)
}

func (f *fileIOCacheImpl) fillBuffered(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	l2Eligible bool,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	source, err := loadCacheSource(ctx, loader)
	if err != nil {
		return nil, err
	}
	raw, readErr := readExactBytes(ctx, source, size)
	closeErr := source.Close()
	if readErr != nil || closeErr != nil {
		f.recordSourceError(readErr)
		return nil, errors.Join(readErr, closeErr)
	}
	if l2Eligible {
		if err := f.fillL2FromBytes(ctx, key, size, raw); err != nil {
			return nil, err
		}
	}
	f.writeL1(key, raw)
	return newBytesStream(raw), nil
}

func (f *fileIOCacheImpl) fillL2FromBytes(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	raw []byte,
) error {
	candidate, err := f.l2.writeCandidate(ctx, key, size, bytes.NewReader(raw))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("fill L2 cache: %w", ctxErr)
		}
		f.stats.fallback.Add(1)
		f.warn(ctx, "skip L2 cache fill", err)
		return nil
	}
	if f.l2.admit(candidate) {
		return nil
	}
	f.stats.reject.Add(1)
	f.l2.cleanupPaths([]string{candidate.path})
	return nil
}

func (f *fileIOCacheImpl) fillDisk(
	ctx context.Context,
	key fileCacheKey,
	size int64,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	source, err := loadCacheSource(ctx, loader)
	if err != nil {
		return nil, err
	}
	candidate, candidateErr := f.l2.writeCandidate(ctx, key, size, source)
	if candidateErr != nil {
		if !errors.Is(candidateErr, errCacheSourceRead) && ctx.Err() == nil {
			f.stats.fallback.Add(1)
			return f.fallbackSource(ctx, source, loader, candidateErr)
		}
		closeErr := source.Close()
		f.recordSourceError(candidateErr)
		return nil, errors.Join(candidateErr, closeErr)
	}
	if closeErr := source.Close(); closeErr != nil {
		f.l2.cleanupPaths([]string{candidate.path})
		return nil, fmt.Errorf("close cache source: %w", closeErr)
	}
	if f.l2.admit(candidate) {
		reader, found, openErr := f.l2.open(ctx, key, size)
		if openErr != nil {
			return nil, openErr
		}
		if found {
			return reader, nil
		}
		f.stats.fallback.Add(1)
		return f.reloadWithoutCache(ctx, loader, fmt.Errorf("%w: admitted L2 entry cannot be opened", errCacheLocalIO))
	}
	f.stats.reject.Add(1)
	file, openErr := os.Open(candidate.path)
	if openErr == nil {
		return &removeOnCloseFile{File: file, cache: f.l2, location: candidate.path}, nil
	}
	f.l2.cleanupPaths([]string{candidate.path})
	f.stats.fallback.Add(1)
	return f.reloadWithoutCache(ctx, loader, fmt.Errorf("%w: open rejected L2 candidate: %w", errCacheLocalIO, openErr))
}

func loadCacheSource(
	ctx context.Context,
	loader func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	source, err := loader(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(
			fmt.Errorf("load cache source: %w", ctxErr),
			closeLoadedStream(source),
		)
	}
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("load cache source: %w", err),
			closeLoadedStream(source),
		)
	}
	if source == nil {
		return nil, fmt.Errorf("%w: cache loader returned a nil stream", errCacheSourceRead)
	}
	return source, nil
}

func (f *fileIOCacheImpl) fallbackSource(
	ctx context.Context,
	source io.ReadSeekCloser,
	loader func(context.Context) (io.ReadSeekCloser, error),
	cacheErr error,
) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("serve cache fallback: %w", err), source.Close())
	}
	if _, err := source.Seek(0, io.SeekStart); err == nil {
		f.warn(ctx, "serve cache source after L2 failure", cacheErr)
		return source, nil
	}
	closeErr := source.Close()
	return f.reloadWithoutCache(ctx, loader, errors.Join(cacheErr, closeErr))
}

func (f *fileIOCacheImpl) reloadWithoutCache(
	ctx context.Context,
	loader func(context.Context) (io.ReadSeekCloser, error),
	cacheErr error,
) (io.ReadSeekCloser, error) {
	stream, err := loader(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Join(
			fmt.Errorf("reload cache source: %w", ctxErr),
			closeLoadedStream(stream),
		)
	}
	if err != nil {
		closeErr := closeLoadedStream(stream)
		return nil, errors.Join(cacheErr, fmt.Errorf("reload cache source: %w", err), closeErr)
	}
	if stream == nil {
		return nil, errors.Join(cacheErr, fmt.Errorf("%w: cache reload returned a nil stream", errCacheSourceRead))
	}
	f.warn(ctx, "serve reloaded source after L2 failure", cacheErr)
	return stream, nil
}

func closeLoadedStream(stream io.ReadSeekCloser) error {
	if stream == nil {
		return nil
	}
	if err := stream.Close(); err != nil {
		return fmt.Errorf("close cache source returned with error: %w", err)
	}
	return nil
}

func readExactBytes(ctx context.Context, source io.Reader, expected int64) ([]byte, error) {
	maxInt := int64(^uint(0) >> 1)
	if expected < 0 || expected > maxInt {
		return nil, fmt.Errorf("%w: expected size %d cannot be buffered", ErrInvalidFileSize, expected)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read cache source: %w", err)
	}
	raw := make([]byte, int(expected))
	count, err := io.ReadFull(source, raw)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("read cache source: %w", ctxErr)
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: expected=%d actual=%d", io.ErrUnexpectedEOF, expected, count)
		}
		return nil, fmt.Errorf("read cache source: %w", err)
	}
	var extra [1]byte
	extraCount, trailerErr := io.ReadFull(source, extra[:])
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("read cache source trailer: %w", ctxErr)
	}
	if extraCount != 0 || trailerErr == nil {
		return nil, fmt.Errorf("%w: expected=%d", ErrCacheSourceSizeMismatch, expected)
	}
	if !errors.Is(trailerErr, io.EOF) {
		return nil, fmt.Errorf("read cache source trailer: %w", trailerErr)
	}
	return raw, nil
}

func (f *fileIOCacheImpl) readL1(key fileCacheKey, expectedSize int64) ([]byte, bool) {
	if f.l1 == nil {
		return nil, false
	}
	value, found := f.l1.Get(key.hex())
	if !found {
		return nil, false
	}
	if int64(len(value)) != expectedSize {
		f.l1.Del(key.hex())
		f.l1.Wait()
		return nil, false
	}
	return value, true
}

func (f *fileIOCacheImpl) writeL1(key fileCacheKey, raw []byte) {
	if f.l1 == nil {
		return
	}
	if !f.l1.Set(key.hex(), raw, int64(len(raw))) {
		f.stats.reject.Add(1)
		return
	}
	f.l1.Wait()
}

func buildFileCacheKey(identity FileCacheIdentity) fileCacheKey {
	hash := sha256.New()
	writeHashUint32(hash, fileCacheKeyFormatVersion)
	_, _ = hash.Write(identity.StorageBinding[:])
	writeHashUint64(hash, identity.FileID)
	writeHashInt64(hash, identity.Size)
	writeHashInt32(hash, identity.PartCount)
	writeHashUint32(hash, identity.State)
	writeHashInt32(hash, identity.LayoutVersion)
	writeHashInt64(hash, identity.Ctime)
	writeHashInt64(hash, identity.Mtime)
	writeHashUint64(hash, uint64(len(identity.ExtInfo)))
	_, _ = hash.Write([]byte(identity.ExtInfo))
	var key fileCacheKey
	copy(key[:], hash.Sum(nil))
	return key
}

func writeHashUint32(writer io.Writer, value uint32) {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], value)
	_, _ = writer.Write(buffer[:])
}

func writeHashUint64(writer io.Writer, value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = writer.Write(buffer[:])
}

func writeHashInt32(writer io.Writer, value int32) {
	_ = binary.Write(writer, binary.BigEndian, value)
}

func writeHashInt64(writer io.Writer, value int64) {
	_ = binary.Write(writer, binary.BigEndian, value)
}

func (f *fileIOCacheImpl) beginFill(key fileCacheKey) (*fillCall, bool, error) {
	f.fillMu.Lock()
	defer f.fillMu.Unlock()
	if f.closed {
		return nil, false, ErrCacheClosed
	}
	if call, exists := f.fills[key]; exists {
		return call, false, nil
	}
	call := &fillCall{done: make(chan struct{})}
	f.fills[key] = call
	return call, true, nil
}

func (f *fileIOCacheImpl) finishFill(key fileCacheKey, call *fillCall) {
	f.fillMu.Lock()
	if current, exists := f.fills[key]; exists && current == call {
		delete(f.fills, key)
		close(call.done)
	}
	f.fillMu.Unlock()
}

func (f *fileIOCacheImpl) beginLoad() error {
	f.fillMu.Lock()
	defer f.fillMu.Unlock()
	if f.closing || f.closed {
		return ErrCacheClosed
	}
	f.loads.Add(1)
	return nil
}

func (f *fileIOCacheImpl) Close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidCache)
	}
	select {
	case <-f.closeCh:
	default:
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire file cache close gate: %w", ctx.Err())
		case <-f.closeCh:
		}
	}
	defer func() { f.closeCh <- struct{}{} }()
	f.fillMu.Lock()
	if f.closed {
		f.fillMu.Unlock()
		return nil
	}
	f.closing = true
	f.fillMu.Unlock()
	if err := waitGroupContext(ctx, &f.loads); err != nil {
		return err
	}
	var closeErr error
	if f.l2 != nil {
		closeErr = errors.Join(closeErr, f.l2.close(ctx))
	}
	if closeErr != nil {
		return closeErr
	}
	if f.l1 != nil {
		f.l1.Close()
	}
	f.fillMu.Lock()
	f.closed = true
	f.fillMu.Unlock()
	f.logStats()
	return nil
}

func waitGroupContext(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for file cache operations: %w", ctx.Err())
	case <-done:
		return nil
	}
}

func (f *fileIOCacheImpl) recordSourceError(err error) {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrCacheSourceSizeMismatch) {
		f.stats.sourceMismatch.Add(1)
	}
}

func (f *fileIOCacheImpl) warn(ctx context.Context, message string, err error) {
	reason := cacheReason(err)
	if !f.warnings.allow(reason) {
		return
	}
	logutil.GetLogger(ctx).Warn(message, zap.String("reason", reason))
}

func (f *fileIOCacheImpl) logStats() {
	logutil.GetLogger(f.ctx).Debug(
		"file cache statistics",
		zap.Uint64("l1_hit", f.stats.l1Hit.Load()),
		zap.Uint64("l2_hit", f.stats.l2Hit.Load()),
		zap.Uint64("miss", f.stats.miss.Load()),
		zap.Uint64("fill_leader", f.stats.fillLeader.Load()),
		zap.Uint64("fill_follower", f.stats.fillFollower.Load()),
		zap.Uint64("source_mismatch", f.stats.sourceMismatch.Load()),
		zap.Uint64("invalid_persisted", f.stats.invalidPersisted.Load()),
		zap.Uint64("evict", f.stats.evict.Load()),
		zap.Uint64("reject", f.stats.reject.Load()),
		zap.Uint64("fallback", f.stats.fallback.Load()),
		zap.Uint64("cleanup_failure", f.stats.cleanupFailure.Load()),
	)
}

func (l *cacheWarningLimiter) allow(reason string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	if previous, exists := l.last[reason]; exists && now.Sub(previous) < cacheWarningInterval {
		return false
	}
	l.last[reason] = now
	return true
}
