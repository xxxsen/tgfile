package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/xxxsen/tgfile/utils"
)

const (
	cachePreflightL1Size = 1024
	cachePreflightL2Size = 256 * 1024
)

type cacheBusinessState struct {
	files          int64
	parts          int64
	mappings       int64
	deleteStates   int64
	nonLiveStates  int64
	deleteAttempts int64
}

type cacheReadOperation struct {
	name string
	run  func() error
}

func (r *soakRunner) runCachePreflight(ctx context.Context) error {
	largeKey := "preflight-cache-l2.bin"
	smallKey := "preflight-cache-l1.bin"
	largeContent := contentFor(r.seed+20, 1, cachePreflightL2Size)
	r.trackKey(largeKey)
	if _, err := r.expectS3Status(
		ctx,
		http.MethodPut,
		largeKey,
		"",
		largeContent,
		nil,
		http.StatusOK,
	); err != nil {
		return errors.Join(err, r.cleanupCachePreflightWithTimeout(ctx, "", largeKey))
	}
	fileKey, link, err := r.createCacheDirectLink(ctx, largeKey)
	if err != nil {
		return errors.Join(err, r.cleanupCachePreflightWithTimeout(ctx, "", largeKey))
	}
	operationErr := r.verifyL2CachePreflight(ctx, largeKey, fileKey, largeContent)
	if operationErr == nil {
		operationErr = r.verifyL1CachePreflight(ctx, smallKey)
	}
	cleanupErr := r.cleanupCachePreflightWithTimeout(ctx, link, largeKey, smallKey)
	return errors.Join(operationErr, cleanupErr)
}

func (r *soakRunner) verifyL2CachePreflight(
	ctx context.Context,
	key, fileKey string,
	content []byte,
) error {
	baselineState, err := r.readCacheBusinessState(ctx)
	if err != nil {
		return err
	}
	baselineBackend := r.block.counts()
	rangeHeaders := make(http.Header)
	rangeHeaders.Set("Range", "bytes=4096-12287")
	operations := []cacheReadOperation{
		{
			name: "S3 GET",
			run: func() error {
				return r.expectCacheS3Body(ctx, key, nil, http.StatusOK, content)
			},
		},
		{
			name: "S3 range GET",
			run: func() error {
				return r.expectCacheS3Body(
					ctx,
					key,
					rangeHeaders,
					http.StatusPartialContent,
					content[4096:12288],
				)
			},
		},
		{
			name: "WebDAV GET",
			run: func() error {
				return r.expectCacheWebDAVBody(ctx, key, content)
			},
		},
		{
			name: "direct-link GET",
			run: func() error {
				return r.expectCacheDirectBody(ctx, fileKey, nil, http.StatusOK, content)
			},
		},
	}
	if err := r.runConcurrentColdCacheReads(ctx, operations); err != nil {
		return err
	}
	if err := r.expectBackendDownloadDelta(baselineBackend, 1, "concurrent L2 cold reads"); err != nil {
		return err
	}
	if err := r.expectCacheReadSafety(ctx, baselineState, baselineBackend); err != nil {
		return err
	}
	return r.verifyL2CacheHitAndRecovery(
		ctx,
		key,
		fileKey,
		content,
		baselineState,
		baselineBackend,
		rangeHeaders,
	)
}

