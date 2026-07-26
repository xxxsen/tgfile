package main

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

const latencyBucketCount = 17

var latencyBounds = [...]time.Duration{
	time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

type latencyHistogram struct {
	buckets       [latencyBucketCount]atomic.Int64
	total         atomic.Int64
	totalDuration atomic.Int64
	maxDuration   atomic.Int64
}

type latencySummary struct {
	P50  time.Duration `json:"p50"`
	P95  time.Duration `json:"p95"`
	P99  time.Duration `json:"p99"`
	Mean time.Duration `json:"mean"`
	Max  time.Duration `json:"max"`
}

func (h *latencyHistogram) record(elapsed time.Duration) {
	nanoseconds := max(elapsed.Nanoseconds(), 0)
	h.total.Add(1)
	h.totalDuration.Add(nanoseconds)
	updateMaxInt64(&h.maxDuration, nanoseconds)
	h.buckets[latencyBucket(elapsed)].Add(1)
}

func (h *latencyHistogram) summary() latencySummary {
	total := h.total.Load()
	if total == 0 {
		return latencySummary{}
	}
	return latencySummary{
		P50:  h.percentile(total, 0.50),
		P95:  h.percentile(total, 0.95),
		P99:  h.percentile(total, 0.99),
		Mean: time.Duration(h.totalDuration.Load() / total),
		Max:  time.Duration(h.maxDuration.Load()),
	}
}

func (h *latencyHistogram) percentile(total int64, quantile float64) time.Duration {
	target := int64(math.Ceil(float64(total) * quantile))
	var observed int64
	for index := range h.buckets {
		observed += h.buckets[index].Load()
		if observed >= target {
			if index < len(latencyBounds) {
				return latencyBounds[index]
			}
			return time.Duration(h.maxDuration.Load())
		}
	}
	return time.Duration(h.maxDuration.Load())
}

func latencyBucket(elapsed time.Duration) int {
	for index, upperBound := range latencyBounds {
		if elapsed <= upperBound {
			return index
		}
	}
	return len(latencyBounds)
}

type stressStepMetrics struct {
	workers         int
	startedAt       time.Time
	operations      atomic.Int64
	operationErrors atomic.Int64
	requestErrors   atomic.Int64
	status4xx       atomic.Int64
	status5xx       atomic.Int64
	operationTime   latencyHistogram
	requestTime     latencyHistogram
}

type stressStepSummary struct {
	Workers             int            `json:"workers"`
	Elapsed             time.Duration  `json:"elapsed"`
	Operations          int64          `json:"operations"`
	OperationErrors     int64          `json:"operation_errors"`
	OperationErrorRate  float64        `json:"operation_error_rate"`
	OperationsPerSecond float64        `json:"operations_per_second"`
	Requests            int64          `json:"requests"`
	RequestErrors       int64          `json:"request_errors"`
	Status4xx           int64          `json:"status_4xx"`
	Status5xx           int64          `json:"status_5xx"`
	RequestsPerSecond   float64        `json:"requests_per_second"`
	OperationLatency    latencySummary `json:"operation_latency"`
	RequestLatency      latencySummary `json:"request_latency"`
	SaturationReason    string         `json:"saturation_reason,omitempty"`
}

func newStressStepMetrics(workers int) *stressStepMetrics {
	return &stressStepMetrics{
		workers:   workers,
		startedAt: time.Now(),
	}
}

func (m *stressStepMetrics) recordRequest(observation requestObservation) {
	m.requestTime.record(observation.elapsed)
	if observation.err != nil {
		m.requestErrors.Add(1)
	}
	switch {
	case observation.status >= 400 && observation.status < 500:
		m.status4xx.Add(1)
	case observation.status >= 500:
		m.status5xx.Add(1)
	}
}

func (m *stressStepMetrics) recordOperation(elapsed time.Duration, err error) {
	m.operations.Add(1)
	m.operationTime.record(elapsed)
	if err != nil {
		m.operationErrors.Add(1)
	}
}

func (m *stressStepMetrics) summary(maxErrorRate float64, maxP99 time.Duration) stressStepSummary {
	elapsed := time.Since(m.startedAt)
	operations := m.operations.Load()
	operationErrors := m.operationErrors.Load()
	requests := m.requestTime.total.Load()
	summary := stressStepSummary{
		Workers:            m.workers,
		Elapsed:            elapsed.Round(time.Millisecond),
		Operations:         operations,
		OperationErrors:    operationErrors,
		OperationErrorRate: ratio(operationErrors, operations),
		Requests:           requests,
		RequestErrors:      m.requestErrors.Load(),
		Status4xx:          m.status4xx.Load(),
		Status5xx:          m.status5xx.Load(),
		OperationLatency:   m.operationTime.summary(),
		RequestLatency:     m.requestTime.summary(),
	}
	if elapsed > 0 {
		summary.OperationsPerSecond = float64(operations) / elapsed.Seconds()
		summary.RequestsPerSecond = float64(requests) / elapsed.Seconds()
	}
	summary.SaturationReason = saturationReason(summary, maxErrorRate, maxP99)
	return summary
}

func saturationReason(
	summary stressStepSummary,
	maxErrorRate float64,
	maxP99 time.Duration,
) string {
	if summary.Operations == 0 {
		return "no completed operations"
	}
	if summary.OperationErrorRate > maxErrorRate {
		return fmt.Sprintf(
			"operation error rate %.4f exceeded %.4f",
			summary.OperationErrorRate,
			maxErrorRate,
		)
	}
	if summary.RequestLatency.P99 > maxP99 {
		return fmt.Sprintf(
			"request p99 %s exceeded %s",
			summary.RequestLatency.P99,
			maxP99,
		)
	}
	return ""
}

func ratio(numerator, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
