package planusage

import (
	"context"
	"time"
)

const quotaFetchAttempts = 3

// FetchWithRetry fetches a quota snapshot, retrying transient fetch failures.
// Each attempt gets its own timeout so a timed-out request cannot consume the
// entire retry budget. Authentication, subscription, and HTTP status errors
// are not retried. Cancellation of the parent context always stops promptly.
func FetchWithRetry(ctx context.Context, fetcher Fetcher, acc AccountView, timeout time.Duration) (Snapshot, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	var snap Snapshot
	var err error
	for attempt := 0; attempt < quotaFetchAttempts; attempt++ {
		if ctx.Err() != nil {
			return snap, ctx.Err()
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		snap, err = fetcher.Fetch(attemptCtx, acc)
		attemptErr := attemptCtx.Err()
		cancel()
		if err == nil {
			return snap, nil
		}
		if attemptErr != nil && ctx.Err() != nil {
			return snap, ctx.Err()
		}
		if ErrorCode(err) != "fetch_failed" || attempt == quotaFetchAttempts-1 {
			return snap, err
		}

		backoff := time.Duration(250*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return snap, ctx.Err()
		case <-timer.C:
		}
	}
	return snap, err
}
