// Package magpieusage reads ClinePass token consumption and account
// credentials from magpie's local files.
//
// ClinePass traffic is routed through magpie (a separate local gateway),
// so prism's own usage ledger never records it; the ClinePass quota
// reversal (internal/planusage) therefore takes its consumed-token sum
// from magpie's usage log.
//
// magpie has NO database. Two files, both read strictly read-only and
// neither one required to exist:
//
//   - usage.jsonl (DefaultUsagePath) is magpie's per-call log: one JSON
//     object per line, appended forever, never rotated and carrying no
//     schema version. It is the ONLY usage detail magpie keeps, so the
//     file — not a table — is the source of truth here. Because it is
//     append-only, magnitude grows without bound; see usageIndex for how
//     the per-file version cache bounds the number of full scans.
//
//   - providers.json (DefaultProvidersPath) holds the provider entries;
//     the ClinePass one carries the account keys.
//
// Every failure — the file missing, unreadable, not a regular file, no
// usable line at all, or a line that is not the shape this package
// expects — is reported as an error and degraded by the caller to "no
// 总额 for this window": it must never affect quota fetching, snapshot
// state or display.
//
// Account identity is magpie's own providerKeyId: the hex of the first 10
// characters of SHA-256 of the provider key (KeyID). magpie stamps that id on
// the usage lines it attributes to a provider key, so the roster
// (providers.json) and the consumption (usage.jsonl) meet on an id and need no
// index and no database server. That is Track 1 of the attribution.
//
// Track 2 covers the OTHER shape magpie writes for the same subscription
// traffic: keyless `cline` lines — no providerKeyId at all (932 of them
// measured 2026-10-07, of which the 921 that name a cline-pass/ model are the
// dropped traffic this track recovers) — which carry magpie's MASKED account
// label instead
// (providerAccount `API key …326d`, or host `cline as API key …326d`). Those
// lines are ClinePass consumption like any other (same provider key, same
// reset instant, the same upstream response ids as the keyed lines — see the
// 20261007 note), so skipping them under-counts the account's consumption and
// shrinks the reversed 总额. They are attributed by matching the mask's key
// tail against the roster the last account discovery installed (clineAttribution,
// see Source.installRoster), and a row that matches no account is counted in
// clinepass_usage_unattributed_rows_total.
//
// A row is placed by EXACTLY ONE track, in this order: the model gate
// (cline-pass/ prefix only, so cline-free/* and every other model family are
// out), then Track 1 when the line carries an id, then Track 2 for a keyless
// `cline` row. A row no track places is in no account's bucket.
package magpieusage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultUsagePath is magpie's per-call usage log on this host. It is a
	// fixed default, not a config key — the same pattern as the agy usage
	// index (agyusage.DefaultIndexPath) and the source this one replaced
	// (metapi's hub.db): a deployment without magpie simply has no
	// such file and loses only the ClinePass 总额 estimate.
	DefaultUsagePath = "/root/.config/magpie/usage.jsonl"

	// DefaultProvidersPath is magpie's provider file on this host: the same
	// kind of fixed default as DefaultUsagePath. It is where the ClinePass
	// account keys come from, so a deployment without it has no ClinePass
	// accounts at all (and stays silent about it, see readProviders).
	DefaultProvidersPath = "/root/.config/magpie/providers.json"

	// clinePassProviderID is the provider id magpie records for ClinePass
	// subscription traffic (`provider` in usage.jsonl and `id` in
	// providers.json). It is matched EXACTLY: both files are written by one
	// program, so a case fold would only turn a magpie-side rename into a
	// silent partial match. A rename is visible as a shrunken roster /
	// missing totals instead (see clinePassRosterDelta in cmd/prism).
	clinePassProviderID = "clinepass"

	// keylessClineProviderID is the provider id magpie records for its
	// CREDENTIAL-LESS `cline` entry (`provider` in usage.jsonl): the sink it
	// falls back to for a bare cline client, with no providerKeyId on its lines
	// and a masked account label instead. Only a keyless row carrying this
	// provider id is Track 2's business — every other provider id is not
	// ClinePass subscription traffic, and a keyless row under
	// clinePassProviderID remains what it always was (a record magpie itself
	// could not attribute to a key, in no account's bucket). Matched exactly,
	// for the same reason as clinePassProviderID.
	keylessClineProviderID = "cline"

	// clinePassModelPrefix selects the ClinePass MODELS inside that provider:
	// metapi and magpie both record them as cline-pass/<model>. The trailing
	// slash is part of the filter, so cline-pass-extra/x and xcline-pass/x
	// never match.
	clinePassModelPrefix = "cline-pass/"

	// KeyIDLen is the length of magpie's providerKeyId: 10 hex characters,
	// the first 5 bytes of SHA-256. Magpie derives its ids this way: on this
	// host the two ClinePass keys of providers.json map exactly to the
	// providerKeyId magpie stamped on their usage lines (d0c4d7aa83 /
	// ae4c5650bd, see the 20261007 note).
	KeyIDLen = 10
)

