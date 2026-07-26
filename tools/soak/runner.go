package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxxsen/common/database"
)

const (
	soakAccessKey  = "soak-access"
	soakSecretKey  = "soak-secret"
	soakBucket     = "soak"
	soakCyclePause = 750 * time.Millisecond
)

var errSoakOperation = errors.New("soak operation failed")

type soakRunner struct {
	transport       http.RoundTripper
	baseURL         string
	database        database.IDatabase
	blockDir        string
	spoolDir        string
	seed            uint64
	clientDelay     time.Duration
	requestObserver func(requestObservation)
	startedAt       time.Time
	requests        atomic.Uint64
	cycles          atomic.Uint64
	s3Cycles        atomic.Uint64
	webDAVCycles    atomic.Uint64
	lockCycles      atomic.Uint64
	multiCycles     atomic.Uint64
	slowCycles      atomic.Uint64
	nextID          atomic.Uint64
	activeMu        sync.Mutex
	activeKeys      map[string]struct{}
	activeUpload    map[string]string
	maxHeapBytes    atomic.Uint64
	maxGoroutine    atomic.Int64
	maxFDs          atomic.Int64
}

type soakSummary struct {
	Elapsed           time.Duration `json:"elapsed"`
	Requests          uint64        `json:"requests"`
	Cycles            uint64        `json:"cycles"`
	S3Cycles          uint64        `json:"s3_cycles"`
	WebDAVCycles      uint64        `json:"webdav_cycles"`
	LockCycles        uint64        `json:"lock_cycles"`
	MultipartCycles   uint64        `json:"multipart_cycles"`
	SlowNetworkCycles uint64        `json:"slow_network_cycles"`
	MaxHeapBytes      uint64        `json:"max_heap_bytes"`
	MaxGoroutines     int64         `json:"max_goroutines"`
	MaxFileHandles    int64         `json:"max_file_handles,omitempty"`
	ChangeJournal     int64         `json:"change_journal_rows"`
	DeleteStateRows   int64         `json:"delete_state_rows"`
	DatabaseBytes     int64         `json:"database_bytes"`
	IntegrityChecked  bool          `json:"integrity_checked"`
}

type progressEvent struct {
	Event         string        `json:"event"`
	Elapsed       time.Duration `json:"elapsed"`
	Requests      uint64        `json:"requests"`
	Cycles        uint64        `json:"cycles"`
	HeapBytes     uint64        `json:"heap_bytes"`
	Goroutines    int64         `json:"goroutines"`
	FileHandles   int64         `json:"file_handles,omitempty"`
	ActiveKeys    int           `json:"active_keys"`
	ActiveUploads int           `json:"active_uploads"`
}

func newSoakRunner(
	transport http.RoundTripper,
	baseURL string,
	databaseClient database.IDatabase,
	blockDir, spoolDir string,
	seed uint64,
	clientDelay time.Duration,
) *soakRunner {
	return &soakRunner{
		transport:    transport,
		baseURL:      baseURL,
		database:     databaseClient,
		blockDir:     blockDir,
		spoolDir:     spoolDir,
		seed:         seed,
		clientDelay:  clientDelay,
		activeKeys:   make(map[string]struct{}),
		activeUpload: make(map[string]string),
	}
}

func (r *soakRunner) run(duration time.Duration, workers int) (*soakSummary, error) {
	r.startedAt = time.Now()
	runContext, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	preflightContext, cancelPreflight := context.WithTimeout(context.Background(), 2*time.Minute)
	err := r.runPreflight(preflightContext)
	cancelPreflight()
	if err != nil {
		return nil, fmt.Errorf("%w: preflight: %w", errSoakOperation, err)
	}
	errorChannel := make(chan error, 1)
	var workerGroup sync.WaitGroup
	for workerID := range workers {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			r.runWorker(runContext, workerID, errorChannel, cancel)
		}()
	}
	workersDone := make(chan struct{})
	go func() {
		workerGroup.Wait()
		close(workersDone)
	}()
	progressTicker := time.NewTicker(progressInterval(duration))
	defer progressTicker.Stop()
	var operationErr error
	for workersDone != nil {
		select {
		case <-progressTicker.C:
			r.sampleResources()
			if err := writeProgress(r.progress()); err != nil && operationErr == nil {
				operationErr = err
				cancel()
			}
		case err := <-errorChannel:
			if operationErr == nil {
				operationErr = err
				cancel()
			}
		case <-workersDone:
			workersDone = nil
		}
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelCleanup()
	cleanupErr := r.cleanupTrackedResources(cleanupContext)
	audit, auditErr := r.audit(cleanupContext)
	r.sampleResources()
	summary := r.summary(audit)
	if err := writeProgress(r.progress()); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	return summary, errors.Join(operationErr, cleanupErr, auditErr)
}

