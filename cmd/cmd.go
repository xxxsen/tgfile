package main

import (
	"flag"
	"fmt"

	_ "github.com/xxxsen/common/webapi/auth"
	"github.com/xxxsen/tgfile/blockio"
	_ "github.com/xxxsen/tgfile/blockio/register"
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
	fmgr, err := buildFileManager(c)
	if err != nil {
		logger.Fatal("init storage fail", zap.Error(err))
	}
	logger.Info("current file protocol feature")
	logger.Info("-- s3 feature", zap.Bool("enable", c.S3.Enable), zap.Strings("buckets", c.S3.Bucket))
	logger.Info("-- webdav feature", zap.Bool("enable", c.Webdav.Enable), zap.String("root", c.Webdav.Root))
	logger.Info("current cache config")
	logger.Info("-- enable l1 cache", zap.Bool("enable", c.IOCache.EnableL1Cache), zap.String("max_cache_mem_usage", humanize.IBytes(uint64(c.IOCache.L1CacheSize))))
	logger.Info("-- enable l2 cache", zap.Bool("enable", c.IOCache.EnableL2Cache), zap.String("max_cache_storage_usage", humanize.IBytes(uint64(c.IOCache.L2CacheSize))))
	svr, err := server.New(c.Bind,
		server.WithEnableS3(c.S3.Enable, c.S3.Bucket),
		server.WithUser(c.UserInfo),
		server.WithEnableWebdav(c.Webdav.Enable, c.Webdav.Root),
		server.WithFileManager(fmgr),
	)
	if err != nil {
		logger.Fatal("init server fail", zap.Error(err))
	}
	logger.Info("init server succ, start it...")
	if err := svr.Run(); err != nil {
		logger.Fatal("run server fail", zap.Error(err))
	}
}

func buildFileManager(c *config.Config) (filemgr.IFileManager, error) {
	blkio, err := blockio.Create(c.BotKind, c.BotInfo)
	if err != nil {
		return nil, fmt.Errorf("init block io failed, kind:%s, err:%w", c.BotKind, err)
	}
	blkio = blockio.NewRotateIO(blkio, int(c.RotateStream))
	cc := &filemgr.FileIOCacheConfig{
		DisableL1Cache: !c.IOCache.EnableL1Cache,
		L1CacheSize:    c.IOCache.L1CacheSize,
		L1KeySizeLimit: c.IOCache.L1KeySizeLimit,
		DisableL2Cache: !c.IOCache.EnableL2Cache,
		L2CacheSize:    c.IOCache.L2CacheSize,
		L2KeySizeLimit: c.IOCache.L2KeySizeLimit,
		L2CacheDir:     c.IOCache.L2CacheDir,
	}
	ioc, err := filemgr.NewFileIOCache(cc)
	if err != nil {
		return nil, fmt.Errorf("create file io cache failed, err:%w", err)
	}
	fmgr := filemgr.NewFileManager(db.GetClient(), blkio, ioc)
	return fmgr, nil
}
