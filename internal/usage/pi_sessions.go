package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	PiSessionKeyID   = "pi-session"
	PiSessionAccount = "pi-session"
	PiSessionSource  = "pi"
	piSessionsPath   = "/root/.pi/agent/sessions"
)

// DefaultPiSessionsDir is the pi agent session tree.
func DefaultPiSessionsDir() string { return piSessionsPath }

type piMessageLine struct {
	Type    string `json:"type"`
	Message *struct {
		Role       string          `json:"role"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
		Timestamp  json.RawMessage `json:"timestamp"`
		StopReason string          `json:"stopReason"`
		Usage      *struct {
			Input       int64           `json:"input"`
			Output      int64           `json:"output"`
			TotalTokens int64           `json:"totalTokens"`
			CacheRead   int64           `json:"cacheRead"`
			CacheWrite  int64           `json:"cacheWrite"`
			Cost        json.RawMessage `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

func piTimestamp(raw json.RawMessage) (int64, bool) {
	var ms int64
	if err := json.Unmarshal(raw, &ms); err != nil || ms <= 0 {
		return 0, false
	}
	return ms / 1000, true
}

func piCost(raw json.RawMessage) (*float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return &n, true
	}
	var obj struct {
		Total *float64 `json:"total"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Total != nil {
		return obj.Total, true
	}
	return nil, false
}

func piEvent(line piMessageLine, requestID string) (Event, bool) {
	if line.Type != "message" || line.Message == nil || line.Message.Role != "assistant" || line.Message.Usage == nil {
		return Event{}, false
	}
	ts, ok := piTimestamp(line.Message.Timestamp)
	if !ok {
		return Event{}, false
	}
	if strings.TrimSpace(line.Message.Provider) == "" || strings.TrimSpace(line.Message.Model) == "" {
		return Event{}, false
	}
	u := line.Message.Usage
	cost, hasCost := piCost(u.Cost)
	total := u.TotalTokens
	if total <= 0 {
		total = u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	success := line.Message.StopReason != "error"
	status := 200
	if !success {
		status = 500
	}
	return Event{
		Ts: time.Unix(ts, 0).UTC(), RequestID: requestID, Path: "pi-session",
		Model: line.Message.Model, Provider: line.Message.Provider, Account: PiSessionAccount, KeyID: PiSessionKeyID,
		Success: success, Status: status, PromptTokens: u.Input, CompletionTokens: u.Output,
		TotalTokens: total, CachedTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
		Source: PiSessionSource, Cost: cost, CostStatus: func() string {
			if hasCost {
				return CostStatusOK
			}
			return ""
		}(),
	}, true
}

// ImportPiSessions replaces pi session rows in the requested inclusive range.
// Invalid JSONL records, headers, missing usage, and invalid timestamps are ignored.
func ImportPiSessions(ctx context.Context, store *SQLiteStore, sessionsDir string, fromUnix, toUnix int64) (int, error) {
	if store == nil || strings.TrimSpace(sessionsDir) == "" || fromUnix <= 0 {
		return 0, nil
	}
	if toUnix <= 0 {
		toUnix = time.Now().Unix()
	}
	var events []Event
	err := filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info == nil || info.IsDir() || filepath.Ext(info.Name()) != ".jsonl" {
			return nil
		}
		if info.ModTime().Unix() < fromUnix-86400 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			var line piMessageLine
			if json.Unmarshal([]byte(sc.Text()), &line) != nil {
				continue
			}
			ev, ok := piEvent(line, fmt.Sprintf("pi-session:%s:%d", path, lineNo))
			if !ok {
				continue
			}
			ts := ev.Ts.Unix()
			if ts >= fromUnix && ts <= toUnix {
				events = append(events, ev)
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) {
		return 0, err
	}
	if _, err := store.DeleteKeyIDRange(ctx, PiSessionKeyID, fromUnix, toUnix); err != nil {
		return 0, err
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		return 0, err
	}
	return len(events), nil
}
