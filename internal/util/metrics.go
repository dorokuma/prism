package util

import (
	"expvar"
	"sync"
	"time"
)

// Metrics collected by the proxy.
var (
	MetricsRequestsTotal    = expvar.NewInt("requests_total")
	MetricsErrorsTotal      = expvar.NewInt("errors_total")
	MetricsRateLimitedTotal = expvar.NewInt("rate_limited_total")
	MetricsUpstreamRetries  = expvar.NewInt("upstream_retries")
	MetricsAccountsHealthy  expvar.Int
	MetricsAccountsExhausted expvar.Int

	metricsRequestDurationMu sync.Mutex
	metricsRequestDuration   time.Duration
	metricsRequestCount      int64
)

func init() {
	expvar.Publish("request_duration_avg_ms", expvar.Func(func() any {
		metricsRequestDurationMu.Lock()
		defer metricsRequestDurationMu.Unlock()
		if metricsRequestCount == 0 {
			return 0
		}
		return float64(metricsRequestDuration.Microseconds()) / float64(metricsRequestCount) / 1000.0
	}))
}

// RecordRequest records a single request with its duration.
func RecordRequest(duration time.Duration) {
	MetricsRequestsTotal.Add(1)
	metricsRequestDurationMu.Lock()
	metricsRequestDuration += duration
	metricsRequestCount++
	metricsRequestDurationMu.Unlock()
}

// RecordError records a single error.
func RecordError() {
	MetricsErrorsTotal.Add(1)
}

// RecordRateLimited records a rate-limited request.
func RecordRateLimited() {
	MetricsRateLimitedTotal.Add(1)
}

// RecordUpstreamRetry records an upstream retry.
func RecordUpstreamRetry() {
	MetricsUpstreamRetries.Add(1)
}

// UpdatePoolMetrics updates the pool health metrics.
func UpdatePoolMetrics(healthy, exhausted int) {
	MetricsAccountsHealthy.Set(int64(healthy))
	MetricsAccountsExhausted.Set(int64(exhausted))
}
