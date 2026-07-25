package filemgr

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/cacheapi"
	cachewrap "github.com/xxxsen/tgfile/cacheapi/adaptor"
	"github.com/xxxsen/tgfile/utils"
)

const (
	defaultFileDelimiter       = "#"
	defaultMaxAllowKeySizeToL1 = 4 * 1024
	defaultMaxAllowKeySizeToL2 = 512 * 1024 // 512k
)

type IFileIOCache interface {
	Load(
		ctx context.Context,
		fileid uint64,
		size int64,
		cb func(ctx context.Context) (io.ReadSeekCloser, error),
	) (io.ReadSeekCloser, error)
}

type fileIOCacheImpl struct {
	ctx context.Context
	c   *FileIOCacheConfig
	l1  cacheapi.ICache[uint64, []byte] // fileid=>[]byte, 内存缓存，速度快，适合小文件
	l2  cacheapi.ICache[uint64, string] // fileid=>filename, 磁盘缓存，速度慢，适合大文件
}

type removeOnCloseFile struct {
	*os.File
	location string
}

func (f *removeOnCloseFile) Close() error {
	closeErr := f.File.Close()
	removeErr := os.Remove(f.location)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return errors.Join(closeErr, removeErr)
	}
	return errors.Join(closeErr)
}

func (f *fileIOCacheImpl) isCacheable(size int64) bool {
	if (int64(f.c.L2KeySizeLimit) > 0 && size > int64(f.c.L2KeySizeLimit)) || (f.c.DisableL1Cache && f.c.DisableL2Cache) {
		return false
	}
	return true
}

