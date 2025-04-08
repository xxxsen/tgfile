package filemgr

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/tgfile/cacheapi"
	cachewrap "github.com/xxxsen/tgfile/cacheapi/adaptor"
	"github.com/xxxsen/tgfile/utils"
	"go.uber.org/zap"
)

const (
	defaultFileDelimiter       = "#"
	defaultMaxAllowKeySizeToL1 = 4 * 1024
	defaultMaxAllowKeySizeToL2 = 512 * 1024 //512k
)

type IFileIOCache interface {
	Load(ctx context.Context, fileid uint64, size int64, cb func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error)
}

type fileIOCacheImpl struct {
	c  *FileIOCacheConfig
	l1 cacheapi.ICache[uint64, []byte] //fileid=>[]byte, 内存缓存，速度快，适合小文件
	l2 cacheapi.ICache[uint64, string] //fileid=>filename, 磁盘缓存，速度慢，适合大文件
}

func (f *fileIOCacheImpl) isCacheable(size int64) bool {
	if size > int64(f.c.L2KeySizeLimit) || (f.c.DisableL1Cache && f.c.DisableL2Cache) {
		return false
	}
	return true
}

func (f *fileIOCacheImpl) readL1Cache(ctx context.Context, fileid uint64, size int64, onMiss func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
	if f.c.DisableL1Cache || size > int64(f.c.L1KeySizeLimit) {
		return onMiss(ctx)
	}
	val, err := f.l1.Get(ctx, fileid)
	if err == nil {
		logutil.GetLogger(ctx).Debug("read fileid from l1 cache", zap.Uint64("fileid", fileid))
		return newBytesStream(val), nil // 直接返回缓存的字节流
	}
	rsc, err := onMiss(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(rsc)
	_ = rsc.Close() // 无论如何都需要直接关闭
	if err != nil {
		return nil, err
	}
	// 将读取到的内容存入L1缓存
	_ = f.l1.Set(ctx, fileid, raw)
	//之后直接通过读到的内存重建字节流返回
	return newBytesStream(raw), nil
}

func (f *fileIOCacheImpl) readL2Cache(ctx context.Context, fileid uint64, size int64, onMiss func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
	if f.c.DisableL2Cache || size > int64(f.c.L2KeySizeLimit) {
		return onMiss(ctx)
	}
	val, err := f.l2.Get(ctx, fileid)
	if err == nil { //fileid缓存存在, 且对应的文件也存在, 则直接返回文件句柄
		fio, err := os.Open(val) //如果打开失败, 那么对应的文件可能已经无了, 这里直接忽略错误, 从底层io再换回数据流
		if err == nil {
			logutil.GetLogger(ctx).Debug("read fileid from l2 cache", zap.Uint64("fileid", fileid))
			return fio, nil // 返回文件句柄
		}
	}
	// 如果L2缓存没有命中，调用回调函数获取数据源
	rsc, err := onMiss(ctx)
	if err != nil {
		return nil, err // 回调函数失败，直接返回错误
	}
	defer rsc.Close()
	// 读取数据并存储到临时变量
	location := f.buildFileIdLocation(fileid, size)
	if err := utils.SafeSaveIOToFile(location, rsc); err != nil {
		return nil, fmt.Errorf("failed to save file to local: %w", err)
	}
	// 将文件路径加入到L2缓存
	_ = f.l2.Set(ctx, fileid, location)
	// 返回文件句柄
	fio, err := os.Open(location)
	return fio, err
}

func (f *fileIOCacheImpl) Load(ctx context.Context, fileid uint64, size int64, cb func(context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
	if !f.isCacheable(size) {
		return cb(ctx)
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

func (f *fileIOCacheImpl) onL1Evict(key uint64, value []byte) {
	logutil.GetLogger(context.Background()).Debug("evit from l1 cache", zap.Uint64("fileid", key))
}

func (f *fileIOCacheImpl) onL2Evict(key uint64, location string) {
	fileid := key
	size, err := f.extractFileIdLocationInfo(location)
	if err != nil {
		logutil.GetLogger(context.Background()).Error("extract file location for delete failed", zap.Error(err), zap.String("location", location))
		return
	}
	_ = os.Remove(location)
	logutil.GetLogger(context.Background()).Debug("evit l2 file cache", zap.Uint64("fileid", fileid), zap.String("path", location), zap.Int64("size", size))
}

func (f *fileIOCacheImpl) extractFileIdLocationInfo(location string) (int64, error) {
	filename := path.Base(location)
	idx := strings.Index(filename, defaultFileDelimiter)
	if idx < 0 {
		return 0, fmt.Errorf("no delimiter found")
	}
	size, err := strconv.ParseInt(filename[:idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode size failed, err:%w", err)
	}
	return size, nil
}

func (f *fileIOCacheImpl) buildFileIdLocation(fileid uint64, size int64) string {
	//文件格式: filename := fileid-expire.cache
	//文件路径:
	// hash := hex.EncodeToString(binary(fileid))
	// fullpath := basedir + "/" + hash[:2] + "/" + filename
	data := hex.EncodeToString(utils.FileIdToHash(fileid))
	filename := fmt.Sprintf("%d.cache", fileid)
	//一层结构即可, 假设每个桶存储1000个文件, 36*36*1000 能够存储129.6w个文件
	return path.Join(f.c.L2CacheDir, data[:2], strconv.FormatInt(size, 10)+defaultFileDelimiter+filename)
}

func (f *fileIOCacheImpl) loadL2FromDisk() error {
	// 1. 遍历f.c.FileCacheDir下的所有文件
	// 2. 对每个文件，解析出fileid和expire
	// 3. 将解析出的fileid和文件路径加入到f.l2缓存中
	if f.c.DisableL2Cache {
		return nil
	}
	if f.l2 == nil {
		return fmt.Errorf("l2 cache is not initialized")
	}
	// 遍历文件目录加载已有的缓存
	if f.c.L2CacheDir == "" {
		return fmt.Errorf("file cache dir is empty")
	}
	// 递归读取文件目录下的所有文件
	err := filepath.Walk(f.c.L2CacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil // 跳过目录
		}
		if strings.HasSuffix(info.Name(), ".temp") { //之前未写入完成的文件, 直接干掉
			logutil.GetLogger(context.Background()).Error("remove unfinished cache temp file", zap.String("path", path))
			_ = os.Remove(path)
		}
		if !strings.HasSuffix(info.Name(), ".cache") {
			logutil.GetLogger(context.Background()).Debug("skip non-cache file", zap.String("file", info.Name()))
			return nil
		}
		// 解析文件名获取fileid和expire
		var size int64
		var fileid uint64
		var filename = info.Name()
		n, err := fmt.Sscanf(filename, "%d#%d.cache", &size, &fileid)
		if err != nil || n != 2 {
			return nil
		}
		_ = f.l2.Set(context.Background(), fileid, path) // 加入到L2缓存中
		logutil.GetLogger(context.Background()).Debug("load file to l2 cache", zap.Uint64("fileid", fileid), zap.String("path", path))
		return nil
	})
	if err != nil {
		// 如果遍历目录失败，返回错误
		return fmt.Errorf("failed to load l2 cache from disk: %w", err)
	}
	return nil
}

func (f *fileIOCacheImpl) buildL1Cache(c *FileIOCacheConfig) error {
	if c.DisableL1Cache {
		return nil
	}
	cc, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters: int64(float64(c.L1CacheSize) / float64(c.L1KeySizeLimit) * 10),
		MaxCost:     int64(c.L1CacheSize),
		BufferItems: 64,
		Cost: func(value []byte) int64 {
			return int64(len(value))
		},
		OnEvict: func(item *ristretto.Item[[]byte]) {
			f.onL1Evict(item.Key, item.Value)
		},
	})
	if err != nil {
		return err
	}
	f.l1 = cachewrap.WrapRistrttoCache(cc)
	return nil
}

func (f *fileIOCacheImpl) buildL2Cache(c *FileIOCacheConfig) error {
	if c.DisableL2Cache {
		return nil
	}
	cc, err := ristretto.NewCache(&ristretto.Config[uint64, string]{
		NumCounters: int64(float64(c.L2CacheSize) / float64(c.L2KeySizeLimit) * 10),
		MaxCost:     int64(c.L2CacheSize),
		BufferItems: 64,
		Cost: func(value string) int64 {
			size, err := f.extractFileIdLocationInfo(value)
			if err != nil {
				logutil.GetLogger(context.Background()).Error("extract file size from location failed, use default", zap.Error(err), zap.String("location", value))
				size = defaultMaxAllowKeySizeToL2
			}
			logutil.GetLogger(context.Background()).Debug("add file to l2 cache", zap.String("location", value), zap.Int64("cost", size))
			return int64(size)
		},
		OnEvict: func(item *ristretto.Item[string]) {
			f.onL2Evict(item.Key, item.Value)
		},
	})
	if err != nil {
		return err
	}
	f.l2 = cachewrap.WrapRistrttoCache(cc)
	if err := f.loadL2FromDisk(); err != nil {
		return err
	}
	return nil
}

func NewFileIOCache(c *FileIOCacheConfig) (IFileIOCache, error) {
	impl := &fileIOCacheImpl{
		c: c,
	}
	if !c.DisableL2Cache && len(c.L2CacheDir) == 0 {
		return nil, fmt.Errorf("l2 cache is enabled but no l2 cache dir provided")
	}
	if err := impl.buildL1Cache(c); err != nil {
		return nil, err
	}
	if err := impl.buildL2Cache(c); err != nil {
		return nil, err
	}
	return impl, nil
}
