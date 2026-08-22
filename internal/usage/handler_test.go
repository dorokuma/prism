package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doRequest(h http.Handler, method, target, remoteAddr, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remoteAddr
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerAuth(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	s := openTestStore(t)
	h := NewSummaryHandler(s)

	// Fail-closed: with PRISM_ADMIN_TOKEN configured, loopback does NOT
	// bypass auth — a direct loopback request without the token is 401,
	// exactly like any remote client (a same-machine reverse proxy also
	// presents a loopback RemoteAddr, so loopback is not a trust boundary).
	for _, addr := range []string{"127.0.0.1:5555", "127.0.0.2:5555", "[::1]:5555"} {
		if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", addr, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("loopback %s without token: got %d, want 401 (fail-closed when PRISM_ADMIN_TOKEN is set)", addr, rec.Code)
		}
		// The same loopback request WITH the correct token passes.
		if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", addr, "Bearer sekret"); rec.Code != http.StatusOK {
			t.Fatalf("loopback %s with token: got %d, want 200", addr, rec.Code)
		}
	}

	// A hostname in RemoteAddr is NOT loopback (RemoteAddr is kernel-provided
	// ip:port; "localhost" never occurs in production): it must authenticate
	// like any remote client.
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "localhost:5555", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("localhost hostname without token: got %d, want 401", rec.Code)
	}

	// remote without token: denied
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote without token: got %d, want 401", rec.Code)
	}

	// remote with correct Bearer token: allowed
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Bearer sekret"); rec.Code != http.StatusOK {
		t.Fatalf("remote with bearer: got %d, want 200", rec.Code)
	}

	// remote with wrong token: denied
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with wrong bearer: got %d, want 401", rec.Code)
	}

	// remote with a much longer wrong token: denied (constant-time pad
	// comparison rejects length classes beyond the pad, like the business
	// auth path)
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Bearer "+strings.Repeat("x", 300)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with overlong bearer: got %d, want 401", rec.Code)
	}

	// remote with Basic auth (not Bearer): denied
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Basic sekret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with basic auth: got %d, want 401", rec.Code)
	}

	// remote with token as raw header, no scheme: denied
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	req.RemoteAddr = "10.1.2.3:9999"
	req.Header.Set("Authorization", "sekret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with bare token: got %d, want 401", rec.Code)
	}
}

// TestHandlerAuth_LoopbackNoTokenNoHeader401 pins the exact acceptance
// case from the fail-closed review: loopback RemoteAddr + token configured
// + no forwarding headers + no auth header => 401.
func TestHandlerAuth_LoopbackNoTokenNoHeader401(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	h := NewSummaryHandler(openTestStore(t))
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	req.RemoteAddr = "127.0.0.1:5555" // loopback, no X-Forwarded-For / X-Real-IP
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback + token configured + no header + no auth: got %d, want 401", rec.Code)
	}
	// The same request with the correct Bearer token passes.
	req2 := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
	req2.RemoteAddr = "127.0.0.1:5555"
	req2.Header.Set("Authorization", "Bearer sekret")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("loopback + token configured + correct bearer: got %d, want 200", rec2.Code)
	}
}

func TestHandlerEmptyTokenDeniesRemote(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	s := openTestStore(t)
	h := NewSummaryHandler(s)

	// no admin token configured: remote is denied even with a Bearer header
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Bearer anything"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote with unset token: got %d, want 401", rec.Code)
	}
	// localhost still allowed
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:1", ""); rec.Code != http.StatusOK {
		t.Fatalf("localhost with unset token: got %d, want 200", rec.Code)
	}
}

// TestHandlerAuth_BearerSchemeCaseInsensitive guards the admin-token scheme
// match: bearer/BEARER/BeArEr all authenticate with the exact same token,
// while a bare token and a wrong token are still rejected.
func TestHandlerAuth_BearerSchemeCaseInsensitive(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	h := NewSummaryHandler(openTestStore(t))
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", scheme+" sekret")
		if rec.Code != http.StatusOK {
			t.Errorf("scheme %q: got %d, want 200", scheme, rec.Code)
		}
	}
	// bare token (no scheme) still denied
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "sekret"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bare token: got %d, want 401", rec.Code)
	}
	// wrong token under a lowercase scheme still denied
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}
}