// KeyID is magpie's providerKeyId for a provider key: the lowercase hex of
// the first KeyIDLen characters of SHA-256 of the key. It is a ONE-WAY short
// id — the raw key can never be recovered from it — which is why it is the
// only form of an account identity this package, its logs and its callers
// ever carry.
func KeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:KeyIDLen]
}

// clinePassLabel is the display name of a ClinePass account: magpie's own
// account label, `<provider id>#<providerKeyId>` (the form magpie writes as
// `who` in its own affinity file), so the name prism renders is the same
// label magpie itself uses for the account.
//
// magpie's provider file carries NO human label for a key — a keys[] element
// holds exactly one field, `key` (see providerKey in providers.go) — so there
// is no username/email/remark to show and the key-derived id is the only
// faithful name available. Two accounts therefore read
// "clinepass#d0c4d7aa83" and "clinepass#ae4c5650bd": distinguishable, and
// impossible to mistake for a credential.
func clinePassLabel(id string) string {
	return clinePassProviderID + "#" + id
}

// usageRow is the subset of one usage.jsonl line this package reads. t,
// provider, model, in and out are POINTERS on purpose: a nil means the field
// was absent (or JSON null), which parseUsageLine treats as format drift — a
// magpie-side rename would otherwise turn every line into a non-matching one
// and silently sum zero instead of failing loudly.
//
// providerKeyId, providerAccount and host are plain strings because they are
// genuinely OPTIONAL — none of them is part of the required line shape, and a
// line without them is not drift: magpie omits providerKeyId on every line it
// could not attribute to a provider key (measured 2026-10-07: 1468 of 8923
// production lines — 932 `cline`, 518 `grok-plugin` and 18 with an empty
// provider). Of those, the 921 `cline` lines that name a cline-pass/ model are
// ClinePass CONSUMPTION: that is what the attribution's Track 2 exists for (see
// the package doc), and providerAccount / host are the masked account label
// they are matched on (providerKeyId is empty there, so the mask is the only
// handle the line offers). The remaining keyless lines — another provider's, or
// one magpie left with no provider at all — are in no account's bucket.//
// cache_read is deliberately NOT decoded: the sum this package serves is the
// upstream plan percent's 口径 (prompt + completion 词元), which excludes
// cache reads. Decoding a field only to ignore it would invite the next
// reader to add it to the molecule.
type usageRow struct {
	Time     *string `json:"t"`
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
	In       *int64  `json:"in"`
	Out      *int64  `json:"out"`
	Host     string  `json:"host"`
	// ProviderAccount is magpie's masked account label (`API key …326d`: a
	// literal prefix plus the LAST FOUR characters of the account's provider
	// key, with a single \u2026 character as the ellipsis). It is what a keyless
	// `cline` line carries instead of an id.
	ProviderAccount string `json:"providerAccount"`
	// ProviderKeyID is the id magpie stamped on the line. It is the whole of
	// Track 1: a non-empty value is the account, whatever the provider says.
	ProviderKeyID string `json:"providerKeyId"`
}

