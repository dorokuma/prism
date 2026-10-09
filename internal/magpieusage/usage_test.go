package magpieusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// usageFixtureRow is one usage.jsonl line for the fixtures below.
//
// keyID is the providerKeyId magpie stamped on the line ("" = the field is
// left OUT of the line, the shape of the unattributed production rows).
// providerAccount and host are the masked account labels magpie writes on its
// keyless `cline` lines (`API key …326d` / `cline as API key …326d`, the
// ellipsis being one \u2026 character); they are omitted from the line when
// empty. cacheRead, when set, is written as magpie's cache_read: the side of
// the line the per-account sum must NOT count. omit drops field names from the
// line entirely (the drift cases), raw replaces the whole line (garbage and
// mis-typed cases).
type usageFixtureRow struct {
	keyID           string
	model           string
	provider        string
	providerAccount string
	host            string
	at              time.Time
	in              int64
	out             int64
	cacheRead       *int64
	omit            []string
	raw             string
}

func i64p(v int64) *int64 { return &v }

// line renders the row as one JSON object. Fields are written explicitly (not
// through the production struct) so a fixture pins the FILE shape, not the
// implementation's own decoding.
func (r usageFixtureRow) line(t *testing.T) string {
	t.Helper()
	if r.raw != "" {
		return r.raw
	}
	provider := r.provider
	if provider == "" {
		provider = clinePassProviderID
	}
	model := r.model
	if model == "" {
		model = clinePassModelPrefix + "x"
	}
	m := map[string]any{
		"t":        r.at.Format(time.RFC3339Nano),
		"provider": provider,
		"model":    model,
		"in":       r.in,
		"out":      r.out,
	}
	if r.keyID != "" {
		m["providerKeyId"] = r.keyID
	}
	if r.providerAccount != "" {
		m["providerAccount"] = r.providerAccount
	}
	if r.host != "" {
		m["host"] = r.host
	}
	if r.cacheRead != nil {
		m["cache_read"] = *r.cacheRead
	}
	for _, k := range r.omit {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeUsageLog writes a usage.jsonl fixture (one line per row, newline
// terminated) and returns its path.
func writeUsageLog(t *testing.T, rows ...usageFixtureRow) string {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.line(t))
		b.WriteString("\n")
	}
	return writeUsageLogText(t, b.String())
}

// writeUsageLogText writes raw file content — the only way to build a TORN
// TAIL (a last line without its newline), which is why it exists next to
// writeUsageLog.
func writeUsageLogText(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestSource returns a source over a usage log fixture. providersPath is
// irrelevant to the sums, so it points at a path that does not exist (the
// sums must not need it).
func newTestSource(t *testing.T, usagePath string) *Source {
	t.Helper()
	return NewSource(usagePath, filepath.Join(t.TempDir(), "providers.json"))
}

// testWindowBase is the fixture anchor: 2026-09-22 12:00:00 UTC.
var testWindowBase = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// TestSumFiltersProviderModelAndWindow pins the selection contract of the
// usage log: the model gate (model starts with "cline-pass/") is the first and
// hardest one, a record must fall inside the window, both window bounds are
// inclusive, and the window is compared at full timestamp precision (a line at
// to+0.5 s shares the `to` SECOND but is outside the window, while a line at
// from+0.5 s is inside it). The provider id is NOT a gate for a line that
// carries a providerKeyId: Track 1 places such a line by the id magpie stamped
// on it (one production line carries provider == "metapi" with a cline-pass/
// model, measured 2026-10-07), which is why the foreign-provider fixtures below
// are KEYLESS — for those the provider id decides what may be attributed at
// all.
func TestSumFiltersProviderModelAndWindow(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"

	path := writeUsageLog(t,
		// counted: prompt + completion, with magpie's cache_read on the line
		// and deliberately NOT in the molecule (60 + 40, not 9860).
		usageFixtureRow{keyID: id, model: "cline-pass/deepseek-v4.1-flash", at: testWindowBase.Add(10 * time.Minute), in: 60, out: 40, cacheRead: i64p(9_800)},
		// counted: both bounds are inclusive instants.
		usageFixtureRow{keyID: id, at: testWindowBase, in: 1},
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Hour), in: 2},
		// counted: half a second after `from` is inside the window.
		usageFixtureRow{keyID: id, at: testWindowBase.Add(500 * time.Millisecond), in: 4},
		// counted: a cline-pass/* MODEL with an id belongs to that id whatever
		// the provider value says (the model gate already proved it is a
		// ClinePass model).
		usageFixtureRow{keyID: id, provider: "metapi", at: testWindowBase.Add(11 * time.Minute), in: 8},
		// excluded: half a second before `from`, and half a second after `to`
		// — the latter shares the `to` second, so a second-granular
		// comparison would wrongly count it.
		usageFixtureRow{keyID: id, at: testWindowBase.Add(-500 * time.Millisecond), in: 1_000},
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Hour + 500*time.Millisecond), in: 2_000},
		// excluded: another model family, a model with the prefix but not the
		// slash separator, a similar-looking model, and keyless rows of
		// providers that are neither `clinepass` (Track 1's provider) nor
		// `cline` (Track 2's): another provider's, and a line magpie recorded
		// with an empty provider value.
		usageFixtureRow{keyID: id, model: "gpt-5", at: testWindowBase.Add(time.Minute), in: 5_000},
		usageFixtureRow{keyID: id, model: "cline-pass-extra/x", at: testWindowBase.Add(time.Minute), in: 5_000},
		usageFixtureRow{keyID: id, model: "xcline-pass/x", at: testWindowBase.Add(time.Minute), in: 5_000},
		usageFixtureRow{provider: "gemini", model: "cline-pass/x", at: testWindowBase.Add(time.Minute), in: 5_000},
		usageFixtureRow{provider: " ", model: "cline-pass/x", at: testWindowBase.Add(time.Minute), in: 5_000},
	)

	src := newTestSource(t, path)
	got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got != 115 {
		t.Fatalf("sum = %d, want 115 (60+40 with cache dropped, +1 and +2 on the inclusive bounds, +4 inside the first second, +8 for the keyed foreign-provider line)", got)
	}

	// A non-positive lower bound is a no-op, matching usage.SumTokensLike.
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, 0, to); err != nil || got != 0 {
		t.Fatalf("from<=0: got (%d, %v), want (0, nil)", got, err)
	}
	// An empty window sums to 0, not an error (the account exists, the window
	// is just empty).
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, to+10, to+20); err != nil || got != 0 {
		t.Fatalf("empty window: got (%d, %v), want (0, nil)", got, err)
	}
}

// TestSumClinePassTokensByAccount pins per-account isolation and the
// molecule. Isolation: only the records magpie attributed to the requested
// providerKeyId count — another account's ClinePass traffic in the SAME
// window must not leak in (each subscription's pool is its own) and rows
// magpie could not attribute are in no account's bucket. Molecule: in + out
// (prompt + completion), with magpie's cache_read EXCLUDED.
func TestSumClinePassTokensByAccount(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const idA, idB = "bdf422fa85", "26b5925350"

	path := writeUsageLog(t,
		// account A: counted (prompt + completion).
		usageFixtureRow{keyID: idA, at: testWindowBase.Add(10 * time.Minute), in: 60, out: 40},
		// account A: a completion-only line (out != 0, in == 0) still counts.
		usageFixtureRow{keyID: idA, at: testWindowBase.Add(20 * time.Minute), in: 7, out: 3},
		// account A, cache-heavy: 9800 cache-read tokens on the line, and the
		// sum MUST count 20 + 0 — the upstream percent it is reversed against
		// is a prompt+completion 口径, so a molecule that took cache_read (or
		// any stored total) would inflate the reversed pool.
		usageFixtureRow{keyID: idA, at: testWindowBase.Add(25 * time.Minute), in: 20, cacheRead: i64p(9_800)},
		// account B in the same window: excluded from account A's sum.
		usageFixtureRow{keyID: idB, at: testWindowBase.Add(15 * time.Minute), in: 5_000, cacheRead: i64p(4_999)},
		// No providerKeyId: in nobody's per-account bucket. This is the legacy
		// unattributed shape (provider == clinepass) — magpie's keyless `cline`
		// rows are attributed by Track 2 when their masked account label names
		// a roster key, and one that names none (a rotated key, another
		// subscription) stays here too; see TestSumAttributionDualTrack.
		usageFixtureRow{at: testWindowBase.Add(16 * time.Minute), in: 5_000},
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026zzzz", at: testWindowBase.Add(17 * time.Minute), in: 5_000},
		// account A outside the window / with another model: excluded.
		usageFixtureRow{keyID: idA, at: testWindowBase.Add(-time.Second), in: 1_000},
		usageFixtureRow{keyID: idA, model: "gpt-5", at: testWindowBase.Add(5 * time.Minute), in: 2_000},
	)

	src := newTestSource(t, path)
	ctx := context.Background()

	if got, err := src.SumClinePassTokensByAccount(ctx, idA, from, to); err != nil || got != 130 {
		t.Fatalf("account A sum = (%d, %v), want (130, nil) — prompt+completion only", got, err)
	}
	if got, err := src.SumClinePassTokensByAccount(ctx, idB, from, to); err != nil || got != 5_000 {
		t.Fatalf("account B sum = (%d, %v), want (5000, nil) — the other account must not leak", got, err)
	}
	// An account with no traffic in the window sums to 0, not an error.
	if got, err := src.SumClinePassTokensByAccount(ctx, "ffffffffff", from, to); err != nil || got != 0 {
		t.Fatalf("unknown account sum = (%d, %v), want (0, nil)", got, err)
	}
	// A non-positive lower bound is a no-op.
	if got, err := src.SumClinePassTokensByAccount(ctx, idA, 0, to); err != nil || got != 0 {
		t.Fatalf("from<=0: got (%d, %v), want (0, nil)", got, err)
	}
	// An EMPTY account id is an error rather than a fallback to summing EVERY
	// bucket: adding both independent pools would print a 总额 belonging to
	// neither (串账). There is no account-less entry point at all — the scope of
	// a sum is always one account.
	if got, err := src.SumClinePassTokensByAccount(ctx, "", from, to); err == nil || got != 0 {
		t.Fatalf("empty account id: got (%d, %v), want (0, error)", got, err)
	}
	if got, err := src.SumClinePassTokensByAccount(ctx, "   ", from, to); err == nil || got != 0 {
		t.Fatalf("blank account id: got (%d, %v), want (0, error)", got, err)
	}
}

