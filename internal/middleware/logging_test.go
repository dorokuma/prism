package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/usagemeta"
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

// TestSetUsageDefaultKeyID_ConcurrentSafe runs concurrent setters and
// EmitAudit readers (via a recorder) to pin the atomic.Value conversion: the
// SIGHUP reload path calls SetUsageDefaultKeyID while request goroutines
// read the default in EmitAudit. Under -race this test fails on a plain
// variable; with the atomic value every observed key_id is either the
// previous or the new default, never "" and never a torn read.
func TestSetUsageDefaultKeyID_ConcurrentSafe(t *testing.T) {
	SetUsageDefaultKeyID("anonymous") // reset to the zero state
	defer SetUsageDefaultKeyID("anonymous")

	fake := &fakeUsageRecorder{}
	SetUsageRecorder(fake)
	defer SetUsageRecorder(nil)

	const writers = 4
	const perWriter = 50
	const readers = 8
	const perReader = 50

	var wg sync.WaitGroup
	// Writers: flip between two configured defaults (and the empty value,
	// which must be ignored — the observed default can never become "").
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if (w+i)%2 == 0 {
					SetUsageDefaultKeyID("default-a")
				} else {
					SetUsageDefaultKeyID("default-b")
				}
				SetUsageDefaultKeyID("") // must be a no-op, never store ""
			}
		}(w)
	}
	// Readers: emit audits with an empty KeyID so the fallback reads the
	// live default concurrently with the writers.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perReader; i++ {
				EmitAudit(&RequestAudit{Req: fmt.Sprintf("conc-%d-%d", r, i), Path: "/v1/chat/completions"})
			}
		}()
	}
	wg.Wait()

	// Every recorded key_id must be a real value: never "", and only one of
	// the values writers ever stored (the empty setter is ignored). Only the
	// READER goroutines produce events (writers only call the setter).
	total := readers * perReader
	evs := fake.events()
	if len(evs) != total {
		t.Fatalf("recorded events = %d, want %d", len(evs), total)
	}
	for i, ev := range evs {
		if ev.KeyID == "" {
			t.Fatalf("event %d has an EMPTY key_id — the default can never be \"\"", i)
		}
		if ev.KeyID != "anonymous" && ev.KeyID != "default-a" && ev.KeyID != "default-b" {
			t.Fatalf("event %d key_id = %q, want one of the values ever stored (anonymous/default-a/default-b)", i, ev.KeyID)
		}
	}
}

// ---------------------------------------------------------------------------
// Round-3 audit: StatusCapture implicit-200 recording (item 8)
// ---------------------------------------------------------------------------

// TestStatusCapture_WriteRecordsImplicit200: a handler that only writes the
// body (no explicit WriteHeader) must record 200 BEFORE the write, exactly
// like net/http's implicit WriteHeader(200) semantics.
func TestStatusCapture_WriteRecordsImplicit200(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	if sc.Code != 0 {
		t.Fatalf("Code before any write = %d, want 0", sc.Code)
	}
	n, err := sc.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("written = %d, want 5", n)
	}
	if sc.Code != http.StatusOK {
		t.Errorf("Code after implicit write = %d, want 200", sc.Code)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code = %d, want 200", inner.Code)
	}
	if inner.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", inner.Body.String())
	}
}

// TestStatusCapture_ExplicitStatusNotOverwritten: an explicit WriteHeader
// (including a non-2xx) must never be overwritten by a later Write.
func TestStatusCapture_ExplicitStatusNotOverwritten(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	sc.WriteHeader(http.StatusBadGateway)
	if sc.Code != http.StatusBadGateway {
		t.Fatalf("Code after WriteHeader = %d, want 502", sc.Code)
	}
	_, _ = sc.Write([]byte("boom"))
	if sc.Code != http.StatusBadGateway {
		t.Errorf("Code after write = %d, want 502 (explicit status must win)", sc.Code)
	}
	if inner.Code != http.StatusBadGateway {
		t.Errorf("inner recorder code = %d, want 502", inner.Code)
	}
}

// TestStatusCapture_FirstStatusWins: the first recorded status — implicit or
// explicit — wins and is the ONLY one forwarded to the underlying writer.
func TestStatusCapture_FirstStatusWins(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	_, _ = sc.Write([]byte("a")) // implicit 200
	sc.WriteHeader(http.StatusCreated)
	if sc.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (implicit 200 recorded first)", sc.Code)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code = %d, want 200 (second WriteHeader must not be forwarded)", inner.Code)
	}
}

// TestStatusCapture_WriteHeaderFirstCallOnly pins item 14: WriteHeader
// accepts ONLY the first call — later calls are no-ops and must NOT be
// forwarded to the underlying writer (net/http's "first WriteHeader wins"
// contract; forwarding a later call could overwrite an already-committed
// status on wrapped writers).
func TestStatusCapture_WriteHeaderFirstCallOnly(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	sc.WriteHeader(http.StatusOK)
	sc.WriteHeader(http.StatusBadGateway)
	sc.WriteHeader(http.StatusServiceUnavailable)

	if sc.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (first call recorded)", sc.Code)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code = %d, want 200 (later WriteHeader calls must not be forwarded; got overwritten to %d)", inner.Code, inner.Code)
	}

	// A Write after the explicit first WriteHeader must not change anything.
	_, _ = sc.Write([]byte("body"))
	if sc.Code != http.StatusOK {
		t.Errorf("Code after write = %d, want 200", sc.Code)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code after write = %d, want 200", inner.Code)
	}
}

