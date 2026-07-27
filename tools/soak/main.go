// Command soak runs manually triggered local S3 and WebDAV protocol tests.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/xxxsen/common/logger"

	"github.com/xxxsen/tgfile/blockio/localfile"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"
)

const (
	testModeSoak          = "soak"
	testModeStress        = "stress"
	defaultSoakDuration   = 15 * time.Minute
	defaultSoakWorkers    = 4
	defaultClientDelay    = 5 * time.Millisecond
	defaultBackendDelay   = 5 * time.Millisecond
	defaultDelayChunkSize = 32 * 1024
	localBlockSize        = 20 * 1024 * 1024
	soakL1CacheSize       = 256 * 1024
	soakL1KeySizeLimit    = 4 * 1024
	soakL2CacheSize       = 4 * 1024 * 1024
	soakL2KeySizeLimit    = 512 * 1024
	cacheCloseTimeout     = 30 * time.Second
)

var (
	errInvalidTestMode     = errors.New("invalid protocol test mode")
	errInvalidDuration     = errors.New("TGFILE_SOAK_DURATION must be a positive duration")
	errInvalidWorkers      = errors.New("TGFILE_SOAK_WORKERS must be a positive integer")
	errInvalidSeed         = errors.New("TGFILE_SOAK_SEED must be an integer")
	errInvalidClientDelay  = errors.New("TGFILE_SOAK_CLIENT_DELAY must be a non-negative duration")
	errInvalidBackendDelay = errors.New("TGFILE_SOAK_BACKEND_DELAY must be a non-negative duration")
)

type soakConfig struct {
	duration     time.Duration
	workers      int
	seed         uint64
	clientDelay  time.Duration
	backendDelay time.Duration
}

type outputEvent struct {
	Event        string        `json:"event"`
	Duration     time.Duration `json:"duration,omitempty"`
	Workers      int           `json:"workers,omitempty"`
	Seed         uint64        `json:"seed,omitempty"`
	ClientDelay  time.Duration `json:"client_delay,omitempty"`
	BackendDelay time.Duration `json:"backend_delay,omitempty"`
	Workspace    string        `json:"workspace,omitempty"`
	Error        string        `json:"error,omitempty"`
	SoakSummary  *soakSummary  `json:"summary,omitempty"`
}

func main() {
	if err := execute(); err != nil {
		log.New(os.Stderr, "tgfile protocol test: ", 0).Print(err)
		os.Exit(1)
	}
}

func execute() error {
	mode, err := requestedMode(os.Args[1:])
	if err != nil {
		return err
	}
	if mode == testModeStress {
		return executeStress()
	}
	return executeSoak()
}

func executeSoak() error {
	config, err := loadSoakConfig()
	if err != nil {
		return err
	}
	workspace, err := os.MkdirTemp("", "tgfile-soak-*")
	if err != nil {
		return fmt.Errorf("create soak workspace: %w", err)
	}
	keepWorkspace := true
	defer func() {
		if !keepWorkspace {
			_ = os.RemoveAll(workspace)
		}
	}()
	if err := writeEvent(outputEvent{
		Event:        "started",
		Duration:     config.duration,
		Workers:      config.workers,
		Seed:         config.seed,
		ClientDelay:  config.clientDelay,
		BackendDelay: config.backendDelay,
		Workspace:    workspace,
	}); err != nil {
		return err
	}
	summary, err := runLocalSoak(config, workspace)
	if err != nil {
		_ = writeEvent(outputEvent{
			Event:     "failed",
			Seed:      config.seed,
			Workspace: workspace,
			Error:     err.Error(),
		})
		return fmt.Errorf("run local soak; artifacts retained at %s: %w", workspace, err)
	}
	keepWorkspace = false
	return writeEvent(outputEvent{
		Event:       "completed",
		Seed:        config.seed,
		Workspace:   workspace,
		SoakSummary: summary,
	})
}

func requestedMode(arguments []string) (string, error) {
	if len(arguments) == 0 {
		return testModeSoak, nil
	}
	if len(arguments) != 1 {
		return "", fmt.Errorf("%w: expected soak or stress", errInvalidTestMode)
	}
	switch arguments[0] {
	case testModeSoak, testModeStress:
		return arguments[0], nil
	default:
		return "", fmt.Errorf("%w: %q", errInvalidTestMode, arguments[0])
	}
}

func loadSoakConfig() (soakConfig, error) {
	config := soakConfig{
		duration:     defaultSoakDuration,
		workers:      defaultSoakWorkers,
		seed:         uint64(time.Now().UnixNano()),
		clientDelay:  defaultClientDelay,
		backendDelay: defaultBackendDelay,
	}
	if raw := os.Getenv("TGFILE_SOAK_DURATION"); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return soakConfig{}, fmt.Errorf("%w: %q", errInvalidDuration, raw)
		}
		config.duration = duration
	}
	if raw := os.Getenv("TGFILE_SOAK_WORKERS"); raw != "" {
		workers, err := strconv.Atoi(raw)
		if err != nil || workers <= 0 {
			return soakConfig{}, fmt.Errorf("%w: %q", errInvalidWorkers, raw)
		}
		config.workers = workers
	}
	if raw := os.Getenv("TGFILE_SOAK_SEED"); raw != "" {
		seed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return soakConfig{}, fmt.Errorf("%w: %q", errInvalidSeed, raw)
		}
		config.seed = seed
	}
	if raw := os.Getenv("TGFILE_SOAK_CLIENT_DELAY"); raw != "" {
		delay, err := parseNonNegativeDuration(raw, errInvalidClientDelay)
		if err != nil {
			return soakConfig{}, err
		}
		config.clientDelay = delay
	}
	if raw := os.Getenv("TGFILE_SOAK_BACKEND_DELAY"); raw != "" {
		delay, err := parseNonNegativeDuration(raw, errInvalidBackendDelay)
		if err != nil {
			return soakConfig{}, err
		}
		config.backendDelay = delay
	}
	return config, nil
}

