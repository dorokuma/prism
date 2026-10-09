package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/magpieusage"
	"github.com/dorokuma/prism/internal/planusage"
	"github.com/dorokuma/prism/internal/render"
)

// magpieFixtureRow is one usage.jsonl line in the CLI fixtures: the
// providerKeyId magpie stamped on the line ("" = the field is absent, which is
// how magpie records the traffic it could not attribute), the model, the
// instant and the 词元 split. cache is magpie's cache_read, written on the line
// but deliberately NOT part of the sum (see the molecule assertions).
type magpieFixtureRow struct {
	keyID string
	model string
	at    time.Time
	in    int64
	out   int64
	cache *int64
}

// cachePtr marks a fixture line as carrying magpie's cache_read.
func cachePtr(v int64) *int64 { return &v }

// magpieLine renders one usage.jsonl line. The fields are written explicitly
// (not through the production struct) so the fixtures pin the FILE shape.
func magpieLine(r magpieFixtureRow) string {
	model := r.model
	if model == "" {
		model = "cline-pass/x"
	}
	m := map[string]any{
		"t":        r.at.Format(time.RFC3339Nano),
		"provider": "clinepass",
		"model":    model,
		"in":       r.in,
		"out":      r.out,
	}
	if r.keyID != "" {
		m["providerKeyId"] = r.keyID
	}
	if r.cache != nil {
		m["cache_read"] = *r.cache
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// seedMagpieUsage writes a usage.jsonl fixture and returns its path.
func seedMagpieUsage(t *testing.T, rows []magpieFixtureRow) string {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(magpieLine(r))
		b.WriteString("\n")
	}
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedMagpieProviders writes magpie's provider file with ONE ClinePass entry:
// primary is its `key`, extra its keys[]. The file shape matches production
// (providers.json holds the provider entries, one id each).
func seedMagpieProviders(t *testing.T, primary string, extra ...string) string {
	t.Helper()
	keys := make([]map[string]string, 0, len(extra))
	for _, k := range extra {
		keys = append(keys, map[string]string{"key": k})
	}
	doc := map[string]any{
		"providers": []any{map[string]any{"id": "clinepass", "key": primary, "keys": keys}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// magpieTestSource builds the source a test's CLI path would use.
func magpieTestSource(t *testing.T, usagePath, providersPath string) *magpieusage.Source {
	t.Helper()
	return magpieusage.NewSource(usagePath, providersPath)
}

// absentProvidersPath is a provider file path that does not exist: the sums
// must not need it.
func absentProvidersPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "nope", "providers.json")
}

// TestApplyQuotaClinePassEstimatePerAccount pins the accountID != "" branch of
// the CLI estimate: with a magpie providerKeyId the sum is scoped to that
// account, so a second account's ClinePass traffic in the same window must not
// inflate the first account's 总额. It also pins the empty-id degrade: with no
// magpie id there is NO estimate — a subscription-wide sum would add one
// independent pool's traffic to the other's percent and print a 总额 that
// belongs to neither (串账).
func TestApplyQuotaClinePassEstimatePerAccount(t *testing.T) {
	now := time.Now()
	const tokA, tokB = "cline-token-alpha", "cline-token-bravo"
	usagePath := seedMagpieUsage(t, []magpieFixtureRow{
		{keyID: magpieusage.KeyID(tokA), at: now.Add(-30 * time.Minute), in: 4_200},
		{keyID: magpieusage.KeyID(tokB), at: now.Add(-30 * time.Minute), in: 7_000},
	})
	src := magpieTestSource(t, usagePath, absentProvidersPath(t))

	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	// A fresh snapshot per call: the estimator writes into the Window SLICE,
	// which the snapshot value shares with its caller.
	snapshot := func() planusage.Snapshot {
		return planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
			{Name: "weekly", Status: "ok", Percent: 34, PeriodStart: &start, ResetsAt: &end},
		}}
	}

	// Baseline of the shared skip counter: this test asserts DELTAS, so a
	// counter left over from another test cannot make it pass or fail.
	skippedBefore := clinepassEstimateSkipped.Value()

	got := applyQuotaClinePassEstimate(context.Background(), snapshot(), src, magpieusage.KeyID(tokA))
	// 4200 / 0.345 = 12174: the integer 34 % percent goes through the midpoint
	// correction, so the pool is 12174 and not the old 12353.
	if got.Windows[0].LimitTokensEstimate != 12_174 {
		t.Fatalf("account A estimate = %d, want 12174 (its own 4200 tokens ÷ 34%%)", got.Windows[0].LimitTokensEstimate)
	}
	other := applyQuotaClinePassEstimate(context.Background(), snapshot(), src, magpieusage.KeyID(tokB))
	if other.Windows[0].LimitTokensEstimate != 20_290 {
		t.Fatalf("account B estimate = %d, want 20290 (its own 7000 tokens ÷ 34%%)", other.Windows[0].LimitTokensEstimate)
	}
	// A scoped account is NOT a skip: the counter must not move for either
	// non-empty id call above.
	if d := clinepassEstimateSkipped.Value() - skippedBefore; d != 0 {
		t.Fatalf("a scoped account must not bump the skipped counter: delta = %d, want 0", d)
	}

	// The empty-id degrade is COUNTED (expvar
	// clinepass_quota_estimate_skipped_total, published on /metrics) so an
	// account that stopped producing a 总额 is visible instead of merely
	// blank: this ONE call must move the counter by exactly 1.
	noneBefore := clinepassEstimateSkipped.Value()
	none := applyQuotaClinePassEstimate(context.Background(), snapshot(), src, "")
	if none.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("an empty account id must yield NO estimate (two pools must not be summed), got %d",
			none.Windows[0].LimitTokensEstimate)
	}
	if d := clinepassEstimateSkipped.Value() - noneBefore; d != 1 {
		t.Fatalf("skipped counter delta = %d, want exactly +1", d)
	}
}

