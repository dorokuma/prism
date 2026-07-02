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

	pool := NewPool(cfg.Accounts)
	wire, _ := ParseWireAPIMode(cfg.WireAPI)
	log.Printf("loaded %d accounts, wire_api=%s, listening on %s", len(cfg.Accounts), wire, cfg.Listen)

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
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           NewProxyHandler(pool, wire, cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout=0: allow long-lived streaming responses to clients.
	}
	go func() {
		<-sigCh
		log.Printf("shutting down...")
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
