package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
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

const cacheShutdownTimeout = 30 * time.Second

type componentResult struct {
	name string
	err  error
}

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
	fileManager, ioCache, err := buildFileManager(ctx, serviceConfig)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}
	runErr := func() error {
		logServerFeatures(serviceConfig, appLogger)
		var backupManager *backupmgr.Manager
		if serviceConfig.Backup.Enable || serviceConfig.Admin.Enable {
			backupManager, err = buildBackupManager(serviceConfig, fileManager)
			if err != nil {
				return err
			}
		}
		httpServer, buildErr := buildHTTPServer(serviceConfig, fileManager, backupManager)
		if buildErr != nil {
			return buildErr
		}
		appLogger.Info("init server succ, start it...")
		return runServerComponents(ctx, httpServer, fileManager, backupManager)
	}()
	closeErr := func() error {
		closeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheShutdownTimeout)
		defer cancel()
		return ioCache.Close(closeContext)
	}()
	return errors.Join(runErr, closeErr)
}

func logServerFeatures(serviceConfig *config.Config, appLogger *zap.Logger) {
	appLogger.Info("current file protocol feature")
	appLogger.Info(
		"-- shared external origins",
		zap.Strings("origins", serviceConfig.ExternalOrigins),
	)
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
	appLogger.Info(
		"-- admin feature",
		zap.Bool("enable", serviceConfig.Admin.Enable),
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
}

func buildBackupManager(
	serviceConfig *config.Config,
	fileManager filemgr.IFileManager,
) (*backupmgr.Manager, error) {
	manager, err := backupmgr.New(
		db.GetClient(),
		fileManager,
		toBackupManagerOptions(serviceConfig, fileManager.BackupMaxPartSize()),
	)
	if err != nil {
		return nil, fmt.Errorf("init backup manager: %w", err)
	}
	return manager, nil
}

func buildHTTPServer(
	serviceConfig *config.Config,
	fileManager filemgr.IFileManager,
	backupManager *backupmgr.Manager,
) (*server.Server, error) {
	authorizer, err := authz.New(serviceConfig.UserPermission)
	if err != nil {
		return nil, fmt.Errorf("initialize authorization policy: %w", err)
	}
	httpServer, err := server.New(
		serviceConfig.Bind,
		server.WithS3(toServerS3Options(serviceConfig.S3)),
		server.WithUser(serviceConfig.UserInfo),
		server.WithAuthorizer(authorizer),
		server.WithWebDAV(toServerWebDAVOptions(
			serviceConfig.Webdav,
			serviceConfig.ExternalOrigins,
		)),
		server.WithFileManager(fileManager),
		server.WithBackup(server.BackupOptions{Enabled: serviceConfig.Backup.Enable}, backupManager),
		server.WithAdmin(toServerAdminOptions(serviceConfig.Admin, serviceConfig)),
	)
	if err != nil {
		return nil, fmt.Errorf("init server: %w", err)
	}
	return httpServer, nil
}

func runServerComponents(
	ctx context.Context,
	httpServer *server.Server,
	fileManager filemgr.IFileManager,
	backupManager *backupmgr.Manager,
) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	componentCount := 3
	if backupManager != nil {
		componentCount++
	}
	componentDone := make(chan componentResult, componentCount)
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
	if backupManager != nil {
		go func() {
			componentDone <- componentResult{name: "backup worker", err: backupManager.Run(runContext)}
		}()
	}

	first := <-componentDone
	contextWasDone := ctx.Err() != nil
	cancel()
	results := make([]componentResult, 0, componentCount)
	results = append(results, first)
	for len(results) < componentCount {
		results = append(results, <-componentDone)
	}
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

func toBackupManagerOptions(
	serviceConfig *config.Config,
	maxPartSize int64,
) backupmgr.Options {
	buckets := make([]backupfmt.RequiredBucket, 0, len(serviceConfig.S3.Buckets))
	for _, bucket := range serviceConfig.S3.Buckets {
		buckets = append(buckets, backupfmt.RequiredBucket{Name: bucket.Name, ACL: bucket.ACL})
	}
	return backupmgr.Options{
		WorkDir: serviceConfig.Backup.WorkDir,
		Limits: backupfmt.Limits{
			MaxArchiveBytes:  serviceConfig.Backup.MaxArchiveBytes,
			MaxExpandedBytes: serviceConfig.Backup.MaxExpandedBytes,
			MaxMappingCount:  serviceConfig.Backup.MaxMappingCount,
			MaxFileCount:     serviceConfig.Backup.MaxFileCount,
			MaxPartCount:     serviceConfig.Backup.MaxPartCount,
			MaxPathBytes:     serviceConfig.Backup.MaxPathBytes,
			MaxManifestBytes: backupfmt.DefaultLimits().MaxManifestBytes,
			MaxPropertyBytes: backupfmt.DefaultLimits().MaxPropertyBytes,
			MaxUserMetaBytes: backupfmt.DefaultLimits().MaxUserMetaBytes,
		},
		RequiredBuckets:   buckets,
		SchemaVersion:     13,
		MaxPartSize:       maxPartSize,
		ArtifactRetention: time.Duration(serviceConfig.Backup.ArtifactRetentionHours) * time.Hour,
		JobRetention:      time.Duration(serviceConfig.Backup.JobRetentionDays) * 24 * time.Hour,
	}
}

func toServerAdminOptions(
	input config.AdminConfig,
	serviceConfig *config.Config,
) server.AdminOptions {
	return server.AdminOptions{
		Enabled:            input.Enable,
		ExternalOrigins:    append([]string(nil), serviceConfig.ExternalOrigins...),
		SessionIdle:        time.Duration(input.SessionIdleMinutes) * time.Minute,
		SessionMaximum:     time.Duration(input.SessionMaxHours) * time.Hour,
		MaxUploadSize:      input.MaxUploadSize,
		MaxPathBytes:       serviceConfig.Backup.MaxPathBytes,
		MaxMutationEntries: serviceConfig.Webdav.MaxMutationEntries,
	}
}

func toServerWebDAVOptions(
	input config.WebdavConfig,
	externalOrigins []string,
) server.WebDAVOptions {
	return server.WebDAVOptions{
		Enabled:            input.Enable,
		Root:               input.Root,
		ExternalOrigins:    append([]string(nil), externalOrigins...),
		MaxUploadSize:      input.MaxUploadSize,
		UploadTempDir:      input.UploadTempDir,
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

func buildFileManager(
	ctx context.Context,
	serviceConfig *config.Config,
) (filemgr.IFileManager, filemgr.IFileIOCache, error) {
	var binding [32]byte
	if serviceConfig.IOCache.EnableL1Cache || serviceConfig.IOCache.EnableL2Cache {
		var err error
		binding, err = filemgr.BuildStorageBinding(
			serviceConfig.DBFile,
			serviceConfig.BotKind,
			serviceConfig.BotInfo,
			serviceConfig.RotateStream,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("build file cache binding: %w", err)
		}
	}
	blockStorage, err := blockio.Create(serviceConfig.BotKind, serviceConfig.BotInfo)
	if err != nil {
		return nil, nil, fmt.Errorf("init block io failed, kind:%s, err:%w", serviceConfig.BotKind, err)
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
		StorageBinding: binding,
	}
	ioCache, err := filemgr.NewFileIOCacheWithContext(ctx, cacheConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("create file io cache failed, err:%w", err)
	}
	fileManager := filemgr.NewFileManager(db.GetClient(), blockStorage, ioCache)
	return fileManager, ioCache, nil
}