// parsedLine is one attributable usage record: the instant it happened, its
// 词元, and what the line offers as an ACCOUNT — either the id magpie stamped on
// it (keyID, Track 1) or the masked account label magpie wrote instead (label,
// Track 2, resolved against the roster's key tails by scanUsageLog, which is
// where the roster is known; parseUsageLine has no roster). It holds no other
// field on purpose — every field here is either summed, compared or matched, so
// nothing can quietly participate in a sum.
type parsedLine struct {
	keyID string
	// label is only ever read when keyID is empty, and only a Track 2 row has
	// one: a keyless `cline` line's providerAccount (or its host when the
	// providerAccount field is empty).
	label string
	// track2 marks the ONE shape Track 2 may attribute: a keyless row (no
	// providerKeyId) under the `cline` provider id. It is what tells
	// scanUsageLog that a row it could not place is a row Track 2 is supposed to
	// place — and so one that belongs in clinepass_usage_unattributed_rows_total
	// — instead of a keyless row no track covers at all (a keyless row under
	// `clinepass`, another provider's row).
	track2 bool
	at     int64 // unix nanoseconds, full RFC3339Nano precision
	tokens int64 // in + out (cache_read excluded)
}

// ErrUsageLine marks a usage.jsonl line that is not a readable magpie usage
// record: invalid JSON, a missing/mis-typed t/provider/model/in/out, or a t
// that does not parse as RFC3339. The line-by-line shape is the log's ONLY
// contract — magpie has no schema and no version — so drift is returned as an
// error and the caller degrades this round to "no estimate" instead of
// summing a fraction of the traffic. A single torn tail line is NOT drift
// (see scanUsageLog): only a terminated line, or an unterminated line that is
// not the last one, fails.
var ErrUsageLine = errors.New("magpieusage: usage line format drift")

// ErrNoUsageLines marks a usage log that exists but holds no usable line at
// all: empty, whitespace only, or nothing that parses. An absent total is the
// honest answer there — reporting 0 would claim the account consumed nothing
// when the real cause is that the log says nothing (a brand-new magpie, a
// truncated or rewritten file), and the upstream percent it would be reversed
// against comes from a different source and would turn that 0 into a bogus
// pool.
var ErrNoUsageLines = errors.New("magpieusage: no usable usage lines")

// parseUsageLine decodes one line body (already trimmed) and classifies it.
//
// A nil error with matched == false means the line is a well-formed magpie
// usage record that simply is not ClinePass traffic, or a keyless row of a
// provider no track covers; it is skipped, never counted as drift. A non-nil
// error is ErrUsageLine (wrapped) and is decided by the caller, which knows
// whether this line is the tolerated tail.
func parseUsageLine(body []byte) (parsedLine, bool, error) {
	var row usageRow
	if err := json.Unmarshal(body, &row); err != nil {
		return parsedLine{}, false, fmt.Errorf("%w: invalid JSON: %v", ErrUsageLine, err)
	}
	switch {
	case row.Time == nil:
		return parsedLine{}, false, fmt.Errorf("%w: missing t", ErrUsageLine)
	case row.Provider == nil:
		return parsedLine{}, false, fmt.Errorf("%w: missing provider", ErrUsageLine)
	case row.Model == nil:
		return parsedLine{}, false, fmt.Errorf("%w: missing model", ErrUsageLine)
	case row.In == nil:
		return parsedLine{}, false, fmt.Errorf("%w: missing in", ErrUsageLine)
	case row.Out == nil:
		return parsedLine{}, false, fmt.Errorf("%w: missing out", ErrUsageLine)
	}
	at, err := time.Parse(time.RFC3339Nano, *row.Time)
	if err != nil {
		// RFC3339Nano is the permissive form of magpie's wire timestamp: it
		// accepts a fractional second and any zone offset (+08:00 on this
		// host, UTC in others), and rejects the shapes that would silently
		// mis-bound a window (epoch seconds, `2006-01-02 15:04:05`, a date
		// without a zone).
		return parsedLine{}, false, fmt.Errorf("%w: t %q is not RFC3339: %v", ErrUsageLine, *row.Time, err)
	}
	// The model gate is the HARD one, and it is the first thing decided: only a
	// cline-pass/<model> line can be ClinePass subscription traffic at all. It
	// is also what excludes cline-free/* (magpie's free tier — a different
	// product with no subscription pool behind it) without a rule of its own,
	// because the filter is the prefix INCLUDING its slash.
	if !strings.HasPrefix(*row.Model, clinePassModelPrefix) {
		return parsedLine{}, false, nil
	}
	keyID, label := row.ProviderKeyID, ""
	track2 := false
	if keyID == "" {
		// No id: the line is attributable only through Track 2, and Track 2 is
		// the keyless `cline` shape alone.
		switch *row.Provider {
		case keylessClineProviderID:
			// providerAccount first, host as the fallback: magpie writes the
			// same key's mask under both field names (measured 2026-10-07:
			// `API key …326d` and `cline as API key …326d`), so the account is
			// named the same way either way. A blank/absent value is not a
			// label, and the row then stays unattributed on its own.
			track2 = true
			label = strings.TrimSpace(row.ProviderAccount)
			if label == "" {
				label = strings.TrimSpace(row.Host)
			}
		case clinePassProviderID:
			// A keyless row under the ClinePass provider id is what it always
			// was — a record magpie itself could not attribute to a key — and
			// it is NOT Track 2's shape (it carries no masked label): it stays in
			// no account's bucket, exactly as before the second track existed.
		default:
			// Every other provider's keyless cline-pass/* row is out of scope:
			// it is not ClinePass subscription traffic (grok-plugin, stepfun, a
			// row magpie left with an empty provider value, …).
			return parsedLine{}, false, nil
		}
	}
	// The molecule is prompt + completion 词元 and NOT any stored total:
	// magpie's lines carry in/out/cache_read separately, and the upstream plan
	// percent this sum is reversed against counts prompt and completion only,
	// so cache_read is deliberately left out. (metapi's total_tokens included
	// it; that difference is why the old source summed prompt+completion
	// explicitly too.)
	return parsedLine{
		keyID:  keyID,
		label:  label,
		track2: track2,
		at:     at.UnixNano(),
		tokens: *row.In + *row.Out,
	}, true, nil
}

