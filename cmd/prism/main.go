package main

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/adminauth"
	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/oauth"
	"github.com/dorokuma/prism/internal/oauth/xai"
	"github.com/dorokuma/prism/internal/planusage"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/proxy"
	"github.com/dorokuma/prism/internal/ratelimit"
	"github.com/dorokuma/prism/internal/usage"
	"github.com/dorokuma/prism/internal/util"
)

func init() {
	config.LogLevelHook = middleware.SetLogLevel
}

// ---------------------------------------------------------------------------
// Usage recording wiring. Degradation constraint (highest priority): every
// usage-related failure — store open/migrate, disk full, SQLITE_BUSY, full
// queue, pricing problems — is logged and counted inside internal/usage and
// NEVER returned to the request path, never panics, never blocks request
// finalization. The recorder is a safe no-op in all failure modes, so HTTP
// serving and /v1 forwarding are unaffected.
// ---------------------------------------------------------------------------

// usageRecorderAdapter maps middleware.UsageEvent onto usage.Event and
// forwards it to the usage.Recorder. It is injected into middleware by main;
// middleware itself never imports internal/usage.
//
// Pricing is deliberately NOT done in Record: cost is computed exactly once,
// on the synchronous request finalization path, by Price (registered with
// middleware.SetUsagePricer) BEFORE the request.complete line is written.
// The resulting amount is carried on the event and the async write path
// never re-prices, so the audit log amount and the database amount are the
// same value by construction (single pricing point: usage.ComputeCost).
type usageRecorderAdapter struct {
	holder *config.ConfigHolder
	rec    *usage.Recorder
}

func (a *usageRecorderAdapter) Record(e middleware.UsageEvent) {
	a.rec.Record(usage.Event{
		Ts:               time.Now(),
		RequestID:        e.RequestID,
		Path:             e.Path,
		Model:            e.Model,
		Provider:         e.Provider,
		Account:          e.Account,
		KeyID:            e.KeyID,
		Stream:           e.Stream,
		Success:          e.Success,
		Status:           e.Status,
		ErrorType:        e.ErrorType,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		TotalTokens:      e.TotalTokens,
		CachedTokens:     e.CachedTokens,
		ReasoningTokens:  e.ReasoningTokens,
		CacheWriteTokens: e.CacheWriteTokens,
		DurationMS:       e.DurationMS,
		Source:           e.Source,
		Cost:             e.Cost,
		CostStatus:       e.CostStatus,
	})
}

// Price computes the USD cost for one audit record from the current config
// (via the ConfigHolder atomic pointer, so a SIGHUP hot reload applies to
// new requests). It is called synchronously by middleware.EmitAudit before
// the request.complete line is written; middleware fills RequestAudit from
// the result and forwards the same value on the usage event, so the audit
// log amount and the persisted amount are identical. A nil result means the
// model has no known price (cost persisted as NULL, status missing_price).
func (a *usageRecorderAdapter) Price(audit *middleware.RequestAudit) (*float64, string) {
	cfg := a.holder.Load()
	// Pricing prefers the ACTUAL upstream model (after model_remap
	// resolution): a virtual model name rarely carries its own price entry,
	// and billing must follow what the upstream actually served. When the
	// upstream model has no known price, fall back to the client-requested
	// model so deployments that price their virtual model names keep
	// working (compatibility fallback).
	price := priceFor(cfg, audit.Provider, audit.UpstreamModel, int64(audit.PromptTokens))
	if price == nil {
		price = priceFor(cfg, audit.Provider, audit.Model, int64(audit.PromptTokens))
	}
	return usage.ComputeCost(
		int64(audit.PromptTokens),
		int64(audit.CompletionTokens),
		int64(audit.CachedTokens),
		int64(audit.CacheWriteTokens),
		audit.UsageSource,
		price,
	)
}

