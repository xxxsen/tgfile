package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLatencyHistogramSummary(t *testing.T) {
	histogram := &latencyHistogram{}
	for _, elapsed := range []time.Duration{
		time.Millisecond,
		2 * time.Millisecond,
		100 * time.Millisecond,
		10 * time.Second,
	} {
		histogram.record(elapsed)
	}
	summary := histogram.summary()
	if summary.P50 != 2*time.Millisecond {
		t.Fatalf("p50 = %s", summary.P50)
	}
	if summary.P95 != 10*time.Second || summary.P99 != 10*time.Second {
		t.Fatalf("p95/p99 = %s/%s", summary.P95, summary.P99)
	}
	if summary.Mean != 2_525_750*time.Microsecond {
		t.Fatalf("mean = %s", summary.Mean)
	}
	if summary.Max != 10*time.Second {
		t.Fatalf("max = %s", summary.Max)
	}
}

func TestStressStepMetricsAndSaturation(t *testing.T) {
	metrics := newStressStepMetrics(4)
	metrics.recordRequest(requestObservation{elapsed: 2 * time.Millisecond, status: 200})
	metrics.recordRequest(requestObservation{
		elapsed: 10 * time.Millisecond,
		err:     errors.New("transport"),
	})
	metrics.recordRequest(requestObservation{elapsed: 5 * time.Millisecond, status: 404})
	metrics.recordRequest(requestObservation{elapsed: 6 * time.Millisecond, status: 500})
	metrics.recordOperation(20*time.Millisecond, nil)
	metrics.recordOperation(30*time.Millisecond, errors.New("operation"))

	summary := metrics.summary(0.4, time.Second)
	if summary.Operations != 2 || summary.OperationErrors != 1 {
		t.Fatalf("operations = %d, errors = %d", summary.Operations, summary.OperationErrors)
	}
	if summary.Requests != 4 ||
		summary.RequestErrors != 1 ||
		summary.Status4xx != 1 ||
		summary.Status5xx != 1 {
		t.Fatalf("unexpected request counters: %+v", summary)
	}
	if !strings.Contains(summary.SaturationReason, "error rate") {
		t.Fatalf("saturation reason = %q", summary.SaturationReason)
	}
}

func TestSaturationReasonBoundaries(t *testing.T) {
	if reason := saturationReason(stressStepSummary{}, 0, time.Second); reason == "" {
		t.Fatal("zero operations must be rejected")
	}
	atThreshold := stressStepSummary{
		Operations:         10,
		OperationErrorRate: 0.1,
		RequestLatency: latencySummary{
			P99: time.Second,
		},
	}
	if reason := saturationReason(atThreshold, 0.1, time.Second); reason != "" {
		t.Fatalf("threshold boundary rejected: %s", reason)
	}
	overP99 := atThreshold
	overP99.RequestLatency.P99 = time.Second + time.Millisecond
	if reason := saturationReason(overP99, 0.1, time.Second); !strings.Contains(reason, "p99") {
		t.Fatalf("p99 reason = %q", reason)
	}
}