// usageIndex is a parsed usage log: every attributable ClinePass record in the
// file, bucketed by magpie's providerKeyId (the empty bucket holds the rows no
// track could place: magpie's keyless rows whose masked account label no
// ClinePass key tail matches, the keyless rows under the ClinePass provider id,
// and every keyless row of a scan that ran with no confirmed tail table — see
// Source.markAttributionStale), plus the FileInfo it was built from.
//
// The records are window-independent and 16 bytes each, while a log line is
// several hundred bytes: one full streaming scan of the file therefore answers
// every account and every window (the poller sums three windows per account),
// and the store keeps only the compact projection instead of the file. The
// log is never rotated, so the index grows with the log; it is bounded by the
// ClinePass share of magpie's traffic (measured 2026-10-07: 6431 ClinePass
// lines accumulated within about 24 h).
//
// Which bucket a row lands in depends on the roster as well as the file (Track
// 2 matches the roster's key tails), so an index is only reusable while BOTH
// are unchanged — see Source.indexFor.
type usageIndex struct {
	info  os.FileInfo
	byKey map[string][]tokenRecord

	// earliest is the instant (unix nanoseconds) of the FIRST attributable
	// record in the file — the oldest instant magpie's log still reaches back
	// to — and 0 when the file holds no attributable record at all. It is what
	// tells a caller that a window starting before it can only be summed to a
	// LOWER BOUND: the log is appended forever and never backfilled, so the
	// traffic older than this instant is not in the file (see
	// observeWindowCoverage).
	earliest int64
}

// tokenRecord is one attributable usage line, reduced to what a sum needs.
type tokenRecord struct {
	at     int64 // unix nanoseconds
	tokens int64 // in + out
}

// matches reports whether idx still describes info: the SAME file (device and
// inode), the same size and the same mtime.
//
// All three are required, and each one earns its place: SameFile catches a
// replaced or renamed file (magpie recreating the log, a restore, an
// atomically swapped file — a new inode with a plausible size), size catches
// an append or a truncation (the log is appended to on every call, so this is
// the common invalidation), and mtime catches an in-place rewrite that
// happened to keep the size. The comparison is deliberately conservative:
// anything that cannot PROVE the index is current is rescanned.
func (idx *usageIndex) matches(info os.FileInfo) bool {
	if idx == nil || idx.info == nil || info == nil {
		return false
	}
	if idx.info.Size() != info.Size() || !idx.info.ModTime().Equal(info.ModTime()) {
		return false
	}
	return os.SameFile(idx.info, info)
}