func (r *soakRunner) verifyL2CacheHitAndRecovery(
	ctx context.Context,
	key, fileKey string,
	content []byte,
	baselineState cacheBusinessState,
	baselineBackend blockIOCounts,
	rangeHeaders http.Header,
) error {
	hotBaseline := r.block.counts()
	if err := r.expectCacheDirectBody(
		ctx,
		fileKey,
		rangeHeaders,
		http.StatusPartialContent,
		content[4096:12288],
	); err != nil {
		return err
	}
	if err := r.expectBackendDownloadDelta(hotBaseline, 0, "L2 hot read"); err != nil {
		return err
	}

	entry, err := findCacheEntryBySize(r.cacheDir, int64(len(content)))
	if err != nil {
		return err
	}
	if err := os.Truncate(entry, int64(len(content)/2)); err != nil {
		return fmt.Errorf("truncate managed L2 cache entry: %w", err)
	}
	recoveryBaseline := r.block.counts()
	if err := r.expectCacheWebDAVBody(ctx, key, content); err != nil {
		return err
	}
	if err := r.expectBackendDownloadDelta(recoveryBaseline, 1, "corrupt L2 recovery"); err != nil {
		return err
	}
	if _, err := inspectL2CacheDirectory(r.cacheDir, soakL2CacheSize); err != nil {
		return fmt.Errorf("verify recovered L2 cache: %w", err)
	}
	return r.expectCacheReadSafety(ctx, baselineState, baselineBackend)
}

func (r *soakRunner) verifyL1CachePreflight(ctx context.Context, smallKey string) error {
	baselineCache, err := inspectL2CacheDirectory(r.cacheDir, soakL2CacheSize)
	if err != nil {
		return fmt.Errorf("inspect L2 before L1 preflight: %w", err)
	}
	smallContent := contentFor(r.seed+21, 2, cachePreflightL1Size)
	r.trackKey(smallKey)
	if _, err := r.expectS3Status(
		ctx,
		http.MethodPut,
		smallKey,
		"",
		smallContent,
		nil,
		http.StatusOK,
	); err != nil {
		return err
	}
	l1State, err := r.readCacheBusinessState(ctx)
	if err != nil {
		return err
	}
	l1Baseline := r.block.counts()
	if err := r.expectCacheS3Body(ctx, smallKey, nil, http.StatusOK, smallContent); err != nil {
		return err
	}
	if err := r.expectBackendDownloadDelta(l1Baseline, 1, "L1 cold read"); err != nil {
		return err
	}
	l1HotBaseline := r.block.counts()
	if err := r.expectCacheWebDAVBody(ctx, smallKey, smallContent); err != nil {
		return err
	}
	if err := r.expectBackendDownloadDelta(l1HotBaseline, 0, "L1 hot read"); err != nil {
		return err
	}
	currentCache, err := inspectL2CacheDirectory(r.cacheDir, soakL2CacheSize)
	if err != nil {
		return fmt.Errorf("inspect L2 after L1 preflight: %w", err)
	}
	if currentCache != baselineCache {
		return fmt.Errorf(
			"%w: L1 preflight changed L2 cache: before=%+v after=%+v",
			errAuditInvariant,
			baselineCache,
			currentCache,
		)
	}
	return r.expectCacheReadSafety(ctx, l1State, l1Baseline)
}

func (r *soakRunner) runCacheLifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("cache", workerID, cycleID)
	sizes := [...]int{8 * 1024, 64 * 1024, 256 * 1024, 512 * 1024}
	content := contentFor(r.seed+22, cycleID, sizes[cycleID%uint64(len(sizes))])
	r.trackKey(key)
	if _, err := r.expectS3Status(
		ctx,
		http.MethodPut,
		key,
		"",
		content,
		nil,
		http.StatusOK,
	); err != nil {
		return errors.Join(err, r.cleanupCachePreflightWithTimeout(ctx, "", key))
	}
	fileKey, link, err := r.createCacheDirectLink(ctx, key)
	if err != nil {
		return errors.Join(err, r.cleanupCachePreflightWithTimeout(ctx, "", key))
	}
	operationErr := r.verifyCacheLifecycle(ctx, key, fileKey, content)
	cleanupErr := r.cleanupCachePreflightWithTimeout(ctx, link, key)
	return errors.Join(operationErr, cleanupErr)
}

func (r *soakRunner) verifyCacheLifecycle(
	ctx context.Context,
	key, fileKey string,
	content []byte,
) error {
	if err := r.expectCacheS3Body(ctx, key, nil, http.StatusOK, content); err != nil {
		return err
	}
	if err := r.expectCacheWebDAVBody(ctx, key, content); err != nil {
		return err
	}
	rangeHeaders := make(http.Header)
	end := min(len(content), 4096) - 1
	rangeHeaders.Set("Range", fmt.Sprintf("bytes=0-%d", end))
	return r.expectCacheDirectBody(
		ctx,
		fileKey,
		rangeHeaders,
		http.StatusPartialContent,
		content[:end+1],
	)
}