// TestParseUsageLineShape pins the log's line contract, the ONLY one this
// source has (magpie has no schema and no version): one JSON object per line
// carrying t (RFC3339), provider, model, in and out. Every drift case must be
// ErrUsageLine, and a well-formed non-ClinePass line must NOT be drift — it is
// simply skipped.
func TestParseUsageLineShape(t *testing.T) {
	valid := usageFixtureRow{keyID: "bdf422fa85", at: testWindowBase, in: 1, out: 1}.line(t)

	cases := []struct {
		name  string
		body  string
		drift bool
		// matched reports whether a non-drift line must be attributed.
		matched bool
	}{
		{name: "production shape", body: valid, matched: true},
		{
			// magpie's wire form on this host: nanosecond fraction and a
			// +08:00 offset.
			name:    "nanoseconds with offset",
			body:    `{"t":"2026-09-25T21:41:03.700943573+08:00","provider":"clinepass","model":"cline-pass/x","in":3,"out":4}`,
			matched: true,
		},
		{
			name:    "no fraction with zulu zone",
			body:    `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","in":3,"out":4}`,
			matched: true,
		},
		{
			name:    "out only",
			body:    `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","in":0,"out":4}`,
			matched: true,
		},
		{
			name: "another provider is skipped, not drift",
			body: `{"t":"2026-09-25T21:41:03Z","provider":"gemini","model":"cli","in":9,"out":9}`,
		},
		{
			name: "another model is skipped, not drift",
			body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"gpt-5","in":9,"out":9}`,
		},
		{name: "missing t", body: `{"provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "missing provider", body: `{"t":"2026-09-25T21:41:03Z","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "missing model", body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","in":1,"out":1}`, drift: true},
		{name: "missing in", body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","out":1}`, drift: true},
		{name: "missing out", body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","in":1}`, drift: true},
		{name: "null t", body: `{"t":null,"provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "null in", body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","in":null,"out":1}`, drift: true},
		{name: "in as a string", body: `{"t":"2026-09-25T21:41:03Z","provider":"clinepass","model":"cline-pass/x","in":"1","out":1}`, drift: true},
		{name: "t as a number", body: `{"t":1758831663,"provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "json array line", body: `[1,2]`, drift: true},
		{name: "truncated json", body: `{"t":"2026-`, drift: true},
		{name: "empty object", body: `{}`, drift: true},
		// created_at-style drift equivalents: metapi's UTC text, epoch
		// seconds, a bare date, a zone-less stamp and an HTTP date. Each one
		// would silently mis-bound a window if it were parsed leniently.
		{name: "space separated utc text", body: `{"t":"2026-09-25 21:41:03","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "epoch seconds as text", body: `{"t":"1758831663","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "bare date", body: `{"t":"2026-09-25","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "zone-less rfc3339", body: `{"t":"2026-09-25T21:41:03","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
		{name: "http date", body: `{"t":"Fri, 25 Sep 2026 21:41:03 GMT","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`, drift: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, matched, err := parseUsageLine([]byte(tc.body))
			if tc.drift {
				if !errors.Is(err, ErrUsageLine) {
					t.Fatalf("parse error = %v, want ErrUsageLine", err)
				}
				if matched {
					t.Fatal("a drifted line must never be attributed")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse error = %v, want nil", err)
			}
			if matched != tc.matched {
				t.Fatalf("matched = %v, want %v", matched, tc.matched)
			}
		})
	}
}

// TestScanRejectsDriftDespiteValidRows pins that one unreadable line makes the
// WHOLE window unavailable — older or non-matching valid lines must not mask
// it. Summing the readable fraction would silently under-report the
// consumption and inflate the reversed pool, so the scan refuses instead: this
// round produces no estimate (the caller's degraded path).
func TestScanRejectsDriftDespiteValidRows(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	validRow := usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 100}

	t.Run("drifted matching line after a valid one", func(t *testing.T) {
		path := writeUsageLog(t, validRow,
			usageFixtureRow{raw: `{"t":"2026-09-25 21:41:03","provider":"clinepass","model":"cline-pass/x","in":1,"out":1}`})
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if !errors.Is(err, ErrUsageLine) || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, ErrUsageLine)", got, err)
		}
	})

	t.Run("drift on a non-matching line still refuses the file", func(t *testing.T) {
		// The drifted line belongs to another provider: it would never be
		// SUMMED, but the file as a whole is no longer the shape we know, so
		// no window is answered from it.
		path := writeUsageLog(t, validRow, usageFixtureRow{
			raw: `{"t":"now","provider":"gemini","model":"cli","in":1,"out":1}`,
		})
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if !errors.Is(err, ErrUsageLine) || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, ErrUsageLine)", got, err)
		}
	})

	t.Run("mid-file garbage", func(t *testing.T) {
		path := writeUsageLogText(t, "not json at all\n"+validRow.line(t)+"\n")
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if !errors.Is(err, ErrUsageLine) || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, ErrUsageLine)", got, err)
		}
	})
}

// TestScanToleratesTornTail pins the ONE drift-shaped case that is not drift:
// magpie appends one line per call, so a reader can catch the file between the
// write and its newline. An unterminated LAST line that does not parse is
// skipped (the record arrives on the next scan); an unterminated last line
// that DOES parse is a complete record whose newline is not flushed yet and is
// counted; a terminated unparsable line is drift wherever it sits.
func TestScanToleratesTornTail(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	first := usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 10}
	second := usageFixtureRow{keyID: id, at: testWindowBase.Add(2 * time.Minute), in: 20}

	t.Run("unterminated unparsable tail is skipped", func(t *testing.T) {
		path := writeUsageLogText(t, first.line(t)+"\n"+second.line(t)+"\n"+`{"t":"2026-09-22T12:0`)
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if got != 30 {
			t.Fatalf("sum with a torn tail = %d, want 30 (the complete lines still count)", got)
		}

		// The next scan sees the finished line: the torn tail is transient by
		// construction, and the reader must not be stuck on it either.
		full := usageFixtureRow{keyID: id, at: testWindowBase.Add(3 * time.Minute), in: 5}
		if err := os.WriteFile(path, []byte(first.line(t)+"\n"+second.line(t)+"\n"+full.line(t)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to); err != nil || got != 35 {
			t.Fatalf("sum after the tail completed = (%d, %v), want (35, nil)", got, err)
		}
	})

	t.Run("unterminated parsable tail counts", func(t *testing.T) {
		path := writeUsageLogText(t, first.line(t)+"\n"+second.line(t))
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if got != 30 {
			t.Fatalf("sum = %d, want 30 (an unflushed newline is not a missing record)", got)
		}
	})

	t.Run("terminated garbage is drift even as the last line", func(t *testing.T) {
		path := writeUsageLogText(t, first.line(t)+"\n"+"garbage\n")
		src := newTestSource(t, path)
		got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
		if !errors.Is(err, ErrUsageLine) || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, ErrUsageLine)", got, err)
		}
	})
}

// TestBlankLinesAreSkipped pins that an empty or whitespace-only line is not
// drift (an append can leave one) while a file with NOTHING usable is
// ErrNoUsageLines.
func TestBlankLinesAreSkipped(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	row := usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 7}

	path := writeUsageLogText(t, "\n"+row.line(t)+"\n   \n\n")
	src := newTestSource(t, path)
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to); err != nil || got != 7 {
		t.Fatalf("sum with blank lines = (%d, %v), want (7, nil)", got, err)
	}
}