// sumTokens sums the records of ONE account in [fromUnix, toUnix] (unix
// seconds, both bounds inclusive). Records are compared at full nanosecond
// precision, so a line written at to+0.5 s is outside the window: the bounds
// are instants, not second-sized buckets.
func (idx *usageIndex) sumTokens(keyID string, fromUnix, toUnix int64) int64 {
	return sumRecords(idx.byKey[keyID], fromUnix, toUnix)
}

func sumRecords(recs []tokenRecord, fromUnix, toUnix int64) int64 {
	from, to := fromUnix*int64(time.Second), toUnix*int64(time.Second)
	var n int64
	for _, r := range recs {
		if r.at < from || r.at > to {
			continue
		}
		n += r.tokens
	}
	return n
}

// scanUsageLog streams path and builds the index for it. info must be the
// FileInfo the caller stat'ed BEFORE opening: stamping the index with the
// pre-scan state means a log that magpie appends to while we read it is
// invalidated on the next call (the size moved) instead of being trusted.
// attr is the Track 2 tail table the scan attributes keyless rows with (nil =
// no keyless row can be attributed; see Source.attributionTable).
//
// The scan is line-oriented and streaming: no line is retained beyond the
// records it contributes, so memory does not scale with the file's size.
//
// Tail policy: a line is "complete" when it ends in a newline. An INCOMPLETE
// line is tolerated ONLY as the very last thing in the file and only when it
// does not parse — that is exactly a torn write (magpie appends one line per
// call; a reader can catch the file between the write and its newline), and
// the record it will become is delivered by the next scan. An incomplete tail
// that DOES parse is a complete record whose newline is not flushed yet and is
// counted normally. Every other unparsable line is ErrUsageLine: mid-file
// garbage, or a terminated line that cannot be read, is drift.
func scanUsageLog(path string, info os.FileInfo, attr clineAttribution) (*usageIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("magpieusage: open %s: %w", path, err)
	}
	defer f.Close()

	idx := &usageIndex{info: info, byKey: map[string][]tokenRecord{}}
	r := bufio.NewReaderSize(f, 64<<10)
	parsed := 0
	for {
		chunk, readErr := r.ReadBytes('\n')
		if len(chunk) > 0 {
			body := bytes.TrimSpace(chunk)
			if len(body) > 0 {
				rec, matched, parseErr := parseUsageLine(body)
				terminated := chunk[len(chunk)-1] == '\n'
				switch {
				case parseErr == nil:
					parsed++
					if matched {
						keyID := rec.keyID
						if keyID == "" && rec.track2 {
							// Track 2: a keyless row is attributed only when its
							// masked account label matches a ClinePass account's key
							// tail UNAMBIGUOUSLY. No match — a label this roster does
							// not know, a key that was rotated out of the roster, no
							// confirmed roster at all — leaves the row in the empty bucket, which
							// is where a keyless row has always gone (see usageIndex),
							// and is COUNTED: one per falling-through row per scan, so
							// the share of the traffic this track silently drops is a
							// number on /metrics instead of only a shrunken sum. (The
							// keyless `clinepass` and foreign-provider rows that skip
							// this branch are in the empty bucket too, but they were
							// never Track 2's to place and must not inflate it.)
							keyID = attr.attribute(rec.label)
							if keyID == "" {
								unattributedRows.Add(1)
							}
						}
						if idx.earliest == 0 || rec.at < idx.earliest {
							idx.earliest = rec.at
						}
						idx.byKey[keyID] = append(idx.byKey[keyID], tokenRecord{at: rec.at, tokens: rec.tokens})
					}
				case !terminated && errors.Is(readErr, io.EOF):
					// Torn tail: skipped, not an error (see the doc comment).
				default:
					return nil, fmt.Errorf("magpieusage: %s: %w", path, parseErr)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("magpieusage: read %s: %w", path, readErr)
		}
	}
	if parsed == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoUsageLines, path)
	}
	return idx, nil
}

