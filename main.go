package main

import (
	"context"
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
	probeExhausted(pool)

	// 启动时验证所有账号的连通性
	log.Println("starting initial health check for all accounts...")
	for _, acc := range pool.AllAccounts() {
		go func(a *Account) {
			url := a.BaseURL() + "/models"
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				log.Printf("startup check %s: failed to create request: %v", a.Name(), err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+a.Key())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			resp, err := a.Client().Do(req)
			if err != nil {
				log.Printf("startup check %s: request failed: %v", a.Name(), err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("startup check %s: OK (200)", a.Name())
			} else {
				log.Printf("startup check %s: WARNING status=%d", a.Name(), resp.StatusCode)
			}
		}(acc)
	}

	stop := make(chan struct{})
	StartProbeLoop(pool, cfg.ProbeInterval, stop)

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