// TestSumUnavailableLogs pins the degraded inputs: a missing file, a path that
// is not a regular file, an empty file and a whitespace-only file all report an
// error instead of a 0 total. Reporting 0 would claim the account consumed
// nothing while the real cause is that the log says nothing — a brand-new
// magpie, a truncated or rewritten file — and the upstream percent it is
// reversed against comes from cline.bot, so that 0 would turn into a bogus pool.
func TestSumUnavailableLogs(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	ctx := context.Background()

	t.Run("missing file", func(t *testing.T) {
		src := newTestSource(t, filepath.Join(t.TempDir(), "nope", "usage.jsonl"))
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err == nil || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, error)", got, err)
		}
	})

	t.Run("directory at the log path", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "usage.jsonl")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		src := newTestSource(t, dir)
		got, err := src.SumClinePassTokensByAccount(ctx, id, from, to)
		if err == nil || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, error)", got, err)
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error = %v, want the not-a-regular-file diagnosis", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		src := NewSource("  ", "")
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err == nil || got != 0 {
			t.Fatalf("sum = (%d, %v), want (0, error)", got, err)
		}
	})

	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "empty file", content: ""},
		{name: "newlines only", content: "\n\n\n"},
		{name: "whitespace only", content: "   \t \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newTestSource(t, writeUsageLogText(t, tc.content))
			got, err := src.SumClinePassTokensByAccount(ctx, id, from, to)
			if !errors.Is(err, ErrNoUsageLines) || got != 0 {
				t.Fatalf("sum = (%d, %v), want (0, ErrNoUsageLines)", got, err)
			}
		})
	}
}

// TestSourceNeverWritesTheLog pins the read-only contract end to end: prism
// must never modify magpie's log. Content, size, mode and mtime are compared
// before and after a round of sums (mtime is what the index cache keys on, so
// a write would also invalidate every later scan).
func TestSourceNeverWritesTheLog(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	path := writeUsageLog(t, usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 1})

	before := readUsageFileState(t, path)
	src := newTestSource(t, path)
	ctx := context.Background()
	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil {
		t.Fatal(err)
	}
	if got := readUsageFileState(t, path); got != before {
		t.Fatalf("the log changed: before %+v, after %+v", before, got)
	}
}

type usageFileState struct {
	content string
	size    int64
	mode    os.FileMode
	mtime   int64
}

func readUsageFileState(t *testing.T, path string) usageFileState {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return usageFileState{content: string(b), size: fi.Size(), mode: fi.Mode(), mtime: fi.ModTime().UnixNano()}
}

// TestSourceIndexReuseAndInvalidation pins the cache rule of the growing log:
// one scan answers every account and every window while the file version is
// unchanged, and ANY change to the file version brings a fresh scan.
func TestSourceIndexReuseAndInvalidation(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	path := writeUsageLog(t, usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 10})
	src := newTestSource(t, path)

	first, err := src.indexFor()
	if err != nil {
		t.Fatal(err)
	}
	second, err := src.indexFor()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("an unchanged log must reuse the index (a rescan per window/account is the cost this cache exists to avoid)")
	}
	// The reused index still answers correctly.
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to); err != nil || got != 10 {
		t.Fatalf("sum = (%d, %v), want (10, nil)", got, err)
	}

	// An append (the common invalidation: magpie writes a line per call).
	appendUsageLog(t, path, usageFixtureRow{keyID: id, at: testWindowBase.Add(2 * time.Minute), in: 5})
	third, err := src.indexFor()
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("an appended log must invalidate the cached index")
	}
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to); err != nil || got != 15 {
		t.Fatalf("sum after append = (%d, %v), want (15, nil)", got, err)
	}

	// A truncation (a rotated or recreated log): size moves down, and the
	// index must not keep serving records that no longer exist.
	if err := os.WriteFile(path, []byte(usageFixtureRow{keyID: id, at: testWindowBase, in: 2}.line(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to); err != nil || got != 2 {
		t.Fatalf("sum after truncation = (%d, %v), want (2, nil)", got, err)
	}
}

// TestSourceRescansReplacedFileWithSameSizeAndMtime pins the SameFile leg of
// the cache key: a REPLACED log (new inode) whose size and mtime happen to
// match the indexed version must still be rescanned. Size+mtime alone would
// keep serving the old file's records forever — a restore, a rollback or a
// re-created log is exactly that shape.
func TestSourceRescansReplacedFileWithSameSizeAndMtime(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const idOld, idNew = "bdf422fa85", "26b5925350"
	path := writeUsageLog(t, usageFixtureRow{keyID: idOld, at: testWindowBase.Add(time.Minute), in: 100})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	src := newTestSource(t, path)
	ctx := context.Background()
	if got, err := src.SumClinePassTokensByAccount(ctx, idOld, from, to); err != nil || got != 100 {
		t.Fatalf("sum before replacement = (%d, %v), want (100, nil)", got, err)
	}

	// Same byte length (both ids and both numbers have equal widths), same
	// mtime, different file.
	replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte(usageFixtureRow{keyID: idNew, at: testWindowBase.Add(time.Minute), in: 100}.line(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Size() != info.Size() || !newInfo.ModTime().Equal(info.ModTime()) {
		t.Fatalf("the fixture must match size and mtime: %d/%v vs %d/%v",
			newInfo.Size(), newInfo.ModTime(), info.Size(), info.ModTime())
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}

	if got, err := src.SumClinePassTokensByAccount(ctx, idOld, from, to); err != nil || got != 0 {
		t.Fatalf("old account after replacement = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := src.SumClinePassTokensByAccount(ctx, idNew, from, to); err != nil || got != 100 {
		t.Fatalf("new account after replacement = (%d, %v), want (100, nil)", got, err)
	}
}

// appendUsageLog appends one more line to an existing fixture log.
func appendUsageLog(t *testing.T, path string, row usageFixtureRow) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(row.line(t) + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestSourceStartupUnavailableThenRecovers pins the boot-ordering rule: a log
// that does not exist while prism starts (magpie installed later, a config
// directory that appears afterwards) must be retried on later rounds, not
// disabled for the process lifetime.
func TestSourceStartupUnavailableThenRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage.jsonl") // parent dir absent
	src := newTestSource(t, path)
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"

	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err == nil {
		t.Fatal("sum with the log absent must fail")
	}

	// magpie comes up later: the very next round must see it.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 42}.line(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := src.SumClinePassTokensByAccount(ctx, id, from, to)
	if err != nil {
		t.Fatalf("recovery sum: %v", err)
	}
	if got != 42 {
		t.Fatalf("sum after recovery = %d, want 42", got)
	}
}

// TestSourceTransitionLogging pins the observability contract: the WARN is
// emitted once per healthy→degraded transition (never once per round), the
// error counter counts every failed sum, the status expvar follows the state,
// and the first success after a failure logs the recovery exactly once.
func TestSourceTransitionLogging(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	errorsBefore := sourceErrors.Value()
	path := filepath.Join(t.TempDir(), "data", "usage.jsonl")
	src := newTestSource(t, path)
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"

	// A single refresh round sums once per ClinePass window (5h / weekly /
	// monthly): three failures in a row are one degraded state, not three
	// warnings.
	for i := 0; i < 3; i++ {
		if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err == nil {
			t.Fatal("sum with the log absent must fail")
		}
	}
	if got := strings.Count(buf.String(), "clinepass usage source unavailable"); got != 1 {
		t.Fatalf("degradation WARNs = %d, want exactly 1 per transition:\n%s", got, buf.String())
	}
	if got := sourceErrors.Value() - errorsBefore; got != 3 {
		t.Fatalf("error counter delta = %d, want 3 (one per failed sum)", got)
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status = %q, want \"degraded\"", got)
	}

	// Recovery: the first success logs once and flips the status back.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 1}.line(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "clinepass usage source recovered"); got != 1 {
		t.Fatalf("recovery logs = %d, want 1:\n%s", got, buf.String())
	}
	if got := sourceStatus.Value(); got != "ok" {
		t.Fatalf("source status after recovery = %q, want \"ok\"", got)
	}

	// Later successes stay silent: recovery is a transition, not a report.
	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "clinepass usage source recovered"); got != 1 {
		t.Fatalf("recovery logs after a second success = %d, want 1", got)
	}

	// A second failure is a new transition: exactly one more WARN.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err == nil {
		t.Fatal("sum after removal must fail")
	}
	if got := strings.Count(buf.String(), "clinepass usage source unavailable"); got != 2 {
		t.Fatalf("degradation WARNs after a second transition = %d, want 2:\n%s", got, buf.String())
	}
	if got := sourceErrors.Value() - errorsBefore; got != 4 {
		t.Fatalf("error counter delta = %d, want 4", got)
	}
}