// Source is the long-lived ClinePass consumption source the service poller
// wires into planusage, and the one-shot equivalent the CLI uses.
//
// It holds no file handle: every (re)scan opens the log, reads it and closes
// it, so
//
//   - a magpie log that is replaced, truncated, rotated away or recreated is
//     read as it is on the next call. A handle (or an index without a file
//     identity) would keep serving the replaced file's records;
//   - a file that was missing when prism started (magpie not installed yet, a
//     config directory that appears later) recovers by itself on a later
//     round instead of being disabled for the process lifetime.
//
// The cost is one stat per call and one full scan per FILE VERSION (see
// usageIndex.matches): the three windows of an account, and every account,
// share one scan, and a round that finds the log unchanged scans nothing at
// all. The provider file is read by account DISCOVERY alone
// (ListClinePassAccounts, run by the poller at startup and on every SIGHUP): the
// tail table a scan attributes keyless rows with is that discovery's own result,
// so a sum can never credit a row to a key set the roster stopped polling (see
// installRoster). Failures degrade this round's estimates only: errors are
// reported to the caller (planusage leaves the windows without a total, never
// touching Snapshot.Err), the degraded state is logged once per transition into
// and out of it (never once per round) and mirrored in expvar.
type Source struct {
	usagePath     string
	providersPath string

	// loadMu serializes index loads. It is held ACROSS a full scan: two
	// ClinePass accounts are polled concurrently (one goroutine per key
	// group), and without it both would scan the same growing file at the
	// same time. Holding it means the second caller waits for the first
	// scan and then hits its index.
	loadMu sync.Mutex
	index  *usageIndex

	// indexAttribution is the Track 2 tail table the cached index was built
	// with. The index is reusable only while the log AND that table are
	// unchanged: the table decides which bucket a keyless row lands in, so a
	// discovery that rotates a provider key (a changed tail → a changed table)
	// must invalidate the index even though it does not touch the usage log.
	indexAttribution clineAttribution

	// mu guards the degradation state machine, the sum-quality state below (the
	// maps that keep a repeating condition from logging per round) and the
	// installed roster's tail table.
	mu       sync.Mutex
	degraded bool

	// attribution is the Track 2 tail table of the roster the last successful
	// account discovery installed (see installRoster; nil = no keyless row is
	// attributed). It is the table a scan reads, so the buckets an index is
	// built with and the keys the caller polls always come out of one discovery.
	attribution clineAttribution

	// attributionFresh says whether that table is still the result of a discovery
	// that READ the provider file. It is true only after installRoster, and false
	// before the first successful discovery and after every discovery that read
	// no file (markAttributionStale). attributionTable hands out a table only
	// while it holds, so no scan — and therefore no sum — can attribute a keyless
	// row with a table prism could not re-confirm.
	attributionFresh bool

	// attributionConfirmed says whether a tail table was EVER confirmed on this
	// Source. It is set by the first installRoster and never cleared, because it
	// is what separates the two unconfirmed shapes: a table that WENT stale is a
	// roster whose keys the caller still polls and whose keyless rows are now
	// silently missing, while a source that never had a table (a host without
	// magpie at all) has lost nothing and nothing to report. Only the first is
	// counted and WARNed about (see observeStaleAttribution).
	attributionConfirmed bool

	// attributionProblem is the keyless-attribution condition already WARNed
	// about ("" = the table is usable / nothing to report). It exists for the
	// same reason as the two maps below: the condition persists until someone
	// edits magpie's provider file (which only the next discovery reads, see
	// installRoster), so it must log once per transition instead of on every
	// discovery (see observeAttribution).
	attributionProblem string

	// unmatched holds the account ids already WARNed about for matching no row
	// at all; lowerBoundFroms the window starts already WARNed about for
	// starting before the log. Both exist so a condition that holds on every
	// round logs once per transition instead of once per round — the same rule
	// observe follows — and both are tiny (see observeAccountMatch and
	// observeWindowCoverage for their bounds).
	unmatched       map[string]bool
	lowerBoundFroms map[int64]bool
}

// NewSource returns a source over magpie's usage log and provider file. Both
// paths are taken as arguments (the caller passes DefaultUsagePath /
// DefaultProvidersPath in production) so tests can point them at fixtures.
// Neither file is touched here: each call makes its own attempt, so a magpie
// that is not installed yet is not a permanent state.
func NewSource(usagePath, providersPath string) *Source {
	return &Source{usagePath: usagePath, providersPath: providersPath}
}

