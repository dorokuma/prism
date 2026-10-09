package magpieusage

import (
	"expvar"
	"log/slog"
	"time"
)

// Source degradation metrics, published on /metrics under the SAME names the
// metapi-backed source used (the metric names are part of the deployment
// contract, so a source swap must not move them). sourceErrors counts failure
// INCIDENTS (one per failed USAGE read — account discovery deliberately does
// not report here, see ListClinePassAccounts); sourceStatus is the current lifecycle state
// ("ok" / "degraded") for management visibility, mirroring usage_recorder_status
// in internal/util. Being process-wide it is republished by EVERY observation
// (see observe), so it describes the source of the last call, not the
// process's history.
var (
	sourceErrors = expvar.NewInt("clinepass_usage_source_errors")
	sourceStatus = expvar.NewString("clinepass_usage_source_status")
)

func init() { sourceStatus.Set("ok") }

// Sum-quality metrics: a per-account sum can SUCCEED (no error, a number a
// caller happily divides by a percent) and still be wrong, and none of the
// shapes below shows up in the number itself. Each gets its own counter
// next to the lifecycle pair above — same rule as those two, the name is the
// deployment contract — rather than sharing clinepass_usage_source_errors /
// source_status, which mean "the source could not be read" and drive the
// degraded state.
//
//   - accountUnmatched counts per-account sums whose account id matched NO
//     record in the log AT ALL. The 0 they return is not "this account consumed
//     nothing", it is "no line carries this id": magpie's providerKeyId
//     derivation drifted, or the provider key was rotated so the id prism now
//     derives has no rows of its own while the log still holds the old id's.
//     Counting CALLS (one per window per account per round, exactly like
//     sourceErrors) means a still-broken state keeps the counter growing
//     instead of going flat.
//
//   - windowLowerBound counts per-account sums whose window starts BEFORE the
//     log's earliest record. magpie's log is appended forever and never
//     backfilled, so its first row is the oldest instant it can answer for and
//     an earlier window can only be summed to a LOWER BOUND: the estimate
//     reversed from that numerator comes out too small. This is expected for
//     the windows that reach back past magpie's start (typically the monthly
//     one) and heals by itself as each window rolls past the log's start.
//
//   - unattributedRows counts ROWS where the two above count calls: keyless
//     `cline` rows of a cline-pass/ model that Track 2 matched to NO account's
//     key tail, so they sit in no account's bucket (see scanUsageLog). It is
//     the row-level companion of accountUnmatched and the two must never be read
//     as one unit: an account id can match no row while every keyless row is
//     attributed (a rotated id), and rows can fall through while every account
//     matches (a mask no roster tail knows) — which is precisely the drift the
//     model-prefix attribution is exposed to.
//
//     Its increment is per ROW per SCAN, and a scan is the WHOLE file: magpie's
//     log is appended to before every round, and a scan re-adds every
//     falling-through row the file still holds, so the counter measures "rows
//     fallen through in the versions scanned", never "new rows since the last
//     round". With the file version unchanged it does not move at all (measured
//     2026-10-07: a fixture holding 3 falling-through rows reads first=3 and then
//     +0 while it idles), while ONE appended line re-adds the whole backlog (the
//     same fixture: after_append=+3, not +0) — so with every keyless row
//     unattributable, one appended line per round makes it climb by the entire
//     keyless share each round (measured ≈+921 while a collision held every
//     keyless row out). Read it as a FLAG, not a rate: non-zero means the log
//     holds keyless rows that no track placed, and its size only says how much is
//     dropped per full scan at that instant. Do not set alert thresholds on its
//     growth — a slope there is an artifact of re-scanning the log, not of new
//     traffic.
//
//   - attributionStale counts SUMS (calls) that ran with no CONFIRMED tail table
//     AFTER one had been confirmed: the last account discovery did not read the
//     provider file (it failed, or there was none), so every keyless row of that
//     sum was deliberately left unattributed rather than placed with a table that
//     may predate a key rotation (see markAttributionStale and
//     observeStaleAttribution). Same unit
//     as accountUnmatched — one tick per sum, i.e. per window per account per
//     round — and a different QUESTION from the row counter above: a sum has one
//     table and any number of keyless rows, so "how many sums went without
//     attribution" and "how many rows fell through" are never the same number and
//     neither is a share of the other. Like the three counters above it never
//     moves clinepass_usage_source_status, which means "the log could not be
//     read".
//
//     A source that never confirmed a table at all is NOT counted: nothing was
//     lost, there is no roster whose rows are being dropped, and a host without
//     magpie would otherwise tick this counter on every sum of its life while the
//     WARN asked for an action that does not exist (see observeStaleAttribution).
var (
	accountUnmatched = expvar.NewInt("clinepass_usage_account_unmatched_total")
	windowLowerBound = expvar.NewInt("clinepass_usage_window_lower_bound_total")
	unattributedRows = expvar.NewInt("clinepass_usage_unattributed_rows_total")
	attributionStale = expvar.NewInt("clinepass_usage_attribution_stale_total")
)