// TestCLIAssemblyCarriesAccountFingerprints pins F1 on the CLI shape: the
// ClinePass accounts come from the real magpie discovery
// (readClinePassAccounts), the snapshots are completed by the same
// buildQuotaSnapshot runQuotaWith uses, and the result is rendered by the
// shared RenderCards. Before the wiring, the CLI never stamped the snapshots'
// fingerprints: a production `prism quota` drew the bare · instead of a colour
// dot, an uncoloured account name and position-keyed rows, so "two accounts
// are told apart by their dot" was unreachable outside the service.
//
// The upstream fetch is the one part not exercised here (tests do not call
// cline.bot): the snapshot a fetcher returns for one account is built directly
// and then run through the CLI assembly.
func TestCLIAssemblyCarriesAccountFingerprints(t *testing.T) {
	now := time.Now()
	// Two provider keys whose fingerprints map to different palette colours
	// (the fixture key choice is deliberate, the palette itself is only 6 wide
	// — see the leftover list).
	const tokA, tokB = "cline-token-alpha", "cline-token-bravo"
	usagePath := seedMagpieUsage(t, []magpieFixtureRow{
		{keyID: magpieusage.KeyID(tokA), at: now.Add(-30 * time.Minute), in: 4_200},
		{keyID: magpieusage.KeyID(tokB), at: now.Add(-30 * time.Minute), in: 7_000},
	})
	providersPath := seedMagpieProviders(t, tokA, tokB)

	ctx := context.Background()
	src := magpieTestSource(t, usagePath, providersPath)
	views, err := readClinePassAccounts(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("discovered accounts = %d, want 2 (the provider's key plus its keys[] entry)", len(views))
	}
	// The display name is magpie's own account label: the provider id and the
	// providerKeyId. magpie's provider file carries NO human label for a key,
	// so the key-derived id is the only faithful name available (and
	// /admin/quota returns it too).
	if got, want := views[0].Name(), "clinepass#"+magpieusage.KeyID(tokA); got != want {
		t.Fatalf("account name = %q, want %q", got, want)
	}

	groups := planusage.GroupByKey(views, planusage.DefaultFetchers())
	if len(groups) != 2 {
		t.Fatalf("key groups = %d, want one snapshot per account key", len(groups))
	}
	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	cfg := &config.Config{}
	snaps := make([]planusage.Snapshot, 0, len(groups))
	for _, g := range groups {
		fetched := planusage.Snapshot{Provider: "clinepass", FetchedAt: now, Windows: []planusage.Window{
			{Name: "weekly", Status: "ok", Percent: 34, PeriodStart: &start, ResetsAt: &end},
		}}
		snaps = append(snaps, buildQuotaSnapshot(ctx, cfg, g, fetched, nil, src))
	}

	// The fingerprint is the KEY fingerprint (the same 口径 as the service
	// poller), it is never empty, and the 总额 of each snapshot counts only its
	// own account's rows.
	byFP := map[string]planusage.Snapshot{}
	for _, s := range snaps {
		fps := s.AccountFPs()
		if len(fps) != 1 || len(s.Accounts) != 1 {
			t.Fatalf("snapshot must carry one account name and one fingerprint: %+v", s)
		}
		if fps[0] != planusage.KeyFingerprint(tokA) && fps[0] != planusage.KeyFingerprint(tokB) {
			t.Fatalf("snapshot fingerprint %q is not the provider key's fingerprint", fps[0])
		}
		byFP[fps[0]] = s
	}
	if len(byFP) != 2 {
		t.Fatalf("fingerprints = %d, want 2 distinct", len(byFP))
	}
	// 4200 / 0.345 = 12174 and 7000 / 0.345 = 20290: the integer 34 % percent
	// goes through the midpoint correction (windowUsedFraction).
	for tok, want := range map[string]int64{tokA: 12_174, tokB: 20_290} {
		s, ok := byFP[planusage.KeyFingerprint(tok)]
		if !ok {
			t.Fatalf("no snapshot for the account keyed by %s", tok)
		}
		if got := s.Windows[0].LimitTokensEstimate; got != want {
			t.Fatalf("account %s estimate = %d, want %d (its own rows only)", tok, got, want)
		}
	}

	colored := planusage.RenderCards(snaps, now)
	plain := planusage.RenderCards(snaps, now, planusage.CardOptions{NoColor: true})
	if n := strings.Count(plain, "╭─ "); n != 1 {
		t.Fatalf("want ONE merged card, got %d:\n%s", n, plain)
	}
	// One row per account, named by magpie's label (no trailing-digit
	// stripping here: both ids end in a letter).
	names := []string{"clinepass#" + magpieusage.KeyID(tokA), "clinepass#" + magpieusage.KeyID(tokB)}
	var rows []string
	for _, line := range strings.Split(strings.TrimSuffix(colored, "\n"), "\n") {
		if strings.Contains(render.StripANSI(line), "│ · "+names[0]+" ") || strings.Contains(render.StripANSI(line), "│ · "+names[1]+" ") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want two account rows %v:\n%s", names, plain)
	}
	colors := map[string]bool{}
	for _, r := range rows {
		dot, name := sgrBefore(t, r, "·"), sgrBefore(t, r, "clinepass#")
		if dot == "" {
			t.Fatalf("the dot must be coloured, not the bare ·: %q", r)
		}
		if dot != name {
			t.Fatalf("the dot and the account name must share one colour: dot=%q name=%q\n%q", dot, name, r)
		}
		colors[dot] = true
	}
	if len(colors) != 2 {
		t.Fatalf("the two rows must carry different colours:\n%s", render.StripANSI(colored))
	}
	if plain != render.StripANSI(colored) {
		t.Fatalf("the no-colour render must be the coloured one minus the escapes")
	}

	// Row identity is the fingerprint, not the position: the same assembled
	// snapshots delivered twice (a roster repeated across snapshots) still
	// render exactly two rows, where position-keyed rows would double to four.
	dup := append(append([]planusage.Snapshot(nil), snaps...), snaps...)
	if n := strings.Count(planusage.RenderCards(dup, now, planusage.CardOptions{NoColor: true}), "│ · clinepass#"); n != 2 {
		t.Fatalf("repeated roster = %d rows, want 2 (rows must be keyed by the fingerprint)", n)
	}
}