// priceFor resolves the per-million-token USD price for (provider, model)
// and selects the long-context tier when contextTokens reaches its threshold.
// A nil result means the model has no known price. Legacy costs without a
// long_context tier always return the original single-tier price.
func priceFor(cfg *config.Config, provider, model string, contextTokens int64) *usage.Price {
	if cfg == nil {
		return nil
	}
	meta, ok := cfg.LookupModelMetadata(provider, model)
	if !ok || meta.Cost == nil {
		return nil
	}
	cost := meta.Cost.EffectiveCost(contextTokens)
	// A configured tier whose four rates are all zero is not an effective
	// price. Return nil so ComputeCost records missing_price rather than
	// silently treating an unpriced request as a genuine zero-dollar charge.
	if cost.Input == 0 && cost.Output == 0 && cost.CacheRead == 0 && cost.CacheWrite == 0 {
		return nil
	}
	return &usage.Price{
		Input:      cost.Input,
		Output:     cost.Output,
		CacheRead:  cost.CacheRead,
		CacheWrite: cost.CacheWrite,
	}
}

// shutdownHTTPAndDrainUsage is the ordered shutdown sequence for the HTTP
// server and the usage recorder. Order matters: srv.Shutdown must complete
// FIRST so every in-flight request has finished its deferred usage emission
// (middleware.EmitAudit → recorder.Record), and only THEN may the recorder
// be closed and its buffered events flushed. Closing the recorder first
// would silently drop the usage of requests that finish during graceful
// shutdown.
//
// Timeout budget: the 30s context is spent entirely on srv.Shutdown (the
// HTTP layer, where in-flight streams can legitimately take a while). The
// recorder drain runs afterwards under its own bounded budget
// (usage.Recorder.Close: normally milliseconds; worst case 2×5s, only when
// the store itself is stuck). The two phases are sequential, so the
// worst-case total is 30s + 10s = 40s. systemd TimeoutStopSec must be at
// least 45s so SIGKILL does not land during that window. The drain phase
// only ever starts once the HTTP layer is quiescent.
func shutdownHTTPAndDrainUsage(srv *http.Server, usageRec *usage.Recorder) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	// Drain buffered usage events. nil-safe no-op when usage is disabled or
	// the recorder failed to start.
	usageRec.Close()
}

// startUsageRecorder builds the usage store + recorder from application
// configuration and starts it. It never returns an error: usage.Open/Migrate
// failures are logged and counted inside usage and the recorder degrades to
// a no-op (Record/Close on a nil or stopped recorder are no-ops), so HTTP
// startup is never blocked. It returns the recorder (nil when usage is
// disabled or failed to start) and the store (nil when disabled) for the
// /admin/usage/summary handler.
func startUsageRecorder(cfg *config.Config) (*usage.Recorder, usage.Store) {
	if cfg == nil || !cfg.Usage.Enabled {
		return nil, nil
	}
	store := usage.NewSQLiteStore(cfg.Usage.DBPath)
	rec := usage.NewRecorder(usage.Config{
		Enabled:       cfg.Usage.Enabled,
		DBPath:        cfg.Usage.DBPath,
		RetentionDays: cfg.Usage.RetentionDays,
		ChannelSize:   cfg.Usage.ChannelSize,
		BatchSize:     cfg.Usage.BatchSize,
		BatchFlushMS:  cfg.Usage.BatchFlushMS,
	}, store)
	rec.Start()
	return rec, store
}

// envTokenMaxBytes is the maximum accepted length (in bytes) of the
// METRICS_TOKEN and PRISM_ADMIN_TOKEN environment variables. Both tokens are
// compared with the same fixed-length constant-time pad as business API keys
// (middleware.authPadLen = 256): a longer token can never authenticate, so
// the deployment would fail every request at runtime. Startup rejects it
// instead (fail fast, see validateEnvTokenLengths).
const envTokenMaxBytes = 256

// validateEnvTokenLengths fails fast at startup when METRICS_TOKEN or
// PRISM_ADMIN_TOKEN exceeds envTokenMaxBytes bytes. The error names only the
// variable and the length — the token value itself must never be echoed
// (it is a credential).
func validateEnvTokenLengths() error {
	for _, name := range []string{"METRICS_TOKEN", "PRISM_ADMIN_TOKEN"} {
		if v := os.Getenv(name); len(v) > envTokenMaxBytes {
			return fmt.Errorf("%s exceeds the maximum supported %d bytes (constant-time auth compares a fixed %d-byte pad)", name, envTokenMaxBytes, envTokenMaxBytes)
		}
	}
	return nil
}

