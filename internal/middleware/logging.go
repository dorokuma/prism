package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/dorokuma/prism/internal/usagemeta"
	"github.com/dorokuma/prism/internal/util"
)

// Logger is the package-level logger used by InitLogger, EmitAudit, etc.
var Logger *slog.Logger

// LogLevel is the package-level log level variable.
var LogLevel slog.LevelVar

func init() {
	// Default logger: LevelInfo JSON to stderr.
	LogLevel.Set(slog.LevelInfo)
	Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &LogLevel,
	}))
}

// InitLogger initialises the global slog logger with the given level string.
// "debug" / "info" / "warn" / "error" (case-insensitive). Defaults to "info".
func InitLogger(level string) {
	l := ParseLevel(level)
	LogLevel.Set(l)
	Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &LogLevel,
	}))
	slog.SetDefault(Logger)
}

// SetLogLevel changes the log level at runtime (for SIGHUP hot-reload).
func SetLogLevel(level string) {
	l := ParseLevel(level)
	LogLevel.Set(l)
}

// ParseLevel converts a log level string to slog.Level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ---------------------------------------------------------------------------
// Request audit log — structured single-line per-request summary emitted at
// request completion.  Fields are populated across the call chain and flushed
// once in a deferred EmitAudit at the top-level handler.
// ---------------------------------------------------------------------------

// RequestAudit carries per-request fields for the final request.complete log line.
// The legacy fields (Req..Concurrency) are kept for backward compatibility;
// the v2 fields below carry the json tags used by the audit serialization.
// KeyID holds the API key NAME — tokens must NEVER be stored or logged.
type RequestAudit struct {
	Req         string  // X-Request-ID
	Method      string  // HTTP method
	Path        string  // URL path
	Model       string  // requested model name
	Account     string  // upstream account used (last successful or attempted)
	Error       string  // error message (empty on success)
	ErrorType   string  // short category for the error (empty on success)
	Status      int     // HTTP status written to client
	DurationMs  float64 // total wall-clock duration in milliseconds
	TokensIn    int     // prompt/input tokens consumed
	TokensOut   int     // completion/output tokens produced
	Concurrency int     // in-flight count on the selected account at select time

	Provider string `json:"provider,omitempty"` // upstream provider name
	KeyID    string `json:"key_id,omitempty"`   // authenticated API key NAME (never the token)
	Stream   bool   `json:"stream,omitempty"`   // streaming request
	Success  bool   `json:"success,omitempty"`  // request completed without error
	// UpstreamModel is the model name actually sent to the upstream after
	// model_remap resolution. It is set ONLY when remap changed the model
	// (empty otherwise — remap disabled, or the model is not remapped), so
	// an audit line without it carries the requested model in Model. The
	// cost pricer prices the upstream model first, with a fallback to the
	// requested model (see the wiring-stage Price).
	UpstreamModel    string  `json:"upstream_model,omitempty"`     // resolved upstream model after model remap
	PromptTokens     int     `json:"prompt_tokens,omitempty"`      // prompt/input tokens (v2 alias of TokensIn)
	CompletionTokens int     `json:"completion_tokens,omitempty"`  // completion/output tokens (v2 alias of TokensOut)
	TotalTokens      int     `json:"total_tokens,omitempty"`       // prompt + completion
	CachedTokens     int     `json:"cached_tokens,omitempty"`      // tokens served from upstream cache
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`   // reasoning/thinking tokens
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"` // tokens written to upstream cache
	UsageSource      string  `json:"usage_source,omitempty"`       // upstream usage wire format: openai / anthropic
	CostUSD          float64 `json:"cost_usd,omitempty"`           // estimated cost in USD (filled by the injected pricer)
	CostStatus       string  `json:"cost_status,omitempty"`        // cost estimation status ("ok" / "no_usage" / "missing_price")
	// cost is the exact *float64 computed by the injected pricer on the
	// synchronous path (nil = no known price). It is the source of truth
	// for the usage event, so the persisted value is bit-identical to the
	// logged CostUSD; unexported so it never leaks into JSON/log output.
	cost *float64
}

