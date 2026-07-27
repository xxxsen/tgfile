package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	errStressThreshold = errors.New("stress threshold exceeded")
	errStressRecovery  = errors.New("stress recovery verification failed")
)

type stressRunner struct {
	protocol *soakRunner
	config   stressConfig
	fixtures stressFixtures
}

type stressFixtures struct {
	s3Key         string
	s3Content     []byte
	webDAVKey     string
	webDAVContent []byte
}

type stressSummary struct {
	Elapsed             time.Duration       `json:"elapsed"`
	Profile             string              `json:"profile"`
	Seed                uint64              `json:"seed"`
	Steps               []stressStepSummary `json:"steps"`
	Recovery            *stressStepSummary  `json:"recovery,omitempty"`
	ThresholdBreached   bool                `json:"threshold_breached"`
	Recovered           bool                `json:"recovered"`
	MaxHeapBytes        uint64              `json:"max_heap_bytes"`
	MaxGoroutines       int64               `json:"max_goroutines"`
	MaxFileHandles      int64               `json:"max_file_handles,omitempty"`
	ChangeJournalRows   int64               `json:"change_journal_rows"`
	DeleteStateRows     int64               `json:"delete_state_rows"`
	DatabaseBytes       int64               `json:"database_bytes"`
	IntegrityChecked    bool                `json:"integrity_checked"`
	BackendUploads      uint64              `json:"backend_uploads"`
	BackendDownloads    uint64              `json:"backend_downloads"`
	BackendDeleteCalls  uint64              `json:"backend_delete_calls"`
	BackendDeleteRefs   uint64              `json:"backend_delete_refs"`
	L2CacheFiles        int64               `json:"l2_cache_files"`
	L2CacheBytes        int64               `json:"l2_cache_bytes"`
	L2CacheChargedBytes int64               `json:"l2_cache_charged_bytes"`
	L2CacheTempFiles    int64               `json:"l2_cache_temp_files"`
	FinalActiveKeys     int                 `json:"final_active_keys"`
	FinalActiveLinks    int                 `json:"final_active_links"`
	FinalActiveUploads  int                 `json:"final_active_uploads"`
	FinalAuditCompleted bool                `json:"final_audit_completed"`
}

type stressEvent struct {
	Event            string             `json:"event"`
	Profile          string             `json:"profile,omitempty"`
	Steps            []int              `json:"steps,omitempty"`
	StepDuration     time.Duration      `json:"step_duration,omitempty"`
	RecoveryDuration time.Duration      `json:"recovery_duration,omitempty"`
	MaxErrorRate     float64            `json:"max_error_rate,omitempty"`
	MaxP99           time.Duration      `json:"max_p99,omitempty"`
	MutationInterval uint64             `json:"mutation_interval,omitempty"`
	Seed             uint64             `json:"seed,omitempty"`
	ClientDelay      time.Duration      `json:"client_delay,omitempty"`
	BackendDelay     time.Duration      `json:"backend_delay,omitempty"`
	Workspace        string             `json:"workspace,omitempty"`
	Step             *stressStepSummary `json:"step,omitempty"`
	Summary          *stressSummary     `json:"summary,omitempty"`
	Error            string             `json:"error,omitempty"`
}

func executeStress() error {
	config, err := loadStressConfig()
	if err != nil {
		return err
	}
	workspace, err := os.MkdirTemp("", "tgfile-stress-*")
	if err != nil {
		return fmt.Errorf("create stress workspace: %w", err)
	}
	keepWorkspace := true
	defer func() {
		if !keepWorkspace {
			_ = os.RemoveAll(workspace)
		}
	}()
	if err := writeStressStarted(config, workspace); err != nil {
		return err
	}
	summary, runErr := runLocalStress(config, workspace)
	if runErr != nil {
		_ = writeJSON(stressEvent{
			Event:     "stress_failed",
			Seed:      config.seed,
			Workspace: workspace,
			Summary:   summary,
			Error:     runErr.Error(),
		})
		return fmt.Errorf("run local stress; artifacts retained at %s: %w", workspace, runErr)
	}
	keepWorkspace = false
	return writeJSON(stressEvent{
		Event:     "stress_completed",
		Seed:      config.seed,
		Workspace: workspace,
		Summary:   summary,
	})
}