func (f *fileIOCacheImpl) readL1Cache(
	ctx context.Context,
	fileid uint64,
	size int64,
	onMiss func(ctx context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	if f.c.DisableL1Cache || size > int64(f.c.L1KeySizeLimit) {
		stream, err := onMiss(ctx)
		if err != nil {
			return nil, fmt.Errorf("load file without L1 cache: %w", err)
		}
		return stream, nil
	}
	val, err := f.l1.Get(ctx, fileid)
	if err == nil {
		logutil.GetLogger(ctx).Debug("read fileid from l1 cache", zap.Uint64("fileid", fileid))
		return newBytesStream(val), nil // 直接返回缓存的字节流
	}
	rsc, err := onMiss(ctx)
	if err != nil {
		return nil, fmt.Errorf("load L1 cache miss: %w", err)
	}
	raw, err := io.ReadAll(rsc)
	_ = rsc.Close() // 无论如何都需要直接关闭
	if err != nil {
		return nil, fmt.Errorf("read L1 cache source: %w", err)
	}
	// 将读取到的内容存入L1缓存
	if err := f.l1.Set(ctx, fileid, raw); err != nil {
		logutil.GetLogger(ctx).Debug("l1 cache set rejected", zap.Uint64("fileid", fileid), zap.Error(err))
	}
	// 之后直接通过读到的内存重建字节流返回
	return newBytesStream(raw), nil
}

func (f *fileIOCacheImpl) readL2Cache(
	ctx context.Context,
	fileid uint64,
	size int64,
	onMiss func(ctx context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	if f.c.DisableL2Cache || size > int64(f.c.L2KeySizeLimit) {
		stream, err := onMiss(ctx)
		if err != nil {
			return nil, fmt.Errorf("load file without L2 cache: %w", err)
		}
		return stream, nil
	}
	val, err := f.l2.Get(ctx, fileid)
	if err == nil { // fileid缓存存在, 且对应的文件也存在, 则直接返回文件句柄
		fio, err := os.Open(val) // 如果打开失败, 那么对应的文件可能已经无了, 这里直接忽略错误, 从底层io再换回数据流
		if err == nil {
			logutil.GetLogger(ctx).Debug("read fileid from l2 cache", zap.Uint64("fileid", fileid))
			return fio, nil // 返回文件句柄
		}
		_ = f.l2.Del(ctx, fileid)
	}
	// 如果L2缓存没有命中，调用回调函数获取数据源
	rsc, err := onMiss(ctx)
	if err != nil {
		return nil, fmt.Errorf("load L2 cache miss: %w", err)
	}
	defer func() {
		_ = rsc.Close()
	}()
	// 读取数据并存储到临时变量
	location := f.buildFileIdLocation(fileid, size)
	if err := utils.SafeSaveIOToFile(location, rsc); err != nil {
		return nil, fmt.Errorf("failed to save file to local: %w", err)
	}
	fio, err := os.Open(location)
	if err != nil {
		return nil, fmt.Errorf("open L2 cache file: %w", err)
	}
	// 将文件路径加入到L2缓存。先打开文件，确保策略拒绝并删除路径时，
	// 当前请求仍可使用已经打开的句柄完成读取。
	if err := f.l2.Set(ctx, fileid, location); err != nil {
		logutil.GetLogger(ctx).Debug(
			"l2 cache set rejected",
			zap.Uint64("fileid", fileid),
			zap.String("location", location),
			zap.Error(err),
		)
		return &removeOnCloseFile{File: fio, location: location}, nil
	}
	return fio, nil
}

func (f *fileIOCacheImpl) Load(
	ctx context.Context,
	fileid uint64,
	size int64,
	cb func(context.Context) (io.ReadSeekCloser, error),
) (io.ReadSeekCloser, error) {
	if !f.isCacheable(size) {
		stream, err := cb(ctx)
		if err != nil {
			return nil, fmt.Errorf("load uncacheable file: %w", err)
		}
		return stream, nil
	}
	return f.readL1Cache(ctx, fileid, size, func(ctx context.Context) (io.ReadSeekCloser, error) {
		return f.readL2Cache(ctx, fileid, size, cb)
	})
}

type FileIOCacheConfig struct {
	DisableL1Cache bool
	L1CacheSize    int
	L1KeySizeLimit int
	DisableL2Cache bool
	L2CacheSize    int
	L2KeySizeLimit int
	L2CacheDir     string // 文件缓存目录，必须存在
}

func NewDefaultFileIOCacheConfig() *FileIOCacheConfig {
	return &FileIOCacheConfig{
		DisableL1Cache: false,
		L1CacheSize:    16 * 1024 * 1024,
		L1KeySizeLimit: 4 * 1024,
		DisableL2Cache: false,
		L2CacheSize:    5 * 1024 * 1024 * 1024,
		L2KeySizeLimit: 512 * 1024,                              // 512k, 最终占用磁盘空间5G
		L2CacheDir:     path.Join(os.TempDir(), "tgfile-cache"), // 默认使用系统临时目录
	}
}

func (f *fileIOCacheImpl) onL1Evict(key uint64, _ []byte) {
	logutil.GetLogger(f.ctx).Debug("evict from l1 cache", zap.Uint64("key_hash", key))
}

func (f *fileIOCacheImpl) removeL2File(action, location string) {
	size, fileid, err := f.extractFileIdLocationInfo(location)
	if err != nil {
		logutil.GetLogger(f.ctx).Error(
			"extract file location for delete failed",
			zap.Error(err),
			zap.String("location", location),
		)
		return
	}
	removeErr := os.Remove(location)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		logutil.GetLogger(f.ctx).Error(
			"remove l2 file cache failed",
			zap.String("action", action),
			zap.Uint64("fileid", fileid),
			zap.String("path", location),
			zap.Int64("size", size),
			zap.Error(removeErr),
		)
		return
	}
	logutil.GetLogger(f.ctx).Debug(
		"remove l2 file cache",
		zap.String("action", action),
		zap.Uint64("fileid", fileid),
		zap.String("path", location),
		zap.Int64("size", size),
	)
}

func (f *fileIOCacheImpl) onL2Evict(_ uint64, location string) {
	f.removeL2File("evict", location)
}

func (f *fileIOCacheImpl) onL2Reject(_ uint64, location string) {
	f.removeL2File("reject", location)
}

func (f *fileIOCacheImpl) extractFileIdLocationInfo(location string) (int64, uint64, error) {
	filename := path.Base(location)
	idx := strings.Index(filename, defaultFileDelimiter)
	if idx < 0 {
		return 0, 0, ErrInvalidCachePath
	}
	size, err := strconv.ParseInt(filename[:idx], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("decode size failed, err:%w", err)
	}
	fileIDText := strings.TrimSuffix(filename[idx+1:], ".cache")
	fileID, err := strconv.ParseUint(fileIDText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("decode file id failed, err:%w", err)
	}
	return size, fileID, nil
}

func (f *fileIOCacheImpl) buildFileIdLocation(fileid uint64, size int64) string {
	// 文件格式: filename := fileid-expire.cache
	// 文件路径:
	// hash := hex.EncodeToString(binary(fileid))
	// fullpath := basedir + "/" + hash[:2] + "/" + filename
	data := hex.EncodeToString(utils.FileIdToHash(fileid))
	filename := fmt.Sprintf("%d.cache", fileid)
	// 一层结构即可, 假设每个桶存储1000个文件, 36*36*1000 能够存储129.6w个文件
	return path.Join(f.c.L2CacheDir, data[:2], strconv.FormatInt(size, 10)+defaultFileDelimiter+filename)
}

func parseCacheFileName(filename string) (int64, uint64, bool) {
	base, found := strings.CutSuffix(filename, ".cache")
	if !found {
		return 0, 0, false
	}
	sizeText, fileIDText, found := strings.Cut(base, defaultFileDelimiter)
	if !found {
		return 0, 0, false
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	fileID, err := strconv.ParseUint(fileIDText, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return size, fileID, true
}

func (f *fileIOCacheImpl) loadL2FromDisk() error {
	// 1. 遍历f.c.FileCacheDir下的所有文件
	// 2. 对每个文件，解析出fileid和expire
	// 3. 将解析出的fileid和文件路径加入到f.l2缓存中
	if f.c.DisableL2Cache {
		return nil
	}
	if f.l2 == nil {
		return fmt.Errorf("%w: L2 cache is not initialized", ErrInvalidCache)
	}
	// 遍历文件目录加载已有的缓存
	if f.c.L2CacheDir == "" {
		return fmt.Errorf("%w: L2 directory is empty", ErrInvalidCache)
	}
	if err := os.MkdirAll(f.c.L2CacheDir, 0o755); err != nil {
		return fmt.Errorf("create l2 cache directory: %w", err)
	}
	var temporaryFiles []string
	// 递归读取文件目录下的所有文件
	err := filepath.Walk(f.c.L2CacheDir, func(cachePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk L2 cache: %w", walkErr)
		}
		if info.IsDir() {
			return nil // 跳过目录
		}
		if strings.HasSuffix(info.Name(), ".temp") { // 之前未写入完成的文件, 直接干掉
			temporaryFiles = append(temporaryFiles, cachePath)
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".cache") {
			logutil.GetLogger(f.ctx).Debug("skip non-cache file", zap.String("file", info.Name()))
			return nil
		}
		// 解析文件名获取fileid和expire
		_, fileid, valid := parseCacheFileName(info.Name())
		if !valid {
			return nil
		}
		if err := f.l2.Set(f.ctx, fileid, cachePath); err != nil {
			logutil.GetLogger(f.ctx).Debug(
				"existing l2 cache entry rejected",
				zap.Uint64("fileid", fileid),
				zap.String("path", cachePath),
				zap.Error(err),
			)
			return nil
		}
		logutil.GetLogger(f.ctx).Debug(
			"load file to l2 cache",
			zap.Uint64("fileid", fileid),
			zap.String("path", cachePath),
		)
		return nil
	})
	if err != nil {
		// 如果遍历目录失败，返回错误
		return fmt.Errorf("failed to load l2 cache from disk: %w", err)
	}
	for _, temporaryFile := range temporaryFiles {
		logutil.GetLogger(f.ctx).Error(
			"remove unfinished cache temp file",
			zap.String("path", temporaryFile),
		)
		if err := os.Remove(temporaryFile); err != nil && !os.IsNotExist(err) {
			logutil.GetLogger(f.ctx).Error(
				"remove unfinished cache temp file failed",
				zap.String("path", temporaryFile),
				zap.Error(err),
			)
		}
	}
	return nil
}

func (f *fileIOCacheImpl) buildL1Cache(c *FileIOCacheConfig) error {
	if c.DisableL1Cache {
		return nil
	}
	numCounters := max(int64(c.L1CacheSize/c.L1KeySizeLimit)*10, 10)
	cc, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters:        numCounters,
		MaxCost:            int64(c.L1CacheSize),
		BufferItems:        64,
		IgnoreInternalCost: true,
		Cost: func(value []byte) int64 {
			return int64(len(value))
		},
		OnEvict: func(item *ristretto.Item[[]byte]) {
			f.onL1Evict(item.Key, item.Value)
		},
		OnReject: func(item *ristretto.Item[[]byte]) {
			logutil.GetLogger(f.ctx).Debug(
				"l1 cache reject",
				zap.Uint64("key_hash", item.Key),
				zap.Int("cost", len(item.Value)),
			)
		},
	})
	if err != nil {
		return fmt.Errorf("create L1 cache: %w", err)
	}
	f.l1 = cachewrap.WrapRistrttoCache(cc)
	return nil
}

