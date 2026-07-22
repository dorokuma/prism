package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

var (
	maxProbeAttempts = config.MaxProbeAttempts
	probeRetryDelay  = config.ProbeRetryDelay
)

func StartProbeLoop(pool *Pool, probeModel string, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	slog.Info("probe loop started", "interval", interval, "model", probeModel)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in probe loop", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ProbeExhausted(pool, probeModel)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				slog.Info("probe loop stopped")
				return
			case <-ticker.C:
				ProbeExhausted(pool, probeModel)
			}
		}
	}()
}

func ProbeExhausted(pool *Pool, probeModel string) {
	exhausted := pool.ExhaustedAccounts()
	if len(exhausted) == 0 {
		return
	}

	probeBody, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens":  1,
	})

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
					url := acc.BaseURL() + "/chat/completions"
					req, err := http.NewRequest("POST", url, bytes.NewReader(probeBody))
					if err != nil {
						slog.Warn("probe failed to create request", "account", acc.Name(), "error", err)
						return true
					}
					req.Header.Set("Authorization", "Bearer "+acc.Key())
					req.Header.Set("Content-Type", "application/json")

					ctx, cancel := context.WithTimeout(context.Background(), config.ProbeTimeout)
					defer cancel()
					req = req.WithContext(ctx)
					resp, err := acc.Client().Do(req)

					if err != nil {
						slog.Warn("probe request failed", "account", acc.Name(), "attempt", attempt, "max_attempts", maxProbeAttempts, "error", err)
						return false
					}
					defer resp.Body.Close()

					respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
					if readErr != nil {
						slog.Warn("probe read body failed", "account", acc.Name(), "error", readErr)
					}

					if resp.StatusCode == 200 {
						pool.MarkHealthy(acc)
						slog.Info("probe recovered account", "account", acc.Name(), "status", 200)
						return true
					}

					if resp.StatusCode == 429 {
						slog.Warn("probe account still exhausted", "account", acc.Name(), "status", 429, "attempt", attempt, "max_attempts", maxProbeAttempts)
						return true
					}

					slog.Warn("probe account still exhausted", "account", acc.Name(), "status", resp.StatusCode, "attempt", attempt, "max_attempts", maxProbeAttempts, "body", util.RedactBody(respBody))
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