// TestClinePassDisplayNameStripsTrailingDigits pins the interaction between
// the account label and the renderer's display rule: a row SHOWS its name with
// the trailing run of digits dropped (stripNumericSuffix, the rule that folds
// "Cline1"/"Cline2" into one reading). A magpie providerKeyId can end in
// digits — the production ids do — so the displayed label of such an account is
// the id MINUS that run. The row is still ONE row and still keyed by the full
// name plus the fingerprint, so nothing is merged; the label is just shorter
// than the id. This test exists so that shortening is a pinned, visible
// behaviour instead of a surprise on a card (see .agents/notes).
func TestClinePassDisplayNameStripsTrailingDigits(t *testing.T) {
	now := time.Now()
	// KeyID("tok-primary") = bdf422fa85: the displayed row must read
	// "clinepass#bdf422fa".
	const tok = "tok-primary"
	if got := magpieusage.KeyID(tok); got != "bdf422fa85" {
		t.Fatalf("fixture key id = %q, want bdf422fa85", got)
	}
	usagePath := seedMagpieUsage(t, []magpieFixtureRow{{keyID: magpieusage.KeyID(tok), at: now.Add(-time.Minute), in: 100}})
	src := magpieTestSource(t, usagePath, absentProvidersPath(t))

	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	snap := applyQuotaClinePassEstimate(context.Background(), planusage.Snapshot{
		Provider: "clinepass", Accounts: []string{"clinepass#" + magpieusage.KeyID(tok)},
		Windows: []planusage.Window{{Name: "weekly", Status: "ok", Percent: 10, PeriodStart: &start, ResetsAt: &end}},
	}, src, magpieusage.KeyID(tok))

	card := planusage.RenderCards([]planusage.Snapshot{snap}, now, planusage.CardOptions{NoColor: true})
	if !strings.Contains(card, "│ · clinepass#bdf422fa ") {
		t.Fatalf("the row must show the account label with its trailing digits dropped:\n%s", card)
	}
	if strings.Contains(card, "clinepass#bdf422fa85") {
		t.Fatalf("the full providerKeyId must not reach the card (stripNumericSuffix drops trailing digits):\n%s", card)
	}
}