// AuditKey is the context key for *RequestAudit.
type AuditKey struct{}

// AuditFromCtx retrieves *RequestAudit from ctx, or nil when absent (nil-safe).
func AuditFromCtx(ctx context.Context) *RequestAudit {
	a, _ := ctx.Value(AuditKey{}).(*RequestAudit)
	return a
}

// ApplyUsage copies token usage from u into the audit record, keeping the
// legacy TokensIn/TokensOut aliases in sync with the v2 fields. It is the
// single fill point used by every capture path (non-streaming, streaming,
// OpenAI form, Anthropic form) so the legacy and v2 fields cannot diverge.
//
// The gate accepts any usage carrying at least one non-zero token count —
// including a usage with ONLY cache tokens (cache-only hits still consume
// upstream quota and cost money). A fully zero usage (the parsers return
// zero Usage for unparseable/empty payloads) is ignored: applying it would
// clobber previously captured counts with zeros.
func (a *RequestAudit) ApplyUsage(u usagemeta.Usage) {
	if !u.HasTokens() {
		return
	}
	a.TokensIn = u.Prompt
	a.TokensOut = u.Completion
	a.PromptTokens = u.Prompt
	a.CompletionTokens = u.Completion
	a.TotalTokens = u.Total
	a.CachedTokens = u.Cached
	a.ReasoningTokens = u.Reasoning
	a.CacheWriteTokens = u.CacheWrite
	a.UsageSource = u.Source
}

// ---------------------------------------------------------------------------
// Usage persistence — injected recorder. middleware deliberately does not
// import internal/usage: it defines the minimal record contract below and
// the wiring stage (cmd/prism) injects an implementation that maps
// UsageEvent onto usage.Event, resolves the model price from the current
// config, and forwards to the usage.Recorder.
// ---------------------------------------------------------------------------

// UsageEvent is the minimal per-request usage record handed to the injected
// UsageRecorder. It mirrors the fields usage.Event needs; defined here so
// middleware never depends on internal/usage. KeyID carries the API key
// NAME — never the token.
type UsageEvent struct {
	RequestID        string
	Path             string
	Model            string
	Provider         string
	Account          string
	KeyID            string
	Stream           bool
	Success          bool
	Status           int
	ErrorType        string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	ReasoningTokens  int64
	CacheWriteTokens int64
	DurationMS       float64
	Source           string // upstream usage wire format: openai / anthropic
	// Cost and CostStatus are computed ONCE on the synchronous request path
	// by the injected pricer (SetUsagePricer) and carried through to the
	// usage layer, which persists them as-is: the audit log and the database
	// store exactly the same amount. Cost nil means the model had no known
	// price (persisted as NULL with cost_status missing_price).
	Cost       *float64
	CostStatus string
}

// UsageRecorder persists one per-request usage record. Implementations must
// never block and never return errors: persistence failures are logged and
// counted internally so the request finalization path is never affected.
type UsageRecorder interface {
	Record(e UsageEvent)
}

// usageRecorder is the injected implementation; nil means usage recording is
// disabled (no-op). Installed via SetUsageRecorder at startup.
var usageRecorder UsageRecorder

// SetUsageRecorder installs the usage recorder implementation; nil clears it
// (usage recording disabled). Called once by the wiring stage.
func SetUsageRecorder(r UsageRecorder) {
	usageRecorder = r
}

// usageDefaultKeyID is the key_id recorded when a request carries no
// authenticated key name (auth disabled, or the request was rejected before
// key attribution). It is installed from config (usage.default_key_id) by the
// wiring stage; the zero state is "anonymous", matching the historical
// auth-disabled behavior. prism deliberately does not hard-code any client
// name: a deployment that wants a specific label configures it.
//
// It is an atomic.Value (holding a string) because the SIGHUP reload path
// (cmd/prism) calls SetUsageDefaultKeyID while request goroutines read it in
// EmitAudit — a plain variable would be a data race. The zero state is
// installed in init below; a Load on an uninitialized Value would panic, so
// the value is never left empty.
var usageDefaultKeyID atomic.Value

