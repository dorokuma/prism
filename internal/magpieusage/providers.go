package magpieusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// providersFile is the subset of magpie's providers.json this package reads:
// the provider entries. The file also carries groups/order/groupOrder, which
// are UI concerns and deliberately not decoded.
type providersFile struct {
	Providers []providerEntry `json:"providers"`
}

// providerEntry is one provider entry. Only the ClinePass entry is used, and
// only its keys: clinePassProviderID selects it, Key is its primary provider
// key and Keys the additional ones.
//
// magpie's entry carries many more fields (name, icon, preset, chat,
// models, contexts, website, pinUpstream, balanceURL/balancePath/balanceToken,
// … — measured 2026-10-07 on the production file). None of them is read here:
// the quota percent comes from cline.bot itself (internal/planusage), and the
// only thing prism needs from magpie is the account's key.
type providerEntry struct {
	ID   string        `json:"id"`
	Key  string        `json:"key"`
	Keys []providerKey `json:"keys"`
}

// providerKey is one element of a provider's keys[]. In the production file
// (measured 2026-10-07) an element holds EXACTLY one field, `key` — there is no
// name, label, email, remark or any other human label to use as a display
// name. That is why accounts are named by their key-derived id
// (clinePassLabel); if magpie ever adds a label field, it would have to be
// added here and preferred in clinePassAccounts.
type providerKey struct {
	Key string `json:"key"`
}

// clinePassAccount is one ClinePass account: magpie's providerKeyId for the
// key, the display name derived from it, and the raw provider key. It is
// deliberately unexported: only callers within this module can request it, and
// they must never log, render or persist Key (the display colour is derived
// from planusage.KeyFingerprint of the key — the SHA-256 first 8 bytes hex —
// never from the key itself).
type clinePassAccount struct {
	AccountID string
	Name      string
	Key       string
}

// ListClinePassAccounts reads magpie's provider file and returns the ClinePass
// accounts, primary key first. The raw Key is what the upstream poll
// authenticates with (Bearer): it goes only to the immediate caller
// (cmd/prism), is never logged and is never rendered.
//
// This is the ONE account-discovery implementation: the service roster
// (cmd/prism refreshQuotaAccounts, at startup and on SIGHUP) and the CLI
// (prism quota) both call it, so the two paths cannot drift on which accounts
// exist. A successful call also installs the roster's Track 2 tail table (see
// installRoster), which is what keeps attribution and polling on one key set.
//
// Discovery DELIBERATELY does not touch the degradation state machine
// (Source.observe): clinepass_usage_source_errors and
// clinepass_usage_source_status mean "the usage log could not be read" and are
// driven by the sums alone. Folding a provider-file read into them counted an
// incident that is not a usage-read failure and — on this function's nil-error
// path — republished "ok" over a genuine usage-log degradation, so a SIGHUP
// arriving while the log was unreadable made the status read ok on the way to
// the next round's WARN. The failure is still REPORTED: the error goes to the
// caller, which is the one that owns the roster (cmd/prism keeps the previous
// roster and logs the error).
//
// What a discovery that did NOT read the provider file does not do — whether it
// failed or found no file — is leave the previous tail table USABLE: it marks it
// stale (markAttributionStale), and a stale table attributes nothing — though one
// that was never confirmed is not reported as a loss either (see
// observeStaleAttribution). The keys the caller keeps are the keys it keeps
// POLLING, but the provider file prism could
// not read is exactly the file magpie rewrites when it rotates a key, so a table
// derived from that file as it looked before the rotation may place a keyless row
// on an account the row does not belong to: magpie rotates the key, the new key's
// last four characters happen to equal those of a key still in the roster, a
// keyless row arrives carrying the NEW key's mask, and the table credits it to
// the OLD account (串账 — the misplacement the whole track exists to prevent).
// Only a successful discovery can say which keys are current, so attribution is
// suspended until one happens: every keyless row then falls into the empty bucket
// and is counted in clinepass_usage_unattributed_rows_total like any other row no
// track places, and the suspension is visible in the sums that ran through it
// (clinepass_usage_attribution_stale_total) instead of hiding as a shrunken total.
//
// "No file" and "a file that names no account" are therefore NOT the same event
// here, even though both give an empty roster to the caller. A file that was read
// and parsed IS this round's answer about which accounts exist — including when
// the answer is "none" — and installs its (empty) table; a missing file answers
// nothing and installs nothing. Only the second may not announce a recovery.
func (s *Source) ListClinePassAccounts(ctx context.Context) ([]clinePassAccount, error) {
	accounts, found, err := clinePassAccounts(s.providersPath)
	if err != nil {
		s.markAttributionStale()
		return nil, err
	}
	if !found {
		// No provider file: there is no roster THIS discovery could confirm, so the
		// table of the last successful one is stale exactly as after a read error.
		// The caller still sees the empty roster it has always seen (a host without
		// magpie reports no accounts and no error, see clinePassAccounts); it is the
		// ATTRIBUTION that treats "no file" as "no confirmation".
		s.markAttributionStale()
		return nil, nil
	}
	s.installRoster(accounts)
	return accounts, nil
}