// TestHandlerAuth_EmptyAndWhitespaceTokenRejected guards the unified Bearer
// semantics on the admin path (shared middleware.SplitBearerToken): an empty
// credential, a whitespace-only credential, and a double-space
// "Bearer  sekret" (which the old TrimSpace implementation accepted as
// "sekret") must all be rejected — the token bytes are byte-strict, exactly
// like middleware.Authenticate / CheckAuth.
func TestHandlerAuth_EmptyAndWhitespaceTokenRejected(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	h := NewSummaryHandler(openTestStore(t))
	for _, auth := range []string{
		"Bearer ",
		"Bearer   ",
		"Bearer \t",
		"Bearer  sekret", // double space: old TrimSpace accepted this as "sekret"
		"bearer  sekret",
	} {
		if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", auth); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: got %d, want 401 (empty/whitespace/mis-spaced token must be rejected)", auth, rec.Code)
		}
	}
	// The correct single-space token still passes.
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "10.1.2.3:9999", "Bearer sekret"); rec.Code != http.StatusOK {
		t.Errorf("Bearer sekret: got %d, want 200", rec.Code)
	}
}

func TestHandlerDBUnavailable(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	h := NewSummaryHandler(nil)       // no store: must 503, not panic
	rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:1", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store: got %d, want 503", rec.Code)
	}

	// store that is not open: query errors → 503
	closed := &SQLiteStore{path: t.TempDir() + "/never.db"}
	h2 := NewSummaryHandler(closed)
	rec = doRequest(h2, http.MethodGet, "/admin/usage/summary", "127.0.0.1:1", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unopened store: got %d, want 503", rec.Code)
	}
}

func TestHandlerInvalidGroupBy(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertBatch(ctx, []Event{testEvent(time.Now(), "a", nil)}); err != nil {
		t.Fatal(err)
	}
	h := NewSummaryHandler(s)

	// injection-shaped group_by must be rejected with 400, not executed
	rec := doRequest(h, http.MethodGet, "/admin/usage/summary?group_by=model%3BDROP+TABLE+usage_events", "127.0.0.1:1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("injection group_by: got %d, want 400", rec.Code)
	}
	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatal("table damaged by rejected group_by")
	}

	// bad parameter values → 400
	for _, target := range []string{
		"/admin/usage/summary?stream=banana",
		"/admin/usage/summary?success=maybe",
		"/admin/usage/summary?from=abc",
		"/admin/usage/summary?limit=-3",
		"/admin/usage/summary?limit=zzz",
	} {
		if rec := doRequest(h, http.MethodGet, target, "127.0.0.1:1", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", target, rec.Code)
		}
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	h := NewSummaryHandler(openTestStore(t))
	rec := doRequest(h, http.MethodPost, "/admin/usage/summary", "127.0.0.1:1", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: got %d, want 405", rec.Code)
	}
}

func TestHandlerSummaryJSON(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 1, Output: 1}
	c, _ := costOf(10, 10, 0, 0, "", price)
	if err := s.InsertBatch(ctx, []Event{
		{Ts: time.Now(), RequestID: "r1", Model: "a", PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20, Cost: c, CostStatus: CostStatusOK},
		{Ts: time.Now(), RequestID: "r2", Model: "b", PromptTokens: 20, CompletionTokens: 20, TotalTokens: 40, Cost: nil, CostStatus: CostStatusMissingPrice},
	}); err != nil {
		t.Fatal(err)
	}
	h := NewSummaryHandler(s)
	rec := doRequest(h, http.MethodGet, "/admin/usage/summary?group_by=model", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("summary: got %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rows []SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows: %d, want 2", len(body.Rows))
	}
	byName := map[string]SummaryRow{}
	for _, row := range body.Rows {
		byName[row.Groups["model"].(string)] = row
	}
	a, okA := byName["a"]
	b, okB := byName["b"]
	if !okA || !okB {
		t.Fatalf("rows: %+v", body.Rows)
	}
	if a.CostUSD == nil || *a.CostUSD != 2*10.0/1e6 {
		t.Fatalf("model a cost: %v", a.CostUSD)
	}
	if b.CostUSD != nil || b.CostMissingRequests != 1 {
		t.Fatalf("model b (missing price): cost=%v missing=%d", b.CostUSD, b.CostMissingRequests)
	}
	if a.Requests != 1 || a.PromptTokens != 10 {
		t.Fatalf("model a aggregates: %+v", a)
	}

	// empty result serializes as an empty array, not null
	rec = doRequest(h, http.MethodGet, "/admin/usage/summary?model=does-not-exist", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty summary: got %d", rec.Code)
	}
	var empty struct {
		Rows []SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Rows == nil {
		t.Fatal("empty result must serialize as [] not null")
	}
}

