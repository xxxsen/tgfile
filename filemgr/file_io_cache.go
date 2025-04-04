package filemgr

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	lru "github.com/hnlq715/golang-lru"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/tgfile/utils"
	"go.uber.org/zap"
)

type IFileIOCache interface {
	Load(ctx context.Context, fileid uint64, size int64, cb func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error)
}

type fileIOCacheImpl struct {
	c  *FileIOCacheConfig
	l1 *lru.Cache //fileid=>[]byte, 内存缓存，速度快，适合小文件
	l2 *lru.Cache //fileid=>filename, 磁盘缓存，速度慢，适合大文件
}

func (f *fileIOCacheImpl) isCacheable(size int64) bool {
	if size > int64(f.c.FileKeySizeLimit) || (f.c.DisableMemCache && f.c.DisableFileCache) {
		return false
	}
	return true
}

func (f *fileIOCacheImpl) readL1Cache(ctx context.Context, fileid uint64, size int64, onMiss func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
	if f.c.DisableMemCache || size > int64(f.c.MemKeySizeLimit) {
		return onMiss(ctx)
	}
	val, ok := f.l1.Get(fileid)
	if ok {
		return newBytesStream(val.([]byte)), nil // 直接返回缓存的字节流
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
	_ = f.l1.AddEx(fileid, raw, time.Duration(f.c.MemCacheTime)*time.Second)
	//之后直接通过读到的内存重建字节流返回
	return newBytesStream(raw), nil
}

func (f *fileIOCacheImpl) readL2Cache(ctx context.Context, fileid uint64, size int64, onMiss func(ctx context.Context) (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
	if f.c.DisableFileCache || size > int64(f.c.FileKeySizeLimit) {
		return onMiss(ctx)
	}
	val, ok := f.l2.Get(fileid)
	if ok { //fileid缓存存在, 且对应的文件也存在, 则直接返回文件句柄
		fio, err := os.Open(val.(string)) //如果打开失败, 那么对应的文件可能已经无了, 这里直接忽略错误, 从底层io再换回数据流
		if err == nil {
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
	location := f.buildFileIdLocation(fileid, uint64(f.c.FileCacheTime))
	if err := utils.SafeSaveIOToFile(location, rsc); err != nil {
		return nil, fmt.Errorf("failed to save file to local: %w", err)
	}
	// 将文件路径加入到L2缓存
	_ = f.l2.AddEx(fileid, location, time.Duration(f.c.FileCacheTime)*time.Second)
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
	DisableMemCache  bool
	MemKeyCount      int
	MemKeySizeLimit  int
	MemCacheTime     int
	DisableFileCache bool
	FileKeyCount     int
	FileKeySizeLimit int
	FileCacheTime    int
	FileCacheDir     string // 文件缓存目录，必须存在
}

func NewDefaultFileIOCacheConfig() *FileIOCacheConfig {
	return &FileIOCacheConfig{
		DisableMemCache:  false,
		MemKeyCount:      1024,
		MemKeySizeLimit:  4 * 1024, // 4k, 最终占用内存大小4M
		MemCacheTime:     0,
		DisableFileCache: false,
		FileKeyCount:     10240,
		FileKeySizeLimit: 512 * 1024,                              // 512k, 最终占用磁盘空间5G
		FileCacheTime:    0,                                       // 磁盘缓存时间，0表示永久保存，直到手动删除或磁盘空间不足
		FileCacheDir:     path.Join(os.TempDir(), "tgfile-cache"), // 默认使用系统临时目录
	}
}

func (f *fileIOCacheImpl) onL1Evict(key interface{}, value interface{}) {

}

func (f *fileIOCacheImpl) onL2Evict(key interface{}, value interface{}) {
	fileid := key.(uint64)
	location := value.(string)
	_ = os.Remove(location)
	logutil.GetLogger(context.Background()).Debug("remove evit file cache", zap.Uint64("fileid", fileid), zap.String("path", location))
}

func (f *fileIOCacheImpl) buildFileIdLocation(fileid uint64, expire uint64) string {
	//文件格式: filename := fileid-expire.cache
	//文件路径:
	// hash := hex.EncodeToString(binary(fileid))
	// fullpath := basedir + "/" + hash[:2] + "/" + hash[2:] + "/" + filename
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, fileid)
	data := hex.EncodeToString(buf)
	filename := fmt.Sprintf("%d-%d.cache", fileid, uint64(time.Now().Unix())+expire)
	return path.Join(f.c.FileCacheDir, data[:2], data[2:4], filename)
}

func (f *fileIOCacheImpl) loadL2FromDisk() error {
	// 1. 遍历f.c.FileCacheDir下的所有文件
	// 2. 对每个文件，解析出fileid和expire
	// 3. 将解析出的fileid和文件路径加入到f.l2缓存中
	if f.c.DisableFileCache {
		return nil
	}
	if f.l2 == nil {
		return fmt.Errorf("l2 cache is not initialized")
	}
	// 遍历文件目录加载已有的缓存
	if f.c.FileCacheDir == "" {
		return fmt.Errorf("file cache dir is empty")
	}
	now := time.Now().Unix()
	// 递归读取文件目录下的所有文件
	err := filepath.Walk(f.c.FileCacheDir, func(path string, info os.FileInfo, err error) error {
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
		var fileid uint64
		var expire int64
		var filename = info.Name()
		n, err := fmt.Sscanf(filename, "%d-%d.cache", &fileid, &expire)
		if err != nil || n != 2 {
			return nil
		}
		spareCacheTime := expire - now
		if expire != 0 && spareCacheTime < 0 { // 如果expire小于当前时间, 则表示缓存已过期, 不加载
			_ = os.Remove(path) // 删除过期的缓存文件
			return nil
		}
		f.l2.AddEx(fileid, path, time.Duration(spareCacheTime)*time.Second) // 加入到L2缓存中
		logutil.GetLogger(context.Background()).Debug("load file to L2 cache", zap.Uint64("fileid", fileid), zap.String("path", path), zap.Int64("spare_cache_time", spareCacheTime))
		return nil
	})
	if err != nil {
		// 如果遍历目录失败，返回错误
		return fmt.Errorf("failed to load L2 cache from disk: %w", err)
	}
	return nil
}

func NewFileIOCache(c *FileIOCacheConfig) (IFileIOCache, error) {
	impl := &fileIOCacheImpl{
		c: c,
	}
	if err := os.MkdirAll(c.FileCacheDir, 0755); err != nil {
		return nil, err
	}
	if !c.DisableFileCache && len(c.FileCacheDir) == 0 {
		return nil, fmt.Errorf("file cache is enabled but no file cache dir provided")
	}
	l1, err := lru.NewWithEvict(c.MemKeyCount, impl.onL1Evict)
	if err != nil {
		return nil, err
	}
	l2, err := lru.NewWithEvict(c.FileKeyCount, impl.onL2Evict)
	if err != nil {
		return nil, err
	}
	impl.l1 = l1
	impl.l2 = l2
	if err := impl.loadL2FromDisk(); err != nil {
		return nil, err
	}
	return impl, nil
}