// newHTTPHandler builds the root HTTP handler: /metrics, the
// /admin/usage/summary admin endpoint, the global api_keys auth gate and the
// proxy dispatch. Extracted from main so tests can exercise the wiring
// invariant that usage degradation never breaks /v1 forwarding.
func newHTTPHandler(holder *config.ConfigHolder, proxyHandler http.Handler, rl *ratelimit.RateLimiter, trustedProxies []*net.IPNet, summaryHandler http.Handler, quotaHandler http.Handler) http.Handler {
	// Bucket key is resolved per request from the live holder so api_keys
	// hot-reloads take effect. Authenticated keys (len(api_keys)>0 and a
	// matching Bearer) share a per-key-name bucket; auth failure and
	// key-less configs stay on GetClientIP.
	bucketKey := func(r *http.Request) string {
		cfg := holder.Load()
		if cfg != nil && len(cfg.APIKeys) > 0 {
			if name, ok := middleware.Authenticate(r, cfg.APIKeys); ok {
				return "key:" + name
			}
		}
		return ratelimit.GetClientIP(r, trustedProxies)
	}
	return middleware.RequestIDMiddleware(ratelimit.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			token := os.Getenv("METRICS_TOKEN")
			// Fail-closed admin auth: when METRICS_TOKEN is configured, EVERY
			// request must present it as a Bearer token — loopback does not
			// bypass (a same-machine reverse proxy also presents a loopback
			// RemoteAddr and can add or strip forwarding headers, so
			// X-Forwarded-For/X-Real-IP presence is not a trust boundary).
			// Only when NO token is configured is a direct local request
			// (loopback RemoteAddr without forwarding headers) allowed.
			allowed := (token != "" && middleware.CheckAuth(r, token)) ||
				(token == "" && middleware.IsLocalhost(r) && !middleware.HasForwardedHeaders(r))
			if !allowed {
				util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			expvar.Handler().ServeHTTP(w, r)
			return
		}
		// /admin/usage/summary carries its own PRISM_ADMIN_TOKEN Bearer auth
		// (fail-closed; token-free direct loopback only when the token is
		// unset) and is deliberately mounted BEFORE the global api_keys gate,
		// mirroring the /metrics precedent.
		if r.URL.Path == "/admin/usage/summary" {
			summaryHandler.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/admin/quota" {
			if !adminauth.Authorized(r) {
				util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
				})
				return
			}
			if quotaHandler == nil {
				util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": map[string]any{"message": "quota unavailable", "code": "quota_unavailable"},
				})
				return
			}
			quotaHandler.ServeHTTP(w, r)
			return
		}
		curCfg := holder.Load()
		if r.URL.Path != "/health" && r.URL.Path != "/ready" {
			keyName, ok := middleware.Authenticate(r, curCfg.APIKeys)
			if !ok {
				slog.Warn("auth_failed", "req", util.RequestIDFromCtx(r.Context()), "path", r.URL.Path, "remote_addr", r.RemoteAddr)
				util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
				})
				return
			}
			// Auth disabled (no api_keys configured): Authenticate returns the
			// fixed "anonymous" label; replace it with the configured
			// usage.default_key_id so the recorded key_id follows config
			// (default "anonymous"). With keys configured the matched key
			// NAME wins and is never overridden.
			authed := len(curCfg.APIKeys) > 0
			if !authed {
				keyName = curCfg.Usage.DefaultKeyID
			}
			// The authentication STATUS is recorded next to the key name: the
			// MCP tool-cache identity must know whether a real credential was
			// presented (auth disabled → the fixed read-only unauthenticated
			// bucket, so local clients cannot pollute each other) while the
			// usage key_id still follows config.
			r = r.WithContext(middleware.WithAuthenticated(r.Context(), authed))
			r = r.WithContext(middleware.WithAPIKey(r.Context(), keyName))
		}
		// Timeout decisions are delegated to proxyChatWithBody which applies
		// per-request timeouts based on the actual stream setting (parsed
		// from the JSON body, not headers).
		proxyHandler.ServeHTTP(w, r)
	}), rl, trustedProxies, bucketKey))
}