func (r *soakRunner) expectCacheS3Body(
	ctx context.Context,
	key string,
	headers http.Header,
	status int,
	expected []byte,
) error {
	result, err := r.expectS3Status(
		ctx,
		http.MethodGet,
		key,
		"",
		nil,
		headers,
		status,
	)
	if err != nil {
		return err
	}
	return expectBody(result, expected)
}

func (r *soakRunner) expectCacheWebDAVBody(
	ctx context.Context,
	key string,
	expected []byte,
) error {
	result, err := r.expectWebDAVStatus(
		ctx,
		http.MethodGet,
		key,
		nil,
		nil,
		http.StatusOK,
	)
	if err != nil {
		return err
	}
	return expectBody(result, expected)
}

func (r *soakRunner) expectCacheDirectBody(
	ctx context.Context,
	fileKey string,
	headers http.Header,
	status int,
	expected []byte,
) error {
	result, err := r.expectDirectStatus(ctx, fileKey, headers, status)
	if err != nil {
		return err
	}
	return expectBody(result, expected)
}

func (r *soakRunner) createCacheDirectLink(
	ctx context.Context,
	key string,
) (string, string, error) {
	object, err := r.manager.StatS3Object(ctx, "/"+soakBucket+"/"+key)
	if err != nil {
		return "", "", fmt.Errorf("stat cache soak object: %w", err)
	}
	hash := hex.EncodeToString(utils.FileIdToHash(object.Link.FileId))
	fileKey := hash + "-soak-cache.bin"
	directory := path.Join("/defaults", fileKey[:2])
	link := path.Join(directory, fileKey)
	if err := r.manager.CreateFileLink(ctx, link, object.Link.FileId, 0, false); err != nil {
		return "", "", fmt.Errorf("create cache soak direct link: %w", err)
	}
	r.trackLink(link)
	r.trackLinkDirectory("/defaults")
	r.trackLinkDirectory(directory)
	return fileKey, link, nil
}

func (r *soakRunner) runConcurrentColdCacheReads(
	ctx context.Context,
	operations []cacheReadOperation,
) error {
	gate := make(chan struct{})
	var release sync.Once
	releaseGate := func() {
		release.Do(func() {
			close(gate)
			r.block.setDownloadGate(nil)
		})
	}
	defer releaseGate()
	r.block.drainDownloadStarts()
	r.block.setDownloadGate(gate)
	openCalls := make(chan struct{}, len(operations))
	r.observedManager.setOpenObserver(openCalls)
	defer r.observedManager.setOpenObserver(nil)
	start := make(chan struct{})
	results := make(chan error, len(operations))
	for _, operation := range operations {
		go func() {
			<-start
			if err := operation.run(); err != nil {
				results <- fmt.Errorf("%s: %w", operation.name, err)
				return
			}
			results <- nil
		}()
	}
	close(start)
	for range operations {
		select {
		case <-ctx.Done():
			releaseGate()
			return fmt.Errorf("wait for concurrent cache opens: %w", ctx.Err())
		case err := <-results:
			releaseGate()
			if err == nil {
				err = fmt.Errorf("%w: cache request completed without opening a file", errAuditInvariant)
			}
			for remaining := len(operations) - 1; remaining > 0; remaining-- {
				select {
				case <-ctx.Done():
					return errors.Join(err, fmt.Errorf("drain concurrent cache reads: %w", ctx.Err()))
				case nextErr := <-results:
					err = errors.Join(err, nextErr)
				}
			}
			return err
		case <-openCalls:
		}
	}
	if err := r.block.waitForDownloadStart(ctx); err != nil {
		releaseGate()
		return err
	}
	releaseGate()
	var resultErr error
	for range operations {
		select {
		case <-ctx.Done():
			return errors.Join(resultErr, fmt.Errorf("wait for concurrent cache reads: %w", ctx.Err()))
		case err := <-results:
			resultErr = errors.Join(resultErr, err)
		}
	}
	return resultErr
}