func writeStressStarted(config stressConfig, workspace string) error {
	return writeJSON(stressEvent{
		Event:            "stress_started",
		Profile:          config.profile,
		Steps:            config.steps,
		StepDuration:     config.stepDuration,
		RecoveryDuration: config.recoveryDuration,
		MaxErrorRate:     config.maxErrorRate,
		MaxP99:           config.maxP99,
		MutationInterval: config.mutationInterval,
		Seed:             config.seed,
		ClientDelay:      config.clientDelay,
		BackendDelay:     config.backendDelay,
		Workspace:        workspace,
	})
}

func runLocalStress(config stressConfig, workspace string) (*stressSummary, error) {
	localConfig := localTestConfig{
		seed:         config.seed,
		clientDelay:  config.clientDelay,
		backendDelay: config.backendDelay,
	}
	return runLocalProtocolTest(localConfig, workspace, func(protocol *soakRunner) (*stressSummary, error) {
		runner := &stressRunner{protocol: protocol, config: config}
		return runner.run()
	})
}

func (r *stressRunner) run() (*stressSummary, error) {
	r.protocol.startedAt = time.Now()
	summary := &stressSummary{
		Profile: r.config.profile,
		Seed:    r.config.seed,
		Steps:   make([]stressStepSummary, 0, len(r.config.steps)),
	}
	runErr := r.runPreflight()
	if runErr == nil {
		runErr = r.runRamp(summary)
	}
	if runErr == nil || errors.Is(runErr, errStressThreshold) {
		runErr = errors.Join(runErr, r.runRecovery(summary))
	}
	audit, finalErr := r.finish()
	r.populateFinalSummary(summary, audit, finalErr == nil)
	return summary, errors.Join(runErr, finalErr)
}

func (r *stressRunner) runPreflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := r.protocol.runCachePreflight(ctx); err != nil {
		return fmt.Errorf("stress cache preflight: %w", err)
	}
	if err := r.prepareFixtures(ctx); err != nil {
		return err
	}
	for index, profile := range []string{
		stressProfileS3,
		stressProfileWebDAV,
		stressProfileCross,
	} {
		cycleID := r.protocol.nextID.Add(1)
		if err := r.protocol.runStressCycle(ctx, profile, index, cycleID); err != nil {
			return fmt.Errorf("stress preflight %s: %w", profile, err)
		}
	}
	return nil
}

func (r *stressRunner) prepareFixtures(ctx context.Context) error {
	r.fixtures = stressFixtures{
		s3Key:         "stress-fixture-s3.bin",
		s3Content:     contentFor(r.config.seed, 1, 64*1024),
		webDAVKey:     "stress-fixture-webdav.bin",
		webDAVContent: contentFor(r.config.seed, 2, 64*1024),
	}
	r.protocol.trackKey(r.fixtures.s3Key)
	if err := r.protocol.stressS3Put(ctx, r.fixtures.s3Key, r.fixtures.s3Content); err != nil {
		return fmt.Errorf("create S3 stress fixture: %w", err)
	}
	r.protocol.trackKey(r.fixtures.webDAVKey)
	if err := r.protocol.stressWebDAVPut(
		ctx,
		r.fixtures.webDAVKey,
		r.fixtures.webDAVContent,
		http.StatusCreated,
	); err != nil {
		return fmt.Errorf("create WebDAV stress fixture: %w", err)
	}
	if err := r.protocol.runStressCrossRead(ctx, r.fixtures, 0); err != nil {
		return fmt.Errorf("verify stress fixtures: %w", err)
	}
	return nil
}

func (r *stressRunner) runRamp(summary *stressSummary) error {
	for _, workers := range r.config.steps {
		step := r.runStep(workers, r.config.stepDuration, r.config.maxErrorRate)
		summary.Steps = append(summary.Steps, step)
		if err := writeJSON(stressEvent{Event: "stress_step_completed", Step: &step}); err != nil {
			return err
		}
		if step.SaturationReason != "" {
			summary.ThresholdBreached = true
			return fmt.Errorf(
				"%w at %d workers: %s",
				errStressThreshold,
				workers,
				step.SaturationReason,
			)
		}
	}
	return nil
}

func (r *stressRunner) runRecovery(summary *stressSummary) error {
	recovery := r.runStep(1, r.config.recoveryDuration, 0)
	summary.Recovery = &recovery
	summary.Recovered = recovery.SaturationReason == ""
	if err := writeJSON(stressEvent{Event: "stress_recovery_completed", Step: &recovery}); err != nil {
		return err
	}
	if !summary.Recovered {
		return fmt.Errorf("%w: %s", errStressRecovery, recovery.SaturationReason)
	}
	return nil
}