// TestSuccessfulCallRepublishesHealthyStatus pins the truthfulness of the
// process-global status metric: clinepass_usage_source_status is ONE expvar for
// the whole process while the `degraded` flag that drives it lives on each
// Source (see source.go), so a SUCCESS must publish "ok" instead of assuming
// the metric already says it. Otherwise a "degraded" left behind by a source
// that failed earlier in the process outlives that source: a brand-new source
// answering fine would be reported as a dead source for the rest of the
// process lifetime.
//
// The republishing call is a SUM. Account discovery deliberately stays out of
// this state machine (see ListClinePassAccounts): it neither publishes "ok" nor
// counts an error, so a SIGHUP arriving during a log outage cannot mask it.
func TestSuccessfulCallRepublishesHealthyStatus(t *testing.T) {
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"

	// A source that cannot read its log degrades the shared status.
	failed := newTestSource(t, filepath.Join(t.TempDir(), "data", "usage.jsonl"))
	if _, err := failed.SumClinePassTokensByAccount(ctx, id, from, to); err == nil {
		t.Fatal("sum with the log absent must fail")
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a failed sum = %q, want \"degraded\"", got)
	}

	// Account discovery does NOT republish, and does not degrade either: it is
	// out of this state machine on purpose (see ListClinePassAccounts), so a
	// discovery that succeeds while the log is unreadable leaves the "degraded"
	// the sums published standing. A host without magpie is still not a degraded
	// source — it is simply not a source that says anything about this metric.
	missing := NewSource(
		filepath.Join(t.TempDir(), "nope", "usage.jsonl"),
		filepath.Join(t.TempDir(), "nope", "providers.json"),
	)
	roster, err := missing.ListClinePassAccounts(ctx)
	if err != nil || len(roster) != 0 {
		t.Fatalf("roster = (%+v, %v), want an empty roster and no error", roster, err)
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a successful roster read = %q, want the sums' \"degraded\" (discovery does not participate)", got)
	}

	// The metric follows the calls in both directions: a later failure
	// republishes "degraded"...
	failedAgain := newTestSource(t, filepath.Join(t.TempDir(), "data", "usage.jsonl"))
	if _, err := failedAgain.SumClinePassTokensByAccount(ctx, id, from, to); err == nil {
		t.Fatal("sum with the log absent must fail")
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a second failed sum = %q, want \"degraded\"", got)
	}

	// ... and a successful sum of a healthy source republishes "ok".
	healthy := newTestSource(t, writeUsageLog(t, usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 5}))
	if n, err := healthy.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || n != 5 {
		t.Fatalf("sum = (%d, %v), want (5, nil)", n, err)
	}
	if got := sourceStatus.Value(); got != "ok" {
		t.Fatalf("source status after a successful sum = %q, want \"ok\"", got)
	}
}

// TestZeroWindowSumDoesNotRepublishHealthyStatus pins the boundary between "this
// call read the log" and "this call did not": a sum with a NON-POSITIVE lower
// bound (a caller with no window start — the guard is the API's own boundary,
// since the estimate pass skips such a window, so no caller passes one today) is
// answered with (0, nil) without touching the log, so it must not move the
// source's health either.
//
// Observing that call — the shape this test replaces — republished "ok" over a
// genuine degradation and printed the recovery line, so one such call was enough
// to report a dead source as healthy (and to clear the `degraded` flag that
// drives the next transition's WARN). The window is unknown: nothing was read, so
// nothing about the source is known.
func TestZeroWindowSumDoesNotRepublishHealthyStatus(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	ctx := context.Background()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"

	// A source that cannot read its log degrades the shared status.
	src := newTestSource(t, filepath.Join(t.TempDir(), "data", "usage.jsonl"))
	if _, err := src.SumClinePassTokensByAccount(ctx, id, testWindowBase.Unix(), to); err == nil {
		t.Fatal("sum with the log absent must fail")
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a failed sum = %q, want \"degraded\"", got)
	}
	buf.Reset()

	// The unknown-window sum: a number and no error, but no read. Both non-positive
	// shapes — an absent start and a negative one — are answered the same way, and
	// neither may move the health metric OR the stale counter: nothing was read, so
	// nothing about this source was learned.
	stale := metricInt(t, "clinepass_usage_attribution_stale_total")
	staleBefore := stale.Value()
	for _, from := range []int64{0, -5} {
		if n, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || n != 0 {
			t.Fatalf("sum with from=%d = (%d, %v), want (0, nil)", from, n, err)
		}
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a sum that read nothing = %q, want the failed read's \"degraded\" still standing", got)
	}
	if d := stale.Value() - staleBefore; d != 0 {
		t.Fatalf("stale counter delta over sums that read nothing = %d, want 0", d)
	}
	if strings.Contains(buf.String(), "recovered") {
		t.Fatalf("a sum that read nothing must not report a recovery:\n%s", buf.String())
	}

	// The same guard on the round API: a round whose window is unknown is just as
	// silent, and the same round over a real window still observes normally (the
	// guard belongs to the call, not to rounds).
	round := src.BeginRound(id)
	for _, from := range []int64{0, -5} {
		if n, err := round.Sum(ctx, from, to); err != nil || n != 0 {
			t.Fatalf("round sum with from=%d = (%d, %v), want (0, nil)", from, n, err)
		}
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status after a round sum that read nothing = %q, want \"degraded\"", got)
	}
	if _, err := round.Sum(ctx, testWindowBase.Unix(), to); err == nil {
		t.Fatal("the same round over a real window must still fail on the unreadable log")
	}
}

// metricInt looks a /metrics counter up by NAME instead of through the package
// variable: the metric names are the deployment contract these counters exist
// for (same rule as clinepass_usage_source_*), so a rename must fail the test
// rather than be invisible to it.
func metricInt(t *testing.T, name string) *expvar.Int {
	t.Helper()
	v := expvar.Get(name)
	if v == nil {
		t.Fatalf("metric %q is not registered on /metrics", name)
	}
	n, ok := v.(*expvar.Int)
	if !ok {
		t.Fatalf("metric %q is %T, want *expvar.Int", name, v)
	}
	return n
}

