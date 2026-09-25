package main

import (
	"context"
	"log/slog"
	"os"
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
	path := metapiUsageDBPath
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	st, err := metapiusage.Open(path)
	if err != nil {
		slog.Warn("metapi usage db unavailable, clinepass total estimate disabled", "error", err)
		return nil
	}
	return st
}

// applyQuotaClinePassEstimate fills the three ClinePass windows' 总额 in
// the CLI path (`prism quota`) from metapi's cline-pass consumption. It
// shares planusage.ApplyClinePassEstimates with the service poller, so the
// CLI and /admin/quota produce the same numbers — including the
// created_at shape guard inside Store.SumClinePassTokens, which degrades a
// drifted database to "no 总额" instead of mis-bounding the window. A
// missing/unreadable metapi database leaves the estimates empty and the
// snapshot untouched.
func applyQuotaClinePassEstimate(ctx context.Context, snap planusage.Snapshot) planusage.Snapshot {
	st := openMetapiUsage()
	if st == nil {
		return snap
	}
	defer st.Close()
	return planusage.ApplyClinePassEstimates(ctx, snap, st.SumClinePassTokens, time.Now())
}