func (f *fileIOCacheImpl) buildL2Cache(c *FileIOCacheConfig) error {
	if c.DisableL2Cache {
		return nil
	}
	numCounters := max(int64(c.L2CacheSize/c.L2KeySizeLimit)*10, 10)
	cc, err := ristretto.NewCache(&ristretto.Config[uint64, string]{
		NumCounters:        numCounters,
		MaxCost:            int64(c.L2CacheSize),
		BufferItems:        64,
		IgnoreInternalCost: true,
		Cost: func(value string) int64 {
			size, _, err := f.extractFileIdLocationInfo(value)
			if err != nil {
				logutil.GetLogger(f.ctx).Error(
					"extract file size from location failed, use default",
					zap.Error(err),
					zap.String("location", value),
				)
				size = defaultMaxAllowKeySizeToL2
			}
			logutil.GetLogger(f.ctx).Debug(
				"add file to l2 cache",
				zap.String("location", value),
				zap.Int64("cost", size),
			)
			return size
		},
		OnEvict: func(item *ristretto.Item[string]) {
			f.onL2Evict(item.Key, item.Value)
		},
		OnReject: func(item *ristretto.Item[string]) {
			logutil.GetLogger(f.ctx).Debug(
				"l2 cache reject",
				zap.Uint64("key_hash", item.Key),
				zap.String("location", item.Value),
			)
			f.onL2Reject(item.Key, item.Value)
		},
	})
	if err != nil {
		return fmt.Errorf("create L2 cache: %w", err)
	}
	f.l2 = cachewrap.WrapRistrttoCache(cc)
	return f.loadL2FromDisk()
}