// seedTableEvents inserts two events (one priced, one missing price) and
// returns the SummaryHandler, mirroring TestHandlerSummaryJSON's dataset.
func seedTableEvents(t *testing.T) *SummaryHandler {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 1000, Output: 1000}
	cost, _ := costOf(100, 50, 0, 0, "", price)
	if err := s.InsertBatch(ctx, []Event{
		{Ts: time.Now(), RequestID: "r1", Model: "a", Stream: true, Success: true, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Cost: cost, CostStatus: CostStatusOK},
		{Ts: time.Now(), RequestID: "r2", Model: "b", Stream: true, Success: true, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Cost: nil, CostStatus: CostStatusMissingPrice},
	}); err != nil {
		t.Fatal(err)
	}
	return NewSummaryHandler(s)
}

// TestHandlerTableFormat is the acceptance test for format=table: the
// response is text/plain with the compact single-line table, the summary
// header comes from Overview (total 300 tokens across both models), the
// missing-price request is warned about, the nil cost renders as a dash,
// and the JSON default behavior is untouched (the existing JSON tests
// still pass).
func TestHandlerTableFormat(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	h := seedTableEvents(t)

	rec := doRequest(h, http.MethodGet, "/admin/usage/summary?group_by=model&format=table", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("table: got %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	// widths: 模型 4 | 请求 4 | 输入词元 8 | 缓存 4 | 命中率 6 | 输出词元 8 | 花费 5 | 未计价 6
	want := "  全部时间  ·  2 请求  ·  300 词元  ·  $0.150\n" +
		"  流式 2 (100.0%)   缓存命中(openai) 0 (0.0%)\n" +
		"  ⚠ 有 1 个请求未算出金额（模型未配置单价），总费用可能偏低\n" +
		"\n" +
		"  模型" + strings.Repeat(" ", 1) + "请求 输入词元 缓存 命中率 输出词元  花费 未计价\n" +
		"  a" + strings.Repeat(" ", 7) + "1" + strings.Repeat(" ", 6) + "100" + strings.Repeat(" ", 4) + "0" + strings.Repeat(" ", 3) + "0.0%" + strings.Repeat(" ", 7) + "50 $0.15" + strings.Repeat(" ", 6) + "0\n" +
		"  b" + strings.Repeat(" ", 7) + "1" + strings.Repeat(" ", 6) + "100" + strings.Repeat(" ", 4) + "0" + strings.Repeat(" ", 3) + "0.0%" + strings.Repeat(" ", 7) + "50" + strings.Repeat(" ", 5) + "-" + strings.Repeat(" ", 6) + "1\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("table body mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}

	// format=table must be equivalent to the CLI renderer: the summary
	// counts come from Overview (not from the LIMIT-truncated rows).
	if !strings.Contains(rec.Body.String(), "2 请求  ·  300 词元") {
		t.Errorf("table summary must come from Overview:\n%s", rec.Body.String())
	}
}

// TestHandlerTableFormatMixedSources drives the two-segment cache summary
// through the full HTTP format=table path (InsertBatch persists usage_source
// → Overview splits → RenderUsageReport). It is the same renderer the CLI
// uses, so the exact segment line here must match the CLI output.
func TestHandlerTableFormatMixedSources(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertBatch(ctx, []Event{
		{Ts: time.Now(), RequestID: "r1", Model: "a", Source: SourceOpenAI,
			PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, CachedTokens: 900},
		{Ts: time.Now(), RequestID: "r2", Model: "b", Source: SourceAnthropic,
			PromptTokens: 1, CompletionTokens: 50, TotalTokens: 51, CachedTokens: 500},
	}); err != nil {
		t.Fatal(err)
	}
	h := NewSummaryHandler(s)
	rec := doRequest(h, http.MethodGet, "/admin/usage/summary?format=table", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("table: got %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// openai 900/1000 = 90.0%; anthropic 500/(1+500+0) = 99.8% — both
	// independent, neither above 100%.
	if !strings.Contains(body, "缓存命中(openai) 900 (90.0%)   缓存命中(anthropic) 500 (99.8%)") {
		t.Errorf("split cache segments missing or wrong:\n%s", body)
	}
	// Ungrouped table row: 1400 hits over openai prompt 1000 + anthropic
	// assembled 501 = 1501 → 93.3%. cached/prompt (1400/1001 = 139.9%)
	// is the bug this path used to show.
	if !strings.Contains(body, "93.3%") {
		t.Errorf("ungrouped table hit rate must be 1400/1501 = 93.3%%:\n%s", body)
	}
	if strings.Contains(body, "139.9%") || strings.Contains(body, "50000") {
		t.Errorf("table hit rate still uses cached/prompt:\n%s", body)
	}
	rec = doRequest(h, http.MethodGet, "/admin/usage/summary?group_by=model&format=table", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("table by model: got %d, body %s", rec.Code, rec.Body.String())
	}
	byModel := rec.Body.String()
	if !strings.Contains(byModel, "90.0%") || !strings.Contains(byModel, "99.8%") {
		t.Errorf("per-model table must keep openai 90.0%% and anthropic 99.8%%:\n%s", byModel)
	}
	if strings.Contains(byModel, "50000") {
		t.Errorf("anthropic model row still uses cached/prompt:\n%s", byModel)
	}
	// JSON default is untouched: the same dataset via the default format
	// still serves the pre-existing row fields (ungrouped → one aggregate
	// row).
	rec = doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("json: got %d", rec.Code)
	}
	var body2 struct {
		Rows []SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if len(body2.Rows) != 1 {
		t.Fatalf("json rows = %d, want 1 ungrouped aggregate", len(body2.Rows))
	}
	if body2.Rows[0].Requests != 2 || body2.Rows[0].PromptTokens != 1001 || body2.Rows[0].CachedTokens != 1400 {
		t.Errorf("json aggregate broken by the split: %+v", body2.Rows[0])
	}
	if body2.Rows[0].HitRateInputTokens != 1501 {
		t.Errorf("hit_rate_input_tokens = %d, want 1501 (1000 + 1+500+0)", body2.Rows[0].HitRateInputTokens)
	}
}

func TestHandlerTableNoData(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	h := NewSummaryHandler(openTestStore(t))
	// group_by=model on an empty store → zero rows → the table renders the
	// no-data hint; the summary still renders from Overview.
	rec := doRequest(h, http.MethodGet, "/admin/usage/summary?group_by=model&format=table", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty table: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "全部时间  ·  0 请求") {
		t.Errorf("empty-range summary missing:\n%s", body)
	}
	if !strings.Contains(body, "(no data)") {
		t.Errorf("no-data hint missing:\n%s", body)
	}
}

// TestHandlerTableFormatValidation pins format=table validation: the
// format value is case-insensitive, anything else is a 400, an invalid
// group_by still 400s, and auth is unchanged for table output.
func TestHandlerTableFormatValidation(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "") // unset: direct loopback allowed
	h := seedTableEvents(t)
	// format=JSON (uppercase) is accepted case-insensitively
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary?format=JSON", "127.0.0.1:1", ""); rec.Code != http.StatusOK {
		t.Errorf("format=JSON: got %d, want 200", rec.Code)
	}
	// any other value is a client error → 400 (including a value with
	// surrounding whitespace, which is trimmed before validation)
	for _, f := range []string{"xml", "tablex", "html", "csv%20", "jsonx"} {
		if rec := doRequest(h, http.MethodGet, "/admin/usage/summary?format="+f, "127.0.0.1:1", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("format=%q: got %d, want 400", f, rec.Code)
		}
	}
	// format=table with an invalid group_by still 400s (validation unchanged)
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary?format=table&group_by=DROP+TABLE", "127.0.0.1:1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("format=table + bad group_by: got %d, want 400", rec.Code)
	}
	// auth is unchanged for table output: remote without token → 401
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary?format=table", "10.1.2.3:9999", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("table from remote without token: got %d, want 401", rec.Code)
	}
}

