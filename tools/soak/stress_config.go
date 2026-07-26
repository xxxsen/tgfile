package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStressStepDuration     = time.Minute
	defaultStressRecoveryDuration = 30 * time.Second
	defaultStressMaxErrorRate     = 0.01
	defaultStressMaxP99           = 5 * time.Second
	defaultStressOperationTimeout = 15 * time.Second
	defaultStressMutationInterval = 1000
	defaultStressWorkers          = "1,4,8,16,32"
	stressProfileMixed            = "mixed"
	stressProfileS3               = "s3"
	stressProfileWebDAV           = "webdav"
	stressProfileCross            = "cross"
)

var (
	errInvalidStressSteps            = errors.New("TGFILE_STRESS_STEPS must be strictly increasing integers")
	errInvalidStressStepDuration     = errors.New("TGFILE_STRESS_STEP_DURATION must be a positive duration")
	errInvalidStressRecoveryDuration = errors.New("TGFILE_STRESS_RECOVERY_DURATION must be a positive duration")
	errInvalidStressProfile          = errors.New("TGFILE_STRESS_PROFILE must be mixed, s3, webdav, or cross")
	errInvalidStressMaxErrorRate     = errors.New("TGFILE_STRESS_MAX_ERROR_RATE must be between 0 and 1")
	errInvalidStressMaxP99           = errors.New("TGFILE_STRESS_MAX_P99 must be a positive duration")
	errInvalidStressOperationTimeout = errors.New("TGFILE_STRESS_OPERATION_TIMEOUT must be a positive duration")
	errInvalidStressMutationInterval = errors.New("TGFILE_STRESS_MUTATION_INTERVAL must be a positive integer")
	errInvalidStressSeed             = errors.New("TGFILE_STRESS_SEED must be an integer")
	errInvalidStressClientDelay      = errors.New("TGFILE_STRESS_CLIENT_DELAY must be a non-negative duration")
	errInvalidStressBackendDelay     = errors.New("TGFILE_STRESS_BACKEND_DELAY must be a non-negative duration")
)

type stressConfig struct {
	steps            []int
	stepDuration     time.Duration
	recoveryDuration time.Duration
	profile          string
	maxErrorRate     float64
	maxP99           time.Duration
	operationTimeout time.Duration
	mutationInterval uint64
	seed             uint64
	clientDelay      time.Duration
	backendDelay     time.Duration
}

func loadStressConfig() (stressConfig, error) {
	return loadStressConfigFrom(os.Getenv, uint64(time.Now().UnixNano()))
}

func loadStressConfigFrom(
	lookup func(string) string,
	defaultSeed uint64,
) (stressConfig, error) {
	config, err := defaultStressConfig(defaultSeed)
	if err != nil {
		return stressConfig{}, err
	}
	if config.steps, err = stressSteps(lookup("TGFILE_STRESS_STEPS")); err != nil {
		return stressConfig{}, err
	}
	if err := loadStressDurations(lookup, &config); err != nil {
		return stressConfig{}, err
	}
	if err := loadStressThresholds(lookup, &config); err != nil {
		return stressConfig{}, err
	}
	if err := loadStressMutationInterval(lookup, &config); err != nil {
		return stressConfig{}, err
	}
	if err := loadStressIdentity(lookup, &config); err != nil {
		return stressConfig{}, err
	}
	return config, nil
}

func defaultStressConfig(seed uint64) (stressConfig, error) {
	steps, err := stressSteps(defaultStressWorkers)
	if err != nil {
		return stressConfig{}, err
	}
	return stressConfig{
		steps:            steps,
		stepDuration:     defaultStressStepDuration,
		recoveryDuration: defaultStressRecoveryDuration,
		profile:          stressProfileMixed,
		maxErrorRate:     defaultStressMaxErrorRate,
		maxP99:           defaultStressMaxP99,
		operationTimeout: defaultStressOperationTimeout,
		mutationInterval: defaultStressMutationInterval,
		seed:             seed,
	}, nil
}

func loadStressDurations(lookup func(string) string, config *stressConfig) error {
	var err error
	config.stepDuration, err = positiveDuration(
		lookup("TGFILE_STRESS_STEP_DURATION"),
		config.stepDuration,
		errInvalidStressStepDuration,
	)
	if err != nil {
		return err
	}
	config.recoveryDuration, err = positiveDuration(
		lookup("TGFILE_STRESS_RECOVERY_DURATION"),
		config.recoveryDuration,
		errInvalidStressRecoveryDuration,
	)
	if err != nil {
		return err
	}
	config.maxP99, err = positiveDuration(
		lookup("TGFILE_STRESS_MAX_P99"),
		config.maxP99,
		errInvalidStressMaxP99,
	)
	if err != nil {
		return err
	}
	config.operationTimeout, err = positiveDuration(
		lookup("TGFILE_STRESS_OPERATION_TIMEOUT"),
		config.operationTimeout,
		errInvalidStressOperationTimeout,
	)
	return err
}

func loadStressThresholds(lookup func(string) string, config *stressConfig) error {
	if raw := lookup("TGFILE_STRESS_PROFILE"); raw != "" {
		config.profile = strings.ToLower(strings.TrimSpace(raw))
	}
	if !validStressProfile(config.profile) {
		return fmt.Errorf("%w: %q", errInvalidStressProfile, config.profile)
	}
	rawRate := lookup("TGFILE_STRESS_MAX_ERROR_RATE")
	if rawRate == "" {
		return nil
	}
	rate, err := strconv.ParseFloat(rawRate, 64)
	if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
		return fmt.Errorf("%w: %q", errInvalidStressMaxErrorRate, rawRate)
	}
	config.maxErrorRate = rate
	return nil
}

func loadStressIdentity(lookup func(string) string, config *stressConfig) error {
	if raw := lookup("TGFILE_STRESS_SEED"); raw != "" {
		seed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: %q", errInvalidStressSeed, raw)
		}
		config.seed = seed
	}
	var err error
	config.clientDelay, err = nonNegativeDuration(
		lookup("TGFILE_STRESS_CLIENT_DELAY"),
		errInvalidStressClientDelay,
	)
	if err != nil {
		return err
	}
	config.backendDelay, err = nonNegativeDuration(
		lookup("TGFILE_STRESS_BACKEND_DELAY"),
		errInvalidStressBackendDelay,
	)
	return err
}

func loadStressMutationInterval(lookup func(string) string, config *stressConfig) error {
	raw := lookup("TGFILE_STRESS_MUTATION_INTERVAL")
	if raw == "" {
		return nil
	}
	interval, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || interval == 0 {
		return fmt.Errorf("%w: %q", errInvalidStressMutationInterval, raw)
	}
	config.mutationInterval = interval
	return nil
}

func stressSteps(raw string) ([]int, error) {
	if raw == "" {
		raw = defaultStressWorkers
	}
	parts := strings.Split(raw, ",")
	steps := make([]int, 0, len(parts))
	previous := 0
	for _, part := range parts {
		workers, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || workers <= previous {
			return nil, fmt.Errorf("%w: %q", errInvalidStressSteps, raw)
		}
		steps = append(steps, workers)
		previous = workers
	}
	return steps, nil
}

func positiveDuration(raw string, fallback time.Duration, invalid error) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%w: %q", invalid, raw)
	}
	return duration, nil
}

func nonNegativeDuration(raw string, invalid error) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	return parseNonNegativeDuration(raw, invalid)
}

func validStressProfile(profile string) bool {
	switch profile {
	case stressProfileMixed, stressProfileS3, stressProfileWebDAV, stressProfileCross:
		return true
	default:
		return false
	}
}
