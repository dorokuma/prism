package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/util"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"Info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"Warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"Error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"garbage", slog.LevelInfo},
	}
	for _, tc := range tests {
		got := ParseLevel(tc.input)
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestInitLogger_LevelParsing(t *testing.T) {
	var buf bytes.Buffer

	// Capture slog output by temporarily replacing the handler.
	for _, level := range []string{"debug", "info", "warn", "error"} {
		InitLogger(level)
		wantDebug := level == "debug"
		wantInfo := level == "debug" || level == "info"
		if got := Logger.Enabled(context.Background(), slog.LevelDebug); got != wantDebug {
			t.Errorf("after InitLogger(%q): debug enabled = %v, want %v", level, got, wantDebug)
		}
		if got := Logger.Enabled(context.Background(), slog.LevelInfo); got != wantInfo {
			t.Errorf("after InitLogger(%q): info enabled = %v, want %v", level, got, wantInfo)
		}
	}

	// Default level for empty/unknown input
	InitLogger("")
	if got := LogLevel.Level(); got != slog.LevelInfo {
		t.Errorf("after InitLogger(\"\"): level = %v, want info", got)
	}

	// Verify output is valid JSON
	InitLogger("info")
	buf.Reset()
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	l := slog.New(h)
	l.Info("test message", "key", "value")
	out := buf.String()
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Errorf("expected INFO level in JSON output, got: %s", out)
	}
	if !strings.Contains(out, `"msg":"test message"`) {
		t.Errorf("expected message in JSON output, got: %s", out)
	}
}

func TestSetLogLevel_HotReload(t *testing.T) {
	// Start at info
	InitLogger("info")
	if Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should not be enabled at info level")
	}

	// Hot reload to debug
	SetLogLevel("debug")
	if !Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be enabled after SetLogLevel(debug)")
	}

	// Hot reload back to error
	SetLogLevel("error")
	if Logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should not be enabled after SetLogLevel(error)")
	}

	// Hot reload to warn
	SetLogLevel("warn")
	if !Logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be enabled after SetLogLevel(warn)")
	}
}

func TestDefaultLogger_Init(t *testing.T) {
	// init() sets a default LevelInfo logger. Verify it exists.
	if Logger == nil {
		t.Fatal("expected init() to create a default logger, got nil")
	}
}

func TestRequestIDMiddleware_GeneratesAndPropagates(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = util.RequestIDFromCtx(r.Context())
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	// No X-Request-ID header set — middleware should generate one.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID == "" {
		t.Fatal("expected non-empty request ID from context after middleware")
	}
	respID := rec.Header().Get("X-Request-ID")
	if respID == "" {
		t.Fatal("expected X-Request-ID response header to be set")
	}
	if capturedID != respID {
		t.Fatalf("context request ID %q != response header %q", capturedID, respID)
	}
}