// startInitialAccountProbes launches the startup connectivity probes without
// waiting. HTTP listen must not block on these: /health is 200 as soon as the
// process is listening; /ready stays fail-closed until at least one account is
// healthy. The semaphore lives inside each goroutine so launching N>10
// accounts cannot stall Listen.
func startInitialAccountProbes(p *pool.Pool) {
	slog.Info("starting initial health check for all accounts")
	sem := make(chan struct{}, 10)
	for _, acc := range p.AllAccounts() {
		go func(a *pool.Account) {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in startup health check", "account", a.Name(), "panic", r, "stack", string(debug.Stack()))
				}
			}()

			statusCode, bodyBytes, skipped, err := pool.ProbeAccountOnce(a)
			if skipped {
				// probe_path: disabled → 不发探活请求、不改状态，只记 Info
				slog.Info("startup check skipped, probe disabled", "account", a.Name())
				return
			}
			if err != nil {
				// Safe fields only: the raw *url.Error embeds the probe URL
				// (query parameters, credentials) and must never reach logs.
				slog.Warn("startup check request failed", "account", a.Name(), "error_type", util.ClassifyConnError(err))
				a.SetCooldown(5 * time.Minute)
				return
			}
			if statusCode == 200 {
				slog.Info("startup check OK", "account", a.Name(), "status", 200)
				return
			}
			// Shared classification (proxy.ClassifyUpstreamError): only
			// 401/402 or a recognized structured permanent error body
			// (credential or quota) marks the account exhausted. A bare
			// 403 is NOT permanent — it cools down like any other
			// temporary failure instead of waiting for the probe loop to
			// recover the account.
			switch proxy.ClassifyUpstreamError(statusCode, bodyBytes) {
			case proxy.UpstreamErrorPermanentCredential:
				slog.Error("startup check permanent error, marking exhausted", "account", a.Name(), "status", statusCode, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{a.Key()})))
				a.MarkExhaustedWithClass(pool.ExhaustPermanentCredential)
			case proxy.UpstreamErrorPermanentQuota:
				if a.PublicService() {
					slog.Info("startup check public_service quota error ignored", "account", a.Name(), "status", statusCode)
					if statusCode == 429 {
						slog.Warn("startup check temporary quota error, cooling down", "account", a.Name(), "status", 429, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{a.Key()})))
						a.SetCooldown(2 * time.Minute)
					}
					return
				}
				slog.Error("startup check permanent error, marking exhausted", "account", a.Name(), "status", statusCode, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{a.Key()})))
				a.MarkExhaustedWithClass(pool.ExhaustPermanentQuota)
			default:
				if statusCode == 429 {
					slog.Warn("startup check temporary quota error, cooling down", "account", a.Name(), "status", 429, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{a.Key()})))
					a.SetCooldown(2 * time.Minute)
				} else {
					slog.Warn("startup check temporary error, cooling down", "account", a.Name(), "status", statusCode, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{a.Key()})))
					a.SetCooldown(5 * time.Minute)
				}
			}
		}(acc)
	}
}