// TestSumUnmatchedAccountIsObservable pins the signal for the silent zero: an
// account id that matches NO row in the log AT ALL still sums to 0 (the
// contract callers rely on) but is no longer invisible — it is counted in
// clinepass_usage_account_unmatched_total and logged once per transition.
//
// This is the drift/rotation shape: magpie's providerKeyId derivation changes,
// or the provider key is rotated so the id prism now derives has no line of its
// own while the log still holds the old id's rows. The 0 it produces is not
// "this account consumed nothing" but "no line carries this id", and without
// this signal the reversed 总额 would just be silently wrong.
//
// What is deliberately NOT this condition: an account whose bucket exists but
// holds no record INSIDE the window (an idle window, the legitimate 0).
func TestSumUnmatchedAccountIsObservable(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	// The window starts AT the log's first row, so this fixture is not also a
	// lower-bound case (that signal has its own test below).
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const known, drifted = "bdf422fa85", "bdf422fa85abcdef"
	path := writeUsageLog(t, usageFixtureRow{keyID: known, at: testWindowBase, in: 10})
	src := newTestSource(t, path)
	ctx := context.Background()
	unmatched := metricInt(t, "clinepass_usage_account_unmatched_total")
	before := unmatched.Value()

	// A matched account is not this condition, even when the window it is
	// summed over holds none of its rows: 0 there is an idle window, not a
	// missing account.
	if got, err := src.SumClinePassTokensByAccount(ctx, known, from, to); err != nil || got != 10 {
		t.Fatalf("matched account sum = (%d, %v), want (10, nil)", got, err)
	}
	if got, err := src.SumClinePassTokensByAccount(ctx, known, to+10, to+20); err != nil || got != 0 {
		t.Fatalf("idle window sum = (%d, %v), want (0, nil)", got, err)
	}
	if d := unmatched.Value() - before; d != 0 {
		t.Fatalf("unmatched counter delta for matched accounts = %d, want 0", d)
	}

	// The drifted id (16 hex: the shape a magpie-side derivation change takes)
	// keeps the (0, nil) contract but is now observable. Three calls are three
	// counted incidents (one per window per round, like sourceErrors) and ONE
	// transition WARN — never one per round.
	for i := 0; i < 3; i++ {
		if got, err := src.SumClinePassTokensByAccount(ctx, drifted, from, to); err != nil || got != 0 {
			t.Fatalf("unmatched account sum = (%d, %v), want (0, nil)", got, err)
		}
	}
	if d := unmatched.Value() - before; d != 3 {
		t.Fatalf("unmatched counter delta = %d, want 3 (one per sum)", d)
	}
	if got := strings.Count(buf.String(), "matched no usage row"); got != 1 {
		t.Fatalf("unmatched WARNs = %d, want exactly 1 per transition:\n%s", got, buf.String())
	}

	// The state is per ACCOUNT: a matched sum in between must neither re-arm
	// the WARN for the unmatched id nor silence it.
	if _, err := src.SumClinePassTokensByAccount(ctx, known, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SumClinePassTokensByAccount(ctx, drifted, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "matched no usage row"); got != 1 {
		t.Fatalf("unmatched WARNs after an interleaved matched sum = %d, want 1:\n%s", got, buf.String())
	}

	// Self-healing: once a row carries the id, the sum answers it, the
	// recovery is logged once, and later sums stay quiet.
	appendUsageLog(t, path, usageFixtureRow{keyID: drifted, at: testWindowBase.Add(2 * time.Minute), in: 5})
	if got, err := src.SumClinePassTokensByAccount(ctx, drifted, from, to); err != nil || got != 5 {
		t.Fatalf("sum after the id reappears = (%d, %v), want (5, nil)", got, err)
	}
	if got := strings.Count(buf.String(), "matches usage rows again"); got != 1 {
		t.Fatalf("recovery logs = %d, want 1:\n%s", got, buf.String())
	}
	if _, err := src.SumClinePassTokensByAccount(ctx, drifted, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "matches usage rows again"); got != 1 {
		t.Fatalf("recovery logs after a second sum = %d, want 1", got)
	}
}

// TestSumWindowBeforeLogStartSignalsLowerBound pins the second shape a
// SUCCESSFUL sum can still be wrong in: magpie's log is appended forever and
// never backfilled, so its first row is the oldest instant it can answer for.
// A window that starts BEFORE that row can only be summed to a LOWER BOUND —
// the traffic before the log's start is simply not there — and the estimate
// reversed from that numerator comes out too small.
//
// The signal is a counter (one per sum, so a still-wrong window keeps growing)
// plus one WARN per window START (not per round, and naming the two instants an
// operator needs: the window's start and the log's start). It heals by itself:
// a window starting at or after the log's first row is complete.
func TestSumWindowBeforeLogStartSignalsLowerBound(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	const id = "bdf422fa85"
	logStart := testWindowBase // the log's first row: the edge of its reach
	path := writeUsageLog(t,
		usageFixtureRow{keyID: id, at: logStart, in: 10},
		usageFixtureRow{keyID: id, at: logStart.Add(30 * time.Minute), in: 10},
	)
	src := newTestSource(t, path)
	ctx := context.Background()
	lb := metricInt(t, "clinepass_usage_window_lower_bound_total")
	before := lb.Value()

	// The weekly-window shape: it starts three days before the log does. The
	// sum still counts what the log HAS (10 — the bound itself, 20 would need
	// the second row which the window stops short of).
	from := logStart.Add(-72 * time.Hour).Unix()
	to := logStart.Add(10 * time.Minute).Unix()
	if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 10 {
		t.Fatalf("lower-bound sum = (%d, %v), want (10, nil)", got, err)
	}
	if d := lb.Value() - before; d != 1 {
		t.Fatalf("lower-bound counter delta = %d, want 1", d)
	}
	if got := strings.Count(buf.String(), "numerator is a lower bound"); got != 1 {
		t.Fatalf("lower-bound WARNs = %d, want exactly 1 per window start:\n%s", got, buf.String())
	}
	// The WARN carries the window's start and the log's start: without both an
	// operator cannot tell how much is missing.
	if s := buf.String(); !strings.Contains(s, logStart.Add(-72*time.Hour).UTC().Format(time.RFC3339)) || !strings.Contains(s, logStart.UTC().Format(time.RFC3339)) {
		t.Fatalf("lower-bound WARN must name the window start and the log start:\n%s", buf.String())
	}

	// The same window on the next round is counted again (it is still a lower
	// bound) but is NOT logged again: once per window start, never per round.
	if _, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil {
		t.Fatal(err)
	}
	if d := lb.Value() - before; d != 2 {
		t.Fatalf("lower-bound counter delta = %d, want 2", d)
	}
	if got := strings.Count(buf.String(), "numerator is a lower bound"); got != 1 {
		t.Fatalf("lower-bound WARNs after a second round = %d, want 1:\n%s", got, buf.String())
	}

	// A window starting AT the log's first row is complete: that row is inside
	// it and nothing earlier can exist — the same for a window further in.
	for _, start := range []time.Time{logStart, logStart.Add(time.Minute)} {
		if _, err := src.SumClinePassTokensByAccount(ctx, id, start.Unix(), to); err != nil {
			t.Fatal(err)
		}
	}
	if d := lb.Value() - before; d != 2 {
		t.Fatalf("lower-bound counter delta = %d, want 2 (covered windows are not lower bounds)", d)
	}
	if got := strings.Count(buf.String(), "numerator is a lower bound"); got != 1 {
		t.Fatalf("lower-bound WARNs after covered windows = %d, want 1", got)
	}
}

// TestSumUnattributedKeylessRowsIsObservable pins the ROW-level signal for the
// other half of Track 2's exposure: a keyless `cline` row of a cline-pass/ model
// whose masked label matches no account's key tail. It stays in no account's
// bucket (the sum is a lower bound) and is counted per row in
// clinepass_usage_unattributed_rows_total — a different unit from
// clinepass_usage_account_unmatched_total, which counts CALLS whose account id
// matched no line at all. The two are independent, and this fixture shows it:
// the account has rows, so only the row counter moves.
func TestSumUnattributedKeylessRowsIsObservable(t *testing.T) {
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85" // KeyID("tok-primary"), whose key tail is "mary"

	path := writeUsageLog(t,
		// Not counted: Track 1 places it by the id magpie stamped.
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 7},
		// Not counted: the label names tok-primary's key tail.
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(2 * time.Minute), in: 11},
		// THE falling-through row: keyless `cline`, cline-pass/ model, and a
		// label no roster tail matches (a rotated key, another subscription).
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026zzzz", at: testWindowBase.Add(3 * time.Minute), in: 200},
	)
	src := newTestSourceWithProviders(t, path, clineRosterFixture(t))

	rows := metricInt(t, "clinepass_usage_unattributed_rows_total")
	before := rows.Value()
	unmatchedAccounts := metricInt(t, "clinepass_usage_account_unmatched_total")
	accountsBefore := unmatchedAccounts.Value()

	if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
		t.Fatalf("sum = (%d, %v), want (18, nil) — the unattributed row joins no bucket", got, err)
	}
	if d := rows.Value() - before; d != 1 {
		t.Fatalf("unattributed-row counter delta = %d, want 1 (the keyless row that matched no roster key tail)", d)
	}
	// A different question, a different unit: that counter is per CALL and
	// account-level, and this account has rows.
	if d := unmatchedAccounts.Value() - accountsBefore; d != 0 {
		t.Fatalf("account-unmatched counter delta = %d, want 0 (account-level, per sum)", d)
	}

	// The increment is per row per SCAN, and a scan is the WHOLE file: the
	// counter re-adds every falling-through row whenever the log's version
	// moves, so a sum over an UNCHANGED file adds nothing at all...
	if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
		t.Fatalf("sum over the unchanged log = (%d, %v), want (18, nil)", got, err)
	}
	if d := rows.Value() - before; d != 1 {
		t.Fatalf("unattributed-row counter delta after a second sum = %d, want 1 (it follows the FILE version, not the call count)", d)
	}
	// ...and ONE appended line re-adds the whole backlog instead of only the
	// new line, so the number must be read as a FLAG ("the log holds keyless
	// rows no track placed") and never as a rate: a slope here is the rescan,
	// not new traffic, and it must not carry an alert threshold.
	appendUsageLog(t, path, usageFixtureRow{keyID: id, at: testWindowBase.Add(4 * time.Minute), in: 1})
	if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 19 {
		t.Fatalf("sum after the append = (%d, %v), want (19, nil)", got, err)
	}
	if d := rows.Value() - before; d != 2 {
		t.Fatalf("unattributed-row counter delta after an append = %d, want 2 (the whole backlog is re-counted, not only the new line)", d)
	}
}

// clineRosterFixture writes a providers.json whose two ClinePass keys end in
// DISTINCT four-character tails (`mary` / `dary`): the shape magpie's masked
// account labels are matched on. The keys are synthetic; the ids the
// assertions use are KeyID of them (bdf422fa85 / 26b5925350).
func clineRosterFixture(t *testing.T) string {
	t.Helper()
	return writeProviders(t, `{"providers":[{"id":"clinepass","key":"tok-primary","keys":[{"key":"tok-secondary"}]}]}`)
}

// newTestSourceWithProviders is newTestSource plus the account discovery the
// caller's sum needs: Track 2 attributes a keyless row with the tail table of
// the roster a DISCOVERY installed (see Source.installRoster) and never with a
// table re-derived from the provider file, so a test that expects attribution
// must discover first, exactly as the service and the CLI do before any sum.
// Track 1 does not need it.
func newTestSourceWithProviders(t *testing.T, usagePath, providersPath string) *Source {
	t.Helper()
	src := NewSource(usagePath, providersPath)
	if _, err := src.ListClinePassAccounts(context.Background()); err != nil {
		t.Fatalf("discover clinepass accounts: %v", err)
	}
	return src
}