func (r *stressRunner) runStep(
	workers int,
	duration time.Duration,
	maxErrorRate float64,
) stressStepSummary {
	metrics := newStressStepMetrics(workers)
	stepContext, cancelStep := context.WithTimeout(context.Background(), duration)
	r.protocol.requestObserver = func(observation requestObservation) {
		if stepContext.Err() != nil &&
			(errors.Is(observation.err, context.Canceled) ||
				errors.Is(observation.err, context.DeadlineExceeded)) {
			observation.err = nil
		}
		metrics.recordRequest(observation)
	}
	var workerGroup sync.WaitGroup
	for workerID := range workers {
		workerGroup.Add(1)
		go func() {
			defer workerGroup.Done()
			r.runStressWorker(stepContext, workerID, metrics)
		}()
	}
	workerGroup.Wait()
	cancelStep()
	r.protocol.requestObserver = nil
	r.protocol.sampleResources()
	return metrics.summary(maxErrorRate, r.config.maxP99)
}

func (r *stressRunner) runStressWorker(
	stepContext context.Context,
	workerID int,
	metrics *stressStepMetrics,
) {
	for stepContext.Err() == nil {
		cycleID := r.protocol.nextID.Add(1)
		operationContext, cancelOperation := context.WithTimeout(
			stepContext,
			r.config.operationTimeout,
		)
		startedAt := time.Now()
		err := r.runLoadCycle(
			operationContext,
			workerID,
			cycleID,
		)
		cancelOperation()
		if err != nil && stepContext.Err() != nil {
			return
		}
		metrics.recordOperation(time.Since(startedAt), err)
	}
}

func (r *stressRunner) runLoadCycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	if cycleID%r.config.mutationInterval == 0 {
		return r.protocol.runStressCycle(ctx, r.config.profile, workerID, cycleID)
	}
	profile, err := selectedStressProfile(r.config.profile, r.config.seed, cycleID)
	if err != nil {
		return err
	}
	switch profile {
	case stressProfileS3:
		return r.protocol.runStressS3Read(ctx, r.fixtures.s3Key, r.fixtures.s3Content)
	case stressProfileWebDAV:
		return r.protocol.runStressWebDAVRead(
			ctx,
			r.fixtures.webDAVKey,
			r.fixtures.webDAVContent,
		)
	case stressProfileCross:
		return r.protocol.runStressCrossRead(ctx, r.fixtures, cycleID)
	default:
		return fmt.Errorf("%w: %q", errInvalidStressProfile, profile)
	}
}

func (r *stressRunner) finish() (auditResult, error) {
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCleanup()
	cleanupErr := r.protocol.cleanupTrackedResources(cleanupContext)
	audit, auditErr := r.protocol.audit(cleanupContext)
	r.protocol.sampleResources()
	return audit, errors.Join(cleanupErr, auditErr)
}

func (r *stressRunner) populateFinalSummary(
	summary *stressSummary,
	audit auditResult,
	auditCompleted bool,
) {
	summary.Elapsed = time.Since(r.protocol.startedAt).Round(time.Millisecond)
	summary.MaxHeapBytes = r.protocol.maxHeapBytes.Load()
	summary.MaxGoroutines = r.protocol.maxGoroutine.Load()
	summary.MaxFileHandles = r.protocol.maxFDs.Load()
	summary.ChangeJournalRows = audit.changeJournalRows
	summary.DeleteStateRows = audit.deleteStateRows
	summary.DatabaseBytes = audit.databaseBytes
	summary.IntegrityChecked = audit.integrityChecked
	backend := r.protocol.block.counts()
	summary.BackendUploads = backend.uploads
	summary.BackendDownloads = backend.downloads
	summary.BackendDeleteCalls = backend.deleteCalls
	summary.BackendDeleteRefs = backend.deleteRefs
	summary.L2CacheFiles = audit.cacheFiles
	summary.L2CacheBytes = audit.cacheBytes
	summary.L2CacheChargedBytes = audit.cacheChargedBytes
	summary.L2CacheTempFiles = audit.cacheTempFiles
	r.protocol.activeMu.Lock()
	summary.FinalActiveKeys = len(r.protocol.activeKeys)
	summary.FinalActiveLinks = len(r.protocol.activeLinks)
	summary.FinalActiveUploads = len(r.protocol.activeUpload)
	r.protocol.activeMu.Unlock()
	summary.FinalAuditCompleted = auditCompleted
}