// sgrBefore returns the ANSI SGR sequence emitted immediately before the first
// occurrence of sub in s, or "" when sub is not colour-wrapped.
func sgrBefore(t *testing.T, s, sub string) string {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("%q not found in %q", sub, s)
	}
	j := strings.LastIndex(s[:i], "\x1b[")
	if j < 0 {
		return ""
	}
	end := strings.Index(s[j:], "m")
	if end < 0 {
		return ""
	}
	return s[j : j+end+1]
}

// stubQuotaView is a minimal planusage.AccountView for the roster rules.
type stubQuotaView struct{ name, provider string }

func (v stubQuotaView) Name() string         { return v.name }
func (v stubQuotaView) Provider() string     { return v.provider }
func (v stubQuotaView) BaseURL() string      { return "" }
func (v stubQuotaView) Key() string          { return "key-" + v.name }
func (v stubQuotaView) AuthHeader() string   { return "" }
func (v stubQuotaView) Client() *http.Client { return nil }

// TestNextQuotaViews pins the SIGHUP re-discovery rule: a successful discovery
// always wins — including an EMPTY one, which is how a removed account
// actually leaves the roster — while a FAILED discovery keeps the previous
// magpie-backed views, so a transient magpie error cannot make the whole
// ClinePass block silently disappear from the cards. Non-clinepass views are
// never carried over from prev: they are rebuilt from the pool every round
// (and must not be duplicated).
func TestNextQuotaViews(t *testing.T) {
	cfgViews := []planusage.AccountView{stubQuotaView{name: "gemini-1", provider: "gemini"}}
	prev := []planusage.AccountView{
		stubQuotaView{name: "gemini-1", provider: "gemini"},
		stubQuotaView{name: "clinepass#aaaaaaaaaa", provider: "clinepass"},
		stubQuotaView{name: "clinepass#bbbbbbbbbb", provider: "clinepass"},
	}
	discovered := []planusage.AccountView{
		stubQuotaView{name: "clinepass#bbbbbbbbbb", provider: "clinepass"},
		stubQuotaView{name: "clinepass#cccccccccc", provider: "clinepass"},
	}

	cases := []struct {
		name       string
		discovered []planusage.AccountView
		err        error
		want       []string
	}{
		{
			name:       "successful discovery replaces the roster",
			discovered: discovered,
			want:       []string{"gemini-1", "clinepass#bbbbbbbbbb", "clinepass#cccccccccc"},
		},
		{
			name: "empty discovery removes the accounts",
			want: []string{"gemini-1"},
		},
		{
			name: "failed discovery keeps the previous clinepass views",
			err:  errors.New("magpie down"),
			want: []string{"gemini-1", "clinepass#aaaaaaaaaa", "clinepass#bbbbbbbbbb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextQuotaViews(cfgViews, tc.discovered, prev, tc.err)
			var names []string
			for _, a := range got {
				names = append(names, a.Name())
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("roster = %v, want %v", names, tc.want)
			}
		})
	}
}

