package main

import (
	"flag"
	"fmt"

	_ "github.com/xxxsen/common/webapi/auth"
	"github.com/xxxsen/tgfile/blockio"
	_ "github.com/xxxsen/tgfile/blockio/register"
	"github.com/xxxsen/tgfile/cache"
	"github.com/xxxsen/tgfile/config"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"

	"github.com/dustin/go-humanize"
	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logger"
	"go.uber.org/zap"
)

var file = flag.String("config", "./config.json", "config file path")

func main() {
	flag.Parse()

	c, err := config.Parse(*file)
	if err != nil {
		panic(err)
	}
	logitem := c.LogInfo
	logger := logger.Init(logitem.File, logitem.Level, int(logitem.FileCount), int(logitem.FileSize), int(logitem.KeepDays), logitem.Console)
	if err := idgen.Init(1); err != nil {
		logger.Fatal("init idgen fail", zap.Error(err))
	}
	logger.Info("recv config", zap.Any("config", c))
	logger.Info("current available blockio", zap.Strings("list", blockio.List()))
	logger.Info("current use blockio impl", zap.String("name", c.BotKind))
	if err := db.InitDB(c.DBFile); err != nil {
		logger.Fatal("init media db fail", zap.Error(err))
	}
	if err := initStorage(c); err != nil {
		logger.Fatal("init storage fail", zap.Error(err))
	}
	if err := initCache(c); err != nil {
		logger.Fatal("init cache fail", zap.Error(err))
	}
	logger.Info("current file protocol feature")
	logger.Info("-- s3 feature", zap.Bool("enable", c.S3.Enable), zap.Strings("buckets", c.S3.Bucket))
	logger.Info("-- webdav feature", zap.Bool("enable", c.Webdav.Enable), zap.String("root", c.Webdav.Root))
	logger.Info("current cache config")
	logger.Info("-- enable memory cache", zap.Bool("enable", c.IOCache.EnableMem), zap.String("max_cache_mem_usage", humanize.IBytes(uint64(c.IOCache.MemKeyCount)*uint64(c.IOCache.MemKeySizeLimit))))
	logger.Info("-- enable file cache", zap.Bool("enable", c.IOCache.EnableFile), zap.String("max_cache_storage_usage", humanize.IBytes(uint64(c.IOCache.FileKeyCount)*uint64(c.IOCache.FileKeySizeLimit))))
	svr, err := server.New(c.Bind,
		server.WithEnableS3(c.S3.Enable, c.S3.Bucket),
		server.WithUser(c.UserInfo),
		server.WithEnableWebdav(c.Webdav.Enable, c.Webdav.Root),
	)
	if err != nil {
		logger.Fatal("init server fail", zap.Error(err))
	}
	logger.Info("init server succ, start it...")
	if err := svr.Run(); err != nil {
		logger.Fatal("run server fail", zap.Error(err))
	}
}

func initStorage(c *config.Config) error {
	blkio, err := blockio.Create(c.BotKind, c.BotInfo)
	if err != nil {
		return fmt.Errorf("init block io failed, kind:%s, err:%w", c.BotKind, err)
	}
	blkio = blockio.NewRotateIO(blkio, int(c.RotateStream))
	cc := &filemgr.FileIOCacheConfig{
		DisableMemCache:  !c.IOCache.EnableMem,
		MemKeyCount:      c.IOCache.MemKeyCount,
		MemKeySizeLimit:  c.IOCache.MemKeySizeLimit,
		DisableFileCache: !c.IOCache.EnableFile,
		FileKeyCount:     c.IOCache.FileKeyCount,
		FileKeySizeLimit: c.IOCache.FileKeySizeLimit,
		FileCacheDir:     c.IOCache.FileCacheDir,
	}
	ioc, err := filemgr.NewFileIOCache(cc)
	if err != nil {
		return fmt.Errorf("create file io cache failed, err:%w", err)
	}
	fmgr := filemgr.NewFileManager(db.GetClient(), blkio, ioc)
	filemgr.SetFileManagerImpl(fmgr)
	return nil
}

func initCache(c *config.Config) error {
	cimpl, err := cache.New(50000)
	if err != nil {
		return err
	}
	cache.SetImpl(cimpl)
	return nil
}