func (r *soakRunner) runWorker(
	runContext context.Context,
	workerID int,
	errorChannel chan<- error,
	cancel context.CancelFunc,
) {
	for {
		select {
		case <-runContext.Done():
			return
		default:
		}
		cycleID := r.nextID.Add(1)
		operationContext, cancelOperation := context.WithTimeout(
			context.WithoutCancel(runContext),
			45*time.Second,
		)
		err := r.runCycle(operationContext, workerID, cycleID)
		cancelOperation()
		if err != nil {
			select {
			case errorChannel <- fmt.Errorf(
				"%w: worker %d cycle %d: %w",
				errSoakOperation,
				workerID,
				cycleID,
				err,
			):
			default:
			}
			cancel()
			return
		}
		r.cycles.Add(1)
		select {
		case <-runContext.Done():
			return
		case <-time.After(soakCyclePause):
		}
	}
}

func (r *soakRunner) runCycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	if cycleID%200 == 0 {
		return r.runMultipartLifecycle(
			ctx,
			objectKey("multipart", workerID, cycleID),
		)
	}
	if cycleID%100 == 0 {
		return r.runSyncReport(ctx)
	}
	if cycleID%50 == 0 {
		return r.runSlowNetworkLifecycle(
			ctx,
			objectKey("slow-network", workerID, cycleID),
			cycleID,
		)
	}
	switch (cycleID + r.seed) % 3 {
	case 0:
		r.s3Cycles.Add(1)
		return r.runS3Lifecycle(ctx, workerID, cycleID)
	case 1:
		r.webDAVCycles.Add(1)
		return r.runWebDAVLifecycle(ctx, workerID, cycleID)
	default:
		r.lockCycles.Add(1)
		return r.runLockLifecycle(ctx, workerID, cycleID)
	}
}

func (r *soakRunner) trackKey(key string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	r.activeKeys[key] = struct{}{}
}

func (r *soakRunner) untrackKey(key string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	delete(r.activeKeys, key)
}

func (r *soakRunner) trackUpload(uploadID, key string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	r.activeUpload[uploadID] = key
}

func (r *soakRunner) untrackUpload(uploadID string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	delete(r.activeUpload, uploadID)
}

func (r *soakRunner) progress() progressEvent {
	r.activeMu.Lock()
	activeKeys := len(r.activeKeys)
	activeUploads := len(r.activeUpload)
	r.activeMu.Unlock()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return progressEvent{
		Event:         "progress",
		Elapsed:       time.Since(r.startedAt).Round(time.Millisecond),
		Requests:      r.requests.Load(),
		Cycles:        r.cycles.Load(),
		HeapBytes:     memory.HeapAlloc,
		Goroutines:    int64(runtime.NumGoroutine()),
		FileHandles:   countOpenFileHandles(),
		ActiveKeys:    activeKeys,
		ActiveUploads: activeUploads,
	}
}

func (r *soakRunner) sampleResources() {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	updateMaxUint64(&r.maxHeapBytes, memory.HeapAlloc)
	updateMaxInt64(&r.maxGoroutine, int64(runtime.NumGoroutine()))
	if fileHandles := countOpenFileHandles(); fileHandles >= 0 {
		updateMaxInt64(&r.maxFDs, fileHandles)
	}
}

func (r *soakRunner) summary(audit auditResult) *soakSummary {
	return &soakSummary{
		Elapsed:           time.Since(r.startedAt).Round(time.Millisecond),
		Requests:          r.requests.Load(),
		Cycles:            r.cycles.Load(),
		S3Cycles:          r.s3Cycles.Load(),
		WebDAVCycles:      r.webDAVCycles.Load(),
		LockCycles:        r.lockCycles.Load(),
		MultipartCycles:   r.multiCycles.Load(),
		SlowNetworkCycles: r.slowCycles.Load(),
		MaxHeapBytes:      r.maxHeapBytes.Load(),
		MaxGoroutines:     r.maxGoroutine.Load(),
		MaxFileHandles:    r.maxFDs.Load(),
		ChangeJournal:     audit.changeJournalRows,
		DeleteStateRows:   audit.deleteStateRows,
		DatabaseBytes:     audit.databaseBytes,
		IntegrityChecked:  audit.integrityChecked,
	}
}

func progressInterval(duration time.Duration) time.Duration {
	interval := 30 * time.Second
	if duration < 2*interval {
		interval = duration / 2
	}
	if interval < time.Second {
		return time.Second
	}
	return interval
}

func updateMaxUint64(target *atomic.Uint64, value uint64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func updateMaxInt64(target *atomic.Int64, value int64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func writeProgress(progress progressEvent) error {
	return writeJSON(progress)
}
