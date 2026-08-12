package util

import (
	"expvar"
	"sync"
	"time"
)

// Metrics collected by the proxy.
var (
	MetricsRequestsTotal     = expvar.NewInt("requests_total")
	MetricsErrorsTotal       = expvar.NewInt("errors_total")
	MetricsRateLimitedTotal  = expvar.NewInt("rate_limited_total")
	MetricsUpstreamRetries   = expvar.NewInt("upstream_retries")
	MetricsAccountsHealthy   expvar.Int
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

// Usage persistence counters. Accounting model (see internal/usage/writer.go
// for the full protocol):
//   - usage_events_written counts EVENTS persisted (+1 per event, after the
//     batch commit).
//   - usage_events_dropped counts EVENTS that will never be persisted (+1
//     per event): queue full, recorder already closed, flush panic batch,
//     failed batch, per-event insert failure, close-without-started drain.
//     Identity: written + dropped == total Record calls (no event counted
//     twice), except the documented Close-timeout case where the worker is
//     stuck forever and the buffer is deliberately not counted.
//   - usage_write_errors counts failure INCIDENTS (+1 per open/migrate/
//     cleanup failure, flush panic, failed batch, per-event insert failure).
//     It is not an event counter: one failed batch of N events is one
//     incident, with the N events accounted by dropped.
//   - usage_recorder_status is the lifecycle state ("unknown" / "disabled" /
//     "started" / "stopped"): management observability for whether usage
//     persistence is live at all (audit round 6, item 2).
var (
	MetricsUsageEventsWritten  = expvar.NewInt("usage_events_written")
	MetricsUsageEventsDropped  = expvar.NewInt("usage_events_dropped")
	MetricsUsageWriteErrors    = expvar.NewInt("usage_write_errors")
	MetricsUsageRecorderStatus = expvar.NewString("usage_recorder_status")
)

func init() {
	MetricsUsageRecorderStatus.Set("unknown")
}

// RecordUsageRecorderStatus updates the usage_recorder_status expvar.
func RecordUsageRecorderStatus(status string) {
	MetricsUsageRecorderStatus.Set(status)
}

// RecordUsageEventsWritten records one usage event persisted to the store.
func RecordUsageEventsWritten() {
	MetricsUsageEventsWritten.Add(1)
}

// RecordUsageEventsDropped records one usage event that will never be
// persisted (queue full, recorder already closed, or a write-layer failure
// that lost the event). Event-granular: one call per lost event.
func RecordUsageEventsDropped() {
	MetricsUsageEventsDropped.Add(1)
}

// RecordUsageWriteErrors records one usage persistence failure INCIDENT
// (open/migrate/cleanup failure, flush panic, failed batch, per-event insert
// failure). Incident-granular, not event-granular: a batch of N lost events
// is one incident (the N events are counted by RecordUsageEventsDropped).
func RecordUsageWriteErrors() {
	MetricsUsageWriteErrors.Add(1)
}