func NewFileIOCache(c *FileIOCacheConfig) (IFileIOCache, error) {
	return NewFileIOCacheWithContext(context.Background(), c)
}

func NewFileIOCacheWithContext(ctx context.Context, c *FileIOCacheConfig) (IFileIOCache, error) {
	impl := &fileIOCacheImpl{
		ctx: ctx,
		c:   c,
	}
	if !c.DisableL2Cache && len(c.L2CacheDir) == 0 {
		return nil, fmt.Errorf("%w: L2 directory is empty", ErrInvalidCache)
	}
	if !c.DisableL1Cache && (c.L1CacheSize <= 0 || c.L1KeySizeLimit <= 0) {
		return nil, fmt.Errorf("%w: L1 size and key limit must be positive", ErrInvalidCache)
	}
	if !c.DisableL2Cache && (c.L2CacheSize <= 0 || c.L2KeySizeLimit <= 0) {
		return nil, fmt.Errorf("%w: L2 size and key limit must be positive", ErrInvalidCache)
	}
	if err := impl.buildL1Cache(c); err != nil {
		return nil, fmt.Errorf("initialize L1 cache: %w", err)
	}
	if err := impl.buildL2Cache(c); err != nil {
		return nil, fmt.Errorf("initialize L2 cache: %w", err)
	}
	return impl, nil
}