// clinePassAccounts parses providers.json at path. The second result reports
// whether the file was actually READ: "no provider file" and "a provider file
// that names no ClinePass account" both yield an empty roster but are different
// facts, and only the second is an answer about which accounts exist (see
// ListClinePassAccounts). A MISSING file is not an error — a host that does not
// run magpie has no provider file, and it must stay SILENT: no warning, no failed
// discovery, just no ClinePass accounts and no 总额. A file that exists but cannot
// be read or parsed IS an error, and the caller keeps the previous roster instead
// of dropping the block.
func clinePassAccounts(path string) ([]clinePassAccount, bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, errors.New("magpieusage: empty providers path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("magpieusage: read %s: %w", path, err)
	}
	var pf providersFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, false, fmt.Errorf("magpieusage: parse %s: %w", path, err)
	}
	var out []clinePassAccount
	seen := map[string]struct{}{}
	for _, p := range pf.Providers {
		// An exact match on the provider id, like the usage-line filter: both
		// files are written by magpie, so a case fold would only hide a rename
		// behind a partial match.
		if p.ID != clinePassProviderID {
			continue
		}
		for _, key := range accountKeys(p) {
			key := strings.TrimSpace(key)
			if key == "" {
				// An empty key cannot be polled and has no id (sha256("")[:10]
				// would be a real-looking id belonging to no account), so the
				// row is skipped — the same treatment the source this one
				// replaced gave a NULL/empty token.
				continue
			}
			id := KeyID(key)
			if _, dup := seen[id]; dup {
				// The same key listed twice (or in both key and keys[]) is one
				// account: magpie identifies an account by the key itself, so
				// deduping on the derived id is deduping on the key.
				continue
			}
			seen[id] = struct{}{}
			out = append(out, clinePassAccount{AccountID: id, Name: clinePassLabel(id), Key: key})
		}
	}
	return out, true, nil
}

// accountKeys is the provider's account keys in discovery order: the primary
// key, then keys[] in file order. The primary key is a key like any other
// (magpie's providerKeyId does not distinguish it), so it is first only
// because magpie itself lists it first — the roster order the cards render.
func accountKeys(p providerEntry) []string {
	out := make([]string, 0, 1+len(p.Keys))
	if strings.TrimSpace(p.Key) != "" {
		out = append(out, p.Key)
	}
	for _, k := range p.Keys {
		out = append(out, k.Key)
	}
	return out
}

// clineAccountTailLen is how many characters of a provider key magpie's masked
// account label shows, and therefore the length of the tail this package
// matches on (`API key …326d` → `326d`).
const clineAccountTailLen = 4

// clineAttribution is Track 2's lookup: the last clineAccountTailLen characters
// of a ClinePass provider key → the account id that key belongs to.
//
// It exists because magpie writes a share of the ClinePass traffic on lines it
// could NOT attribute to a provider key: no providerKeyId at all, and a MASKED
// account label instead (`providerAccount` `API key …326d`). That mask — four
// characters, not a name and not an id — is the only handle such a line offers,
// so the account is recovered by matching it against the roster's own key
// tails. The table is therefore key-derived but carries no key: a 4-character
// tail and a one-way id, nothing that could be turned back into a credential.
//
// A nil/empty table means NO keyless row may be attributed — the shape of a
// host with no ClinePass provider entry, of a roster no discovery has installed
// yet, of a roster whose tails collide (see newClineAttribution), and of a table
// the last discovery attempt did not confirm (see markAttributionStale) — and the
// reason scanUsageLog puts such a row in the empty bucket.
type clineAttribution map[string]string