func TestRequestIDMiddleware_PreservesClientHeader(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = util.RequestIDFromCtx(r.Context())
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	clientID := "my-client-id-123"
	req.Header.Set("X-Request-ID", clientID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != clientID {
		t.Fatalf("context request ID %q != client ID %q", capturedID, clientID)
	}
	respID := rec.Header().Get("X-Request-ID")
	if respID != clientID {
		t.Fatalf("response header X-Request-ID %q != client ID %q", respID, clientID)
	}
}

func TestRequestIDFromCtx_EmptyWithoutMiddleware(t *testing.T) {
	ctx := context.Background()
	if id := util.RequestIDFromCtx(ctx); id != "" {
		t.Fatalf("expected empty string from bare context, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// Audit log test helpers
// ---------------------------------------------------------------------------

// capturingHandler collects log records into a []byte slice for later
// inspection.  It is safe for concurrent use within a single test.
type capturingHandler struct {
	mu  sync.Mutex
	buf []byte
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b bytes.Buffer
	b.WriteByte('{')
	// Always emit time, level, and msg first.
	b.WriteString(fmt.Sprintf(`"time":"%s"`, r.Time.Format(time.RFC3339Nano)))
	b.WriteString(fmt.Sprintf(`,"level":"%s"`, r.Level.String()))
	b.WriteString(fmt.Sprintf(`,"msg":"%s"`, r.Message))
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(',')
		b.WriteString(fmt.Sprintf(`"%s":`, a.Key))
		val := a.Value.Resolve()
		switch val.Kind() {
		case slog.KindString:
			b.WriteString(fmt.Sprintf(`"%s"`, val.String()))
		case slog.KindInt64:
			b.WriteString(fmt.Sprintf(`%d`, val.Int64()))
		case slog.KindFloat64:
			b.WriteString(fmt.Sprintf(`%v`, val.Float64()))
		case slog.KindBool:
			b.WriteString(fmt.Sprintf(`%v`, val.Bool()))
		default:
			b.WriteString(fmt.Sprintf(`"%s"`, val.String()))
		}
		return true
	})
	b.WriteByte('}')
	h.buf = append(h.buf, b.Bytes()...)
	h.buf = append(h.buf, '\n')
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(name string) slog.Handler       { return h }
func (h *capturingHandler) output() string                           { h.mu.Lock(); defer h.mu.Unlock(); return string(h.buf) }

// stashSlog replaces the default slog.Logger with one that writes into h
// and returns a restore function.  Callers must defer the restore func.
func stashSlog(h *capturingHandler) func() {
	old := slog.Default()
	l := slog.New(h)
	slog.SetDefault(l)
	return func() { slog.SetDefault(old) }
}

func TestAuditFromCtx_NilSafe(t *testing.T) {
	// Bare context: AuditFromCtx returns nil, no panic.
	ctx := context.Background()
	if a := AuditFromCtx(ctx); a != nil {
		t.Fatal("expected nil from bare context")
	}
	// EmitAudit must not panic with nil.
	EmitAudit(nil) // no-op, no panic
}

// TestEmitAudit_NewFieldsIncluded verifies EmitAudit emits all new v2 fields
// (provider / key_id / stream / success / token breakdown / cost) alongside
// the legacy tokens_in / tokens_out fields.
func TestEmitAudit_NewFieldsIncluded(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	aud := &RequestAudit{
		Req:              "req-1",
		Method:           "POST",
		Path:             "/v1/chat/completions",
		Model:            "deepseek-v4-pro",
		Account:          "account-1",
		Status:           200,
		DurationMs:       12.5,
		TokensIn:         100,
		TokensOut:        50,
		Concurrency:      3,
		Provider:         "opencode-go",
		KeyID:            "ci-bot",
		Stream:           true,
		Success:          true,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CachedTokens:     40,
		ReasoningTokens:  10,
		CacheWriteTokens: 5,
		CostUSD:          0.0042,
		CostStatus:       "estimated",
	}
	EmitAudit(aud)

	out := h.output()
	for _, want := range []string{
		`"key_id":"ci-bot"`,
		`"provider":"opencode-go"`,
		`"stream":true`,
		`"success":true`,
		`"prompt_tokens":100`,
		`"completion_tokens":50`,
		`"total_tokens":150`,
		`"cached_tokens":40`,
		`"reasoning_tokens":10`,
		`"cache_write_tokens":5`,
		`"cost_usd":0.0042`,
		`"cost_status":"estimated"`,
		// legacy fields must remain (backward compat)
		`"tokens_in":100`,
		`"tokens_out":50`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output missing %s; got:\n%s", want, out)
		}
	}
}

// TestEmitAudit_NoTokenPlaintext enforces the hard constraint: the audit log
// may only ever contain the key NAME (KeyID), never any token-like secret.
func TestEmitAudit_NoTokenPlaintext(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	secret := "sk-super-secret-token-abc123"
	aud := &RequestAudit{
		Req:   "req-1",
		Path:  "/v1/responses",
		KeyID: "ci-bot",
	}
	EmitAudit(aud)
	out := h.output()
	if strings.Contains(out, secret) {
		t.Fatalf("audit output must not contain token plaintext; got:\n%s", out)
	}
	if strings.Contains(out, "sk-") {
		t.Fatalf("audit output must not contain any key-looking secret; got:\n%s", out)
	}
}

// TestRequestAudit_JSONTagsOmitEmpty verifies the json tags on the new v2
// fields: zero values are omitted, populated values marshal under their json
// names (and KeyID carries the name, not a token).
func TestRequestAudit_JSONTagsOmitEmpty(t *testing.T) {
	zero := RequestAudit{Req: "r", KeyID: "ci-bot"}
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{
		"provider", "stream", "success", "prompt_tokens", "completion_tokens",
		"total_tokens", "cached_tokens", "reasoning_tokens",
		"cache_write_tokens", "cost_usd", "cost_status",
	} {
		if strings.Contains(s, `"`+key+`"`) {
			t.Errorf("zero value %q should be omitted by omitempty, got: %s", key, s)
		}
	}
	if !strings.Contains(s, `"key_id":"ci-bot"`) {
		t.Errorf("key_id should be present when set, got: %s", s)
	}

	full := RequestAudit{
		Provider:         "opencode-go",
		KeyID:            "ci-bot",
		Stream:           true,
		Success:          true,
		PromptTokens:     1,
		CompletionTokens: 2,
		TotalTokens:      3,
		CachedTokens:     4,
		ReasoningTokens:  5,
		CacheWriteTokens: 6,
		CostUSD:          0.5,
		CostStatus:       "estimated",
	}
	b2, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(b2)
	for _, want := range []string{
		`"provider":"opencode-go"`, `"key_id":"ci-bot"`, `"stream":true`,
		`"success":true`, `"prompt_tokens":1`, `"completion_tokens":2`,
		`"total_tokens":3`, `"cached_tokens":4`, `"reasoning_tokens":5`,
		`"cache_write_tokens":6`, `"cost_usd":0.5`, `"cost_status":"estimated"`,
	} {
		if !strings.Contains(s2, want) {
			t.Errorf("marshal output missing %s; got: %s", want, s2)
		}
	}
}

// ---------------------------------------------------------------------------
// Usage recorder injection tests
// ---------------------------------------------------------------------------

// fakeUsageRecorder collects UsageEvents for assertion.
type fakeUsageRecorder struct {
	mu  sync.Mutex
	evs []UsageEvent
}

func (f *fakeUsageRecorder) Record(e UsageEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, e)
}

func (f *fakeUsageRecorder) events() []UsageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]UsageEvent(nil), f.evs...)
}

