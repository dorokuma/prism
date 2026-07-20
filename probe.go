package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

func StartProbeLoop(pool *Pool, probeModel string, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	log.Printf("probe loop started (interval=%v, model=%s)", interval, probeModel)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in probe loop: %v\n%s", r, debug.Stack())
			}
		}()
		probeExhausted(pool, probeModel)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				log.Printf("probe loop stopped")
				return
			case <-ticker.C:
				probeExhausted(pool, probeModel)
			}
		}
	}()
}

func probeExhausted(pool *Pool, probeModel string) {
	exhausted := pool.ExhaustedAccounts()
	if len(exhausted) == 0 {
		return
	}

	probeBody, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens":  1,
	})

	const maxAttempts = 3
	const retryDelay = 2 * time.Second

	var wg sync.WaitGroup
	for _, acc := range exhausted {
		wg.Add(1)
		go func(acc *Account) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("panic in probe for %s: %v\n%s", acc.Name(), r, debug.Stack())
				}
			}()

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				url := acc.BaseURL() + "/chat/completions"
				req, err := http.NewRequest("POST", url, bytes.NewReader(probeBody))
				if err != nil {
					log.Printf("probe %s: failed to create request: %v", acc.Name(), err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+acc.Key())
				req.Header.Set("Content-Type", "application/json")

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				req = req.WithContext(ctx)
				resp, err := acc.Client().Do(req)

				if err != nil {
					cancel()
					log.Printf("probe %s: request failed (attempt %d/%d): %v", acc.Name(), attempt, maxAttempts, err)
					if attempt < maxAttempts {
						time.Sleep(retryDelay)
						continue
					}
					return
				}

				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				cancel()

				if resp.StatusCode == 200 {
					pool.MarkHealthy(acc)
					log.Printf("probe %s: recovered (200), returned to pool", acc.Name())
					return
				}

				if resp.StatusCode == 429 {
					log.Printf("probe %s: still exhausted (429 quota, attempt %d/%d)", acc.Name(), attempt, maxAttempts)
					return
				}

				log.Printf("probe %s: still exhausted (status=%d, attempt %d/%d) body=%s", acc.Name(), resp.StatusCode, attempt, maxAttempts, redactBody(respBody))

				if attempt < maxAttempts {
					time.Sleep(retryDelay)
				}
			}
		}(acc)
	}
	wg.Wait()
}
