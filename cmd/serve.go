package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xxxsen/tgfile/blockio"
	_ "github.com/xxxsen/tgfile/blockio/register"
	"github.com/xxxsen/tgfile/config"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"

	"github.com/spf13/cobra"
	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logger"
	"go.uber.org/zap"
)

var (
	errIntegerRange          = errors.New("integer value exceeds platform range")
	errUnexpectedServiceExit = errors.New("service component exited unexpectedly")
)

func newServeCommand(ctx context.Context) *cobra.Command {
	var configFile string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Start the tgfile HTTP service",
		Args:  noPositionalArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			serviceConfig, err := config.Parse(configFile)
			if err != nil {
				return fmt.Errorf("parse config: %w", err)
			}
			if err := serviceConfig.Validate(); err != nil {
				return fmt.Errorf("validate config: %w", err)
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := runServer(ctx, serviceConfig); err != nil {
				return fmt.Errorf("run server: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	return command
}

func uint64ToInt(value uint64, field string) (int, error) {
	const maxInt = int(^uint(0) >> 1)
	if value > uint64(maxInt) {
		return 0, fmt.Errorf("%w: %s", errIntegerRange, field)
	}
	return int(value), nil
}

func runServer(ctx context.Context, serviceConfig *config.Config) error {
	logConfig := serviceConfig.LogInfo
	fileCount, err := uint64ToInt(logConfig.FileCount, "log file_count")
	if err != nil {
		return err
	}
	fileSize, err := uint64ToInt(logConfig.FileSize, "log file_size")
	if err != nil {
		return err
	}
	keepDays, err := uint64ToInt(uint64(logConfig.KeepDays), "log keep_days")
	if err != nil {
		return err
	}
	appLogger := logger.Init(
		logConfig.File,
		logConfig.Level,
		fileCount,
		fileSize,
		keepDays,
		logConfig.Console,
	)
	if err := idgen.Init(1); err != nil {
		return fmt.Errorf("init idgen: %w", err)
	}
	appLogger.Info("recv config", serviceConfig.SafeLogFields()...)
	appLogger.Info("current available blockio", zap.Strings("list", blockio.List()))
	appLogger.Info("current use blockio impl", zap.String("name", serviceConfig.BotKind))
	if err := db.InitDBContext(ctx, serviceConfig.DBFile); err != nil {
		return fmt.Errorf("init media db: %w", err)
	}
	runErr := runHTTPServer(ctx, serviceConfig, appLogger)
	return errors.Join(runErr, db.Close())
}

func runHTTPServer(ctx context.Context, serviceConfig *config.Config, appLogger *zap.Logger) error {
	fileManager, err := buildFileManager(ctx, serviceConfig)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	appLogger.Info("current file protocol feature")
	appLogger.Info(
		"-- s3 feature",
		zap.Bool("enable", serviceConfig.S3.Enable),
		zap.Strings("buckets", serviceConfig.S3.BucketNames()),
	)
	appLogger.Info(
		"-- webdav feature",
		zap.Bool("enable", serviceConfig.Webdav.Enable),
		zap.String("root", serviceConfig.Webdav.Root),
	)
	appLogger.Info("current cache config")
	appLogger.Info(
		"-- enable l1 cache",
		zap.Bool("enable", serviceConfig.IOCache.EnableL1Cache),
		zap.Int("max_cache_mem_usage_bytes", serviceConfig.IOCache.L1CacheSize),
	)
	appLogger.Info(
		"-- enable l2 cache",
		zap.Bool("enable", serviceConfig.IOCache.EnableL2Cache),
		zap.Int("max_cache_storage_usage_bytes", serviceConfig.IOCache.L2CacheSize),
	)
	httpServer, err := server.New(
		serviceConfig.Bind,
		server.WithS3(toServerS3Options(serviceConfig.S3)),
		server.WithUser(serviceConfig.UserInfo),
		server.WithWebDAV(toServerWebDAVOptions(serviceConfig.Webdav)),
		server.WithFileManager(fileManager),
	)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}
	appLogger.Info("init server succ, start it...")
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type componentResult struct {
		name string
		err  error
	}
	componentDone := make(chan componentResult, 3)
	go func() {
		componentDone <- componentResult{
			name: "block delete worker",
			err:  fileManager.RunBlockDeleteWorker(runContext),
		}
	}()
	go func() {
		componentDone <- componentResult{
			name: "multipart cleanup worker",
			err:  fileManager.RunMultipartCleanupWorker(runContext),
		}
	}()
	go func() {
		componentDone <- componentResult{name: "HTTP server", err: httpServer.Run(runContext)}
	}()

	first := <-componentDone
	contextWasDone := ctx.Err() != nil
	cancel()
	results := []componentResult{first, <-componentDone, <-componentDone}
	var runErr error
	for index, result := range results {
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			runErr = errors.Join(runErr, fmt.Errorf("%s: %w", result.name, result.err))
		}
		if index == 0 && !contextWasDone && result.err == nil {
			runErr = errors.Join(
				runErr,
				fmt.Errorf("%w: %s", errUnexpectedServiceExit, result.name),
			)
		}
	}
	return runErr
}

func toServerWebDAVOptions(input config.WebdavConfig) server.WebDAVOptions {
	return server.WebDAVOptions{
		Enabled:            input.Enable,
		Root:               input.Root,
		ExternalOrigin:     input.ExternalOrigin,
		MaxUploadSize:      input.MaxUploadSize,
		UploadTempDir:      input.UploadTempDir,
		Users:              input.Users,
		QuotaBytes:         input.QuotaBytes,
		MaxMutationEntries: input.MaxMutationEntries,
		SyncPageSize:       input.SyncPageSize,
	}
}

func toServerS3Options(input config.S3Config) server.S3Options {
	buckets := make([]server.S3BucketOptions, 0, len(input.Buckets))
	for _, bucket := range input.Buckets {
		buckets = append(buckets, server.S3BucketOptions{
			Name: bucket.Name,
			ACL:  server.BucketACL(bucket.ACL),
		})
	}
	return server.S3Options{
		Enabled:              input.Enable,
		Buckets:              buckets,
		MaxObjectSize:        input.MaxObjectSize,
		MultipartExpireHours: input.MultipartExpireHours,
	}
}

func buildFileManager(ctx context.Context, serviceConfig *config.Config) (filemgr.IFileManager, error) {
	blockStorage, err := blockio.Create(serviceConfig.BotKind, serviceConfig.BotInfo)
	if err != nil {
		return nil, fmt.Errorf("init block io failed, kind:%s, err:%w", serviceConfig.BotKind, err)
	}
	blockStorage = blockio.NewRotateIO(blockStorage, serviceConfig.RotateStream)
	cacheConfig := &filemgr.FileIOCacheConfig{
		DisableL1Cache: !serviceConfig.IOCache.EnableL1Cache,
		L1CacheSize:    serviceConfig.IOCache.L1CacheSize,
		L1KeySizeLimit: serviceConfig.IOCache.L1KeySizeLimit,
		DisableL2Cache: !serviceConfig.IOCache.EnableL2Cache,
		L2CacheSize:    serviceConfig.IOCache.L2CacheSize,
		L2KeySizeLimit: serviceConfig.IOCache.L2KeySizeLimit,
		L2CacheDir:     serviceConfig.IOCache.L2CacheDir,
	}
	ioCache, err := filemgr.NewFileIOCacheWithContext(ctx, cacheConfig)
	if err != nil {
		return nil, fmt.Errorf("create file io cache failed, err:%w", err)
	}
	fileManager := filemgr.NewFileManager(db.GetClient(), blockStorage, ioCache)
	return fileManager, nil
}