// TestReadClinePassAccounts pins the CLI adapter's two halves: a host WITHOUT
// a magpie provider file yields no accounts, NO error and NO log line (a
// magpie-less host must stay silent on every `prism quota`), while a provider
// file that exists but cannot be read or parsed IS an error the CLI reports.
func TestReadClinePassAccounts(t *testing.T) {
	ctx := context.Background()

	t.Run("no magpie stays silent", func(t *testing.T) {
		var buf bytes.Buffer
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(oldDefault) })

		src := magpieTestSource(t, filepath.Join(t.TempDir(), "usage.jsonl"), absentProvidersPath(t))
		views, err := readClinePassAccounts(ctx, src)
		if err != nil {
			t.Fatalf("a missing provider file must not be an error: %v", err)
		}
		if len(views) != 0 {
			t.Fatalf("accounts = %+v, want none", views)
		}
		if buf.Len() != 0 {
			t.Fatalf("a host without magpie must stay silent, got:\n%s", buf.String())
		}
	})

	t.Run("unusable provider file is an error", func(t *testing.T) {
		broken := filepath.Join(t.TempDir(), "providers.json")
		if err := os.WriteFile(broken, []byte(`{"providers": [`), 0o600); err != nil {
			t.Fatal(err)
		}
		src := magpieTestSource(t, filepath.Join(t.TempDir(), "usage.jsonl"), broken)
		views, err := readClinePassAccounts(ctx, src)
		if err == nil || views != nil {
			t.Fatalf("accounts = (%+v, %v), want (nil, error)", views, err)
		}
		if !strings.Contains(err.Error(), "list clinepass accounts") {
			t.Fatalf("error = %v, want the discovery context", err)
		}
	})
}

