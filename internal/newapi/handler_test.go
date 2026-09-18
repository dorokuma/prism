package newapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/usage"
)

func newTestStore(t *testing.T) usage.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "usage_test.db")
	s := usage.NewSQLiteStore(dbPath)
	if err := s.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	return s
}

func TestUserSelf_AuthSuccess(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cost1 := 0.001
	cost2 := 0.001
	events := []usage.Event{
		{
			Ts:           time.Unix(1700000000, 0),
			RequestID:    "req-1",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      true,
			Status:       200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: &cost1, CostStatus: "ok",
		},
		{
			Ts:           time.Unix(1700000010, 0),
			RequestID:    "req-2",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      true,
			Status:       200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: &cost2, CostStatus: "ok",
		},
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	quotaUSD := 10.0
	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "secret-token-alice", QuotaUSD: &quotaUSD},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	req := httptest.NewRequest("GET", "/api/user/self", nil)
	req.Header.Set("Authorization", "Bearer secret-token-alice")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp UserSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if !resp.Success {
		t.Errorf("resp.Success = false, want true")
	}
	if resp.Data.Username != "alice" {
		t.Errorf("Username = %q, want alice", resp.Data.Username)
	}
	if resp.Data.UsedQuota != 1000 {
		t.Errorf("UsedQuota = %d, want 1000 (0.002 * 500000)", resp.Data.UsedQuota)
	}
	// Remaining: (10.0 - 0.002) * 500000 = 4999000
	if resp.Data.Quota != 4999000 {
		t.Errorf("Quota = %d, want 4999000", resp.Data.Quota)
	}
	if resp.Data.RequestCount != 2 {
		t.Errorf("RequestCount = %d, want 2", resp.Data.RequestCount)
	}
}

func TestUserSelf_Unauthorized(t *testing.T) {
	store := newTestStore(t)
	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "secret-token-alice"},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	// No token
	req := httptest.NewRequest("GET", "/api/user/self", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}

	// Bad token
	req2 := httptest.NewRequest("GET", "/api/user/self", nil)
	req2.Header.Set("Authorization", "Bearer wrong-token")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", rec2.Code)
	}
}

func TestLogSelf_PaginationAndSort(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := make([]usage.Event, 5)
	for i := 0; i < 5; i++ {
		events[i] = usage.Event{
			Ts:           time.Unix(int64(1000+i*10), 0),
			RequestID:    "req",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      true,
			Status:       200,
			PromptTokens: 100,
		}
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "token123"},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	// Page 1, Size 2 (should return ts 1040, 1030)
	req := httptest.NewRequest("GET", "/api/log/self?page=1&size=2", nil)
	req.Header.Set("Authorization", "Bearer token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp LogSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Total != 5 {
		t.Errorf("total = %d, want 5", resp.Data.Total)
	}
	if len(resp.Data.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Data.Items))
	}
	if resp.Data.Items[0].CreatedAt != 1040 || resp.Data.Items[1].CreatedAt != 1030 {
		t.Errorf("items timestamps = [%d, %d], want [1040, 1030]", resp.Data.Items[0].CreatedAt, resp.Data.Items[1].CreatedAt)
	}

	// Page 2, Size 2 (should return ts 1020, 1010)
	req2 := httptest.NewRequest("GET", "/api/log/self?page=2&size=2", nil)
	req2.Header.Set("Authorization", "Bearer token123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	var resp2 LogSelfResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if len(resp2.Data.Items) != 2 {
		t.Fatalf("page 2 len(items) = %d, want 2", len(resp2.Data.Items))
	}
	if resp2.Data.Items[0].CreatedAt != 1020 || resp2.Data.Items[1].CreatedAt != 1010 {
		t.Errorf("page 2 timestamps = [%d, %d], want [1020, 1010]", resp2.Data.Items[0].CreatedAt, resp2.Data.Items[1].CreatedAt)
	}
}

func TestLogSelf_MultiTenantIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	events := []usage.Event{
		{
			Ts:           time.Unix(1700000000, 0),
			RequestID:    "req-alice",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      true,
			Status:       200,
			PromptTokens: 100,
		},
		{
			Ts:           time.Unix(1700000010, 0),
			RequestID:    "req-bob",
			Path:         "/v1/chat/completions",
			Model:        "claude-3-5-sonnet",
			KeyID:        "bob",
			Success:      true,
			Status:       200,
			PromptTokens: 200,
		},
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "token-alice"},
			{Name: "bob", Token: "token-bob"},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	req := httptest.NewRequest("GET", "/api/log/self", nil)
	req.Header.Set("Authorization", "Bearer token-alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp LogSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Total != 1 {
		t.Errorf("alice total = %d, want 1", resp.Data.Total)
	}
	if len(resp.Data.Items) != 1 {
		t.Fatalf("alice items len = %d, want 1", len(resp.Data.Items))
	}
	if resp.Data.Items[0].Username != "alice" || resp.Data.Items[0].TokenName != "alice" {
		t.Errorf("alice items[0] leaked or wrong username: %+v", resp.Data.Items[0])
	}
}

