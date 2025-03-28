package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	_ "tgfile/auth"
	"tgfile/blockio"
	"tgfile/blockio/localfile"
	"tgfile/blockio/mem"
	"tgfile/blockio/telegram"
	"tgfile/cache"
	"tgfile/config"
	"tgfile/db"
	"tgfile/filemgr"
	"tgfile/server"

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
	if err := db.InitDB(c.DBFile); err != nil {
		logger.Fatal("init media db fail", zap.Error(err))
	}
	if err := initStorage(c); err != nil {
		logger.Fatal("init storage fail", zap.Error(err))
	}
	if err := initCache(c); err != nil {
		logger.Fatal("init cache fail", zap.Error(err))
	}
	svr, err := server.New(c.Bind,
		server.WithS3Buckets(c.S3Bucket),
		server.WithUser(c.UserInfo),
		server.WithEnableWebdav(c.Webdav.Enable),
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
	getter := func() (blockio.IBlockIO, error) {
		return telegram.New(int64(c.BotInfo.Chatid), c.BotInfo.Token)
	}
	if c.DebugMode.Enable {
		getter = func() (blockio.IBlockIO, error) {
			switch c.DebugMode.BlockType {
			case "file":
				return localfile.New(filepath.Join(os.TempDir(), "tgfile-temp"), c.DebugMode.BlockSize)
			case "mem":
				return mem.New(c.DebugMode.BlockSize), nil
			default:
				return nil, fmt.Errorf("unknown debug block type:%s", c.DebugMode.BlockType)
			}
		}
	}
	bkio, err := getter()
	if err != nil {
		return err
	}
	bkio = blockio.NewRotateIO(bkio, int(c.RotateStream))
	fmgr := filemgr.NewFileManager(bkio)
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