// newClineAttribution derives Track 2's tail lookup from a ClinePass roster.
//
// Uniqueness is a REQUIREMENT, not a preference: the label carries a key's four
// characters and nothing else, so two accounts whose keys end in the same
// characters make every such label ambiguous, and attributing the traffic to
// either one would add one subscription's traffic to the other's pool (串账 —
// the hazard the per-account sum exists to avoid). A collision is therefore
// REPORTED rather than resolved by picking a winner: the second return value
// names the ids that share a tail, and Source.installRoster then drops the
// whole table (every keyless row unattributed) and WARNs. Note that "two
// accounts share a tail" and "a label matches two accounts" are the SAME event
// for fixed-length tails, which is why one check covers both.
//
// A key shorter than the tail length cannot produce a tail a 4-character mask
// would show, so it is skipped rather than matched on a shorter suffix.
func newClineAttribution(accounts []clinePassAccount) (clineAttribution, []string) {
	tails := make(map[string]string, len(accounts))
	var colliding []string
	for _, a := range accounts {
		if len(a.Key) < clineAccountTailLen {
			continue
		}
		tail := a.Key[len(a.Key)-clineAccountTailLen:]
		if other, dup := tails[tail]; dup {
			colliding = append(colliding, other, a.AccountID)
			continue
		}
		tails[tail] = a.AccountID
	}
	if len(colliding) > 0 {
		return nil, colliding
	}
	return clineAttribution(tails), nil
}

// attribute returns the account id the masked label of a keyless `cline` row
// belongs to, or "" when it belongs to none (including when there is no table
// at all).
//
// The test is a SUFFIX test on the whole label rather than an equality on a
// parsed tail, on purpose: the text around the mask is magpie's presentation,
// and the same key's mask is written under two different prefixes (`API key
// …326d` in providerAccount, `cline as API key …326d` in host, measured
// 2026-10-07), so a suffix test keeps matching when that text changes. Every
// tail has the same length (clineAccountTailLen), so at most one of them can be
// a suffix of any single label — a match is unambiguous by construction, and a
// roster whose tails collide never yields a table in the first place.
func (a clineAttribution) attribute(label string) string {
	if label == "" {
		return ""
	}
	for tail, id := range a {
		if strings.HasSuffix(label, tail) {
			return id
		}
	}
	return ""
}

// same reports whether two tables would place every row identically: the test
// Source.indexFor uses to decide whether an index built with one still
// describes the file under the other.
func (a clineAttribution) same(b clineAttribution) bool {
	if len(a) != len(b) {
		return false
	}
	for tail, id := range a {
		if b[tail] != id {
			return false
		}
	}
	return true
}

// The three conditions that leave all keyless `cline` rows unattributed, and so
// make each account's summed consumption a LOWER BOUND. They are the state keys of
// observeAttribution — a condition is WARNed once per transition, never per
// discovery or per round — and the string is the `reason` an operator reads:
//
//   - attribTailsCollide: two roster keys end in the same clineAccountTailLen
//     characters, so their masked labels are ambiguous and the whole table is
//     dropped instead of picking a winner (see newClineAttribution);
//   - attribNoKeyTails: the discovery SUCCEEDED and the table came out EMPTY — no
//     ClinePass provider entry, no key at all, or only keys shorter than the mask
//     — so there is no key tail to match a label against. An empty table is a
//     successful discovery and is installed as such (it IS the truth about which
//     accounts prism may poll), but it attributes nothing, so it must not clear
//     the WARN either;
//   - attribTableStale: the last discovery attempt did not read the provider file
//     (it failed, or there was none), so the table of the last successful one may
//     describe keys magpie no longer hands out. It is not used at all (see
//     markAttributionStale) and the condition is reported by the sums, which are
//     where it takes effect (see observeStaleAttribution).
const (
	attribTailsCollide = "clinepass account key tails collide"
	attribNoKeyTails   = "no clinepass account key tail to attribute against"
	attribTableStale   = "clinepass account attribution table is not confirmed"
)

