package magpieusage

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providersFixture is a providers.json fixture written as a literal, not
// through the production struct: the FILE shape is the contract, so a drifted
// providerEntry must fail a test instead of certifying itself.
//
// It carries the entries the discovery must handle: a ClinePass entry with a
// primary key plus a keys[] entry, a second provider that must be ignored, a
// ClinePass entry whose keys are all empty, an upper-case id that must NOT
// match (the filter is exact), and a ClinePass entry with keys[] only.
const providersFixture = `{
  "providers": [
    {"id": "clinepass", "name": "Cline", "key": "tok-primary", "keys": [{"key": "tok-secondary"}]},
    {"id": "gemini", "name": "Gemini", "key": "gem-key", "keys": [{"key": "gem-key-2"}]},
    {"id": "clinepass", "key": "", "keys": [{"key": ""}]},
    {"id": "CLINEPASS", "key": "tok-case-fold", "keys": []},
    {"id": "clinepass", "keys": [{"key": "tok-third"}]},
    {"id": "clinepass", "key": "tok-primary", "keys": [{"key": "tok-primary"}, {"key": "tok-secondary"}]}
  ],
  "groups": [{"id": "g1", "name": "all"}],
  "order": ["clinepass"],
  "groupOrder": []
}`

// writeProviders writes a providers.json fixture and returns its path.
func writeProviders(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestListClinePassAccounts pins the account discovery: only the "clinepass"
// provider entry (an exact id match) contributes, its primary key comes first
// and its keys[] entries follow in file order, empty keys are skipped, and a
// key listed twice is ONE account. The account id is magpie's providerKeyId
// for the key, and the display name is derived from it (magpie's provider file
// carries no human label for a key).
func TestListClinePassAccounts(t *testing.T) {
	path := writeProviders(t, providersFixture)
	src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), path)

	got, err := src.ListClinePassAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"tok-primary", "tok-secondary", "tok-third"}
	if len(got) != len(wantKeys) {
		t.Fatalf("roster = %+v, want %d accounts", got, len(wantKeys))
	}
	for i, want := range wantKeys {
		a := got[i]
		if a.Key != want {
			t.Fatalf("roster[%d].Key = %q, want %q (primary key first, then keys[] in file order)", i, a.Key, want)
		}
		if a.AccountID != KeyID(want) {
			t.Fatalf("roster[%d].AccountID = %q, want the key's providerKeyId %q", i, a.AccountID, KeyID(want))
		}
		if a.Name != clinePassProviderID+"#"+a.AccountID {
			t.Fatalf("roster[%d].Name = %q, want magpie's own label %q", i, a.Name, clinePassProviderID+"#"+a.AccountID)
		}
		if len(a.AccountID) != KeyIDLen {
			t.Fatalf("roster[%d].AccountID = %q, want %d hex characters", i, a.AccountID, KeyIDLen)
		}
	}
	// The upper-case id and the other provider must not have contributed.
	for _, a := range got {
		if a.Key == "tok-case-fold" || a.Key == "gem-key" {
			t.Fatalf("roster leaked a non-ClinePass or case-folded entry: %+v", a)
		}
	}

	// Listing is read-only: magpie's file must come back byte for byte.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.ListClinePassAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the provider file changed: prism must read magpie's files, never write them")
	}
}

