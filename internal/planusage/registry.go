package planusage

import (
	"crypto/sha256"
	"encoding/hex"
)

// DefaultFetchers is the built-in registry. Add a Fetcher here for the
// next upstream family; do not invent endpoints that are not public.
func DefaultFetchers() []Fetcher {
	return []Fetcher{GoFetcher{}}
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