// TestClinePassRosterDelta pins the shrink-to-zero signal added for the
// silent-shrink paths (O8/O9): a round that leaves an empty ClinePass roster
// although the previous round had accounts must produce a reason (the WARN +
// clinepass_quota_roster_drops_total counter), and the count is the size of the
// new roster for clinepass_quota_accounts.
func TestClinePassRosterDelta(t *testing.T) {
	generic := []planusage.AccountView{stubQuotaView{name: "gemini-1", provider: "gemini"}}
	prev := []planusage.AccountView{
		stubQuotaView{name: "gemini-1", provider: "gemini"},
		stubQuotaView{name: "clinepass#aaaaaaaaaa", provider: "clinepass"},
		stubQuotaView{name: "clinepass#bbbbbbbbbb", provider: "clinepass"},
	}
	cases := []struct {
		name             string
		prev, next       []planusage.AccountView
		providersPresent bool
		err              error
		wantCount        int
		wantReason       string
	}{
		{
			name: "provider file absent drops the block", prev: prev, next: generic,
			providersPresent: false, wantCount: 0, wantReason: "magpie providers absent",
		},
		{
			// A magpie-side rename of the provider id, a removed entry or an
			// entry whose keys are all empty all surface as a successful
			// discovery that returned nothing.
			name: "successful discovery without accounts drops the block", prev: prev, next: generic,
			providersPresent: true, wantCount: 0, wantReason: "no clinepass accounts discovered",
		},
		{
			name: "failed discovery keeps the previous views and stays quiet", prev: prev, next: prev,
			providersPresent: true, err: errors.New("magpie down"), wantCount: 2, wantReason: "",
		},
		{
			// Nothing was removed: an already empty roster must not warn.
			name: "empty to empty stays quiet", prev: generic, next: generic,
			providersPresent: false, wantCount: 0, wantReason: "",
		},
		{
			name: "shrinking to a non-zero size stays quiet", prev: prev,
			next:             []planusage.AccountView{stubQuotaView{name: "clinepass#bbbbbbbbbb", provider: "clinepass"}},
			providersPresent: true, wantCount: 1, wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, reason := clinePassRosterDelta(tc.prev, tc.next, tc.providersPresent, tc.err)
			if count != tc.wantCount || reason != tc.wantReason {
				t.Fatalf("delta = (%d, %q), want (%d, %q)", count, reason, tc.wantCount, tc.wantReason)
			}
		})
	}
}

