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

	"github.com/dorokuma/prism/internal/metapiusage"
	"github.com/dorokuma/prism/internal/planusage"
)

// metapiUsageDBPath is the metapi hub database the ClinePass 总额 estimate
// reads. Its default is the production path and it is deliberately NOT a
// config key — the same pattern as agyIndexPath /
// agyusage.DefaultIndexPath. It is a package variable so CLI tests can
// redirect it and a live database cannot leak into fixtures.
var metapiUsageDBPath = metapiusage.DefaultDBPath

// metapiAccountView is an AccountView backed by a metapi ClinePass
// account. It carries the metapi account_id (for per-account token sums)
// and exposes the api_token as the quota key. The raw api_token is never
// logged or rendered; the display colour is derived from
// planusage.KeyFingerprint of the key (the same 口径 as the service poller
// and the CLI, see planusage.AssignAccountViews).
type metapiAccountView struct {
	accountID int64
	name      string
	apiToken  string
}

func (a metapiAccountView) Name() string         { return a.name }
func (a metapiAccountView) Provider() string     { return "clinepass" }
func (a metapiAccountView) BaseURL() string      { return "https://cline.bot" }
func (a metapiAccountView) Key() string          { return a.apiToken }
func (a metapiAccountView) AuthHeader() string   { return "Bearer " + a.apiToken }
func (a metapiAccountView) Client() *http.Client { return nil }
func (a metapiAccountView) AccountID() int64     { return a.accountID }

// metapiUsageDBPathIfPresent returns the metapi hub database path when this
// host actually has one (a regular file) and "" otherwise. A deployment
// without metapi must stay SILENT — no warning, no failure, just no ClinePass
// accounts and no 总额 estimate — so both the account discovery and the
// estimate start from this check instead of reporting an absent database as an
// error on every call.
func metapiUsageDBPathIfPresent() string {
	path := metapiUsageDBPath
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return path
}

// openMetapiUsage opens the metapi usage database strictly read-only for
// the one-shot CLI path (`prism quota`): it opens once per invocation and
// closes before exit, so a replaced file or a recovered database is picked
// up by the next invocation by construction. The service poller does NOT
// use this helper — main wires metapiusage.Source, which re-opens per
// refresh round (see source.go) so neither a long-lived handle nor a
// startup failure can freeze the estimate. A missing or non-regular file
// is expected on hosts without metapi and returns nil silently; an
// unreadable/failing database is logged and also returns nil. Callers must
// NEVER fail a quota fetch because of this: a nil handle simply means the
// ClinePass windows carry no 总额.
func openMetapiUsage() *metapiusage.Store {
	path := metapiUsageDBPathIfPresent()
	if path == "" {
		return nil
	}
	st, err := metapiusage.Open(path)
	if err != nil {
		slog.Warn("metapi usage db unavailable, clinepass total estimate disabled", "error", err)
		return nil
	}
	return st
}

// readClinePassAccounts reads the ACTIVE ClinePass accounts (metapi site 49)
// and turns each into an AccountView carrying the api_token needed for Bearer
// auth. The raw api_token is never logged or rendered. src is the metapi
// source the caller also uses for the token sums; the discovery and the sums
// therefore share one implementation of "which accounts exist".
//
// A host WITHOUT a metapi database reports no accounts and NO error (there is
// simply no metapi here, see metapiUsageDBPathIfPresent). A database that
// exists but cannot list IS an error, and it is propagated instead of being
// swallowed: the service roster keeps its previous ClinePass views on that
// path (refreshQuotaAccounts), so a transient failure cannot make the whole
// ClinePass block silently disappear.
func readClinePassAccounts(ctx context.Context, src *metapiusage.Source) ([]planusage.AccountView, error) {
	if metapiUsageDBPathIfPresent() == "" {
		return nil, nil
	}
	metapiAccounts, err := src.ListClinePassAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("metapiusage: list clinepass accounts: %w", err)
	}
	out := make([]planusage.AccountView, 0, len(metapiAccounts))
	for _, a := range metapiAccounts {
		// api_token is carried only for Bearer auth; it is never logged.
		out = append(out, metapiAccountView{
			accountID: a.AccountID,
			name:      a.Username,
			apiToken:  a.APIToken,
		})
	}
	return out, nil
}