// indexFor returns the current index, rescanning the log when the cached one
// no longer matches the file on disk. It is the ONE-SHOT form: it attributes
// with the tail table of this instant, i.e. the table a round begun here would
// carry.
//
// The installed roster's tail table is read first (see attributionTable): it is
// what decides which bucket a keyless row lands in, so a cached index is only
// reusable while that table is unchanged as well as the log — a discovery that
// rotates a provider key invalidates it even though the log did not move, and so
// does a discovery that read no provider file at all, whose stale table reads as
// nil (see markAttributionStale).
func (s *Source) indexFor() (*usageIndex, error) {
	return s.indexForAttribution(s.attributionTable())
}

// indexForAttribution is indexFor with the tail table supplied by the caller: a
// scan uses EXACTLY that table (never a live read of the source's own state) and
// the cached index is reused only while that table is unchanged as well as the
// file, so one round's snapshot decides the buckets of every sum it makes (see
// Round.Sum).
func (s *Source) indexForAttribution(attribution clineAttribution) (*usageIndex, error) {
	if strings.TrimSpace(s.usagePath) == "" {
		return nil, errors.New("magpieusage: empty usage path")
	}
	info, err := os.Stat(s.usagePath)
	if err != nil {
		return nil, fmt.Errorf("magpieusage: stat %s: %w", s.usagePath, err)
	}
	if !info.Mode().IsRegular() {
		// A directory or a device at the log path is a configuration mistake,
		// not "no magpie": reading it would either fail obscurely or (for a
		// fifo) block the caller. Report it as an error for the degraded path.
		return nil, fmt.Errorf("magpieusage: %s is not a regular file", s.usagePath)
	}
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.index.matches(info) && s.indexAttribution.same(attribution) {
		return s.index, nil
	}
	idx, err := scanUsageLog(s.usagePath, info, attribution)
	if err != nil {
		// The failed index is NOT cached: the next call retries the scan, so
		// a torn or half-written state cannot stick for the process lifetime.
		return nil, err
	}
	s.index = idx
	s.indexAttribution = attribution
	return idx, nil
}

// SumClinePassTokensByAccount sums 词元 of ONE ClinePass account in
// [fromUnix, toUnix] (unix seconds, inclusive).
//
// The per-line amount is in + out, i.e. prompt + completion 词元, deliberately
// NOT including cache_read: the upstream plan percent this sum is reversed
// against counts prompt and completion only (剔除 cache). metapi's old
// total_tokens included the cache reads, which skewed the reversed pool badly
// because cache is a far larger share of a week than of a month (49 % vs 13 %
// measured); the same 口径 is what this sum exists to match.
//
// A row is counted for an account when the MODEL gate and one of the two
// attribution tracks place it there: the id magpie stamped on the line (Track
// 1), or — for a keyless `cline` line — the masked account label matching the
// installed roster's key tail (Track 2, see the package doc). The sum is
// therefore not "every line that happens to carry this id": the keyless share of
// the same account's traffic is counted too (921 such lines were being dropped,
// measured 2026-10-07), while the rows NO track can place stay out and leave the
// sum a LOWER BOUND — the keyless ones among them are counted per row in
// clinepass_usage_unattributed_rows_total, and every keyless row stays out while
// the roster's tails collide or its discovery is unconfirmed (see
// observeAttribution and markAttributionStale).
//
// It is the ONE-SHOT form of Round.Sum: it takes its own round (see BeginRound)
// and therefore works with the attribution state of this instant.
//
// A caller that sums SEVERAL windows of one fetch must NOT call this once per
// window. The three windows of one account belong to ONE round (the poller sums
// 5h / weekly / monthly of one fetch, and the CLI's estimate pass does the same),
// because their numerators are combined with ONE upstream percent; take one
// Round and sum through it instead, so a rediscovery landing between two windows
// cannot split one fetch between two rosters.
//
// A non-positive lower bound returns 0 without touching the file, and an empty
// account id is an error (an account with no magpie id must not fall back to
// summing every bucket: the two ClinePass accounts are two independent pools, so
// a shared numerator would print a 总额 belonging to neither, 串账). Errors
// are the ones indexForAttribution and scanUsageLog report — missing file, not a
// regular file, no usable lines, line format drift — and each failure is counted
// and logged once per transition (see observe); callers degrade to "no estimate"
// and must never fail the quota fetch.
func (s *Source) SumClinePassTokensByAccount(ctx context.Context, accountID string, fromUnix, toUnix int64) (int64, error) {
	return s.BeginRound(accountID).Sum(ctx, fromUnix, toUnix)
}