// TestApplyQuotaClinePassEstimate pins the CLI path of `prism quota`: the
// ClinePass POOL windows (weekly / monthly) get ONE weekly-anchored 总额 from
// magpie's cline-pass consumption of THAT account — monthly is exactly twice
// the weekly pool — the 5-hour window gets its own, and the numbers are the
// ones the shared planusage.ApplyClinePassEstimates produces from the same
// source (CLI and /admin/quota cannot drift).
func TestApplyQuotaClinePassEstimate(t *testing.T) {
	now := time.Now()
	end5h := now.Add(2 * time.Hour)
	start5h := end5h.Add(-5 * time.Hour)
	endW := now.Add(72 * time.Hour)
	startW := endW.Add(-7 * 24 * time.Hour)
	endM := now.Add(10 * 24 * time.Hour)
	startM := endM.AddDate(0, -1, 0)
	const tok = "cline-token-alpha"
	id := magpieusage.KeyID(tok)

	usagePath := seedMagpieUsage(t, []magpieFixtureRow{
		// In the 5h, weekly AND monthly window: 20000 tokens (in + out, with
		// 9800 cache-read tokens on the line that must NOT count).
		{keyID: id, model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-time.Hour), in: 12_000, out: 8_000, cache: cachePtr(9_800)},
		// In the weekly and monthly window: 30000 more.
		{keyID: id, model: "cline-pass/kimi-k3", at: now.Add(-48 * time.Hour), in: 18_000, out: 12_000},
		// Older than the weekly window, still in the monthly one: 40000 more.
		{keyID: id, model: "cline-pass/kimi-k3", at: now.Add(-10 * 24 * time.Hour), in: 25_000, out: 15_000},
		// Ignored: another provider, another ACCOUNT (its pool is its own),
		// a cline-pass row older than the monthly window, and a line magpie
		// could not attribute to any account.
		{model: "gpt-5", at: now.Add(-time.Hour), in: 9_999_999},
		{keyID: magpieusage.KeyID("cline-token-bravo"), at: now.Add(-time.Hour), in: 9_999_999},
		{keyID: id, model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-40 * 24 * time.Hour), in: 9_999_999},
		{at: now.Add(-time.Hour), in: 9_999_999},
	})
	src := magpieTestSource(t, usagePath, absentProvidersPath(t))

	snap := planusage.Snapshot{Provider: "clinepass", Accounts: []string{"clinepass#" + id}, Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 10, PeriodStart: &start5h, ResetsAt: &end5h},
		{Name: "weekly", Status: "ok", Percent: 50, PeriodStart: &startW, ResetsAt: &endW},
		{Name: "monthly", Status: "ok", Percent: 5, PeriodStart: &startM, ResetsAt: &endM},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, src, id)
	// Midpoint correction on every integer percent (10 % → 0.105, 50 % → 0.505,
	// 5 % → 0.055): the weekly molecule is 20000+30000 = 50000 at 50 % →
	// L = 50000/0.505 = 99010, the monthly window is 2L whatever its own 5 %
	// would have reversed to, and the 5-hour window is reversed on its OWN
	// molecule (20000 in the window) and its own percent: 20000/0.105 = 190476
	// — not 0 and not the weekly-anchored pool.
	want := map[string]int64{"5h": 190_476, "weekly": 99_010, "monthly": 198_020}
	for _, w := range got.Windows {
		if w.LimitTokensEstimate != want[w.Name] {
			t.Fatalf("%s estimate = %d, want %d", w.Name, w.LimitTokensEstimate, want[w.Name])
		}
	}

	// The service-side path over the same source must produce identical
	// numbers: both run planusage.ApplyClinePassEstimates.
	direct := planusage.ApplyClinePassEstimates(context.Background(), snap, func(c context.Context, from, to int64) (int64, error) {
		return src.SumClinePassTokensByAccount(c, id, from, to)
	}, now)
	for i := range got.Windows {
		if got.Windows[i].LimitTokensEstimate != direct.Windows[i].LimitTokensEstimate {
			t.Fatalf("CLI %s = %d, service path = %d",
				got.Windows[i].Name, got.Windows[i].LimitTokensEstimate, direct.Windows[i].LimitTokensEstimate)
		}
	}

	// The CLI renders the totals: the derived pool is written like any other
	// total (no "~" marker, no 估算池 title segment), and the 5-hour row now
	// carries its own pair. The reset countdown moved to the title, once per
	// card.
	//
	// The unit follows the TOTAL of each window (亿 / 万 / 千 — 1e4 here), and
	// BOTH halves of a pair are rendered in it: these small fixtures used to read
	// "0亿/0亿" while the snapshot said 190476, i.e. the card and /admin/quota's
	// JSON disagreed about the same window. The used side is the percent of that
	// total (10 % of 190476 = 19047 → "1.9万"), so a pair like "5万/9.9万" is two
	// numbers in one unit the reader can compare directly.
	cards := planusage.RenderCards([]planusage.Snapshot{got}, now, planusage.CardOptions{NoColor: true})
	for _, wantText := range []string{
		"╭─ ClinePass · 5小时限额 · 2小时00分 ",
		"╭─ ClinePass · 周限额 · 3天 ",
		"╭─ ClinePass · 月限额 · 10天 ",
		"1.9万/19万",
		"5万/9.9万",
		"1万/19.8万",
	} {
		if !strings.Contains(cards, wantText) {
			t.Fatalf("cards missing %q:\n%s", wantText, cards)
		}
	}
	if strings.Contains(cards, "0亿") {
		t.Fatalf("a measured pool must not be reported as 0亿 (the 亿 unit alone collapsed these fixtures):\n%s", cards)
	}
	// The countdown is window-level: once on the 5h title, never on a row.
	if n := strings.Count(cards, "2小时00分"); n != 1 {
		t.Fatalf("countdown occurs %d times, want 1 (the title):\n%s", n, cards)
	}
}

