package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/xxxsen/common/webapi/auth"

	"github.com/xxxsen/tgfile/blockio"
	_ "github.com/xxxsen/tgfile/blockio/register"
	"github.com/xxxsen/tgfile/config"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/maintenance"
	"github.com/xxxsen/tgfile/server"
	filehandler "github.com/xxxsen/tgfile/server/handler/file"

	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logger"
	"go.uber.org/zap"
)

var errIntegerRange = errors.New("integer value exceeds platform range")

var (
	file            = flag.String("config", "./config.json", "config file path")
	maintenanceMode = flag.String("maintenance", "", "maintenance mode: audit, migrate-default-prefix, check-key")
	output          = flag.String("output", "", "maintenance output file")
	direction       = flag.String("direction", "", "migration direction: forward or reverse")
	dryRun          = flag.Bool("dry-run", true, "inspect a migration without changing data")
	checkKey        = flag.String("key", "", "file key to validate in check-key mode")
)

func main() {
	os.Exit(run())
}

func run() int {
	flag.Parse()
	if *maintenanceMode != "" {
		return runMaintenance()
	}

	c, err := config.Parse(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse config failed: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, c); err != nil {
		fmt.Fprintf(os.Stderr, "run server failed: %v\n", err)
		return 1
	}
	return 0
}

func runMaintenance() int {
	ctx := context.Background()
	switch *maintenanceMode {
	case "audit":
		if *output == "" {
			fmt.Fprintln(os.Stderr, "audit requires -output")
			return 2
		}
		databaseFile, err := maintenance.DatabaseFileFromConfig(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read maintenance config failed: %v\n", err)
			return 1
		}
		report, err := maintenance.Audit(ctx, databaseFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit failed: %v\n", err)
			return 1
		}
		if err := maintenance.WriteAuditReport(*output, report); err != nil {
			fmt.Fprintf(os.Stderr, "write audit failed: %v\n", err)
			return 1
		}
		return 0
	case "migrate-default-prefix":
		databaseFile, err := maintenance.DatabaseFileFromConfig(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read maintenance config failed: %v\n", err)
			return 1
		}
		result, err := maintenance.MigrateDefaultPrefix(ctx, databaseFile, *direction, *dryRun)
		if result != nil {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if encodeErr := encoder.Encode(result); encodeErr != nil {
				fmt.Fprintf(os.Stderr, "encode migration result failed: %v\n", encodeErr)
				return 1
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "prefix migration failed: %v\n", err)
			if maintenance.IsPreconditionError(err) {
				return 2
			}
			return 1
		}
		return 0
	case "check-key":
		link, err := filehandler.ExtractLinkFromFileKey(*checkKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid key: %v\n", err)
			return 2
		}
		fmt.Fprintln(os.Stdout, link)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown maintenance mode %q\n", *maintenanceMode)
		return 2
	}
}

func uint64ToInt(value uint64, field string) (int, error) {
	const maxInt = int(^uint(0) >> 1)
	if value > uint64(maxInt) {
		return 0, fmt.Errorf("%w: %s", errIntegerRange, field)
	}
	return int(value), nil
}

func runServer(ctx context.Context, c *config.Config) error {
	logitem := c.LogInfo
	fileCount, err := uint64ToInt(logitem.FileCount, "log file_count")
	if err != nil {
		return err
	}
	fileSize, err := uint64ToInt(logitem.FileSize, "log file_size")
	if err != nil {
		return err
	}
	keepDays, err := uint64ToInt(uint64(logitem.KeepDays), "log keep_days")
	if err != nil {
		return err
	}
	appLogger := logger.Init(
		logitem.File,
		logitem.Level,
		fileCount,
		fileSize,
		keepDays,
		logitem.Console,
	)
	if err := idgen.Init(1); err != nil {
		return fmt.Errorf("init idgen: %w", err)
	}
	appLogger.Info("recv config", c.SafeLogFields()...)
	appLogger.Info("current available blockio", zap.Strings("list", blockio.List()))
	appLogger.Info("current use blockio impl", zap.String("name", c.BotKind))
	if err := db.InitDBContext(ctx, c.DBFile); err != nil {
		return fmt.Errorf("init media db: %w", err)
	}
	runErr := runHTTPServer(ctx, c, appLogger)
	return errors.Join(runErr, db.Close())
}

func runHTTPServer(ctx context.Context, c *config.Config, appLogger *zap.Logger) error {
	fmgr, err := buildFileManager(ctx, c)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	appLogger.Info("current file protocol feature")
	appLogger.Info(
		"-- s3 feature",
		zap.Bool("enable", c.S3.Enable),
		zap.Strings("buckets", c.S3.Bucket),
	)
	appLogger.Info(
		"-- webdav feature",
		zap.Bool("enable", c.Webdav.Enable),
		zap.String("root", c.Webdav.Root),
	)
	appLogger.Info("current cache config")
	appLogger.Info(
		"-- enable l1 cache",
		zap.Bool("enable", c.IOCache.EnableL1Cache),
		zap.Int("max_cache_mem_usage_bytes", c.IOCache.L1CacheSize),
	)
	appLogger.Info(
		"-- enable l2 cache",
		zap.Bool("enable", c.IOCache.EnableL2Cache),
		zap.Int("max_cache_storage_usage_bytes", c.IOCache.L2CacheSize),
	)
	svr, err := server.New(c.Bind,
		server.WithEnableS3(c.S3.Enable, c.S3.Bucket),
		server.WithUser(c.UserInfo),
		server.WithEnableWebdav(c.Webdav.Enable, c.Webdav.Root),
		server.WithFileManager(fmgr),
	)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}
	appLogger.Info("init server succ, start it...")
	if err := svr.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}

func buildFileManager(ctx context.Context, c *config.Config) (filemgr.IFileManager, error) {
	blkio, err := blockio.Create(c.BotKind, c.BotInfo)
	if err != nil {
		return nil, fmt.Errorf("init block io failed, kind:%s, err:%w", c.BotKind, err)
	}
	blkio = blockio.NewRotateIO(blkio, c.RotateStream)
	cc := &filemgr.FileIOCacheConfig{
		DisableL1Cache: !c.IOCache.EnableL1Cache,
		L1CacheSize:    c.IOCache.L1CacheSize,
		L1KeySizeLimit: c.IOCache.L1KeySizeLimit,
		DisableL2Cache: !c.IOCache.EnableL2Cache,
		L2CacheSize:    c.IOCache.L2CacheSize,
		L2KeySizeLimit: c.IOCache.L2KeySizeLimit,
		L2CacheDir:     c.IOCache.L2CacheDir,
	}
	ioc, err := filemgr.NewFileIOCacheWithContext(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("create file io cache failed, err:%w", err)
	}
	fmgr := filemgr.NewFileManager(db.GetClient(), blkio, ioc)
	return fmgr, nil
}
