package planusage

import (
	"strings"
	"testing"
	"time"
)

// TestAccountSortKeyProviderDisplayOrder pins the curated order Gemini →
// SuperGrok → ClinePass. The account names alone would sort ClinePass
// FIRST ("ClinePass" < "Gemini" < "SuperGrok"), so the provider rank has
// to decide before the name.
func TestAccountSortKeyProviderDisplayOrder(t *testing.T) {
	key := func(provider, account string) string {
		return accountSortKey(Snapshot{Provider: provider, Accounts: []string{account}})
	}
	gemini := key("gemini", "Gemini")
	clinepass := key("clinepass", "ClinePass")
	xai := key("xai", "SuperGrok")
	if !(gemini < xai && xai < clinepass) {
		t.Fatalf("provider display order broken: gemini=%q xai=%q clinepass=%q", gemini, xai, clinepass)
	}
	// Case-insensitive provider key (config keys are lowercase, but the
	// rank lookup normalizes).
	if upper := key("ClinePass", "ClinePass"); upper != clinepass {
		t.Fatalf("case-folded provider key = %q, want %q", upper, clinepass)
	}
}

// TestAccountSortKeyUncuratedFallback keeps the pre-existing behavior for
// providers without a curated rank: they sort after the curated ones and
// lexicographically by account name among themselves.
func TestAccountSortKeyUncuratedFallback(t *testing.T) {
	key := func(provider, account string) string {
		return accountSortKey(Snapshot{Provider: provider, Accounts: []string{account}})
	}
	xai := key("xai", "SuperGrok")
	acme := key("acme", "a1")
	z9 := key("opencode-go", "z9")
	if !(xai < acme && acme < z9) {
		t.Fatalf("uncurated fallback broken: xai=%q acme=%q z9=%q", xai, acme, z9)
	}
	// Provider fallback when a snapshot carries no account name.
	if got := accountSortKey(Snapshot{Provider: "opencode-go"}); got >= z9 {
		t.Fatalf("provider fallback %q must order before account z9 %q", got, z9)
	}
}

// TestRenderCardsCuratedProviderOrder is the card-level contract the CLI
// shows: shuffled snapshots render Gemini, SuperGrok, ClinePass — and a
// provider without a curated rank stays after them.
func TestRenderCardsCuratedProviderOrder(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	h := now.Add(time.Hour)
	got := RenderCards([]Snapshot{
		{Provider: "xai", Accounts: []string{"SuperGrok"}, Windows: []Window{{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &h}}},
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: []Window{{Name: "rolling", Status: "ok", Percent: 1, ResetsAt: &h}}},
		{Provider: "clinepass", Accounts: []string{"ClinePass"}, Windows: []Window{{Name: "5h", Status: "ok", Percent: 7, ResetsAt: &h}}},
		{Provider: "gemini", Accounts: []string{"Gemini"}, Windows: []Window{{Name: "5h", Status: "ok", Percent: 4, ResetsAt: &h}}},
	}, now, CardOptions{NoColor: true})

	var titles []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "╭─ ") {
			titles = append(titles, line)
		}
	}
	// Every title is the provider + its window label: the ACCOUNT lives on the
	// card's row (see RenderCards), never on the title.
	want := []string{"╭─ Gemini ·", "╭─ SuperGrok ·", "╭─ ClinePass ·", "╭─ Opus ·"}
	if len(titles) != len(want) {
		t.Fatalf("got %d cards, want %d:\n%s", len(titles), len(want), got)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(titles[i], prefix) {
			t.Fatalf("card %d = %q, want prefix %q:\n%s", i, titles[i], prefix, got)
		}
	}
	// The rows still carry their accounts, in the same provider order.
	var rows []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "│ · ") {
			rows = append(rows, line)
		}
	}
	for i, acc := range []string{"Gemini", "SuperGrok", "ClinePass", "a"} {
		if i >= len(rows) || !strings.HasPrefix(rows[i], "│ · "+acc+" ") {
			t.Fatalf("row %d = %q, want the account %q:\n%s", i, rows, acc, got)
		}
	}
}
