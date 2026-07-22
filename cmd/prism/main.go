package main

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/proxy"
	"github.com/dorokuma/prism/internal/ratelimit"
	"github.com/dorokuma/prism/internal/util"
)

func init() {
	config.LogLevelHook = middleware.SetLogLevel
}

func main() {
	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	holder := config.NewConfigHolder(cfg)

	middleware.InitLogger(cfg.LogLevel)

	util.DebugMode = cfg.Debug
	mcp.LoadMCPTools(cfg.MCPToolsJSON)
	p := pool.NewPool(cfg.Accounts)
	wire, _ := config.ParseWireAPIMode(cfg.WireAPI)
	slog.Info("prism starting", "accounts", len(cfg.Accounts), "wire_api", string(wire), "listen", cfg.Listen, "debug", util.DebugMode, "auth", cfg.AuthToken != "", "tls", cfg.TLSCertFile != "")

	// Initial health probe: check all accounts on startup, warn but don't block
	pool.ProbeExhausted(p, cfg.ProbeModel)

	// 启动时验证所有账号的连通性
	slog.Info("starting initial health check for all accounts")
	probeBody, _ := json.Marshal(map[string]any{
		"model":      cfg.ProbeModel,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	sem := make(chan struct{}, 10)
	var startupWg sync.WaitGroup
	for _, acc := range p.AllAccounts() {
		sem <- struct{}{}
		startupWg.Add(1)
		go func(a *pool.Account) {
			defer startupWg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in startup health check", "account", a.Name(), "panic", r, "stack", string(debug.Stack()))
				}
			}()

			url := a.BaseURL() + "/chat/completions"
			req, err := http.NewRequest("POST", url, bytes.NewReader(probeBody))
			if err != nil {
				slog.Warn("startup check failed to create request", "account", a.Name(), "error", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+a.Key())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			resp, err := a.Client().Do(req)
			if err != nil {
				slog.Warn("startup check request failed", "account", a.Name(), "error", err)
				a.SetCooldown(5 * time.Minute)
				return
			}
			defer resp.Body.Close()
			limitReader := io.LimitReader(resp.Body, 4096)
			bodyBytes, readErr := io.ReadAll(limitReader)
			if readErr != nil {
				slog.Warn("startup check read body failed", "account", a.Name(), "error", readErr)
			}
			if resp.StatusCode == 200 {
				slog.Info("startup check OK", "account", a.Name(), "status", 200)
			} else if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 || proxy.IsPermanentCredentialError(bodyBytes) || proxy.IsQuotaError(bodyBytes) {
				slog.Error("startup check permanent error, marking exhausted", "account", a.Name(), "status", resp.StatusCode, "body", util.RedactBody(bodyBytes))
				a.MarkExhausted()
			} else if resp.StatusCode == 429 {
				slog.Warn("startup check temporary quota error, cooling down", "account", a.Name(), "status", 429, "body", util.RedactBody(bodyBytes))
				a.SetCooldown(2 * time.Minute)
			} else {
				slog.Warn("startup check temporary error, cooling down", "account", a.Name(), "status", resp.StatusCode, "body", util.RedactBody(bodyBytes))
				a.SetCooldown(5 * time.Minute)
			}
		}(acc)
	}
	startupWg.Wait()

	stop := make(chan struct{})
	pool.StartProbeLoop(p, cfg.ProbeModel, cfg.ProbeInterval, stop)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	proxyHandler := proxy.NewProxyHandler(p, wire, holder)

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
		Addr: cfg.Listen,
		Handler: middleware.RequestIDMiddleware(ratelimit.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				token := os.Getenv("METRICS_TOKEN")
				allowed := (token != "" && middleware.CheckAuth(r, token)) || middleware.IsLocalhost(r)
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
			curCfg := holder.Load()
			if curCfg.AuthToken != "" && r.URL.Path != "/health" {
				if !middleware.CheckAuth(r, curCfg.AuthToken) {
					slog.Warn("auth_failed", "req", util.RequestIDFromCtx(r.Context()), "path", r.URL.Path, "remote_addr", r.RemoteAddr)
					util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
						"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
					})
					return
				}
			}
			// Timeout decisions are delegated to proxyChatWithBody which
			// applies per-request timeouts based on the actual stream
			// setting (parsed from the JSON body, not headers).
			proxyHandler.ServeHTTP(w, r)
		}), rl, trustedProxies)),
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
					util.DebugMode = newCfg.Debug
				}
				// Always reload MCP tools from current config (new or old).
				curCfg := holder.Load()
				mcp.ClearMCPCache()
				mcp.LoadMCPTools(curCfg.MCPToolsJSON)
				slog.Info("mcp_tools.json reloaded", "path", curCfg.MCPToolsJSON)
				continue
			}
			slog.Info("shutting down", "signal", sig.String())
			close(stop)
			metricCancel()
			mcp.StopMCPCache()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				slog.Error("shutdown error", "error", err)
			}
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
