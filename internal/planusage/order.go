package planusage

import "strings"

// providerDisplayOrder is the curated display order of the quota
// providers, lowest first:
//
//	Gemini (gemini) < ClinePass (clinepass) < SuperGrok (xai)
//
// It is keyed by the snapshot's provider KEY, never by the account name:
// "ClinePass" sorts before "Gemini" lexicographically, which is the wrong
// reading order for the quota cards. Both RenderTableAt and RenderCards
// sort through accountSortKey, so this table is the single implementation
// of the card/table order. A provider not listed here sorts AFTER every
// curated one and falls back to lexicographic order among its peers;
// within one provider the first account name still breaks ties, so a
// provider's cards stay contiguous.
var providerDisplayOrder = []string{"gemini", "clinepass", "xai"}

// providerDisplayRank returns the rank of a provider in
// providerDisplayOrder and whether it is curated. Uncurated providers
// share the trailing rank len(providerDisplayOrder), so they order by
// account name alone, exactly as before this table existed.
func providerDisplayRank(provider string) (int, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	for i, known := range providerDisplayOrder {
		if p == known {
			return i, true
		}
	}
	return len(providerDisplayOrder), false
}