func main() {
	// 检查是否运行 setup
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		if err := runSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "setup 失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// prism usage: 直接只读查询 usage 数据库，不依赖服务运行。分发方式与
	// setup 一致（os.Args[1] 硬编码分支），在加载配置之前处理。
	if len(os.Args) > 1 && os.Args[1] == "usage" {
		if err := runUsage(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "usage 失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "quota" {
		if err := runQuota(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "quota 失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := runAuth(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "auth 失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Fail fast on over-long admin/metrics tokens: a token longer than the
	// constant-time comparison pad (256 bytes) can never authenticate (the
	// comparison rejects it outright), so every request would fail at
	// runtime. The error names the variable and the byte length but NEVER
	// the token value.
	if err := validateEnvTokenLengths(); err != nil {
		slog.Error("invalid environment token", "error", err)
		os.Exit(1)
	}

	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	holder := config.NewConfigHolder(cfg)

	middleware.InitLogger(cfg.LogLevel)

	util.DebugMode.Store(cfg.Debug)
	mcp.LoadMCPTools(cfg.MCPToolsJSON)
	p := pool.NewPoolWithTotalCap(cfg.Accounts, config.ResolveAccountTotalCap(cfg))
	oauthCancel := attachOAuth(p, cfg)
	wire, _ := config.ParseWireAPIMode(cfg.WireAPI)
	slog.Info("prism starting", "accounts", len(cfg.Accounts), "wire_api", string(wire), "listen", cfg.Listen, "debug", util.DebugMode.Load(), "auth", len(cfg.APIKeys) > 0, "auth_keys", len(cfg.APIKeys), "tls", cfg.TLSCertFile != "")

	// 初始化模型缓存
	cacheDir := "/var/lib/prism/model_cache"
	mc, err := cache.New(cacheDir, p, cfg)
	if err != nil {
		slog.Error("init model cache", "error", err)
		os.Exit(1)
	}
	mc.LoadFromDisk()

	// 后台异步填充缺失缓存，就位后同步 tools
	mc.FetchAllAsync(func() {
		mc.SyncTools(holder.Load())
	})

	// 启动 24h 后台刷新（刷新后也同步 tools）
	mc.StartRefreshLoop(24*time.Hour, func() {
		// 重新读 config，因为可能被 SIGHUP 热重载过
		curCfg := holder.Load()
		mc.SyncTools(curCfg)
	})

	// Initial health probe: check all accounts on startup, warn but don't block
	pool.ProbeExhausted(p)

	// Startup probes must not delay Listen: /health is 200 as soon as the
	// process is listening; /ready stays fail-closed until an account is healthy.
	startInitialAccountProbes(p)

	stop := make(chan struct{})
	pool.StartProbeLoop(p, cfg.ProbeInterval, stop)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	proxyHandler := proxy.NewProxyHandler(p, wire, holder, mc)

	// Usage recording wiring: opt-in, never blocks startup, degrades to a
	// no-op on any failure. The summary handler is always registered; when
	// usage is disabled or the store failed it answers 503 store_unavailable.
	// The default key_id for requests without an authenticated api key applies
	// to the audit log and usage rows regardless of whether recording is
	// enabled, so it is installed unconditionally.
	usageRec, usageStore := startUsageRecorder(cfg)
	middleware.SetUsageDefaultKeyID(cfg.Usage.DefaultKeyID)
	if usageRec != nil {
		adapter := &usageRecorderAdapter{holder: holder, rec: usageRec}
		middleware.SetUsageRecorder(adapter)
		middleware.SetUsagePricer(adapter.Price)
	}
	summaryHandler := usage.NewSummaryHandler(usageStore)

	quotaCache := planusage.NewCache()
	quotaPoller := planusage.NewPoller(planusage.DefaultFetchers(), quotaCache, cfg.Quota.RefreshInterval, cfg.Quota.RequestTimeout)
	var quotaViews []planusage.AccountView
	for _, a := range p.AllAccounts() {
		quotaViews = append(quotaViews, a)
	}
	quotaPoller.SetAccounts(quotaViews)
	quotaPoller.SetOptions(cfg.Quota.Enabled, cfg.Quota.RefreshInterval, cfg.Quota.RequestTimeout)
	if ss, ok := usageStore.(*usage.SQLiteStore); ok && ss != nil {
		quotaPoller.SetGrokEstimate(ss.SumGrokTokens, planusage.DefaultGrokEstimatePath)
		quotaPoller.SetGrokBuildImport(func(ctx context.Context, from, to int64) error {
			_, err := usage.ImportGrokBuild(ctx, ss, usage.DefaultGrokSessionsDir(), from, to, grokPriceFor(holder.Load()))
			return err
		})
	}
	quotaPoller.Start()
	quotaHandler := planusage.NewHandler(quotaCache, quotaPoller.Enabled)
	summaryHandler.DefaultFrom = func() int64 {
		return planusage.WeekStartUnix(quotaCache.List(), planusage.DefaultGrokEstimatePath, time.Now())
	}

	metricCtx, metricCancel := context.WithCancel(context.Background())

	// Rate limiter: 60 req/s per IP with burst of 100
	rl := ratelimit.NewRateLimiter(config.RateLimitPerSecond, config.RateLimitBurst)
	rl.StartCleanupLoop(metricCtx)

	trustedProxies, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		slog.Error("trusted_proxies parse error", "error", err)
		os.Exit(1)
	}

	// Periodically update pool metrics and per-account expvar
	for _, a := range p.AllAccounts() {
		acc := a
		expvar.Publish("pool_account_"+acc.Name()+"_in_flight", expvar.Func(func() any {
			return acc.InFlightCount()
		}))
		expvar.Publish("pool_account_"+acc.Name()+"_total_requests", expvar.Func(func() any {
			return acc.TotalRequests()
		}))
		expvar.Publish("pool_account_"+acc.Name()+"_cooldown_total", expvar.Func(func() any {
			return acc.CooldownCount()
		}))
		expvar.Publish("pool_account_"+acc.Name()+"_exhaust_total", expvar.Func(func() any {
			return acc.ExhaustCount()
		}))
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-metricCtx.Done():
				return
			case <-ticker.C:
				snap := p.SnapshotStats()
				util.UpdatePoolMetrics(snap.Healthy, snap.Exhausted)
				slog.Debug("pool stats",
					"total", snap.Total,
					"healthy", snap.Healthy,
					"exhausted", snap.Exhausted,
					"in_cooldown", snap.InCooldown,
					"in_flight_sum", snap.InFlightSum,
				)
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newHTTPHandler(holder, proxyHandler, rl, trustedProxies, summaryHandler, quotaHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout=0: allow long-lived streaming responses to clients.
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in signal handler", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				slog.Info("received SIGHUP, reloading config and mcp_tools.json")
				warnings, err := config.ReloadConfig(holder, "config.yaml")
				if err != nil {
					slog.Error("reload config failed, keeping old config", "error", err)
				} else {
					slog.Info("config reloaded successfully")
					for _, w := range warnings {
						slog.Warn("config reload warning", "warning", w)
					}
					newCfg := holder.Load()
					util.DebugMode.Store(newCfg.Debug)
					// Keep the EmitAudit key_id fallback in sync with the
					// reloaded usage.default_key_id: the auth-disabled path in
					// newHTTPHandler reads the holder live, so the fallback
					// must follow or the two paths would drift until restart.
					middleware.SetUsageDefaultKeyID(newCfg.Usage.DefaultKeyID)
					quotaPoller.SetOptions(newCfg.Quota.Enabled, newCfg.Quota.RefreshInterval, newCfg.Quota.RequestTimeout)
				}
				// Always reload MCP tools from current config (new or old).
				curCfg := holder.Load()
				mcp.ClearMCPCache()
				mcp.LoadMCPTools(curCfg.MCPToolsJSON)
				slog.Info("mcp_tools.json reloaded", "path", curCfg.MCPToolsJSON)

				// SIGHUP 时强制刷新模型缓存并同步 tools。刷新与 tools 同步在后台
				// goroutine 中执行（RefreshAllAsync 内部可取消、禁止并发重入），
				// 信号循环绝不阻塞在慢上游上；Stop 时会取消正在进行的刷新。
				mc.UpdateConfig(holder.Load())
				mc.RefreshAllAsync(func() {
					mc.SyncTools(holder.Load())
				})

				continue
			}
			slog.Info("shutting down", "signal", sig.String())
			close(stop)
			oauthCancel()
			mc.Stop()
			quotaPoller.Stop()
			metricCancel()
			mcp.StopMCPCache()
			// Graceful shutdown order: stop the HTTP server first and wait for
			// in-flight requests to finish (their deferred EmitAudit → Record
			// runs during Shutdown), THEN drain the usage recorder. The old
			// order (Close before Shutdown) silently dropped the usage of
			// requests still in flight during graceful shutdown.
			shutdownHTTPAndDrainUsage(srv, usageRec)
			return
		}
	}()

	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		slog.Info("starting HTTPS server", "listen", cfg.Listen)
		if err := srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile); err != http.ErrServerClosed {
			slog.Error("listen tls", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("starting HTTP server", "listen", cfg.Listen)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("listen", "error", err)
			os.Exit(1)
		}
	}
}

// attachOAuth wires file-backed xAI token sources onto oauth: xai accounts.
func attachOAuth(p *pool.Pool, cfg *config.Config) context.CancelFunc {
	client := &http.Client{Timeout: 20 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	for _, acc := range p.AllAccounts() {
		if acc.OAuth() != "xai" {
			continue
		}
		name := acc.Name()
		src := oauth.NewXAISource(cfg.OAuthDir, name, func(ctx context.Context, refresh string) (xai.Tokens, error) {
			return xai.Refresh(ctx, xai.Config{HTTP: client}, refresh)
		})
		acc.SetTokenSource(src)
		go func(acc *pool.Account, src *oauth.Source) {
			ticker := time.NewTicker(2 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := src.ForceRefresh(ctx); err != nil {
						slog.Warn("xAI OAuth keepalive refresh failed", "account", acc.Name(), "error", err)
					}
				}
			}
		}(acc, src)
	}
	return cancel
}
