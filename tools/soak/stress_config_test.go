package main

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestLoadStressConfigDefaultsAndOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		config, err := loadStressConfigFrom(func(string) string { return "" }, 42)
		if err != nil {
			t.Fatalf("load defaults: %v", err)
		}
		if !reflect.DeepEqual(config.steps, []int{1, 4, 8, 16, 32}) {
			t.Fatalf("steps = %v", config.steps)
		}
		if config.stepDuration != time.Minute ||
			config.recoveryDuration != 30*time.Second ||
			config.profile != stressProfileMixed ||
			config.maxErrorRate != 0.01 ||
			config.maxP99 != 5*time.Second ||
			config.operationTimeout != 15*time.Second ||
			config.mutationInterval != 1000 ||
			config.seed != 42 ||
			config.clientDelay != 0 ||
			config.backendDelay != 0 {
			t.Fatalf("unexpected defaults: %+v", config)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		values := map[string]string{
			"TGFILE_STRESS_STEPS":             "2, 3,9",
			"TGFILE_STRESS_STEP_DURATION":     "2s",
			"TGFILE_STRESS_RECOVERY_DURATION": "3s",
			"TGFILE_STRESS_PROFILE":           "S3",
			"TGFILE_STRESS_MAX_ERROR_RATE":    "0.2",
			"TGFILE_STRESS_MAX_P99":           "750ms",
			"TGFILE_STRESS_OPERATION_TIMEOUT": "900ms",
			"TGFILE_STRESS_MUTATION_INTERVAL": "50",
			"TGFILE_STRESS_SEED":              "99",
			"TGFILE_STRESS_CLIENT_DELAY":      "1ms",
			"TGFILE_STRESS_BACKEND_DELAY":     "2ms",
		}
		config, err := loadStressConfigFrom(mapLookup(values), 42)
		if err != nil {
			t.Fatalf("load overrides: %v", err)
		}
		if !reflect.DeepEqual(config.steps, []int{2, 3, 9}) {
			t.Fatalf("steps = %v", config.steps)
		}
		if config.stepDuration != 2*time.Second ||
			config.recoveryDuration != 3*time.Second ||
			config.profile != stressProfileS3 ||
			config.maxErrorRate != 0.2 ||
			config.maxP99 != 750*time.Millisecond ||
			config.operationTimeout != 900*time.Millisecond ||
			config.mutationInterval != 50 ||
			config.seed != 99 ||
			config.clientDelay != time.Millisecond ||
			config.backendDelay != 2*time.Millisecond {
			t.Fatalf("unexpected overrides: %+v", config)
		}
	})
}

func TestLoadStressConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		expected error
	}{
		{"empty step", "TGFILE_STRESS_STEPS", "1,,2", errInvalidStressSteps},
		{"descending steps", "TGFILE_STRESS_STEPS", "2,1", errInvalidStressSteps},
		{"zero step", "TGFILE_STRESS_STEPS", "0,1", errInvalidStressSteps},
		{"step duration", "TGFILE_STRESS_STEP_DURATION", "0s", errInvalidStressStepDuration},
		{"recovery duration", "TGFILE_STRESS_RECOVERY_DURATION", "-1s", errInvalidStressRecoveryDuration},
		{"profile", "TGFILE_STRESS_PROFILE", "all", errInvalidStressProfile},
		{"negative error rate", "TGFILE_STRESS_MAX_ERROR_RATE", "-0.1", errInvalidStressMaxErrorRate},
		{"nan error rate", "TGFILE_STRESS_MAX_ERROR_RATE", "NaN", errInvalidStressMaxErrorRate},
		{"p99", "TGFILE_STRESS_MAX_P99", "bad", errInvalidStressMaxP99},
		{"operation timeout", "TGFILE_STRESS_OPERATION_TIMEOUT", "0s", errInvalidStressOperationTimeout},
		{"mutation interval", "TGFILE_STRESS_MUTATION_INTERVAL", "0", errInvalidStressMutationInterval},
		{"seed", "TGFILE_STRESS_SEED", "bad", errInvalidStressSeed},
		{"client delay", "TGFILE_STRESS_CLIENT_DELAY", "-1ms", errInvalidStressClientDelay},
		{"backend delay", "TGFILE_STRESS_BACKEND_DELAY", "-1ms", errInvalidStressBackendDelay},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadStressConfigFrom(mapLookup(map[string]string{
				test.key: test.value,
			}), 1)
			if !errors.Is(err, test.expected) {
				t.Fatalf("error = %v, want %v", err, test.expected)
			}
		})
	}
}

func TestRequestedMode(t *testing.T) {
	for _, test := range []struct {
		arguments []string
		expected  string
		wantError bool
	}{
		{nil, testModeSoak, false},
		{[]string{testModeSoak}, testModeSoak, false},
		{[]string{testModeStress}, testModeStress, false},
		{[]string{"unknown"}, "", true},
		{[]string{testModeSoak, testModeStress}, "", true},
	} {
		mode, err := requestedMode(test.arguments)
		if (err != nil) != test.wantError {
			t.Fatalf("requestedMode(%v) error = %v", test.arguments, err)
		}
		if mode != test.expected {
			t.Fatalf("requestedMode(%v) = %q, want %q", test.arguments, mode, test.expected)
		}
	}
}

func mapLookup(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