func init() {
	usageDefaultKeyID.Store("anonymous")
}

// SetUsageDefaultKeyID installs the key_id used for requests without an
// authenticated key name. Called by the wiring stage from
// config.UsageConfig.DefaultKeyID (and again on SIGHUP reload). An empty
// value is ignored so the recorded key_id can never be forced to "" (an
// empty key_id would split one GROUP BY group into two). Concurrently safe
// with EmitAudit readers.
func SetUsageDefaultKeyID(id string) {
	if id != "" {
		usageDefaultKeyID.Store(id)
	}
}

// usageDefaultKeyIDValue returns the current default key_id (never "").
func usageDefaultKeyIDValue() string {
	v, _ := usageDefaultKeyID.Load().(string)
	if v == "" {
		return "anonymous"
	}
	return v
}

// UsagePricer computes the USD cost for one audit record on the synchronous
// request finalization path, BEFORE the request.complete line is written, so
// the logged amount and the persisted amount are the same value (single
// pricing point: implementations delegate to usage.ComputeCost). It is a
// pure function of the audit fields (config lookup + float arithmetic); a
// nil cost means the model has no known price.
type UsagePricer func(a *RequestAudit) (cost *float64, status string)

// usagePricer is the injected pricer; nil means cost computation is disabled
// (the audit log and usage rows carry no cost).
var usagePricer UsagePricer

// SetUsagePricer installs the cost pricer; nil disables cost computation.
// Called once by the wiring stage, together with SetUsageRecorder.
func SetUsagePricer(p UsagePricer) {
	usagePricer = p
}

// EmitAudit writes a single-line structured log at INFO level for the given
// audit record and hands the record to the injected usage recorder (the sole
// write path into the usage store). It is a no-op when a is nil.
func EmitAudit(a *RequestAudit) {
	if a == nil {
		return
	}
	// key_id must never be persisted (or logged) as an empty string: an empty
	// and a default value would split one GROUP BY group into two rows that
	// should be merged. Requests rejected before key attribution (e.g. the
	// early 400 for a missing X-Prism-Provider header, which returns before
	// the provider/stream/key fields are filled) carry "" here; this single
	// choke point fills the configured default for every path, so the audit
	// log and the usage row agree. A real authenticated key NAME is always
	// non-empty and is never overwritten.
	if a.KeyID == "" {
		a.KeyID = usageDefaultKeyIDValue()
	}
	// Compute the cost BEFORE the log line so the amount is visible in the
	// audit log; the same value is forwarded with the usage event below, so
	// the log and the database cannot diverge.
	if usagePricer != nil {
		applyCost(a)
	}
	slog.Info("request.complete",
		"req", a.Req,
		"method", a.Method,
		"path", a.Path,
		"model", a.Model,
		"upstream_model", a.UpstreamModel,
		"account", a.Account,
		"status", a.Status,
		"duration_ms", a.DurationMs,
		"tokens_in", a.TokensIn,
		"tokens_out", a.TokensOut,
		"concurrency", a.Concurrency,
		"provider", a.Provider,
		"key_id", a.KeyID,
		"stream", a.Stream,
		"success", a.Success,
		"prompt_tokens", a.PromptTokens,
		"completion_tokens", a.CompletionTokens,
		"total_tokens", a.TotalTokens,
		"cached_tokens", a.CachedTokens,
		"reasoning_tokens", a.ReasoningTokens,
		"cache_write_tokens", a.CacheWriteTokens,
		"cost_usd", a.CostUSD,
		"cost_status", a.CostStatus,
		"error", a.Error,
		"error_type", a.ErrorType,
	)
	if usageRecorder != nil {
		recordUsage(a)
	}
}

// applyCost fills the audit's cost fields from the injected pricer. The call
// is panic-guarded (degradation constraint): a broken pricer must never
// disturb the request finalization path — the failure is logged and counted,
// and the record is persisted without a cost (nil → NULL / missing_price)
// instead of panicking the request.
func applyCost(a *RequestAudit) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("usage: pricing panic, cost unavailable", "panic", p, "req", a.Req)
			util.RecordUsageWriteErrors()
		}
	}()
	cost, status := usagePricer(a)
	a.cost = cost
	a.CostStatus = status
	if cost != nil {
		a.CostUSD = *cost
	}
}