// panickingUsageRecorder simulates a broken recorder implementation.
type panickingUsageRecorder struct{}

func (panickingUsageRecorder) Record(UsageEvent) { panic("boom") }

// TestEmitAudit_UsageRecorderInjected: with a recorder installed, EmitAudit
// forwards one UsageEvent carrying every mapped field.
func TestEmitAudit_UsageRecorderInjected(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)

	aud := &RequestAudit{
		Req:              "req-42",
		Method:           "POST",
		Path:             "/v1/chat/completions",
		Model:            "deepseek-v4-pro",
		Account:          "account-1",
		Status:           200,
		DurationMs:       12.5,
		Provider:         "opencode-go",
		KeyID:            "ci-bot",
		Stream:           true,
		Success:          true,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CachedTokens:     40,
		ReasoningTokens:  10,
		CacheWriteTokens: 5,
	}
	EmitAudit(aud)

	evs := fake.events()
	if len(evs) != 1 {
		t.Fatalf("recorder got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.RequestID != "req-42" || ev.Path != "/v1/chat/completions" || ev.Model != "deepseek-v4-pro" ||
		ev.Provider != "opencode-go" || ev.Account != "account-1" || ev.KeyID != "ci-bot" {
		t.Errorf("event identity fields mismatch: %+v", ev)
	}
	if !ev.Stream || !ev.Success || ev.Status != 200 {
		t.Errorf("event stream/success/status mismatch: %+v", ev)
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 50 || ev.TotalTokens != 150 ||
		ev.CachedTokens != 40 || ev.ReasoningTokens != 10 || ev.CacheWriteTokens != 5 {
		t.Errorf("event token fields mismatch: %+v", ev)
	}
	if ev.DurationMS != 12.5 {
		t.Errorf("event duration = %v, want 12.5", ev.DurationMS)
	}
}

// TestEmitAudit_UsageRecorderNilSafe: no recorder installed → EmitAudit
// completes without panic and the recorder stays unset.
func TestEmitAudit_UsageRecorderNilSafe(t *testing.T) {
	SetUsageRecorder(nil)
	EmitAudit(&RequestAudit{Req: "r", Path: "/v1/responses"}) // no panic
}

// TestEmitAudit_UsageRecorderPanicSwallowed: a panicking recorder must never
// propagate to the request finalization path (degradation constraint).
func TestEmitAudit_UsageRecorderPanicSwallowed(t *testing.T) {
	SetUsageRecorder(panickingUsageRecorder{})
	defer SetUsageRecorder(nil)
	EmitAudit(&RequestAudit{Req: "r", Path: "/v1/responses", Success: true}) // must not panic
}

// TestSetUsageRecorder_ClearRestoresNoOp: installing and then clearing the
// recorder must return EmitAudit to the no-op path.
func TestSetUsageRecorder_ClearRestoresNoOp(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	EmitAudit(&RequestAudit{Req: "r1"})
	if n := len(fake.events()); n != 1 {
		t.Fatalf("events after install = %d, want 1", n)
	}
	SetUsageRecorder(nil)
	EmitAudit(&RequestAudit{Req: "r2"})
	if n := len(fake.events()); n != 1 {
		t.Fatalf("events after clear = %d, want still 1 (recorder must not be called)", n)
	}
}

// ---------------------------------------------------------------------------
// Cost pricer tests (single pricing point on the synchronous path)
// ---------------------------------------------------------------------------

// TestEmitAudit_PricerFillsLogAndEvent is the acceptance test for the
// single-pricing-point rule: the pricer result must appear in the
// request.complete log line AND be carried on the usage event, so the log
// amount and the persisted amount are the same value.
func TestEmitAudit_PricerFillsLogAndEvent(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)
	SetUsagePricer(func(a *RequestAudit) (*float64, string) {
		if a.Model != "m1" {
			t.Errorf("pricer got model %q, want m1", a.Model)
		}
		c := 0.0042
		return &c, "ok"
	})
	defer SetUsagePricer(nil)

	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	aud := &RequestAudit{
		Req: "req-1", Model: "m1", PromptTokens: 100, CompletionTokens: 50,
		TotalTokens: 150, UsageSource: "openai",
	}
	EmitAudit(aud)

	out := h.output()
	if !strings.Contains(out, `"cost_usd":0.0042`) || !strings.Contains(out, `"cost_status":"ok"`) {
		t.Errorf("audit log must carry the pricer's amount and status; got:\n%s", out)
	}
	evs := fake.events()
	if len(evs) != 1 {
		t.Fatalf("recorder got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Cost == nil || *ev.Cost != 0.0042 {
		t.Errorf("event cost = %v, want 0.0042 (same value as the log)", ev.Cost)
	}
	if ev.CostStatus != "ok" {
		t.Errorf("event cost_status = %q, want ok", ev.CostStatus)
	}
}

// TestEmitAudit_PricerMissingPrice: a nil cost from the pricer must be
// carried through as nil (the usage layer persists NULL / missing_price) and
// the log must not show an amount.
func TestEmitAudit_PricerMissingPrice(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)
	SetUsagePricer(func(*RequestAudit) (*float64, string) { return nil, "missing_price" })
	defer SetUsagePricer(nil)

	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	EmitAudit(&RequestAudit{Req: "r", Model: "unknown"})

	if !strings.Contains(h.output(), `"cost_status":"missing_price"`) {
		t.Errorf("audit log must carry cost_status missing_price; got:\n%s", h.output())
	}
	evs := fake.events()
	if len(evs) != 1 || evs[0].Cost != nil || evs[0].CostStatus != "missing_price" {
		t.Errorf("event must carry nil cost + missing_price, got %+v", evs)
	}
}

// TestEmitAudit_PricerPanicSwallowed: a panicking pricer must never propagate
// to the request finalization path (degradation constraint): the failure is
// logged and counted, and the event is still recorded with no cost (the usage
// layer then persists NULL / missing_price).
func TestEmitAudit_PricerPanicSwallowed(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)
	SetUsagePricer(func(*RequestAudit) (*float64, string) { panic("price boom") })
	defer SetUsagePricer(nil)

	errorsBefore := util.MetricsUsageWriteErrors.Value()
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	EmitAudit(&RequestAudit{Req: "r", Model: "m"}) // must not panic

	if util.MetricsUsageWriteErrors.Value() == errorsBefore {
		t.Error("pricing panic must be counted as a write error")
	}
	evs := fake.events()
	if len(evs) != 1 || evs[0].Cost != nil {
		t.Errorf("event must still be recorded with nil cost after a pricing panic, got %+v", evs)
	}
	if !strings.Contains(h.output(), "pricing panic") {
		t.Errorf("pricing panic must be logged; got:\n%s", h.output())
	}
}