// attributionSnapshot is the attribution state ONE fetch works with, read in a
// single step so that its parts can never disagree with each other: the Track 2
// tail table in force, whether that table is still confirmed, and whether a table
// was EVER confirmed on this source (see observeStaleAttribution for why the last
// two are different questions).
//
// table is nil whenever fresh is false (markAttributionStale) and may also be nil
// while fresh is true (a roster whose key tails collide, or one with no key tail
// at all: a confirmed table that places nothing).
type attributionSnapshot struct {
	table     clineAttribution
	fresh     bool
	confirmed bool
}

// Round is ONE account fetch's immutable attribution snapshot: the tail table in
// force when the fetch began plus the two verdicts that go with it. Every sum of
// the round is attributed with exactly that snapshot (see Round.Sum).
//
// It exists because a fetch sums the SAME account three times and combines those
// sums with ONE upstream percent: all three windows have to be attributed with
// one tail table and one freshness verdict, or the windows of a single fetch would
// be reversed against numerators from different rosters — a SIGHUP between two of
// them installs a new table (or marks the current one stale) and one window would
// count rows the other does not.
//
// A round is a VALUE, not Source state: the poller fetches the accounts
// concurrently, so each fetch takes its own round (BeginRound) and reads no live
// attribution state after that. A discovery mutating the source's own state
// therefore cannot reach a round in flight — which is the guarantee the three
// sums of one fetch need.
type Round struct {
	src       *Source
	accountID string
	snap      attributionSnapshot
}

// BeginRound takes the snapshot and returns the round of ONE account fetch: call
// it once per fetch — never once per window (see SumClinePassTokensByAccount) —
// and sum every window of that fetch through the returned round.
func (s *Source) BeginRound(accountID string) *Round {
	return &Round{src: s, accountID: accountID, snap: s.snapshotAttribution()}
}

// Sum sums the round's account in [fromUnix, toUnix] (unix seconds, inclusive),
// attributed with the round's own snapshot and nothing else.
//
// The molecule, the two attribution tracks and the error semantics are the ones
// SumClinePassTokensByAccount documents; the ONE difference is where the
// attribution state comes from, and that is the point of a round.
//
// A non-positive lower bound is answered with (0, nil) WITHOUT touching the log
// and WITHOUT observing the source: there is no window to sum, so no read
// happened and this call says nothing about the source's health. Publishing "ok"
// from it — let alone the recovery line — would let a call that never read the
// log clear a genuine degradation (see observe). It is the API's own boundary
// rather than a caller's need: the estimate passes skip a window whose period
// start is unknown (planusage.ApplyClinePassEstimates), so no caller passes a
// non-positive bound today.
func (r *Round) Sum(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	if strings.TrimSpace(r.accountID) == "" {
		err := errors.New("magpieusage: empty account id")
		r.src.observe(err)
		return 0, err
	}
	if fromUnix <= 0 {
		return 0, nil
	}
	n, err := r.src.sumByAccount(ctx, r.accountID, r.snap, fromUnix, toUnix)
	r.src.observe(err)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Source) sumByAccount(ctx context.Context, accountID string, snap attributionSnapshot, fromUnix, toUnix int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	idx, err := s.indexForAttribution(snap.table)
	if err != nil {
		return 0, err
	}
	// A sum can succeed and still be wrong, and none of the shapes shows up in the
	// number it returns: no tail table was confirmed (so the keyless share of the
	// traffic is missing), the account id can match no row at all, and the window
	// can start before the log does (see the three observers).
	s.observeStaleAttribution(snap)
	s.observeAccountMatch(idx, accountID)
	s.observeWindowCoverage(idx, fromUnix)
	return idx.sumTokens(accountID, fromUnix, toUnix), nil
}

// snapshotAttribution reads the source's attribution state in ONE step, which is
// what makes a round internally consistent: the table and the two verdicts all
// describe the same instant (see Round).
func (s *Source) snapshotAttribution() attributionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := attributionSnapshot{fresh: s.attributionFresh, confirmed: s.attributionConfirmed}
	if snap.fresh {
		snap.table = s.attribution
	}
	return snap
}