// recordUsage forwards the audit to the injected usage recorder. The whole
// call is panic-guarded (degradation constraint): a broken recorder must
// never disturb the request finalization path — the failure is logged and
// counted, never propagated.
func recordUsage(a *RequestAudit) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("usage: recorder panicked, usage event dropped", "panic", p, "req", a.Req)
			util.RecordUsageWriteErrors()
		}
	}()
	usageRecorder.Record(UsageEvent{
		RequestID:        a.Req,
		Path:             a.Path,
		Model:            a.Model,
		Provider:         a.Provider,
		Account:          a.Account,
		KeyID:            a.KeyID,
		Stream:           a.Stream,
		Success:          a.Success,
		Status:           a.Status,
		ErrorType:        a.ErrorType,
		PromptTokens:     int64(a.PromptTokens),
		CompletionTokens: int64(a.CompletionTokens),
		TotalTokens:      int64(a.TotalTokens),
		CachedTokens:     int64(a.CachedTokens),
		ReasoningTokens:  int64(a.ReasoningTokens),
		CacheWriteTokens: int64(a.CacheWriteTokens),
		DurationMS:       a.DurationMs,
		Source:           a.UsageSource,
		Cost:             a.cost,
		CostStatus:       a.CostStatus,
	})
}

// ---------------------------------------------------------------------------
// StatusCapture — ResponseWriter wrapper that records the HTTP status code
// written via WriteHeader without interfering with SSE/streaming Flush.
// ---------------------------------------------------------------------------

// StatusCapture wraps an http.ResponseWriter and captures the first status
// code written via WriteHeader.  Flush() is transparently forwarded to the
// inner writer when it implements http.Flusher, so SSE streaming is unaffected.
type StatusCapture struct {
	http.ResponseWriter
	Code int
}

func (s *StatusCapture) WriteHeader(code int) {
	if s.Code != 0 {
		// net/http semantics: only the FIRST WriteHeader (or implicit 200
		// via the first Write) takes effect. Later calls are no-ops and must
		// NOT be forwarded to the underlying writer — forwarding could
		// overwrite an already-committed status on wrapped writers (e.g. a
		// test recorder) and diverges from the standard library contract.
		return
	}
	s.Code = code
	s.ResponseWriter.WriteHeader(code)
}

// StatusCommitter reports whether the response status has been committed
// (headers written via WriteHeader, or an implicit status from a
// successful/partial Write). Committed() == false means nothing reached the
// client yet, so the handler may still choose a real error status (e.g. a
// structured 502) instead of being locked into an empty 200. Handlers that
// need to know the commit state must depend on this stable interface — not
// on the concrete *StatusCapture type.
type StatusCommitter interface {
	Committed() bool
}

// Committed reports whether the status has been committed (see
// StatusCommitter).
func (s *StatusCapture) Committed() bool { return s.Code != 0 }

// Write implements http.ResponseWriter and records the implicit 200 ONLY
// when the underlying write actually commits bytes (n > 0). A first write
// that fails with 0 bytes leaves the status uncommitted (Code == 0), so the
// caller can still answer a real error status (e.g. 502) instead of being
// locked into an empty 200 — a partial write (n > 0 with an error) IS
// committed, mirroring net/http: once the first byte/header reached the
// wire the status can never be changed. An explicit status written earlier
// is never overwritten: the first recorded code wins.
func (s *StatusCapture) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	if s.Code == 0 && n > 0 {
		s.Code = http.StatusOK
	}
	return n, err
}

// Flush implements http.Flusher by forwarding to the inner ResponseWriter
// when it also implements http.Flusher.  Without this explicit method the
// embedded interface promotion does not expose Flush to type-assertion
// callers like streamResponseBody / translateChatStreamToResponses, which
// would break SSE streaming.
func (s *StatusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the inner ResponseWriter (for use with io.Writer assertions).
func (s *StatusCapture) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