func (r *soakRunner) expectBackendDownloadDelta(
	baseline blockIOCounts,
	want uint64,
	operation string,
) error {
	current := r.block.counts()
	delta := current.downloads - baseline.downloads
	if delta != want {
		return fmt.Errorf(
			"%w: %s used %d backend downloads, expected %d",
			errAuditInvariant,
			operation,
			delta,
			want,
		)
	}
	return nil
}

func (r *soakRunner) expectCacheReadSafety(
	ctx context.Context,
	baselineState cacheBusinessState,
	baselineBackend blockIOCounts,
) error {
	currentState, err := r.readCacheBusinessState(ctx)
	if err != nil {
		return err
	}
	if currentState != baselineState {
		return fmt.Errorf(
			"%w: cache read changed business state from %+v to %+v",
			errAuditInvariant,
			baselineState,
			currentState,
		)
	}
	currentBackend := r.block.counts()
	if currentBackend.deleteCalls != baselineBackend.deleteCalls ||
		currentBackend.deleteRefs != baselineBackend.deleteRefs {
		return fmt.Errorf(
			"%w: cache read invoked backend deletion",
			errAuditInvariant,
		)
	}
	return nil
}

func (r *soakRunner) readCacheBusinessState(ctx context.Context) (cacheBusinessState, error) {
	queries := []struct {
		target *int64
		query  string
	}{
		{query: "SELECT COUNT(*) FROM tg_file_tab"},
		{query: "SELECT COUNT(*) FROM tg_file_part_tab"},
		{query: "SELECT COUNT(*) FROM tg_file_mapping_tab"},
		{query: "SELECT COUNT(*) FROM tg_file_part_delete_state_tab"},
		{query: "SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state <> 'live'"},
		{query: "SELECT COALESCE(SUM(attempt_count), 0) FROM tg_file_part_delete_state_tab"},
	}
	state := cacheBusinessState{}
	queries[0].target = &state.files
	queries[1].target = &state.parts
	queries[2].target = &state.mappings
	queries[3].target = &state.deleteStates
	queries[4].target = &state.nonLiveStates
	queries[5].target = &state.deleteAttempts
	for _, item := range queries {
		value, err := queryInt64(ctx, r.database, item.query)
		if err != nil {
			return cacheBusinessState{}, err
		}
		*item.target = value
	}
	return state, nil
}

func (r *soakRunner) cleanupCachePreflight(
	ctx context.Context,
	link string,
	keys ...string,
) error {
	var cleanupErr error
	if link != "" {
		if err := r.manager.RemoveFileLink(ctx, link); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove cache preflight direct link: %w", err))
		} else {
			r.untrackLink(link)
		}
	}
	for _, key := range keys {
		result, err := r.doS3(ctx, http.MethodDelete, key, "", nil, nil)
		if err == nil {
			err = expectStatus(result, http.StatusNoContent)
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete cache preflight key %s: %w", key, err))
			continue
		}
		r.untrackKey(key)
	}
	return cleanupErr
}

func (r *soakRunner) cleanupCachePreflightWithTimeout(
	ctx context.Context,
	link string,
	keys ...string,
) error {
	cleanupContext, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelCleanup()
	return r.cleanupCachePreflight(cleanupContext, link, keys...)
}

func findCacheEntryBySize(cacheDir string, size int64) (string, error) {
	matches := make([]string, 0, 1)
	err := filepath.WalkDir(cacheDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("visit managed L2 cache entry: %w", walkErr)
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".cache" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect managed L2 cache entry: %w", err)
		}
		if info.Size() == size {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("find managed L2 cache entry: %w", err)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf(
			"%w: found %d L2 entries with size %d",
			errAuditInvariant,
			len(matches),
			size,
		)
	}
	return matches[0], nil
}
