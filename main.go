package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	for _, acc := range pool.AllAccounts() {
		go func(a *Account) {
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
				a.ResetFailures()
			} else if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 || isPermanentCredentialError(bodyBytes) || isQuotaError(bodyBytes) {
				log.Printf("startup check %s: permanent error status=%d, marking exhausted. body: %s", a.Name(), resp.StatusCode, string(bodyBytes))
				a.MarkExhausted()
			} else if resp.StatusCode == 429 {
				log.Printf("startup check %s: temporary quota error status=429, cooling down. body: %s", a.Name(), string(bodyBytes))
				a.SetCooldown(2 * time.Minute)
			} else {
				log.Printf("startup check %s: temporary error status=%d, cooling down. body: %s", a.Name(), resp.StatusCode, string(bodyBytes))
				a.SetCooldown(5 * time.Minute)
			}
		}(acc)
	}

	stop := make(chan struct{})
	StartProbeLoop(pool, cfg.ProbeModel, cfg.ProbeInterval, stop)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewProxyHandler(pool, wire, cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout=0: allow long-lived streaming responses to clients.
	}
	go func() {
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
