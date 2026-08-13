package pool

import (
	"context"
	"encoding/json"
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

// ProbeConcurrency is the max number of accounts probed at once.
// Matches the startup health-check semaphore in cmd/prism.
const ProbeConcurrency = 10

var (
	maxProbeAttempts = config.MaxProbeAttempts
	probeRetryDelay  = config.ProbeRetryDelay
	// recentProbeSkipWindow skips an account that ProbeExhausted already
	// hit this recently so StartProbeLoop's immediate first tick does not
	// re-blast the same batch the startup ProbeExhausted just finished.
	// Zero disables the skip (tests that must probe twice in a row).
	recentProbeSkipWindow = 30 * time.Second
)

// resolveProbePath resolves an account's probe_path into the probe endpoint
// path and whether probing is disabled:
//   - empty/"default" → "/v1/models" (legacy behavior)
//   - "-"/"disabled"/"none" → disabled (no HTTP request is sent and the
//     account state is deliberately left untouched — an exhausted account
//     stays exhausted until the operator restores it; see ProbeAccountOnce)
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
// state; the caller must NOT treat "disabled" as "healthy" (an exhausted
// account stays exhausted until the operator restores it).
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

	sem := make(chan struct{}, ProbeConcurrency)
	var wg sync.WaitGroup
	for _, acc := range exhausted {
		if acc.recentlyProbed(recentProbeSkipWindow) {
			continue
		}
		acc.markProbed()
		sem <- struct{}{}
		wg.Add(1)
		go func(acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in probe", "account", acc.Name(), "panic", r, "stack", string(debug.Stack()))
				}
			}()
			probeExhaustedAccount(pool, acc)
		}(acc)
	}
	wg.Wait()
}

func probeExhaustedAccount(pool *Pool, acc *Account) {
	for attempt := 1; attempt <= maxProbeAttempts; attempt++ {
		stop := func() bool {
			statusCode, respBody, skipped, err := ProbeAccountOnce(acc)
			if skipped {
				// probe_path disabled: no HTTP request is sent and the
				// account state is deliberately NOT touched. An exhausted
				// account must not be optimistically revived here: the
				// exhausted flag is only set for permanent upstream
				// rejection (401/402 or a recognized structured
				// credential/quota error), and "probing disabled" does
				// not mean "the credential recovered". The operator
				// restores the account (or re-enables probing) explicitly.
				slog.Info("probe disabled, keeping account state unchanged", "account", acc.Name())
				return true
			}
			if err != nil {
				// Safe fields only: the raw *url.Error embeds the probe
				// URL (query parameters, credentials) and must never
				// reach logs. Transport errors are temporary: retry.
				slog.Warn("probe request failed", "account", acc.Name(), "attempt", attempt, "max_attempts", maxProbeAttempts, "error_type", util.ClassifyConnError(err))
				return false
			}

			if statusCode == 200 {
				// /v1/models 200 does not mean chat quota recovered.
				// PermanentQuota stays exhausted until a later window
				// or a manual MarkHealthy. Permanent credential (and
				// unspecified MarkExhausted) may revive on 200.
				if acc.LastExhaustClass() == ExhaustPermanentQuota {
					slog.Info("probe 200 ignored for quota-exhausted account", "account", acc.Name(), "status", 200)
					return true
				}
				pool.MarkHealthy(acc)
				slog.Info("probe recovered account", "account", acc.Name(), "status", 200)
				return true
			}

			if statusCode == 429 {
				slog.Warn("probe account still exhausted", "account", acc.Name(), "status", 429, "attempt", attempt, "max_attempts", maxProbeAttempts)
				return true
			}

			// Same rules as proxy.ClassifyUpstreamError (pool cannot
			// import proxy: proxy already imports pool).
			class := classifyProbeError(statusCode, respBody)
			if class == ExhaustPermanentCredential || class == ExhaustPermanentQuota {
				acc.noteExhaustClass(class)
				slog.Warn("probe permanent error, stopping this round", "account", acc.Name(), "status", statusCode, "attempt", attempt, "max_attempts", maxProbeAttempts, "class", int(class), "body", string(util.RedactBodyBytesWithKeys(respBody, []string{acc.Key()})))
				return true
			}

			// Only temporary 5xx retries. Bare 403 and other non-5xx
			// temporary classes stop this round.
			if statusCode >= 500 {
				slog.Warn("probe account still exhausted", "account", acc.Name(), "status", statusCode, "attempt", attempt, "max_attempts", maxProbeAttempts, "body", string(util.RedactBodyBytesWithKeys(respBody, []string{acc.Key()})))
				return false
			}

			slog.Warn("probe account still exhausted", "account", acc.Name(), "status", statusCode, "attempt", attempt, "max_attempts", maxProbeAttempts, "body", string(util.RedactBodyBytesWithKeys(respBody, []string{acc.Key()})))
			return true
		}()

		if stop {
			return
		}
		if attempt < maxProbeAttempts {
			time.Sleep(probeRetryDelay)
		}
	}
}

// classifyProbeError mirrors proxy.ClassifyUpstreamError. The pool package
// cannot import proxy (proxy already imports pool). Keep the two in sync:
// 401 → permanent credential, 402 → permanent quota, then structured
// credential/quota bodies, else temporary. A bare 403 is temporary.
func classifyProbeError(statusCode int, body []byte) ExhaustClass {
	switch {
	case statusCode == 401:
		return ExhaustPermanentCredential
	case statusCode == 402:
		return ExhaustPermanentQuota
	case probePermanentCredentialBody(body):
		return ExhaustPermanentCredential
	case probeQuotaBody(body):
		return ExhaustPermanentQuota
	default:
		return ExhaustTemporary
	}
}

type probeErrorEnvelope struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
}

func probePermanentCredentialBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp probeErrorEnvelope
	if err := json.Unmarshal(body, &errResp); err != nil || errResp.Error.Code == "" {
		return false
	}
	code := strings.ToLower(errResp.Error.Code)
	return code == "invalid_api_key" || code == "revoked" || code == "account_deactivated"
}

func probeQuotaBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp probeErrorEnvelope
	_ = json.Unmarshal(body, &errResp)
	if errResp.Error.Type != "" && strings.ToLower(errResp.Error.Type) == "gousagelimiterror" {
		return true
	}
	if errResp.Error.Code != "" && strings.ToLower(errResp.Error.Code) == "insufficient_quota" {
		return true
	}
	return false
}