// TestHandlerAuth_ForwardHeadersRequireToken guards the same-machine reverse
// proxy case: a loopback RemoteAddr carrying X-Forwarded-For or X-Real-IP is
// NOT a direct local client and must present PRISM_ADMIN_TOKEN (mirrors the
// /metrics rule). With the token configured, auth is fail-closed: even a
// direct loopback request WITHOUT forwarding headers must present it.
func TestHandlerAuth_ForwardHeadersRequireToken(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	s := openTestStore(t)
	h := NewSummaryHandler(s)

	// Direct loopback without forwarding headers: fail-closed, the token is
	// still required when PRISM_ADMIN_TOKEN is configured.
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("direct loopback without token: got %d, want 401 (fail-closed)", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", "Bearer sekret"); rec.Code != http.StatusOK {
		t.Fatalf("direct loopback with token: got %d, want 200", rec.Code)
	}

	for name, val := range map[string]string{"X-Forwarded-For": "10.0.0.9", "X-Real-IP": "10.0.0.9"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set(name, val)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("loopback with %s and no token: got %d, want 401", name, rec.Code)
		}

		req2 := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
		req2.RemoteAddr = "127.0.0.1:5555"
		req2.Header.Set(name, val)
		req2.Header.Set("Authorization", "Bearer sekret")
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Errorf("loopback with %s and correct token: got %d, want 200", name, rec2.Code)
		}
	}
}

