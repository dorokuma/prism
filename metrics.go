package main

import (
	"expvar"
	"sync"
	"time"
)

// Metrics collected by the proxy.
var (
	metricsRequestsTotal    = expvar.NewInt("requests_total")
	metricsErrorsTotal      = expvar.NewInt("errors_total")
	metricsRateLimitedTotal = expvar.NewInt("rate_limited_total")
	metricsUpstreamRetries  = expvar.NewInt("upstream_retries")
	metricsAccountsHealthy  expvar.Int
	metricsAccountsExhausted expvar.Int

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

func recordRequest(duration time.Duration) {
	metricsRequestsTotal.Add(1)
	metricsRequestDurationMu.Lock()
	metricsRequestDuration += duration
	metricsRequestCount++
	metricsRequestDurationMu.Unlock()
}

func recordError() {
	metricsErrorsTotal.Add(1)
}

func recordRateLimited() {
	metricsRateLimitedTotal.Add(1)
}

func recordUpstreamRetry() {
	metricsUpstreamRetries.Add(1)
}

func updatePoolMetrics(healthy, exhausted int) {
	metricsAccountsHealthy.Set(int64(healthy))
	metricsAccountsExhausted.Set(int64(exhausted))
}