// installRoster adopts the roster a SUCCESSFUL discovery just returned: it
// derives Track 2's tail table from exactly those accounts and keeps it for
// every later scan, so a keyless row can only ever join an account whose key the
// caller is polling.
//
// The table is derived HERE, from the discovery result, and never re-derived
// from the provider file by a sum. Two key sets would open a window nothing can
// reconcile: a provider key rotated in magpie is visible to a re-read at once,
// while the poller's roster — and with it the account id the upstream percent
// and the Track 1 sum are keyed by — stays frozen until the next SIGHUP (see
// cmd/prism refreshQuotaAccounts). Inside that window the percentage would come
// from the OLD key, Track 1 would still know only the OLD id, and a table
// re-derived from the file would already be pointing the historical keyless rows
// at the NEW id — which nothing polls, so those rows would be counted for no
// account at all (a silent under-count: the drift this track exists to recover,
// inverted). Deriving the table from the discovery that installs the roster makes
// that window impossible — attribution and polling move together, on one SIGHUP.
//
// This is the ONLY place a tail table is (re)made and the only place the
// attribution state machine is moved forward on a healthy answer: a table exists
// if and only if a discovery read the provider file, and "attributable again" can
// only be said about a table that discovery just built.
//
// A roster whose tails collide installs NO table (every keyless row stays
// unattributed, see newClineAttribution) and is reported once per transition — as
// is a successfully READ table that came out empty (attribNoKeyTails): with no key
// tail in it, no keyless row can be placed either, so neither may announce a
// recovery. A discovery that did NOT read the file never reaches this function at
// all; it marks the table stale instead (markAttributionStale).
//
// This is also the moment a table becomes CONFIRMED for the first time
// (Source.attributionConfirmed), which is what lets a later sum tell a table that
// went missing from one that never existed.
func (s *Source) installRoster(accounts []clinePassAccount) {
	table, colliding := newClineAttribution(accounts)
	problem := ""
	switch {
	case len(colliding) > 0:
		table = nil
		problem = attribTailsCollide
	case len(table) == 0:
		// A legitimately empty account table (magpie installed, no ClinePass
		// provider entry — a magpie-side rename or a removed subscription) is a
		// SUCCESSFUL discovery, and it installs its table: the empty roster is the
		// truth about which accounts may be polled. It is not a table anything can
		// be attributed WITH, though, so the not-attributable condition stands and
		// is reported.
		problem = attribNoKeyTails
	}
	s.mu.Lock()
	s.attribution, s.attributionFresh = table, true
	s.attributionConfirmed = true
	s.mu.Unlock()
	s.observeAttribution(problem, colliding)
}

// markAttributionStale records that the last account-discovery attempt read no
// provider file — it failed, or there was none — so the tail table the previous
// successful discovery installed may no longer describe the keys magpie is
// handing out. Until a discovery succeeds again, attributionTable returns nil and
// so no keyless row is attributed: the row stays in the empty bucket and is
// counted in clinepass_usage_unattributed_rows_total, exactly like a row whose
// label matches nothing (see ListClinePassAccounts for why a discovery prism could
// not complete is when a stale table is most dangerous).
//
// The installed table is KEPT, not dropped: the next successful discovery replaces
// it wholesale, and the id set is what a collision WARN names. Nothing reads it
// while it is stale, and the freshness flag — not the table — is the state.
//
// The attribution state machine is deliberately NOT advanced here. This runs on
// discovery, where silence is a contract (a host without magpie logs nothing, see
// clinePassAccounts), while the condition matters in the SUMS that had to do
// without attribution: those report it, and their first sight of the stale table
// is the transition (see observeStaleAttribution).
func (s *Source) markAttributionStale() {
	s.mu.Lock()
	s.attributionFresh = false
	s.mu.Unlock()
}

// attributionTable returns Track 2's tail table: the one the last successful
// discovery installed (see installRoster), or nil when there is nothing to
// attribute with — no discovery has confirmed a roster on this Source yet, the
// last attempt did not read the provider file (stale, see markAttributionStale),
// the roster's key tails collide, or the table is simply empty.
//
// The freshness check is what makes attributing by a STALE table structurally
// impossible instead of a convention: every scan reads its input through this
// function, and the only writer of a fresh table is a discovery that just
// succeeded, so no sum, at any moment, can place a keyless row with a table prism
// could not re-confirm.
//
// It reads Source state only — the provider file is read by discovery alone — so
// it is cheap enough to call on every sum, and the table it returns is the one
// the caller's roster was built from. It is the one-shot form of
// snapshotAttribution: a round carries its own copy of the whole state
// (see Round).
func (s *Source) attributionTable() clineAttribution {
	return s.snapshotAttribution().table
}