// TestSetUsagePricer_ClearDisablesCost: clearing the pricer returns EmitAudit
// to the no-cost behavior (no amount, no status), matching the pre-pricer
// behavior when usage is disabled.
func TestSetUsagePricer_ClearDisablesCost(t *testing.T) {
	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)
	SetUsagePricer(func(*RequestAudit) (*float64, string) {
		c := 1.0
		return &c, "ok"
	})

	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()
	EmitAudit(&RequestAudit{Req: "r1"})

	SetUsagePricer(nil)
	h2 := &capturingHandler{}
	restore2 := stashSlog(h2)
	defer restore2()
	EmitAudit(&RequestAudit{Req: "r2"})

	if !strings.Contains(h.output(), `"cost_usd":1`) {
		t.Errorf("with pricer installed the log must carry the amount; got:\n%s", h.output())
	}
	// slog emits the cost attributes unconditionally: after clearing the
	// pricer they must be the zero values (0 amount, empty status), i.e.
	// exactly the pre-pricer "no cost" state.
	if !strings.Contains(h2.output(), `"cost_usd":0`) || !strings.Contains(h2.output(), `"cost_status":""`) {
		t.Errorf("after clearing the pricer the log must carry no cost (0 amount, empty status); got:\n%s", h2.output())
	}
}