func TestLogSelf_FieldMapping(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cost := 0.004
	events := []usage.Event{
		{
			Ts:               time.Unix(1700000000, 0),
			RequestID:        "req-test",
			Path:             "/v1/chat/completions",
			Model:            "gpt-4o",
			KeyID:            "alice",
			Stream:           true,
			Success:          true,
			Status:           200,
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
			CachedTokens:     20,
			CacheWriteTokens: 10,
			ReasoningTokens:  15,
			Cost:             &cost,
			CostStatus:       "ok",
			DurationMS:       250.5,
		},
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "token-alice"},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	req := httptest.NewRequest("GET", "/api/log/self", nil)
	req.Header.Set("Authorization", "Bearer token-alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp LogSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Data.Items))
	}
	item := resp.Data.Items[0]

	if item.CreatedAt != 1700000000 {
		t.Errorf("CreatedAt = %d, want 1700000000", item.CreatedAt)
	}
	if item.UserID != 1 {
		t.Errorf("UserID = %d, want 1", item.UserID)
	}
	if item.Username != "alice" || item.TokenName != "alice" {
		t.Errorf("Username/TokenName = %q/%q, want alice", item.Username, item.TokenName)
	}
	if item.ModelName != "gpt-4o" {
		t.Errorf("ModelName = %q, want gpt-4o", item.ModelName)
	}
	if item.Type != 1 {
		t.Errorf("Type = %d, want 1", item.Type)
	}
	if item.PromptTokens != 100 || item.CompletionTokens != 50 || item.TotalTokens != 150 {
		t.Errorf("tokens = (%d, %d, %d), want (100, 50, 150)", item.PromptTokens, item.CompletionTokens, item.TotalTokens)
	}
	if item.Quota != 2000 { // 0.004 * 500000
		t.Errorf("Quota = %d, want 2000", item.Quota)
	}
	if item.ActualCost != 0.004 {
		t.Errorf("ActualCost = %v, want 0.004", item.ActualCost)
	}
	if item.Duration != 251 || item.RequestTime != 251 {
		t.Errorf("Duration = %d, RequestTime = %d, want 251", item.Duration, item.RequestTime)
	}
	if !item.IsStream {
		t.Errorf("IsStream = false, want true")
	}
	wantOther := `{"model_ratio":1.0,"completion_ratio":1.0,"cache_ratio":0.1,"cache_creation_ratio":1.25,"cache_tokens":20,"cache_creation_tokens":10}`
	if item.Other != wantOther {
		t.Errorf("Other = %q, want %q", item.Other, wantOther)
	}
}

func TestLogSelf_MillisecondTimestampCompatibility(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert event at 1700000000 (seconds)
	event := usage.Event{
		Ts:           time.Unix(1700000000, 0),
		RequestID:    "req-ms-test",
		Path:         "/v1/chat/completions",
		Model:        "gpt-4o",
		KeyID:        "alice",
		Success:      true,
		Status:       200,
		PromptTokens: 100,
	}
	if err := store.InsertBatch(ctx, []usage.Event{event}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "token-alice"},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	// Provide millisecond timestamps: 1699999999000 and 1700000001000 (> 1e11)
	req := httptest.NewRequest("GET", "/api/log/self?start_timestamp=1699999999000&end_timestamp=1700000001000", nil)
	req.Header.Set("Authorization", "Bearer token-alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp LogSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Total != 1 {
		t.Errorf("Total = %d, want 1 (millisecond timestamp should match)", resp.Data.Total)
	}
	if len(resp.Data.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(resp.Data.Items))
	}
	if resp.Data.Items[0].CreatedAt != 1700000000 {
		t.Errorf("CreatedAt = %d, want 1700000000", resp.Data.Items[0].CreatedAt)
	}
}

func TestUserSelf_BrokenStreamIncluded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cost1 := 0.001
	cost2 := 0.002 // broken stream
	events := []usage.Event{
		{
			Ts:           time.Unix(1700000000, 0),
			RequestID:    "req-ok",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      true,
			Status:       200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: &cost1, CostStatus: "ok",
		},
		{
			Ts:           time.Unix(1700000010, 0),
			RequestID:    "req-broken",
			Path:         "/v1/chat/completions",
			Model:        "gpt-4o",
			KeyID:        "alice",
			Success:      false,
			ErrorType:    "stream_read_error",
			Status:       200,
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300,
			Cost: &cost2, CostStatus: "ok",
		},
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		t.Fatalf("insert batch: %v", err)
	}

	quotaUSD := 10.0
	cfg := &config.Config{
		APIKeys: []config.APIKey{
			{Name: "alice", Token: "token-alice", QuotaUSD: &quotaUSD},
		},
	}
	holder := config.NewConfigHolder(cfg)
	handler := NewHandler(store, holder, nil)

	req := httptest.NewRequest("GET", "/api/user/self", nil)
	req.Header.Set("Authorization", "Bearer token-alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp UserSelfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Total cost = 0.001 + 0.002 = 0.003
	// UsedQuota = 0.003 * 500000 = 1500
	if resp.Data.UsedQuota != 1500 {
		t.Errorf("UsedQuota = %d, want 1500", resp.Data.UsedQuota)
	}
	if resp.Data.RequestCount != 2 {
		t.Errorf("RequestCount = %d, want 2", resp.Data.RequestCount)
	}
}