// nextQuotaViews is the roster rule of one (re)discovery round: the config
// accounts in order, followed by the metapi-discovered ClinePass accounts (in
// discovery order). When the discovery FAILED (err != nil) the previous
// metapi-backed views are kept instead, so a transient metapi read error never
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
// pattern as clinepass_usage_source_* (internal/metapiusage):
//
//   - clinepass_quota_accounts is the CURRENT number of ClinePass views in the
//     quota roster. Account discovery is the only writer of the roster, so a
//     roster that shrank to zero — the metapi database file gone, every
//     site-49 account disabled, a NULL/empty api_token row skipped, a site_id
//     rebuild — is visible here instead of only as a silently missing card;
//   - clinepass_quota_roster_drops_total counts the (re)discovery rounds that
//     dropped a NON-EMPTY ClinePass roster to zero. Every one of them also
//     logs a WARN with the reason (see clinePassRosterDelta), so the three
//     silent-shrink paths (missing FILE + SIGHUP, status matched exactly,
//     NULL/empty token or site_id rebuild) leave a trace once each instead of
//     none.
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
//   - "metapi database absent": the hub database FILE is not there, which
//     readClinePassAccounts reports as a successful EMPTY result (a host
//     without metapi must stay silent, and that semantic is unchanged). So one
//     SIGHUP during a database swap/rollback/rename window, while the file is
//     momentarily gone, removes the whole ClinePass block with no other trace;
//   - "no clinepass accounts discovered": the discovery succeeded but returned
//     nothing — every site-49 account is disabled (status is matched exactly,
//     so a third state value or a case change drops it silently), or its row
//     was skipped for a NULL/empty api_token, or site_id 49 no longer exists
//     after a metapi rebuild;
//   - "discovery failed": the discovery returned an error. nextQuotaViews
//     keeps the previous ClinePass views on that path, so a zero count here
//     means the previous roster had none either; the label is kept for callers
//     that pass a different roster rule.
//
// An empty roster that was already empty stays quiet: nothing was removed.
func clinePassRosterDelta(prev, next []planusage.AccountView, dbPresent bool, discoveryErr error) (count int, dropReason string) {
	count = clinePassViewCount(next)
	if clinePassViewCount(prev) == 0 || count > 0 {
		return count, ""
	}
	switch {
	case !dbPresent:
		return count, "metapi database absent"
	case discoveryErr != nil:
		return count, "discovery failed"
	default:
		return count, "no clinepass accounts discovered"
	}
}

// clinepassEstimateSkipped counts the ClinePass estimates dropped because
// the account carried no metapi account_id (see
// applyQuotaClinePassEstimate). Published on /metrics next to
// clinepass_quota_accounts, so an account that silently stopped producing a
// 总额 becomes visible instead of only missing from a card.
var clinepassEstimateSkipped = expvar.NewInt("clinepass_quota_estimate_skipped_total")

// applyQuotaClinePassEstimate fills the ClinePass windows' 总额 in the CLI
// path (`prism quota`) from metapi's cline-pass consumption for ONE metapi
// account. It shares planusage.ApplyClinePassEstimates with the service
// poller, so the CLI and /admin/quota produce the same numbers — including
// the created_at shape guard inside Store.SumClinePassTokensByAccount, which
// degrades a drifted database to "no 总额" instead of mis-bounding the
// window. A missing/unreadable metapi database leaves the estimates empty
// and the snapshot untouched.
//
// accountID <= 0 does NOT fall back to a subscription-wide sum. Two metapi
// site-49 accounts are TWO INDEPENDENT pools with their own api_token, so a
// subscription-wide numerator would add one subscription's traffic to the
// other's percent and print a 总额 belonging to neither (串账). The estimate
// is dropped instead: one WARN naming the account, counted in
// clinepass_quota_estimate_skipped_total.
func applyQuotaClinePassEstimate(ctx context.Context, snap planusage.Snapshot, accountID int64) planusage.Snapshot {
	if accountID <= 0 {
		clinepassEstimateSkipped.Add(1)
		slog.Warn("clinepass estimate skipped: no metapi account id",
			"provider", snap.Provider, "accounts", snap.Accounts)
		return snap
	}
	st := openMetapiUsage()
	if st == nil {
		return snap
	}
	defer st.Close()
	sum := func(ctx context.Context, from, to int64) (int64, error) {
		return st.SumClinePassTokensByAccount(ctx, accountID, from, to)
	}
	return planusage.ApplyClinePassEstimates(ctx, snap, sum, time.Now())
}