// TestApplyQuotaClinePassEstimateMissingLog pins the degradation: a missing
// magpie usage log leaves every window without a 总额 and does not touch the
// snapshot (no error, no state change).
func TestApplyQuotaClinePassEstimateMissingLog(t *testing.T) {
	src := magpieTestSource(t, filepath.Join(t.TempDir(), "nope", "usage.jsonl"), absentProvidersPath(t))

	start := time.Now().Add(-time.Hour)
	end := start.Add(5 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 7, PeriodStart: &start, ResetsAt: &end},
	}}
	// A missing log leaves every window without a 总额 and does not touch the
	// snapshot. The account id is a real one: the empty-id path is a
	// DIFFERENT degrade and is pinned by
	// TestApplyQuotaClinePassEstimatePerAccount.
	got := applyQuotaClinePassEstimate(context.Background(), snap, src, magpieusage.KeyID("tok-primary"))
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("missing log must leave the snapshot estimate empty: %+v", got)
	}
}

// TestApplyQuotaClinePassEstimateNoUsableLines pins the other degraded file
// shape: the log EXISTS but holds nothing usable (empty, truncated to
// whitespace, or written before magpie recorded a call). Reporting 0 would
// claim the account consumed nothing and inflate the reversed pool, so the sum
// must refuse and the CLI must leave the window empty.
func TestApplyQuotaClinePassEstimateNoUsableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := magpieTestSource(t, path, absentProvidersPath(t))

	start := time.Now().Add(-time.Hour)
	end := start.Add(5 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 7, PeriodStart: &start, ResetsAt: &end},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, src, magpieusage.KeyID("tok-primary"))
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("a log with no usable line must leave the snapshot estimate empty: %+v", got)
	}
}

// TestApplyQuotaClinePassEstimateNonRegularLog pins the path-shape degrade: a
// directory at the usage-log path is a configuration mistake, and it must
// degrade this round's estimate instead of failing the quota fetch (or —
// for a fifo — blocking it).
func TestApplyQuotaClinePassEstimateNonRegularLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	src := magpieTestSource(t, path, absentProvidersPath(t))

	start := time.Now().Add(-time.Hour)
	end := start.Add(5 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 7, PeriodStart: &start, ResetsAt: &end},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, src, magpieusage.KeyID("tok-primary"))
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("a non-regular log must leave the snapshot estimate empty: %+v", got)
	}
}

// TestApplyQuotaClinePassEstimateLineDrift pins the M11 degrade end to end on
// the CLI path: a usage line whose `t` is no longer RFC3339 makes the sums
// fail, so the CLI leaves the windows without a 总额 and never touches the
// snapshot. The fixture mixes the drifted line with a VALID in-window one, so a
// lenient reader would happily produce a number (the valid line's tokens) —
// the assertion is that it produces NOTHING instead.
func TestApplyQuotaClinePassEstimateLineDrift(t *testing.T) {
	now := time.Now()
	const tok = "tok-primary"
	id := magpieusage.KeyID(tok)
	valid := magpieFixtureRow{keyID: id, at: now.Add(-time.Minute), in: 100}
	usagePath := seedMagpieUsage(t, []magpieFixtureRow{valid})
	// Append a line in metapi's old created_at shape (space separated, no
	// zone): a reader without the shape guard would either skip it or parse it
	// as local time and mis-bound the window.
	f, err := os.OpenFile(usagePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":"2026-09-25 21:41:03","provider":"clinepass","model":"cline-pass/x","providerKeyId":"` + id + `","in":200,"out":0}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	src := magpieTestSource(t, usagePath, absentProvidersPath(t))

	start := now.AddDate(0, -1, 0)
	end := now.Add(10 * 24 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "monthly", Status: "ok", Percent: 10, PeriodStart: &start, ResetsAt: &end},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, src, id)
	if got.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("drifted t must not produce a 总额: %+v", got.Windows[0])
	}
	if got.Err != "" {
		t.Fatalf("drift must not touch Snapshot.Err: %+v", got)
	}
	if got.Windows[0].Percent != 10 || got.Windows[0].Status != "ok" {
		t.Fatalf("drift must not touch the snapshot window: %+v", got.Windows[0])
	}
}