// ---------------------------------------------------------------------------
// key_id default fallback (usage.default_key_id)
// ---------------------------------------------------------------------------

// TestEmitAudit_EmptyKeyIDFallsBackToDefaultKeyID is the acceptance test for
// the "no empty key_id anywhere" rule: an audit whose KeyID was never filled
// (early rejections, e.g. the 400 for a missing X-Prism-Provider header,
// return before key attribution) must be recorded with the configured default
// instead of "" — an empty and a default value would split one GROUP BY
// group into two rows. The default follows usage.default_key_id (zero state
// "anonymous"), applies to the audit log AND the usage event (single choke
// point in EmitAudit), and an empty value passed to SetUsageDefaultKeyID is
// ignored so the invariant can never be configured away.
func TestEmitAudit_EmptyKeyIDFallsBackToDefaultKeyID(t *testing.T) {
	SetUsageDefaultKeyID("anonymous") // reset to the zero state
	defer SetUsageDefaultKeyID("anonymous")

	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)

	t.Run("zero state defaults to anonymous", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		EmitAudit(&RequestAudit{Req: "r1", Path: "/v1/chat/completions"})

		evs := fake.events()
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		if evs[0].KeyID != "anonymous" {
			t.Fatalf("event key_id = %q, want anonymous (never empty)", evs[0].KeyID)
		}
		if !strings.Contains(h.output(), `"key_id":"anonymous"`) {
			t.Errorf("audit log must carry the default key_id; got:\n%s", h.output())
		}
	})

	t.Run("configured default applied", func(t *testing.T) {
		SetUsageDefaultKeyID("gateway-pi")

		EmitAudit(&RequestAudit{Req: "r2", Path: "/v1/chat/completions"})

		evs := fake.events()
		if len(evs) != 2 {
			t.Fatalf("events = %d, want 2", len(evs))
		}
		if evs[1].KeyID != "gateway-pi" {
			t.Fatalf("event key_id = %q, want gateway-pi (configured default)", evs[1].KeyID)
		}
	})

	t.Run("empty setter value is ignored", func(t *testing.T) {
		SetUsageDefaultKeyID("") // must NOT reset the default to ""

		EmitAudit(&RequestAudit{Req: "r3", Path: "/v1/chat/completions"})

		evs := fake.events()
		if len(evs) != 3 {
			t.Fatalf("events = %d, want 3", len(evs))
		}
		if evs[2].KeyID != "gateway-pi" {
			t.Fatalf("event key_id = %q, want gateway-pi (empty setter ignored)", evs[2].KeyID)
		}
	})
}

// TestEmitAudit_RealKeyIDNotOverridden: a real authenticated key NAME must
// pass through untouched — the fallback only fills the empty string and never
// replaces a key that was actually matched.
func TestEmitAudit_RealKeyIDNotOverridden(t *testing.T) {
	SetUsageDefaultKeyID("gateway-pi")
	defer SetUsageDefaultKeyID("anonymous")

	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)

	EmitAudit(&RequestAudit{Req: "r", Path: "/v1/chat/completions", KeyID: "ci-bot"})

	evs := fake.events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].KeyID != "ci-bot" {
		t.Fatalf("event key_id = %q, want ci-bot (real key name must win over the default)", evs[0].KeyID)
	}
}