// failWriteWriter fails the FIRST underlying Write (0 bytes, or `partial`
// bytes before the error) and forwards every later call — the simulation of
// a client connection that dies on the first event write.
type failWriteWriter struct {
	http.ResponseWriter
	failed  bool
	partial int
}

func (f *failWriteWriter) Write(p []byte) (int, error) {
	if !f.failed {
		f.failed = true
		if f.partial > 0 {
			return f.partial, errors.New("simulated partial write failure")
		}
		return 0, errors.New("simulated first-write failure")
	}
	return f.ResponseWriter.Write(p)
}

// TestStatusCapture_Write_ZeroByteFirstWriteFailureKeepsUncommitted pins
// item 8 rework: when the FIRST underlying Write fails with 0 bytes, the
// implicit 200 must NOT be recorded — the response is still uncommitted, so
// the handler can answer a real error status (e.g. 502) and the audit is
// not locked into an empty 200.
func TestStatusCapture_Write_ZeroByteFirstWriteFailureKeepsUncommitted(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: &failWriteWriter{ResponseWriter: inner}}

	n, err := sc.Write([]byte("event"))
	if err == nil {
		t.Fatal("the failing underlying write must return its error")
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 (no bytes were delivered)", n)
	}
	if sc.Code != 0 {
		t.Errorf("Code after a 0-byte failed first write = %d, want 0 (uncommitted)", sc.Code)
	}
	if sc.Committed() {
		t.Error("Committed() must be false after a 0-byte failed first write")
	}

	// The status is still ours to choose: a later WriteHeader must take
	// effect (the 502 path depends on this).
	sc.WriteHeader(http.StatusBadGateway)
	if sc.Code != http.StatusBadGateway {
		t.Errorf("Code after WriteHeader = %d, want 502", sc.Code)
	}
	if inner.Code != http.StatusBadGateway {
		t.Errorf("inner recorder code = %d, want 502 (the error status must reach the client)", inner.Code)
	}
}

// TestStatusCapture_Write_PartialWriteFailureCommits200 pins the other half:
// a first write that delivers SOME bytes and then fails IS committed — the
// response is under way and net/http can never change the status afterwards.
func TestStatusCapture_Write_PartialWriteFailureCommits200(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: &failWriteWriter{ResponseWriter: inner, partial: 2}}

	n, err := sc.Write([]byte("event"))
	if err == nil {
		t.Fatal("the partial-failing write must return its error")
	}
	if n != 2 {
		t.Errorf("n = %d, want 2 (two bytes were delivered)", n)
	}
	if sc.Code != http.StatusOK {
		t.Errorf("Code after a partial failed write = %d, want 200 (committed once bytes were delivered)", sc.Code)
	}
	if !sc.Committed() {
		t.Error("Committed() must be true after a partial failed write")
	}

	// A later WriteHeader must NOT reach the underlying writer (the status
	// is already committed; net/http ignores it too). The recorder's Code
	// defaults to 200, so a forwarded WriteHeader(503) would be visible as
	// 503.
	sc.WriteHeader(http.StatusServiceUnavailable)
	if sc.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (the committed status must win)", sc.Code)
	}
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code = %d, want 200 (later WriteHeader must not be forwarded after commit)", inner.Code)
	}
}

// TestStatusCapture_Write_SuccessfulWriteCommits200 guards the happy path
// after the rework: a successful write records the implicit 200 exactly once
// and stays committed.
func TestStatusCapture_Write_SuccessfulWriteCommits200(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	n, err := sc.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if sc.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", sc.Code)
	}
	if !sc.Committed() {
		t.Error("Committed() must be true after a successful write")
	}
	if inner.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", inner.Body.String())
	}
}