func parseNonNegativeDuration(raw string, invalid error) (time.Duration, error) {
	delay, err := time.ParseDuration(raw)
	if err != nil || delay < 0 {
		return 0, fmt.Errorf("%w: %q", invalid, raw)
	}
	return delay, nil
}

func runLocalSoak(config soakConfig, workspace string) (*soakSummary, error) {
	localConfig := localTestConfig{
		seed:         config.seed,
		clientDelay:  config.clientDelay,
		backendDelay: config.backendDelay,
	}
	return runLocalProtocolTest(localConfig, workspace, func(runner *soakRunner) (*soakSummary, error) {
		return runner.run(config.duration, config.workers)
	})
}

type localTestConfig struct {
	seed         uint64
	clientDelay  time.Duration
	backendDelay time.Duration
}

func runLocalProtocolTest[T any](
	config localTestConfig,
	workspace string,
	run func(*soakRunner) (T, error),
) (T, error) {
	var zero T
	logger.Init(filepath.Join(workspace, "server.log"), "error", 1, 10*1024*1024, 1, false)
	databaseClient, err := db.Open(filepath.Join(workspace, "soak.db"))
	if err != nil {
		return zero, fmt.Errorf("open protocol test database: %w", err)
	}
	block, err := localfile.New(filepath.Join(workspace, "blocks"), localBlockSize)
	if err != nil {
		return zero, errors.Join(
			fmt.Errorf("create localfile backend: %w", err),
			databaseClient.Close(),
		)
	}
	slowBlock := newSlowBlockIO(block, config.backendDelay, defaultDelayChunkSize)
	block = slowBlock
	cacheDir := filepath.Join(workspace, "cache")
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		L1CacheSize:    soakL1CacheSize,
		L1KeySizeLimit: soakL1KeySizeLimit,
		L2CacheSize:    soakL2CacheSize,
		L2KeySizeLimit: soakL2KeySizeLimit,
		L2CacheDir:     cacheDir,
	})
	if err != nil {
		return zero, errors.Join(
			fmt.Errorf("create file cache: %w", err),
			databaseClient.Close(),
		)
	}
	manager := filemgr.NewFileManager(databaseClient, block, cache)
	observedManager := newObservedFileManager(manager)
	testServer, err := newLocalProtocolHTTPServer(workspace, observedManager)
	if err != nil {
		return zero, errors.Join(err, closeLocalProtocolCache(cache), databaseClient.Close())
	}
	stopWorkers := startLocalProtocolWorkers(manager)
	runner := newSoakRunner(
		testServer.Client().Transport,
		testServer.URL,
		databaseClient,
		manager,
		observedManager,
		slowBlock,
		filepath.Join(workspace, "blocks"),
		filepath.Join(workspace, "spool"),
		cacheDir,
		config.seed,
		config.clientDelay,
	)
	result, runErr := run(runner)
	stopWorkers()
	testServer.Close()
	return result, errors.Join(runErr, closeLocalProtocolCache(cache), databaseClient.Close())
}

func closeLocalProtocolCache(cache filemgr.IFileIOCache) error {
	closeContext, cancelClose := context.WithTimeout(context.Background(), cacheCloseTimeout)
	defer cancelClose()
	if err := cache.Close(closeContext); err != nil {
		return fmt.Errorf("close local protocol cache: %w", err)
	}
	return nil
}

func newLocalProtocolHTTPServer(
	workspace string,
	manager filemgr.IFileManager,
) (*httptest.Server, error) {
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithUser(map[string]string{soakAccessKey: soakSecretKey}),
		server.WithS3(server.S3Options{
			Enabled: true,
			Buckets: []server.S3BucketOptions{{
				Name: soakBucket,
				ACL:  server.BucketACLPrivate,
			}},
			MaxObjectSize:        5 * 1024 * 1024 * 1024,
			MultipartExpireHours: 24,
		}),
		server.WithWebDAV(server.WebDAVOptions{
			Enabled:            true,
			Root:               "/",
			UploadTempDir:      filepath.Join(workspace, "spool"),
			Users:              map[string]string{soakAccessKey: "read-write"},
			MaxUploadSize:      5 * 1024 * 1024 * 1024,
			MaxMutationEntries: 10000,
			SyncPageSize:       1000,
		}),
		server.WithFileManager(manager),
	)
	if err != nil {
		return nil, fmt.Errorf("create protocol test HTTP server: %w", err)
	}
	return httptest.NewServer(handler), nil
}

func startLocalProtocolWorkers(manager filemgr.IFileManager) func() {
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	var workerGroup sync.WaitGroup
	workerGroup.Add(2)
	go func() {
		defer workerGroup.Done()
		_ = manager.RunBlockDeleteWorker(workerContext)
	}()
	go func() {
		defer workerGroup.Done()
		_ = manager.RunMultipartCleanupWorker(workerContext)
	}()
	return func() {
		cancelWorkers()
		workerGroup.Wait()
	}
}

func writeEvent(event outputEvent) error {
	return writeJSON(event)
}