// observeAccountMatch reports the per-account sum that matched no record at all
// — the silent zero of a drifted or rotated providerKeyId. The number itself
// stays 0 (callers must not start failing on an idle account) and the signal is
// out of band: one counter tick per sum, one WARN per account transition.
//
// The condition is per ACCOUNT and so is its state: the two ClinePass accounts
// are summed concurrently, so a single shared flag would flap — one account's
// success clears it and the other's failure sets it again — and WARN once per
// round. The map is bounded by the roster (a handful of ids).
//
// A bucket that exists but holds no record INSIDE the window is NOT this
// condition: that is an idle window, the legitimate 0.
func (s *Source) observeAccountMatch(idx *usageIndex, accountID string) {
	matched := len(idx.byKey[accountID]) > 0
	s.mu.Lock()
	defer s.mu.Unlock()
	if matched {
		if s.unmatched[accountID] {
			delete(s.unmatched, accountID)
			slog.Info("clinepass account matches usage rows again",
				"path", s.usagePath, "account", accountID)
		}
		return
	}
	accountUnmatched.Add(1)
	if s.unmatched == nil {
		s.unmatched = map[string]bool{}
	}
	if s.unmatched[accountID] {
		return
	}
	s.unmatched[accountID] = true
	slog.Warn("clinepass account matched no usage row, total estimate unavailable",
		"path", s.usagePath, "account", accountID)
}

// observeWindowCoverage reports the sum whose window starts before the log's
// earliest record: the numerator is a LOWER BOUND there, because magpie's log
// (appended forever, never backfilled) simply does not hold the traffic that
// came before its first line. Nothing is degraded and no number is withheld —
// the estimate is still produced from what the log has — so the signal is a
// counter plus one WARN per window START (not per round: a window rolls
// forward, so its start identifies it, and the same start re-observes every
// round until the window rolls on).
//
// The warned set is bounded for good: a window start is recorded only while it
// reaches back past the log's start, and every window rolls forward past it
// within one window span (weeks at most), after which nothing is added again.
func (s *Source) observeWindowCoverage(idx *usageIndex, fromUnix int64) {
	if idx.earliest <= 0 || fromUnix <= 0 || fromUnix*int64(time.Second) >= idx.earliest {
		return
	}
	windowLowerBound.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lowerBoundFroms == nil {
		s.lowerBoundFroms = map[int64]bool{}
	}
	if s.lowerBoundFroms[fromUnix] {
		return
	}
	s.lowerBoundFroms[fromUnix] = true
	slog.Warn("clinepass window starts before the usage log, numerator is a lower bound",
		"path", s.usagePath,
		"from", time.Unix(fromUnix, 0).UTC().Format(time.RFC3339),
		"log_start", time.Unix(0, idx.earliest).UTC().Format(time.RFC3339))
}

