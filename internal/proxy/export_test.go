package proxy

import (
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// SetUpstreamCooldownForTest overrides the upstream temporary-failure
// cooldown (upstreamCooldown) for the duration of a test and returns a
// restore function. Tests that drive the retry loop (5xx / connection error /
// 429) use this to shrink the cooldown to milliseconds so the pool select
// does not wait ~30s for cooldown expiry. Production behavior is untouched:
// the default remains 30s.
func SetUpstreamCooldownForTest(d time.Duration) func() {
	old := upstreamCooldown
	upstreamCooldown = d
	return func() { upstreamCooldown = old }
}

// SetAccountSelectTimeoutForTest overrides config.AccountSelectTimeout for
// the duration of a test and returns a restore function. Used as a fast-fail
// safety net: with a millisecond-scale select timeout, any retry-loop select
// that unexpectedly blocks fails the test quickly instead of hanging 30s.
// Production behavior is untouched: the default remains 30s.
func SetAccountSelectTimeoutForTest(d time.Duration) func() {
	old := config.AccountSelectTimeout
	config.AccountSelectTimeout = d
	return func() { config.AccountSelectTimeout = old }
}
