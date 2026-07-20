package main

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

func main() {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	debugMode = cfg.Debug
	loadMCPTools(cfg.MCPToolsJSON)
	pool := NewPool(cfg.Accounts)
	wire, _ := ParseWireAPIMode(cfg.WireAPI)
	log.Printf("loaded %d accounts, wire_api=%s, listening on %s, debug=%v", len(cfg.Accounts), wire, cfg.Listen, debugMode)

	// Initial health probe: check all accounts on startup, warn but don't block
	probeExhausted(pool, cfg.ProbeModel)

	// 启动时验证所有账号的连通性
	log.Println("starting initial health check for all accounts...")
	probeBody, _ := json.Marshal(map[string]any{
		"model":      cfg.ProbeModel,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	sem := make(chan struct{}, 10)
	var startupWg sync.WaitGroup
	for _, acc := range pool.AllAccounts() {
		sem <- struct{}{}
		startupWg.Add(1)
		go func(a *Account) {
			defer startupWg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("panic in startup health check for %s: %v\n%s", a.Name(), r, debug.Stack())
				}
			}()

			url := a.BaseURL() + "/chat/completions"
			req, err := http.NewRequest("POST", url, bytes.NewReader(probeBody))
			if err != nil {
				log.Printf("startup check %s: failed to create request: %v", a.Name(), err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+a.Key())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			resp, err := a.Client().Do(req)
			if err != nil {
				log.Printf("startup check %s: request failed: %v, cooling down", a.Name(), err)
				a.SetCooldown(5 * time.Minute)
				return
			}
			limitReader := io.LimitReader(resp.Body, 4096)
			bodyBytes, _ := io.ReadAll(limitReader)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("startup check %s: OK (200)", a.Name())
			} else if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 || isPermanentCredentialError(bodyBytes) || isQuotaError(bodyBytes) {
				log.Printf("startup check %s: permanent error status=%d, marking exhausted. body: %s", a.Name(), resp.StatusCode, redactBody(bodyBytes))
				a.MarkExhausted()
			} else if resp.StatusCode == 429 {
				log.Printf("startup check %s: temporary quota error status=429, cooling down. body: %s", a.Name(), redactBody(bodyBytes))
				a.SetCooldown(2 * time.Minute)
			} else {
				log.Printf("startup check %s: temporary error status=%d, cooling down. body: %s", a.Name(), resp.StatusCode, redactBody(bodyBytes))
				a.SetCooldown(5 * time.Minute)
			}
		}(acc)
	}
	startupWg.Wait()

	stop := make(chan struct{})
	StartProbeLoop(pool, cfg.ProbeModel, cfg.ProbeInterval, stop)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	proxyHandler := NewProxyHandler(pool, wire, cfg)

	metricCtx, metricCancel := context.WithCancel(context.Background())

	// Rate limiter: 60 req/s per IP with burst of 100
	rl := newRateLimiter(60, 100)
	rl.startCleanupLoop(metricCtx)

	// Periodically update pool metrics
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-metricCtx.Done():
				return
			case <-ticker.C:
				all := pool.AllAccounts()
				healthy, exhausted := 0, 0
				for _, a := range all {
					if a.IsHealthy() {
						healthy++
					} else {
						exhausted++
					}
				}
				updatePoolMetrics(healthy, exhausted)
			}
		}
	}()

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				token := os.Getenv("METRICS_TOKEN")
				if token != "" {
					if r.Header.Get("Authorization") != "Bearer "+token {
						host, _, err := net.SplitHostPort(r.RemoteAddr)
						if err != nil || (host != "127.0.0.1" && host != "::1") {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusUnauthorized)
							json.NewEncoder(w).Encode(map[string]any{
								"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
							})
							return
						}
					}
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				expvar.Handler().ServeHTTP(w, r)
				return
			}
			// Add a 15-minute total timeout for every request to prevent
			// indefinite hanging. Streaming paths also get this limit,
			// which is generous enough for typical long-lived connections.
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
			defer cancel()
			proxyHandler.ServeHTTP(w, r.WithContext(ctx))
		}), rl),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout=0: allow long-lived streaming responses to clients.
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in signal handler: %v\n%s", r, debug.Stack())
			}
		}()
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				log.Printf("received SIGHUP, reloading mcp_tools.json")
				clearMCPCache()
				loadMCPTools(cfg.MCPToolsJSON)
				log.Printf("mcp_tools.json reloaded")
				continue
			}
			log.Printf("shutting down sig=%v...", sig)
			close(stop)
			metricCancel()
			if mcpCacheCtxCancel != nil {
				mcpCacheCtxCancel()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				log.Printf("shutdown: %v", err)
			}
			return
		}
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