// observeAttribution moves the keyless-attribution state machine, the same way
// observe moves the degradation one: the first call carrying a problem after a
// healthy state WARNs and remembers it, further calls with the SAME problem are
// silent, and the first call with no problem after one with a problem logs the
// recovery.
//
// It has two callers, one per kind of condition: installRoster moves it with the
// table it just built (a discovery is when a table's usability can change, so a
// discovery that finds the same condition again must not repeat the WARN), and
// observeStaleAttribution moves it from the sums, which are where a table prism
// could NOT confirm takes effect — and only once a table had been confirmed at
// all, so a source that never had one (no magpie on this host) stays silent
// instead of warning about a roster it never had. That split is also what keeps it quiet in the
// normal case: a healthy roster installs a table on every discovery and logs
// nothing, and DISCOVERY itself stays silent about a file it could not read —
// that silence is a contract (a host without magpie logs nothing), and the
// condition is already reported to the caller, which owns the roster rule
// (cmd/prism keeps the previous roster and logs the error).
//
// A provider file prism cannot read therefore does not advance the machine at its
// own moment; it marks the installed table STALE (markAttributionStale), and the
// first sum that has to do without attribution turns that into the transition
// (attribTableStale). What it must never do is announce a recovery: the recovery
// line belongs to a table a successful discovery just confirmed, which is why an
// empty discovery (no file read, or a file that names no account) cannot reach
// it.
//
// Why it is worth a WARN at all: the keyless `cline` rows are real ClinePass
// consumption (same provider key, same reset instant, the same upstream
// response ids as the keyed rows — see the 20261007 note), so while they cannot
// be attributed each account's summed consumption is a LOWER BOUND and the
// reversed 总额 is too small — the estimate keeps being produced, and nothing in
// the number says so. problem is "" when the table is usable (the healthy
// state); colliding names the ids sharing a key tail, which is why the table was
// dropped. The unallocated share is counted per row in
// clinepass_usage_unattributed_rows_total.
// observeStaleAttribution reports the sum that ran with NO confirmed tail table:
// the roster of the last successful discovery cannot be re-confirmed (the
// discovery after it did not read the provider file), so the keyless rows of this
// sum were deliberately left unattributed instead of being placed with a table
// that may predate a key rotation (see markAttributionStale).
//
// It is counted per SUM — the same unit as accountUnmatched, and a different one
// from unattributedRows, which is per row per scan — because the question it
// answers is "how many totals are lower bounds for this reason": a sum has one
// table and any number of keyless rows, so the two counters are independent and
// neither is a share of the other. A still-unconfirmed discovery keeps the counter
// growing instead of going flat, and the WARN is per transition, logged once when
// a sum first runs without a confirmed table; the recovery is the line a
// SUCCESSFUL discovery logs (see installRoster), so "attributable again" can
// never be printed for a table prism did not just confirm.
//
// snap comes from the ROUND, not from the live source: a round keeps the verdicts
// it began with (see Round), so the three windows of one fetch either all report
// the same condition or none of them does.
//
// A table that was NEVER confirmed is not this condition. "Unconfirmed" describes
// two very different states, and only one of them is a loss: a table that WENT
// stale is a roster the caller still polls with an upstream percent that still
// comes from those keys, so its keyless rows are now missing from every total
// while the total keeps being produced — worth a counter and one WARN on the
// transition. A source that never had a table (no magpie on this host, or a
// discovery that never succeeded) has lost nothing and has nothing to re-confirm,
// and a per-sum tick there would grow forever on a healthy deployment while the
// WARN told the operator to fix a roster that was never installed. Silence is
// therefore the contract for that state, exactly as it is for a host without a
// provider file (see clinePassAccounts) — the initial "no accounts yet" state must
// not read as a degradation.
func (s *Source) observeStaleAttribution(snap attributionSnapshot) {
	if snap.fresh || !snap.confirmed {
		return
	}
	attributionStale.Add(1)
	s.observeAttribution(attribTableStale, nil)
}

func (s *Source) observeAttribution(problem string, colliding []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if problem == s.attributionProblem {
		return
	}
	s.attributionProblem = problem
	if problem == "" {
		slog.Info("clinepass keyless usage rows are attributable again",
			"path", s.usagePath, "providers", s.providersPath)
		return
	}
	attrs := []any{"path", s.usagePath, "providers", s.providersPath, "reason", problem}
	if len(colliding) > 0 {
		// The ids are one-way (sha256 of the key) — never the keys themselves:
		// the tail that collided is a piece of a credential and gets no part of
		// this line either.
		attrs = append(attrs, "accounts", colliding)
	}
	slog.Warn("clinepass keyless usage rows are not attributable, account totals are a lower bound", attrs...)
}

// observe moves the degradation state machine: the first failure after a
// healthy state logs a WARN and flips the status, further failures only count
// (no per-round spam), and the first success after a failure logs the recovery
// and flips the status back.
//
// The WARN/Info pair is transition-based, but the STATUS is republished on
// every observation, because the two pieces of state it joins have different
// scopes: `degraded` is per Source, while sourceStatus is one expvar per
// PROCESS. Publishing "ok" only on this Source's own degraded→healthy
// transition leaves a "degraded" written by an earlier Source standing for the
// rest of the process lifetime — a source that never failed (prism builds a
// fresh one per process: the poller at startup, the CLI for one-shot runs)
// would then be reported as dead while it keeps answering fine.
//
// The counter counts CALLS, not causes: one refresh round sums one window per
// account (plus any CLI call), and a missing log fails all of them, so the
// counter's delta is the round's failure count — the same shape as before.
func (s *Source) observe(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		if s.degraded {
			s.degraded = false
			slog.Info("clinepass usage source recovered", "path", s.usagePath)
		}
		// A successful read is the source's CURRENT state: publish it, so the
		// metric describes the source that just answered instead of the last
		// source that ever failed.
		sourceStatus.Set("ok")
		return
	}
	sourceErrors.Add(1)
	if !s.degraded {
		s.degraded = true
		sourceStatus.Set("degraded")
		slog.Warn("clinepass usage source unavailable, total estimate disabled",
			"path", s.usagePath, "providers", s.providersPath, "error", err)
	}
}