// TestStatusCapture_Write_ZeroLengthWriteStaysUncommitted pins the empty-
// write edge: a zero-length write that succeeds delivers no bytes, so the
// status stays uncommitted.
func TestStatusCapture_Write_ZeroLengthWriteStaysUncommitted(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	n, err := sc.Write(nil)
	if err != nil || n != 0 {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if sc.Code != 0 {
		t.Errorf("Code after a 0-byte write = %d, want 0 (nothing committed)", sc.Code)
	}
}

// ---------------------------------------------------------------------------
// Round-4 audit: ApplyUsage gate consistency (item 13)
// ---------------------------------------------------------------------------

// TestApplyUsage_AcceptsCacheOnlyUsage pins item 13: a usage carrying ONLY
// cache tokens (zero prompt/completion) must be applied — a cache-only hit
// still consumes upstream quota and costs money.
func TestApplyUsage_AcceptsCacheOnlyUsage(t *testing.T) {
	a := &RequestAudit{}
	a.ApplyUsage(usagemeta.Usage{Cached: 50, Source: usagemeta.SourceAnthropic})
	if a.CachedTokens != 50 {
		t.Errorf("CachedTokens = %d, want 50", a.CachedTokens)
	}
	if a.PromptTokens != 0 || a.CompletionTokens != 0 {
		t.Errorf("cache-only usage must not fabricate prompt/completion tokens: prompt=%d completion=%d", a.PromptTokens, a.CompletionTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want anthropic", a.UsageSource)
	}
	// Legacy aliases stay in sync.
	if a.TokensIn != 0 || a.TokensOut != 0 {
		t.Errorf("legacy aliases out of sync: %+v", a)
	}
}

// TestApplyUsage_IgnoresFullyZeroUsage pins the other half of the gate: a
// fully zero usage (parsers return this for unparseable/empty payloads)
// must not clobber already-captured counts.
func TestApplyUsage_IgnoresFullyZeroUsage(t *testing.T) {
	a := &RequestAudit{PromptTokens: 5, CachedTokens: 7, UsageSource: usagemeta.SourceOpenAI}
	a.ApplyUsage(usagemeta.Usage{})
	if a.PromptTokens != 5 || a.CachedTokens != 7 {
		t.Errorf("zero usage must be ignored, got %+v", a)
	}
	if a.UsageSource != usagemeta.SourceOpenAI {
		t.Errorf("zero usage must not clear UsageSource, got %q", a.UsageSource)
	}
}

// ---------------------------------------------------------------------------
// Final-review: StatusCapture Flush / Unwrap / ResponseController compat
// ---------------------------------------------------------------------------

// flushCountingRecorder is an httptest.ResponseRecorder that also counts
// Flush calls, standing in for a real streaming ResponseWriter.
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCountingRecorder) Flush() { f.flushes++ }

// TestStatusCapture_FlushForwards: Flush must be transparently forwarded to
// an inner writer implementing http.Flusher — SSE streaming depends on it
// (without the explicit Flush method the promoted interface does not expose
// it to type-assertion callers).
func TestStatusCapture_FlushForwards(t *testing.T) {
	inner := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	sc := &StatusCapture{ResponseWriter: inner}

	// Compile-time assertion: StatusCapture must implement http.Flusher
	// (the whole point of the explicit method — interface promotion alone
	// would not expose it to type-assertion callers).
	var _ http.Flusher = sc
	fl := http.Flusher(sc)
	fl.Flush()
	fl.Flush()
	if inner.flushes != 2 {
		t.Errorf("inner flushes = %d, want 2 (Flush must reach the inner writer)", inner.flushes)
	}
}

// nonFlusherWriter is a bare ResponseWriter WITHOUT Flush — it models an
// inner writer that does not support streaming.
type nonFlusherWriter struct {
	code int
}

func (w *nonFlusherWriter) Header() http.Header         { return http.Header{} }
func (w *nonFlusherWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nonFlusherWriter) WriteHeader(code int)        { w.code = code }

// TestStatusCapture_FlushWithoutFlusher: Flush on an inner writer that does
// NOT implement http.Flusher must be a safe no-op (no panic, no nil
// dereference).
func TestStatusCapture_FlushWithoutFlusher(t *testing.T) {
	inner := &nonFlusherWriter{}
	sc := &StatusCapture{ResponseWriter: inner}

	var _ http.Flusher = sc // must still implement http.Flusher even when the inner writer does not
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Flush panicked on a non-flusher inner writer: %v", r)
		}
	}()
	sc.Flush() // must not panic
}

// TestStatusCapture_Unwrap: Unwrap must return the inner ResponseWriter so
// controller-style wrappers (http.NewResponseController) can reach the
// original writer.
func TestStatusCapture_Unwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	sc := &StatusCapture{ResponseWriter: inner}

	if sc.Unwrap() != inner {
		t.Errorf("Unwrap() = %v, want the inner recorder", sc.Unwrap())
	}
}

// TestStatusCapture_ResponseController: http.NewResponseController must
// resolve Flush through StatusCapture's Unwrap chain (Go 1.20+ ResponseWriter
// unwrapping) so standard-library streaming helpers keep working.
func TestStatusCapture_ResponseController(t *testing.T) {
	inner := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	sc := &StatusCapture{ResponseWriter: inner}

	ctrl := http.NewResponseController(sc)
	if err := ctrl.Flush(); err != nil {
		t.Errorf("ResponseController.Flush through StatusCapture: %v", err)
	}
	if inner.flushes != 1 {
		t.Errorf("inner flushes = %d, want 1 (controller must reach the flusher via Unwrap)", inner.flushes)
	}

	// The controller must correctly resolve an unsupported capability
	// through the wrapper chain (the inner recorder is not a Hijacker):
	// the error must be the not-supported error, not a panic or a nil
	// success.
	if _, _, err := http.NewResponseController(sc).Hijack(); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("ResponseController.Hijack = %v, want a not-supported error (resolved through Unwrap)", err)
	}
}