// TestSumAttributionDualTrack pins how a usage row is placed on a ClinePass
// account, and the line the second track must not cross.
//
// The shape Track 2 exists for is real and was being DROPPED: magpie writes
// cline-pass/* lines with no providerKeyId at all — 932 keyless `cline` lines,
// 921 of them a cline-pass/ model (measured 2026-10-07) — which are ClinePass
// consumption like any other (same provider key, same reset instant, the same
// upstream response ids as the keyed rows), so skipping them under-counted each
// account and shrank the reversed 总额.
//
// Track 2 matches the MASKED account label's key tail against the roster, so
// the assertions are about suffixes, the providerAccount → host fallback, and
// the one case that must REFUSE to attribute: a label that names more than one
// account.
func TestSumAttributionDualTrack(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	ctx := context.Background()
	const (
		idPrimary   = "bdf422fa85" // tail "mary"
		idSecondary = "26b5925350" // tail "dary"
	)

	t.Run("a keyless cline row joins the account its masked label names", func(t *testing.T) {
		path := writeUsageLog(t,
			// Track 1: the id magpie stamped, whatever the provider says.
			usageFixtureRow{keyID: idPrimary, at: testWindowBase.Add(time.Minute), in: 7},
			// Track 2: no providerKeyId at all, so magpie's masked account label
			// is the only handle the line offers — `…mary` is the last four
			// characters of tok-primary.
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", host: "cline as API key \u2026mary", at: testWindowBase.Add(2 * time.Minute), in: 11},
			// Track 2 for the OTHER account, same shape.
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026dary", at: testWindowBase.Add(3 * time.Minute), in: 13},
			// Excluded by the MODEL gate even though it is keyless `cline`
			// traffic from an account in the roster: cline-free/* is magpie's
			// free tier — a different product with no subscription pool behind
			// it — and the prefix filter already keeps it out.
			usageFixtureRow{provider: "cline", model: "cline-free/mimo-v2.6-flash", providerAccount: "API key \u2026mary", at: testWindowBase.Add(4 * time.Minute), in: 100},
			// Unattributed: a keyless `cline` label no account's key tail
			// matches (a rotated key, another subscription).
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026zzzz", at: testWindowBase.Add(5 * time.Minute), in: 200},
		)
		src := newTestSourceWithProviders(t, path, clineRosterFixture(t))
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 18 {
			t.Fatalf("account %s sum = (%d, %v), want (18, nil) — 7 by id + 11 by tail", idPrimary, got, err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, idSecondary, from, to); err != nil || got != 13 {
			t.Fatalf("account %s sum = (%d, %v), want (13, nil)", idSecondary, got, err)
		}
		// Neither row lands in an account (7+11+13 attributed; +200 whose label
		// no account owns; +100 under cline-free/), and no account-less entry
		// point can see them: the unplaced keyless row is visible at the ROW
		// level in clinepass_usage_unattributed_rows_total (see
		// TestSumUnattributedKeylessRowsIsObservable), while the cline-free line
		// is not a scope question at all — the model gate is a filter, and it
		// holds for both tracks.
	})

	t.Run("host is the fallback when the masked account label is absent", func(t *testing.T) {
		path := writeUsageLog(t,
			// providerAccount is empty: magpie's other field carries the same
			// mask (`cline as API key …dary`) and answers for the account.
			usageFixtureRow{provider: "cline", host: "cline as API key \u2026dary", at: testWindowBase.Add(time.Minute), in: 23},
			// Both fields present and disagreeing: providerAccount is the field
			// magpie fills for the account, so it wins.
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", host: "cline as API key \u2026dary", at: testWindowBase.Add(2 * time.Minute), in: 41},
		)
		src := newTestSourceWithProviders(t, path, clineRosterFixture(t))
		if got, err := src.SumClinePassTokensByAccount(ctx, idSecondary, from, to); err != nil || got != 23 {
			t.Fatalf("account %s sum = (%d, %v), want (23, nil) — the host label", idSecondary, got, err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 41 {
			t.Fatalf("account %s sum = (%d, %v), want (41, nil) — providerAccount wins", idPrimary, got, err)
		}
	})

	t.Run("a rotated key moves only with the next discovery", func(t *testing.T) {
		path := writeUsageLog(t,
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(time.Minute), in: 5},
		)
		providers := clineRosterFixture(t)
		src := newTestSourceWithProviders(t, path, providers)
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 5 {
			t.Fatalf("account %s sum = (%d, %v), want (5, nil)", idPrimary, got, err)
		}
		// magpie's provider key rotates. Until the next discovery the poller
		// still polls tok-primary, so the row MUST stay with the account that is
		// being polled: a table re-read here would hand it to tok-rotated's id —
		// which nothing polls — and the row would be counted for no account at
		// all while the percentage still came from the old key.
		if err := os.WriteFile(providers, []byte(`{"providers":[{"id":"clinepass","key":"tok-rotated"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 5 {
			t.Fatalf("account %s sum before the next discovery = (%d, %v), want (5, nil): attribution follows the polled roster", idPrimary, got, err)
		}
		// The next discovery (startup / SIGHUP) replaces the roster: the tail
		// table moves with it, the cached index is invalidated, and the
		// historical row — whose label names a key no longer in the roster — is
		// credited to no account.
		if _, err := src.ListClinePassAccounts(ctx); err != nil {
			t.Fatal(err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 0 {
			t.Fatalf("account %s sum after the discovery = (%d, %v), want (0, nil)", idPrimary, got, err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, KeyID("tok-rotated"), from, to); err != nil || got != 0 {
			t.Fatalf("rotated account sum = (%d, %v), want (0, nil)", got, err)
		}
	})

	t.Run("a rescan uses the discovered table, never a re-read of the file", func(t *testing.T) {
		path := writeUsageLog(t,
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(time.Minute), in: 5},
		)
		providers := clineRosterFixture(t)
		src := newTestSourceWithProviders(t, path, providers)
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 5 {
			t.Fatalf("account %s sum = (%d, %v), want (5, nil)", idPrimary, got, err)
		}
		// magpie rotates the key AND the log grows, so the next sum cannot reuse
		// the cached index: the rescan has to attribute with the table the
		// DISCOVERY installed — the roster the poller is still polling — and not
		// with a freshly read provider file. A re-read on this path would hand the
		// row to tok-rotated's id, which nothing polls, while the percentage still
		// came from the old key: the total would silently drop to 0 (and the row
		// would be counted as unattributed) for a roster rotation prism has not
		// been told about yet. Only the next discovery may move the table.
		if err := os.WriteFile(providers, []byte(`{"providers":[{"id":"clinepass","key":"tok-rotated"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		appendUsageLog(t, path, usageFixtureRow{keyID: idPrimary, model: "gpt-5", at: testWindowBase.Add(2 * time.Minute), in: 1_000})
		if got, err := src.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 5 {
			t.Fatalf("account %s sum after the rescan = (%d, %v), want (5, nil): a rescan must reuse the discovered tail table", idPrimary, got, err)
		}
	})

	t.Run("colliding key tails refuse every keyless row and warn once", func(t *testing.T) {
		var buf bytes.Buffer
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(oldDefault) })

		path := writeUsageLog(t,
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(time.Minute), in: 29},
		)
		// Two accounts whose keys end in the SAME four characters (`mary`):
		// the label names both, so attributing either would add one
		// subscription's traffic to the other's percent (串账). The track is
		// disabled instead of picking a winner — the row is in no account's
		// bucket, and the reason is one WARN per transition, never per round.
		providers := writeProviders(t, `{"providers":[{"id":"clinepass","key":"tok-primary","keys":[{"key":"alt-primary"}]}]}`)
		src := newTestSourceWithProviders(t, path, providers)
		for _, id := range []string{KeyID("tok-primary"), KeyID("alt-primary")} {
			if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 0 {
				t.Fatalf("colliding account %s sum = (%d, %v), want (0, nil)", id, got, err)
			}
		}
		logs := buf.String()
		if got := strings.Count(logs, "keyless usage rows are not attributable"); got != 1 {
			t.Fatalf("attribution WARNs = %d, want 1 (once per transition, not per sum):\n%s", got, logs)
		}
		if !strings.Contains(logs, KeyID("tok-primary")) || !strings.Contains(logs, KeyID("alt-primary")) {
			t.Fatalf("the WARN must name the colliding accounts (by one-way id):\n%s", logs)
		}
		// The tail itself is a piece of a credential: it belongs in the
		// comparison, never in a log line.
		for _, leak := range []string{"tok-primary", "alt-primary", "mary"} {
			if strings.Contains(logs, leak) {
				t.Fatalf("the WARN leaked %q:\n%s", leak, logs)
			}
		}
		// A usable roster logs nothing: the condition is a problem, not a
		// state to announce.
		buf.Reset()
		plain := newTestSourceWithProviders(t, path, clineRosterFixture(t))
		if got, err := plain.SumClinePassTokensByAccount(ctx, idPrimary, from, to); err != nil || got != 29 {
			t.Fatalf("account %s sum with a usable roster = (%d, %v), want (29, nil)", idPrimary, got, err)
		}
		if strings.Contains(buf.String(), "keyless usage rows") {
			t.Fatalf("a usable roster must stay silent:\n%s", buf.String())
		}
	})
}

// TestAttributionSuspendedUntilDiscoveryConfirms pins the rule that a tail table
// may place keyless rows only while the discovery that built it is still
// CONFIRMED. A discovery that did NOT read the provider file — it failed, or the
// file is not there — installs nothing and marks the previous table STALE, and a
// stale table attributes nothing: the file prism could not read is the one magpie
// rewrites when it rotates a key, so an unconfirmed table can credit a keyless row
// to an account the row does not belong to (串账) — the single thing this track
// exists to prevent. Suspending attribution costs a lower bound that the two
// counters make visible; guessing costs a wrong account.
//
// That "no provider file" and "a provider file that names no ClinePass account"
// are different events is the other half of the rule: only a READ file confirms a
// roster, so an empty discovery may not clear the not-attributable WARN either —
// the recovery line belongs to a table a discovery just built.
//
// That a sum shares its keys with the caller's roster (same-round consistency) is
// pinned by "a rotated key moves only with the next discovery" in
// TestSumAttributionDualTrack.
func TestAttributionSuspendedUntilDiscoveryConfirms(t *testing.T) {
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85" // KeyID("tok-primary"), whose key tail is "mary"

	// Two keyless rows: one whose masked label names tok-primary's tail (and is
	// therefore attributable while the table is confirmed), one whose label
	// names no roster key (and is therefore never attributable).
	newLog := func(t *testing.T) string {
		t.Helper()
		return writeUsageLog(t,
			usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 7},
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(2 * time.Minute), in: 11},
			usageFixtureRow{provider: "cline", providerAccount: "API key \u2026zzzz", at: testWindowBase.Add(3 * time.Minute), in: 200},
		)
	}
	captureLogs := func(t *testing.T) *bytes.Buffer {
		t.Helper()
		buf := &bytes.Buffer{}
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(oldDefault) })
		return buf
	}
	rows := metricInt(t, "clinepass_usage_unattributed_rows_total")
	stale := metricInt(t, "clinepass_usage_attribution_stale_total")

	t.Run("a failed discovery suspends attribution until the next success", func(t *testing.T) {
		buf := captureLogs(t)
		log := newLog(t)
		providers := clineRosterFixture(t)
		src := newTestSourceWithProviders(t, log, providers)

		rowsBefore, staleBefore := rows.Value(), stale.Value()
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
			t.Fatalf("sum with a confirmed roster = (%d, %v), want (18, nil) — 7 by id + 11 by tail", got, err)
		}
		if d := stale.Value() - staleBefore; d != 0 {
			t.Fatalf("stale counter delta with a confirmed table = %d, want 0", d)
		}
		if strings.Contains(buf.String(), "not attributable") {
			t.Fatalf("a confirmed table must not warn:\n%s", buf.String())
		}

		// The provider file becomes unreadable — a directory, so the read fails
		// with an error that is NOT fs.ErrNotExist. Discovery fails, the caller
		// keeps the previous roster (that is its rule, not this package's), and
		// the table it kept must stop placing keyless rows.
		if err := os.Remove(providers); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(providers, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := src.ListClinePassAccounts(ctx); err == nil {
			t.Fatal("discovery over an unreadable provider file must fail (it is the caller that logs it)")
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 7 {
			t.Fatalf("sum after a failed discovery = (%d, %v), want (7, nil) — a stale table may place nothing", got, err)
		}
		// The first sum counted the row no label can ever place; the stale scan
		// counted BOTH keyless rows, because a table that places nothing makes the
		// row that WAS attributable fall through too: 1 + 2.
		if d := rows.Value() - rowsBefore; d != 3 {
			t.Fatalf("unattributed-row counter delta = %d, want 3 (the never-attributable row, + both keyless rows of the stale scan)", d)
		}
		if d := stale.Value() - staleBefore; d != 1 {
			t.Fatalf("stale counter delta = %d, want 1 (one SUM without a confirmed table)", d)
		}

		// A second sum is no new transition (no WARN spam) but IS a new sum: the
		// stale counter is per sum, unlike the per-scan row counter.
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 7 {
			t.Fatalf("second sum after a failed discovery = (%d, %v), want (7, nil)", got, err)
		}
		if d := stale.Value() - staleBefore; d != 2 {
			t.Fatalf("stale counter delta after two sums = %d, want 2 (per SUM, not per transition)", d)
		}
		if got := strings.Count(buf.String(), "keyless usage rows are not attributable"); got != 1 {
			t.Fatalf("stale WARNs = %d, want 1 (once per transition):\n%s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "attribution table is not confirmed") {
			t.Fatalf("the WARN must name the unconfirmed table as the reason:\n%s", buf.String())
		}

		// The next start / SIGHUP reads the file again: the table is confirmed,
		// the attributable row is placed again, and the recovery line comes from
		// the DISCOVERY — never from a sum, which cannot verify it.
		if err := os.Remove(providers); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(providers, []byte(`{"providers":[{"id":"clinepass","key":"tok-primary","keys":[{"key":"tok-secondary"}]}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := src.ListClinePassAccounts(ctx); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); !strings.Contains(got, "attributable again") {
			t.Fatalf("a successful discovery must announce the recovery:\n%s", got)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
			t.Fatalf("sum after the recovery = (%d, %v), want (18, nil)", got, err)
		}
		if d := stale.Value() - staleBefore; d != 2 {
			t.Fatalf("stale counter after the recovery = %d, want no further ticks", d)
		}
	})

	t.Run("a missing provider file does not confirm a roster", func(t *testing.T) {
		buf := captureLogs(t)
		log := newLog(t)
		providers := clineRosterFixture(t)
		src := newTestSourceWithProviders(t, log, providers)

		rowsBefore, staleBefore := rows.Value(), stale.Value()
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
			t.Fatalf("sum with a confirmed roster = (%d, %v), want (18, nil)", got, err)
		}
		// The file is gone (a magpie upgrade, a wiped config dir). Discovery
		// stays SILENT — a host without magpie has no provider file and must not
		// warn — but it confirms nothing either, so the sums must stop placing
		// keyless rows with a table no read backs.
		if err := os.Remove(providers); err != nil {
			t.Fatal(err)
		}
		accounts, err := src.ListClinePassAccounts(ctx)
		if err != nil || accounts != nil {
			t.Fatalf("discovery without a provider file = (%v, %v), want (nil, nil) — an empty roster, not a failure", accounts, err)
		}
		if got := strings.Count(buf.String(), "keyless usage rows"); got != 0 {
			t.Fatalf("discovery without a provider file must not warn on its own:\n%s", buf.String())
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 7 {
			t.Fatalf("sum after the file disappeared = (%d, %v), want (7, nil)", got, err)
		}
		if d := rows.Value() - rowsBefore; d != 3 {
			t.Fatalf("unattributed-row counter delta = %d, want 3 (the never-attributable row, + both keyless rows of the unconfirmed scan)", d)
		}
		if d := stale.Value() - staleBefore; d != 1 {
			t.Fatalf("stale counter delta = %d, want 1 (one sum without a confirmed table)", d)
		}
		if !strings.Contains(buf.String(), "attribution table is not confirmed") {
			t.Fatalf("the sum must report the unconfirmed table:\n%s", buf.String())
		}
	})

	t.Run("a file that names no account is a success that attributes nothing", func(t *testing.T) {
		buf := captureLogs(t)
		log := newLog(t)
		// A providers.json that parses and simply has no clinepass entry: the
		// discovery SUCCEEDS with an empty roster — that IS the truth about
		// which accounts may be polled, so the caller gets it without an error —
		// but the table it installs holds no key tail, so no keyless row can be
		// placed and the not-attributable condition stands. Treating this as a
		// recovery would announce rows as attributable that still are not.
		providers := writeProviders(t, `{"providers":[{"id":"other","key":"tok-x"}]}`)
		src := NewSource(log, providers)

		rowsBefore, staleBefore := rows.Value(), stale.Value()
		accounts, err := src.ListClinePassAccounts(ctx)
		if err != nil || len(accounts) != 0 {
			t.Fatalf("discovery over a file without a clinepass entry = (%v, %v), want (empty, nil)", accounts, err)
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 7 {
			t.Fatalf("sum with an empty account table = (%d, %v), want (7, nil)", got, err)
		}
		if d := rows.Value() - rowsBefore; d != 2 {
			t.Fatalf("unattributed-row counter delta = %d, want 2 (both keyless rows)", d)
		}
		// The empty table is CONFIRMED — the discovery READ the file — so this is
		// not the unconfirmed condition: the rows fall through because the table
		// holds no key tail, which is the discovery's own report (once), and no
		// sum may turn it into a recovery.
		if d := stale.Value() - staleBefore; d != 0 {
			t.Fatalf("stale counter delta = %d, want 0 (an empty table is confirmed, not unconfirmed)", d)
		}
		if got := strings.Count(buf.String(), "no clinepass account key tail"); got != 1 {
			t.Fatalf("an empty table must report its own reason exactly once:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "attributable again") {
			t.Fatalf("an empty account table must not announce a recovery:\n%s", buf.String())
		}

		// The next discovery reads a real roster: THAT is the transition that
		// makes the recovery true, and attribution resumes with it.
		if err := os.WriteFile(providers, []byte(`{"providers":[{"id":"clinepass","key":"tok-primary"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := src.ListClinePassAccounts(ctx); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(buf.String(), "attributable again"); got != 1 {
			t.Fatalf("recovery lines = %d, want 1 (from the discovery that confirmed the table):\n%s", got, buf.String())
		}
		if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 18 {
			t.Fatalf("sum after the recovery = (%d, %v), want (18, nil)", got, err)
		}
	})
}

// TestRoundPinsAttributionAcrossRediscovery pins what a Round is FOR: the windows
// of ONE fetch must be summed with the tail table (and the freshness verdict) that
// fetch began with, so a discovery landing between two of them cannot split one
// fetch across two rosters.
//
// A fetch sums the SAME account three times (5h / weekly / monthly) and combines
// those sums with ONE upstream percent — the percent of the key that is being
// polled. Reading the source's live table per window instead meant a SIGHUP
// between two of them was enough to attribute the earlier window with the old
// table and the later one with the new: the row lands on the account the key is
// polled under in one window and on nobody (or on a colliding tail's account) in
// the next, so one fetch reported two different accounts' worth of tokens against
// one percent.
func TestRoundPinsAttributionAcrossRediscovery(t *testing.T) {
	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const (
		idPrimary = "bdf422fa85" // KeyID("tok-primary"), the key the poller is polling
	)
	idRotated := KeyID("tok-rotated")
	path := writeUsageLog(t,
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(time.Minute), in: 11},
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026zzzz", at: testWindowBase.Add(2 * time.Minute), in: 200},
	)
	providers := clineRosterFixture(t)
	src := newTestSourceWithProviders(t, path, providers)

	// The fetch begins here: the round snapshots the roster that is being polled.
	round := src.BeginRound(idPrimary)

	// Between two windows of that fetch, magpie rotates the key and the next
	// discovery (startup / SIGHUP) installs the rotated table.
	if err := os.WriteFile(providers, []byte(`{"providers":[{"id":"clinepass","key":"tok-rotated"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ListClinePassAccounts(ctx); err != nil {
		t.Fatal(err)
	}

	// The round in flight keeps its own snapshot, so the keyless row is still the
	// POLLED account's: 11 by tail, and the row no label can place stays out.
	if got, err := round.Sum(ctx, from, to); err != nil || got != 11 {
		t.Fatalf("round sum begun before the rediscovery = (%d, %v), want (11, nil): a round keeps the table its fetch began with", got, err)
	}
	// The rediscovery DID move the source's live table — otherwise the number
	// above would prove nothing. A round begun now, and the one-shot form, both
	// read the rotated roster: no row's label ends in the rotated key's tail, and
	// the polled key's rows are no longer placed either.
	if got, err := src.BeginRound(idPrimary).Sum(ctx, from, to); err != nil || got != 0 {
		t.Fatalf("round sum begun after the rediscovery = (%d, %v), want (0, nil): the rotated table places nothing", got, err)
	}
	if got, err := src.SumClinePassTokensByAccount(ctx, idRotated, from, to); err != nil || got != 0 {
		t.Fatalf("rotated account sum = (%d, %v), want (0, nil)", got, err)
	}

	// The freshness VERDICT is part of the snapshot too, for the same reason: a
	// discovery that reads no file between two windows of one fetch must not make
	// the first one look as if it had placed its rows without attribution.
	stale := metricInt(t, "clinepass_usage_attribution_stale_total")
	providers = writeProviders(t, `{"providers":[{"id":"clinepass","key":"tok-primary"}]}`)
	fresh := newTestSourceWithProviders(t, path, providers)
	r := fresh.BeginRound(idPrimary)
	staleBefore := stale.Value()
	if got, err := r.Sum(ctx, from, to); err != nil || got != 11 {
		t.Fatalf("sum with a confirmed table = (%d, %v), want (11, nil)", got, err)
	}
	if err := os.Remove(providers); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.ListClinePassAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got, err := r.Sum(ctx, from, to); err != nil || got != 11 {
			t.Fatalf("round sum %d after the table went stale = (%d, %v), want (11, nil)", i+1, got, err)
		}
	}
	if d := stale.Value() - staleBefore; d != 0 {
		t.Fatalf("stale counter delta over a round in flight = %d, want 0: the round keeps the verdict its fetch began with", d)
	}
	// A round begun after it went stale reports it — once per sum, so the three
	// windows of that later fetch are all visible.
	staleRound := fresh.BeginRound(idPrimary)
	for i := 0; i < 3; i++ {
		if got, err := staleRound.Sum(ctx, from, to); err != nil || got != 0 {
			t.Fatalf("stale round sum %d = (%d, %v), want (0, nil): a stale table places no keyless row", i+1, got, err)
		}
	}
	if d := stale.Value() - staleBefore; d != 3 {
		t.Fatalf("stale counter delta over a stale round = %d, want 3 (one per window, same unit as the account counter)", d)
	}
}

// TestStaleAttributionIsSilentBeforeTheFirstConfirmedTable pins the OTHER half of
// clinepass_usage_attribution_stale_total's condition: it counts sums that ran
// without a table prism LOST, not sums that ran without a table it never had.
//
// "Unconfirmed" describes two states, and only one of them is a loss. A table
// that went stale is a roster whose keys the caller still polls and whose keyless
// rows are now missing from a total that keeps being produced — a counter and one
// WARN. A host that runs no magpie never had a table: nothing was lost, there is
// no roster to re-confirm, and an alert here would ask the operator to fix
// something that was never installed — on every sum, forever.
func TestStaleAttributionIsSilentBeforeTheFirstConfirmedTable(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	ctx := context.Background()
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	stale := metricInt(t, "clinepass_usage_attribution_stale_total")

	// No provider file at all (newTestSource points providersPath at a path that
	// does not exist): discovery answers nothing, so no table was ever confirmed.
	src := newTestSource(t, writeUsageLog(t,
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 5},
		usageFixtureRow{provider: "cline", providerAccount: "API key \u2026mary", at: testWindowBase.Add(2 * time.Minute), in: 11},
	))
	if _, err := src.ListClinePassAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	staleBefore := stale.Value()
	if got, err := src.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 5 {
		t.Fatalf("sum on a source that never had a table = (%d, %v), want (5, nil) — the keyed row is still summed", got, err)
	}
	if d := stale.Value() - staleBefore; d != 0 {
		t.Fatalf("stale counter delta on a source that never confirmed a table = %d, want 0: there is no roster whose rows went missing", d)
	}
	if strings.Contains(buf.String(), "not attributable") {
		t.Fatalf("a source that never had a table must not warn about a lost one:\n%s", buf.String())
	}

	// Once a table IS confirmed and then lost, the same sum does move the counter
	// (the transition the counter exists for), so the silence above is a condition
	// and not an accident of the fixture.
	providers := clineRosterFixture(t)
	confirmed := newTestSourceWithProviders(t, writeUsageLog(t,
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 5},
	), providers)
	if err := os.Remove(providers); err != nil {
		t.Fatal(err)
	}
	if _, err := confirmed.ListClinePassAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	staleBefore = stale.Value()
	if got, err := confirmed.SumClinePassTokensByAccount(ctx, id, from, to); err != nil || got != 5 {
		t.Fatalf("sum after the confirmed table went stale = (%d, %v), want (5, nil)", got, err)
	}
	if d := stale.Value() - staleBefore; d != 1 {
		t.Fatalf("stale counter delta after a confirmed table went stale = %d, want 1", d)
	}
	if !strings.Contains(buf.String(), "not attributable") {
		t.Fatalf("a lost table must warn:\n%s", buf.String())
	}
}

// TestSourceConcurrentSumsShareTheIndex pins that one Source may be used by
// the accounts the poller refreshes concurrently (one goroutine per key group,
// each summing three windows): every caller sees the same totals, the index is
// built once, and the shared state stays race-free (run with -race).
func TestSourceConcurrentSumsShareTheIndex(t *testing.T) {
	from := testWindowBase.Unix()
	to := testWindowBase.Add(time.Hour).Unix()
	const id = "bdf422fa85"
	path := writeUsageLog(t,
		usageFixtureRow{keyID: id, at: testWindowBase.Add(time.Minute), in: 100},
		usageFixtureRow{keyID: id, at: testWindowBase.Add(2 * time.Minute), in: 30},
	)
	src := newTestSource(t, path)

	const workers = 8
	const rounds = 5
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	results := make(chan int64, workers*rounds)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				got, err := src.SumClinePassTokensByAccount(context.Background(), id, from, to)
				if err != nil {
					errs <- err
					return
				}
				results <- got
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("concurrent sum: %v", err)
	}
	for got := range results {
		if got != 130 {
			t.Fatalf("concurrent sum = %d, want 130 (one shared index, one answer)", got)
		}
	}
}