// TestHandlerAuth_ForwardedHeaderRequiresToken extends the forwarding-header
// gate to the RFC 7239 Forwarded header: a loopback request carrying
// Forwarded is a proxied request and must present PRISM_ADMIN_TOKEN even
// when the token-free loopback path would otherwise apply.
func TestHandlerAuth_ForwardedHeaderRequiresToken(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	s := openTestStore(t)
	h := NewSummaryHandler(s)

	// Token unset + direct loopback: allowed.
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", ""); rec.Code != http.StatusOK {
		t.Fatalf("direct loopback without token: got %d, want 200", rec.Code)
	}

	// Token unset + loopback with the Forwarded header: denied (proxied
	// request cannot be distinguished from remote without a token).
	for _, val := range []string{"for=10.0.0.9;proto=https", "for=10.0.0.9"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/usage/summary", nil)
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("Forwarded", val)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("loopback with Forwarded=%q and no token: got %d, want 401", val, rec.Code)
		}
	}
}

// TestHandlerAuth_TokenHotReload pins the per-request PRISM_ADMIN_TOKEN
// read: a token set or rotated AFTER the handler was constructed takes
// effect immediately (no restart, no stale-token window) — the same
// behavior as the /metrics METRICS_TOKEN path.
func TestHandlerAuth_TokenHotReload(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	s := openTestStore(t)
	h := NewSummaryHandler(s)

	// No token configured: direct loopback passes without one.
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", ""); rec.Code != http.StatusOK {
		t.Fatalf("no token: direct loopback got %d, want 200", rec.Code)
	}

	// Configure the token AFTER construction: every request must now
	// present it (fail-closed), even direct loopback.
	t.Setenv("PRISM_ADMIN_TOKEN", "hot-token")
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token set after construction: direct loopback without it got %d, want 401", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", "Bearer hot-token"); rec.Code != http.StatusOK {
		t.Fatalf("token set after construction: request with it got %d, want 200", rec.Code)
	}

	// Rotate the token: the old token stops working, the new one works.
	t.Setenv("PRISM_ADMIN_TOKEN", "rotated-token")
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", "Bearer hot-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token rotated: stale token got %d, want 401", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", "Bearer rotated-token"); rec.Code != http.StatusOK {
		t.Fatalf("token rotated: new token got %d, want 200", rec.Code)
	}

	// Unset the token again: direct loopback passes without one (the
	// loopback shortcut returns).
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	if rec := doRequest(h, http.MethodGet, "/admin/usage/summary", "127.0.0.1:5555", ""); rec.Code != http.StatusOK {
		t.Fatalf("token unset: direct loopback got %d, want 200 (loopback shortcut restored)", rec.Code)
	}
}
