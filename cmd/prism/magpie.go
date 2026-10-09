package main

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/magpieusage"
	"github.com/dorokuma/prism/internal/planusage"
)

// magpieUsagePath and magpieProvidersPath are the magpie files the ClinePass
// 总额 estimate reads (usage.jsonl) and the ClinePass accounts are discovered
// from (providers.json). Their defaults are the production paths and they are
// deliberately NOT config keys — the same pattern as agyIndexPath /
// agyusage.DefaultIndexPath. They are package variables so CLI tests can
// redirect them and live files cannot leak into fixtures.
var (
	magpieUsagePath     = magpieusage.DefaultUsagePath
	magpieProvidersPath = magpieusage.DefaultProvidersPath
)

// newMagpieSource returns a source over the configured magpie files. It is the
// single construction point for both the one-shot CLI invocation and the
// service poller, so the two paths cannot read different files. Creating a
// source touches nothing on disk: magpie may not be installed yet, and each
// call re-attempts the read (see magpieusage.Source), so a magpie that appears
// later is picked up without a restart.
func newMagpieSource() *magpieusage.Source {
	return magpieusage.NewSource(magpieUsagePath, magpieProvidersPath)
}

// magpieAccountView is an AccountView backed by a magpie ClinePass provider
// key. It carries magpie's providerKeyId (for per-account token sums) and
// exposes the provider key as the quota key. The raw key is never logged or
// rendered; the display colour is derived from planusage.KeyFingerprint of the
// key (the same 口径 as the service poller and the CLI, see
// planusage.AssignAccountViews).
type magpieAccountView struct {
	accountID string
	name      string
	provider  string
	apiKey    string
}

func (a magpieAccountView) Name() string         { return a.name }
func (a magpieAccountView) Provider() string     { return a.provider }
func (a magpieAccountView) BaseURL() string      { return "https://cline.bot" }
func (a magpieAccountView) Key() string          { return a.apiKey }
func (a magpieAccountView) AuthHeader() string   { return "Bearer " + a.apiKey }
func (a magpieAccountView) Client() *http.Client { return nil }

// AccountID is the magpie providerKeyId this account's consumption is summed
// by (planusage.AccountIDFrom picks it up). It is the id magpie stamps on the
// account's usage lines, so the roster entry and the sum meet on it.
func (a magpieAccountView) AccountID() string { return a.accountID }

// magpieProvidersPathIfPresent returns magpie's provider file path when this
// host actually has one (a regular file) and "" otherwise. It is used only to
// NAME the reason a ClinePass roster shrank to zero (see clinePassRosterDelta);
// the reads themselves never pre-check, because magpieusage already treats a
// missing file as "no accounts, no error". A deployment without magpie must
// stay SILENT — no warning, no failure, just no ClinePass accounts and no 总额
// estimate.
func magpieProvidersPathIfPresent() string {
	path := magpieProvidersPath
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return path
}

// readClinePassAccounts discovers the ClinePass accounts from magpie's provider
// file and turns each into an AccountView carrying the provider key needed for
// Bearer auth. The raw key is never logged or rendered. src is the magpie
// source the caller also uses for the token sums; the discovery and the sums
// therefore share one implementation of "which accounts exist".
//
// A host WITHOUT a magpie provider file reports no accounts and NO error
// (there is simply no magpie here, see magpieusage.clinePassAccounts). A file
// that exists but cannot be read or parsed IS an error, and it is propagated
// instead of being swallowed: the service roster keeps its previous ClinePass
// views on that path (refreshQuotaAccounts), so a transient failure cannot make
// the whole ClinePass block silently disappear.
func readClinePassAccounts(ctx context.Context, src *magpieusage.Source) ([]planusage.AccountView, error) {
	magpieAccounts, err := src.ListClinePassAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("magpieusage: list clinepass accounts: %w", err)
	}
	out := make([]planusage.AccountView, 0, len(magpieAccounts))
	for _, a := range magpieAccounts {
		out = append(out, magpieAccountView{
			accountID: a.AccountID,
			name:      a.Name,
			provider:  "clinepass",
			// The key is carried only for Bearer auth; it is never logged.
			apiKey: a.Key,
		})
	}
	return out, nil
}

// nextQuotaViews is the roster rule of one (re)discovery round: the config
// accounts in order, followed by the magpie-discovered ClinePass accounts (in
// discovery order). When the discovery FAILED (err != nil) the previous
// magpie-backed views are kept instead, so a transient magpie read error never
// drops the whole ClinePass block from the cards. A successful discovery
// always wins — including an empty one, which is how a disabled or removed
// account actually leaves the roster. Non-clinepass views of prev are ignored:
// those are rebuilt from the pool on every round.
func nextQuotaViews(configViews, discovered, prev []planusage.AccountView, err error) []planusage.AccountView {
	views := append([]planusage.AccountView(nil), configViews...)
	if err != nil {
		for _, a := range prev {
			if strings.EqualFold(a.Provider(), "clinepass") {
				views = append(views, a)
			}
		}
		return views
	}
	return append(views, discovered...)
}

