package util_test

import (
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/util"
)

func TestRecordMetrics(t *testing.T) {
	// Reset metrics
	util.MetricsRequestsTotal.Set(0)
	util.MetricsErrorsTotal.Set(0)
	util.MetricsRateLimitedTotal.Set(0)
	util.MetricsUpstreamRetries.Set(0)

	util.RecordRequest(100 * time.Millisecond)
	util.RecordError()
	util.RecordRateLimited()
	util.RecordUpstreamRetry()

	if util.MetricsRequestsTotal.Value() != 1 {
		t.Errorf("requests_total = %d, want 1", util.MetricsRequestsTotal.Value())
	}
	if util.MetricsErrorsTotal.Value() != 1 {
		t.Errorf("errors_total = %d, want 1", util.MetricsErrorsTotal.Value())
	}
	if util.MetricsRateLimitedTotal.Value() != 1 {
		t.Errorf("rate_limited_total = %d, want 1", util.MetricsRateLimitedTotal.Value())
	}
	if util.MetricsUpstreamRetries.Value() != 1 {
		t.Errorf("upstream_retries = %d, want 1", util.MetricsUpstreamRetries.Value())
	}
}
