package planusage

import (
	"crypto/sha256"
	"encoding/hex"
)

// DefaultFetchers is the built-in registry: SuperGrok weekly pool
// (cli-chat-proxy billing), Gemini 5h/weekly (retrieveUserQuotaSummary)
// and ClinePass 5h/weekly/monthly (users/me/plan/usage-limits).
// OpenCode Go windows are not polled.
func DefaultFetchers() []Fetcher {
	return []Fetcher{XAIFetcher{}, GeminiFetcher{}, ClinePassFetcher{}}
}

// MatchFetcher returns the first fetcher that owns this account, or nil.
func MatchFetcher(fetchers []Fetcher, provider, baseURL string) Fetcher {
	for _, f := range fetchers {
		if f.Match(provider, baseURL) {
			return f
		}
	}
	return nil
}

// KeyFingerprint is a short non-reversible id for de-duplicating accounts
// that share an API key. Never log the raw key; this value is hex of the
// first 8 bytes of SHA-256.
func KeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// AccountIDFrom extracts the ClinePass account id from an AccountView when
// the view carries it (the magpie-backed clinepass accounts do). Non-
// magpie accounts return "".
//
// The id is magpie's providerKeyId — the hex of the first 10 characters of
// SHA-256 of the account's provider key (see magpieusage.KeyID) — and NOT a
// number: the source it is summed from is a JSONL log keyed by that string,
// not a SQL row with an integer primary key. An account whose view carries no
// id yields "", which the ClinePass estimate treats as "cannot be summed"
// rather than as "sum everything" (see the note in cmd/prism).
func AccountIDFrom(acc AccountView) string {
	type accountIDER interface {
		AccountID() string
	}
	if a, ok := acc.(accountIDER); ok {
		return a.AccountID()
	}
	return ""
}

// GroupByKey collapses accounts that share a key. Order of first appearance
// is preserved. Accounts with no matching fetcher are skipped.
func GroupByKey(accounts []AccountView, fetchers []Fetcher) []KeyGroup {
	seen := make(map[string]int)
	var groups []KeyGroup
	for _, acc := range accounts {
		f := MatchFetcher(fetchers, acc.Provider(), acc.BaseURL())
		if f == nil {
			continue
		}
		fp := KeyFingerprint(acc.Key())
		if i, ok := seen[fp]; ok {
			groups[i].Accounts = append(groups[i].Accounts, acc)
			continue
		}
		seen[fp] = len(groups)
		groups = append(groups, KeyGroup{
			Fingerprint: fp,
			Fetcher:     f,
			Accounts:    []AccountView{acc},
		})
	}
	return groups
}

// KeyGroup is one unique API key and the accounts that share it.
type KeyGroup struct {
	Fingerprint string
	Fetcher     Fetcher
	Accounts    []AccountView
}