// ClinePass roster observability, published on /metrics in the same expvar
// pattern as clinepass_usage_source_* (internal/magpieusage):
//
//   - clinepass_quota_accounts is the CURRENT number of ClinePass views in the
//     quota roster. Account discovery is the only writer of the roster, so a
//     roster that shrank to zero — magpie's provider file gone, every provider
//     key removed, an empty key skipped, a magpie-side provider rename — is
//     visible here instead of only as a silently missing card;
//   - clinepass_quota_roster_drops_total counts the (re)discovery rounds that
//     dropped a NON-EMPTY ClinePass roster to zero. Every one of them also logs
//     a WARN with the reason (see clinePassRosterDelta), so the silent-shrink
//     paths leave a trace once each instead of none.
var (
	clinepassQuotaAccounts    = expvar.NewInt("clinepass_quota_accounts")
	clinepassQuotaRosterDrops = expvar.NewInt("clinepass_quota_roster_drops_total")
)

// clinePassViewCount counts the ClinePass views of one quota roster.
func clinePassViewCount(views []planusage.AccountView) int {
	n := 0
	for _, a := range views {
		if strings.EqualFold(a.Provider(), "clinepass") {
			n++
		}
	}
	return n
}

// clinePassRosterDelta reports the ClinePass size of the roster one
// (re)discovery round produced and — when a non-empty ClinePass roster
// collapsed to ZERO — why, as a short reason string for the WARN. The reason,
// in the order the discovery produces it:
//
//   - "magpie providers absent": magpie's provider file is not there, which
//     readClinePassAccounts reports as a successful EMPTY result (a host
//     without magpie must stay silent, and that semantic is unchanged). So one
//     SIGHUP during a config swap/rollback/rename window, while the file is
//     momentarily gone, removes the whole ClinePass block with no other trace;
//   - "no clinepass accounts discovered": the discovery succeeded but returned
//     nothing — every ClinePass provider entry is gone (a magpie-side rename of
//     the provider id is matched EXACTLY, so it reads as "no entry"), or its
//     keys were empty;
//   - "discovery failed": the discovery returned an error. nextQuotaViews keeps
//     the previous ClinePass views on that path, so a zero count here means the
//     previous roster had none either; the label is kept for callers that pass
//     a different roster rule.
//
// An empty roster that was already empty stays quiet: nothing was removed.
func clinePassRosterDelta(prev, next []planusage.AccountView, providersPresent bool, discoveryErr error) (count int, dropReason string) {
	count = clinePassViewCount(next)
	if clinePassViewCount(prev) == 0 || count > 0 {
		return count, ""
	}
	switch {
	case !providersPresent:
		return count, "magpie providers absent"
	case discoveryErr != nil:
		return count, "discovery failed"
	default:
		return count, "no clinepass accounts discovered"
	}
}

// clinepassEstimateSkipped counts the ClinePass estimates dropped because the
// account carried no magpie providerKeyId (see applyQuotaClinePassEstimate).
// Published on /metrics next to clinepass_quota_accounts, so an account that
// silently stopped producing a 总额 becomes visible instead of only missing
// from a card.
var clinepassEstimateSkipped = expvar.NewInt("clinepass_quota_estimate_skipped_total")

// applyQuotaClinePassEstimate fills the ClinePass windows' 总额 in the CLI path
// (`prism quota`) from magpie's cline-pass consumption for ONE account. It
// shares planusage.ApplyClinePassEstimates with the service poller, so the CLI
// and /admin/quota produce the same numbers — including the log's line-shape
// guard inside the source, which degrades a drifted log to "no 总额" instead of
// mis-bounding the window. A missing or unusable magpie usage log leaves the
// estimates empty and the snapshot untouched (the source reports the error, the
// CLI stays silent about it exactly as it did for a missing database: the CLI
// has no long-lived state to degrade).
//
// An empty accountID does NOT fall back to a subscription-wide sum. Two
// ClinePass accounts are TWO INDEPENDENT pools with their own provider key, so
// a subscription-wide numerator would add one subscription's traffic to the
// other's percent and print a 总额 belonging to neither (串账). The estimate is
// dropped instead: one WARN naming the account, counted in
// clinepass_quota_estimate_skipped_total.
func applyQuotaClinePassEstimate(ctx context.Context, snap planusage.Snapshot, src *magpieusage.Source, accountID string) planusage.Snapshot {
	if accountID == "" {
		clinepassEstimateSkipped.Add(1)
		slog.Warn("clinepass estimate skipped: no magpie account id",
			"provider", snap.Provider, "accounts", snap.Accounts)
		return snap
	}
	// ONE round for the whole estimate pass: the three windows of this account
	// are reversed against the same upstream percents and combined into one
	// snapshot, so they must all be attributed with the tail table (and the
	// freshness verdict) taken here. Reading the source's live state per window
	// instead would let a SIGHUP landing mid-pass split one account's windows
	// across two rosters.
	round := src.BeginRound(accountID)
	sum := func(ctx context.Context, from, to int64) (int64, error) {
		return round.Sum(ctx, from, to)
	}
	return planusage.ApplyClinePassEstimates(ctx, snap, sum, time.Now())
}
