package pool

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

var (
	maxProbeAttempts = config.MaxProbeAttempts
	probeRetryDelay  = config.ProbeRetryDelay
)

// resolveProbePath resolves an account's probe_path into the probe endpoint
// path and whether probing is disabled:
//   - empty/"default" → "/v1/models" (legacy behavior)
//   - "-"/"disabled"/"none" → disabled (no HTTP, optimistic recovery)
//   - any other explicit value → used as the path (a leading "/" is added
//     when missing)
func resolveProbePath(acc *Account) (path string, disabled bool) {
	p := strings.TrimSpace(acc.ProbePath())
	switch p {
	case "", "default":
		return "/v1/models", false
	case "-", "disabled", "none":
		return "", true
	default:
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return p, false
	}
}

// ProbeAccountOnce performs a single GET probe for one account using its
// resolved probe_path (default /v1/models) with account headers and the
// account-level auth header applied.
// When probing is disabled (probe_path: disabled/-/none) it returns
// skipped=true without sending any HTTP request and without touching account
// state; the caller decides whether to apply optimistic recovery.
// On HTTP success/failure it returns the status code and a body snippet
// (redacted by the caller), with err non-nil only for transport-level errors.
func ProbeAccountOnce(acc *Account) (statusCode int, body []byte, skipped bool, err error) {
	path, disabled := resolveProbePath(acc)
	if disabled {
		return 0, nil, true, nil
	}
	url := util.JoinURLPath(acc.BaseURL(), path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, false, err
	}
	ApplyAccountHeaders(req.Header, acc)
	ApplyAuthHeader(req.Header, acc)

	ctx, cancel := context.WithTimeout(context.Background(), config.ProbeTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := acc.Client().Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		slog.Warn("probe read body failed", "account", acc.Name(), "error", readErr)
	}
	return resp.StatusCode, body, false, nil
}

func StartProbeLoop(pool *Pool, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	slog.Info("probe loop started", "interval", interval)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in probe loop", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ProbeExhausted(pool)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				slog.Info("probe loop stopped")
				return
			case <-ticker.C:
				ProbeExhausted(pool)
			}
		}
	}()
}

func ProbeExhausted(pool *Pool) {
	exhausted := pool.ExhaustedAccounts()
	if len(exhausted) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, acc := range exhausted {
		wg.Add(1)
		go func(acc *Account) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in probe", "account", acc.Name(), "panic", r, "stack", string(debug.Stack()))
				}
			}()

			for attempt := 1; attempt <= maxProbeAttempts; attempt++ {
				stop := func() bool {
					statusCode, respBody, skipped, err := ProbeAccountOnce(acc)
					if skipped {
						// probe_path disabled: no HTTP request is sent; the
						// exhausted account is optimistically marked healthy
						// so it recovers within one probe_interval.
						pool.MarkHealthy(acc)
						slog.Info("probe disabled, optimistic recover", "account", acc.Name())
						return true
					}
					if err != nil {
						slog.Warn("probe request failed", "account", acc.Name(), "attempt", attempt, "max_attempts", maxProbeAttempts, "error", err)
						return false
					}

					if statusCode == 200 {
						pool.MarkHealthy(acc)
						slog.Info("probe recovered account", "account", acc.Name(), "status", 200)
						return true
					}

					if statusCode == 429 {
						slog.Warn("probe account still exhausted", "account", acc.Name(), "status", 429, "attempt", attempt, "max_attempts", maxProbeAttempts)
						return true
					}

					slog.Warn("probe account still exhausted", "account", acc.Name(), "status", statusCode, "attempt", attempt, "max_attempts", maxProbeAttempts, "body", string(util.RedactBodyBytesWithKeys(respBody, []string{acc.Key()})))
					return false
				}()

				if stop {
					return
				}
				if attempt < maxProbeAttempts {
					time.Sleep(probeRetryDelay)
				}
			}
		}(acc)
	}
	wg.Wait()
}
