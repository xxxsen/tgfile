package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

func (r *soakRunner) runStressCycle(
	ctx context.Context,
	profile string,
	workerID int,
	cycleID uint64,
) error {
	selectedProfile, err := selectedStressProfile(profile, r.seed, cycleID)
	if err != nil {
		return err
	}
	switch selectedProfile {
	case stressProfileS3:
		return r.runStressS3Lifecycle(ctx, workerID, cycleID)
	case stressProfileWebDAV:
		return r.runStressWebDAVLifecycle(ctx, workerID, cycleID)
	case stressProfileCross:
		return r.runStressCrossLifecycle(ctx, workerID, cycleID)
	default:
		return fmt.Errorf("%w: %q", errInvalidStressProfile, selectedProfile)
	}
}

func selectedStressProfile(profile string, seed, cycleID uint64) (string, error) {
	if profile != stressProfileMixed {
		if !validStressProfile(profile) {
			return "", fmt.Errorf("%w: %q", errInvalidStressProfile, profile)
		}
		return profile, nil
	}
	profiles := [...]string{stressProfileS3, stressProfileWebDAV, stressProfileCross}
	return profiles[(cycleID+seed)%uint64(len(profiles))], nil
}

func (r *soakRunner) runStressS3Read(
	ctx context.Context,
	key string,
	content []byte,
) error {
	result, err := r.expectS3Status(ctx, http.MethodHead, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if result.header.Get("Content-Length") != strconv.Itoa(len(content)) {
		return fmt.Errorf("%w: stress S3 fixture HEAD length", errResponseMismatch)
	}
	result, err = r.stressS3Get(ctx, key)
	if err != nil {
		return err
	}
	return expectBody(result, content)
}

func (r *soakRunner) runStressWebDAVRead(
	ctx context.Context,
	key string,
	content []byte,
) error {
	headers := make(http.Header)
	headers.Set("Depth", "0")
	_, err := r.expectWebDAVStatus(
		ctx,
		"PROPFIND",
		key,
		nil,
		headers,
		http.StatusMultiStatus,
	)
	if err != nil {
		return err
	}
	result, err := r.stressWebDAVGet(ctx, key)
	if err != nil {
		return err
	}
	return expectBody(result, content)
}

func (r *soakRunner) runStressCrossRead(
	ctx context.Context,
	fixtures stressFixtures,
	cycleID uint64,
) error {
	if cycleID%2 == 0 {
		result, err := r.stressWebDAVGet(ctx, fixtures.s3Key)
		if err != nil {
			return err
		}
		return expectBody(result, fixtures.s3Content)
	}
	result, err := r.stressS3Get(ctx, fixtures.webDAVKey)
	if err != nil {
		return err
	}
	return expectBody(result, fixtures.webDAVContent)
}

func (r *soakRunner) runStressS3Lifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("stress-s3", workerID, cycleID)
	content := cycleContent(r.seed, cycleID)
	r.trackKey(key)
	if err := r.stressS3Put(ctx, key, content); err != nil {
		return err
	}
	result, err := r.expectS3Status(ctx, http.MethodHead, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if result.header.Get("Content-Length") != strconv.Itoa(len(content)) {
		return fmt.Errorf("%w: stress S3 HEAD length", errResponseMismatch)
	}
	result, err = r.stressS3Get(ctx, key)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	_, err = r.expectS3Status(ctx, http.MethodDelete, key, "", nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return nil
}

func (r *soakRunner) runStressWebDAVLifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("stress-dav", workerID, cycleID)
	content := cycleContent(r.seed+1, cycleID)
	r.trackKey(key)
	if err := r.stressWebDAVPut(ctx, key, content, http.StatusCreated); err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Depth", "0")
	_, err := r.expectWebDAVStatus(
		ctx,
		"PROPFIND",
		key,
		nil,
		headers,
		http.StatusMultiStatus,
	)
	if err != nil {
		return err
	}
	result, err := r.stressWebDAVGet(ctx, key)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return nil
}

func (r *soakRunner) runStressCrossLifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("stress-cross", workerID, cycleID)
	content := cycleContent(r.seed+2, cycleID)
	r.trackKey(key)
	if err := r.stressS3Put(ctx, key, content); err != nil {
		return err
	}
	result, err := r.stressWebDAVGet(ctx, key)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	updated := contentFor(r.seed+3, cycleID, max(1, len(content)/2))
	if err := r.stressWebDAVPut(ctx, key, updated, http.StatusNoContent); err != nil {
		return err
	}
	result, err = r.stressS3Get(ctx, key)
	if err != nil {
		return err
	}
	if err := expectBody(result, updated); err != nil {
		return err
	}
	_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return nil
}

func (r *soakRunner) stressS3Put(ctx context.Context, key string, content []byte) error {
	if r.clientDelay <= 0 {
		_, err := r.expectS3Status(
			ctx,
			http.MethodPut,
			key,
			"",
			content,
			nil,
			http.StatusOK,
		)
		return err
	}
	result, err := r.doS3SlowUpload(ctx, key, content)
	if err != nil {
		return err
	}
	return expectStatus(result, http.StatusOK)
}

func (r *soakRunner) stressS3Get(ctx context.Context, key string) (*httpResult, error) {
	if r.clientDelay <= 0 {
		return r.expectS3Status(ctx, http.MethodGet, key, "", nil, nil, http.StatusOK)
	}
	result, err := r.doS3SlowDownload(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(result, http.StatusOK); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *soakRunner) stressWebDAVPut(
	ctx context.Context,
	key string,
	content []byte,
	expectedStatus int,
) error {
	if r.clientDelay <= 0 {
		_, err := r.expectWebDAVStatus(
			ctx,
			http.MethodPut,
			key,
			content,
			nil,
			expectedStatus,
		)
		return err
	}
	result, err := r.doWebDAVSlowUpload(ctx, key, content)
	if err != nil {
		return err
	}
	return expectStatus(result, expectedStatus)
}

func (r *soakRunner) stressWebDAVGet(ctx context.Context, key string) (*httpResult, error) {
	if r.clientDelay <= 0 {
		return r.expectWebDAVStatus(ctx, http.MethodGet, key, nil, nil, http.StatusOK)
	}
	result, err := r.doWebDAVSlowDownload(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(result, http.StatusOK); err != nil {
		return nil, err
	}
	return result, nil
}