// TestKeyID pins the providerKeyId derivation against hand-written literals
// (never against the production constant, which would certify itself): the
// lowercase hex of the first 10 characters of SHA-256 of the key. It is the id
// magpie itself stamps on the usage lines, and it is the ONLY form of an
// account identity that leaves this package — the raw key cannot be recovered
// from it.
func TestKeyID(t *testing.T) {
	cases := []struct{ key, want string }{
		{"tok-primary", "bdf422fa85"},
		{"tok-secondary", "26b5925350"},
		{"tok-a", "4f66a4283f"},
		// sha256("") = e3b0c44298fc1c14…: an empty key has a real-looking id,
		// which is exactly why the discovery skips empty keys instead of
		// admitting them as an account.
		{"", "e3b0c44298"},
	}
	for _, tc := range cases {
		if got := KeyID(tc.key); got != tc.want {
			t.Fatalf("KeyID(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	// Length and alphabet are part of the contract (they are what the sums key
	// on, and a longer id would change the providerKeyId magpie writes).
	if got := KeyID("tok-primary"); len(got) != KeyIDLen || strings.ToLower(got) != got || strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("KeyID = %q, want %d lowercase hex characters", got, KeyIDLen)
	}
}

// TestListClinePassAccountsMissingFileIsSilent pins the deployment rule for a
// host without magpie: no provider file is an EMPTY successful roster with NO
// log output — not an error and not a warning, on every round. That silence is
// what keeps a magpie-less host quiet while the estimate degrades on its own.
//
// It also pins the other half of that rule: discovery does NOT participate in
// the degradation state machine (see ListClinePassAccounts), so it must not move
// clinepass_usage_source_status — that metric means "the usage log could not be
// read" and is driven by the sums.
func TestListClinePassAccountsMissingFileIsSilent(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	statusBefore := sourceStatus.Value()
	src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), filepath.Join(t.TempDir(), "nope", "providers.json"))
	got, err := src.ListClinePassAccounts(context.Background())
	if err != nil {
		t.Fatalf("a missing provider file must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("roster = %+v, want none", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("a host without magpie must stay silent, got:\n%s", buf.String())
	}
	if v := sourceStatus.Value(); v != statusBefore {
		t.Fatalf("source status moved from %q to %q: account discovery does not publish this metric", statusBefore, v)
	}
}

// TestListClinePassAccountsUnusableFile pins the other half of that rule: a
// file that EXISTS but cannot be read or parsed IS an error, so the caller can
// keep the previous roster instead of silently dropping the ClinePass block —
// and it is an error of the DISCOVERY, not of the usage source: neither
// clinepass_usage_source_errors nor clinepass_usage_source_status moves, because
// both mean "the usage log could not be read" (the failure is reported to the
// caller, which owns the roster rule and logs it).
func TestListClinePassAccountsUnusableFile(t *testing.T) {
	ctx := context.Background()
	errorsBefore, statusBefore := sourceErrors.Value(), sourceStatus.Value()

	t.Run("broken json", func(t *testing.T) {
		src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), writeProviders(t, `{"providers": [`))
		got, err := src.ListClinePassAccounts(ctx)
		if err == nil || got != nil {
			t.Fatalf("roster = (%+v, %v), want (nil, error)", got, err)
		}
	})

	t.Run("not an object", func(t *testing.T) {
		src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), writeProviders(t, `["clinepass"]`))
		if got, err := src.ListClinePassAccounts(ctx); err == nil || got != nil {
			t.Fatalf("roster = (%+v, %v), want (nil, error)", got, err)
		}
	})

	t.Run("directory at the path", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "providers.json")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), dir)
		if got, err := src.ListClinePassAccounts(ctx); err == nil || got != nil {
			t.Fatalf("roster = (%+v, %v), want (nil, error)", got, err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), " ")
		if got, err := src.ListClinePassAccounts(ctx); err == nil || got != nil {
			t.Fatalf("roster = (%+v, %v), want (nil, error)", got, err)
		}
	})

	if got := sourceErrors.Value(); got != errorsBefore {
		t.Fatalf("discovery failures moved clinepass_usage_source_errors by %d: the counter means the usage log could not be read", got-errorsBefore)
	}
	if got := sourceStatus.Value(); got != statusBefore {
		t.Fatalf("discovery failures moved clinepass_usage_source_status from %q to %q", statusBefore, got)
	}
}

// TestClinePassAccountsEmptyRoster pins that a provider file WITHOUT a
// ClinePass entry (magpie installed, that provider removed or renamed) is a
// successful empty roster — the caller's roster rule decides what to do with
// it, and the drop is reported there (clinePassRosterDelta in cmd/prism).
func TestClinePassAccountsEmptyRoster(t *testing.T) {
	cases := []struct{ name, content string }{
		{name: "no providers", content: `{"providers": []}`},
		{name: "only other providers", content: `{"providers": [{"id": "gemini", "key": "k"}]}`},
		{name: "clinepass without keys", content: `{"providers": [{"id": "clinepass", "keys": []}]}`},
		{name: "renamed provider id", content: `{"providers": [{"id": "cline-pass", "key": "k"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := NewSource(filepath.Join(t.TempDir(), "usage.jsonl"), writeProviders(t, tc.content))
			got, err := src.ListClinePassAccounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("roster = %+v, want none", got)
			}
		})
	}
}
