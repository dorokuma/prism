package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"strings"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/magpieusage"
	"github.com/dorokuma/prism/internal/oauth"
	"github.com/dorokuma/prism/internal/oauth/google"
	"github.com/dorokuma/prism/internal/oauth/xai"
	"github.com/dorokuma/prism/internal/planusage"
	"github.com/dorokuma/prism/internal/usage"
)

type cliAccount struct {
	name, provider, base, key, authHeader string
}

func (a cliAccount) Name() string         { return a.name }
func (a cliAccount) Provider() string     { return a.provider }
func (a cliAccount) BaseURL() string      { return a.base }
func (a cliAccount) Key() string          { return a.key }
func (a cliAccount) AuthHeader() string   { return a.authHeader }
func (a cliAccount) Client() *http.Client { return nil }

func runQuota(args []string) error {
	return runQuotaWith(args, os.Stdout)
}

func applyQuotaGeminiEstimate(ctx context.Context, cfg *config.Config, snap planusage.Snapshot) planusage.Snapshot {
	var parts []planusage.GrokTokenSum
	var store *usage.SQLiteStore
	path := cfg.Usage.DBPath
	if path != "" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			s := usage.NewReadOnlyStore(path)
			if err := s.Open(); err == nil {
				store = s
				parts = append(parts, func(c context.Context, f, t int64) (int64, error) {
					return store.SumTokensLike(c, f, t, "gemini-%", "gemini")
				})
			}
		}
	}
	if store != nil {
		defer store.Close()
	}
	if idx := openAgyIndex(); idx != nil {
		defer idx.Close()
		_ = idx.Refresh(ctx)
		parts = append(parts, idx.SumTokens)
	}
	sum := planusage.CombineTokenSums(parts...)
	if sum == nil {
		return snap
	}
	return planusage.ApplyWeekEstimate(ctx, snap, sum, planusage.DefaultGeminiEstimatePath, time.Now())
}

func applyQuotaGrokEstimate(ctx context.Context, cfg *config.Config, snap planusage.Snapshot) planusage.Snapshot {
	path := cfg.Usage.DBPath
	if path == "" {
		return snap
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return snap
	}
	store := usage.NewReadOnlyStore(path)
	if err := store.Open(); err != nil {
		return snap
	}
	defer store.Close()
	return planusage.ApplyGrokWeekEstimate(ctx, snap, store.SumGrokTokens, planusage.DefaultGrokEstimatePath, time.Now())
}

// quotaCredential is the upstream credential for prism quota. OAuth
// accounts have no static YAML key; the CLI reads the same token file
// the service uses (`prism auth xai` / `prism auth google`).
func quotaCredential(cfg *config.Config, a config.AccountConfig) string {
	if strings.TrimSpace(a.Key) != "" {
		return a.Key
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var src *oauth.Source
	switch strings.TrimSpace(a.OAuth) {
	case "xai":
		src = oauth.NewSource(cfg.OAuthDir, a.Name, "xai", func(ctx context.Context, refresh string) (xai.Tokens, error) {
			return xai.Refresh(ctx, xai.Config{HTTP: client}, refresh)
		})
	case "google":
		src = oauth.NewSource(cfg.OAuthDir, a.Name, "google", func(ctx context.Context, refresh string) (xai.Tokens, error) {
			tok, err := google.Refresh(ctx, google.Config{HTTP: client}, refresh)
			if err != nil {
				return xai.Tokens{}, err
			}
			return xai.Tokens{Access: tok.Access, Refresh: tok.Refresh, ExpiresAt: tok.ExpiresAt}, nil
		})
	default:
		return a.Key
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		return ""
	}
	return tok
}

// buildQuotaSnapshot completes one CLI snapshot for a key group. It is the
// CLI counterpart of the service poller's fetchOne:
//
//   - planusage.AssignAccountViews stamps the group's account names AND their
//     per-account key fingerprints in one aligned step, exactly like the
//     service path. Without it a production `prism quota` snapshot carried no
//     fingerprint at all, so a ClinePass row degraded to the bare ·, an
//     uncoloured account name and a position-keyed row — the "same-name
//     accounts are told apart by their dot" behaviour was unreachable in the
//     CLI while it worked on /admin/quota;
//   - the provider's 总额 estimate is applied on success only. A failed fetch
//     keeps the snapshot's error code and gets no estimate, like before.
//
// magpieSrc is the caller's magpie source, shared with the account discovery
// of this same invocation so the 总额 sums and "which accounts exist" read the
// same files and reuse one scan of the usage log. It is only used for a
// ClinePass snapshot.
//
// The HTTP fetch itself stays in the caller: this function is pure assembly.
func buildQuotaSnapshot(ctx context.Context, cfg *config.Config, g planusage.KeyGroup, snap planusage.Snapshot, ferr error, magpieSrc *magpieusage.Source) planusage.Snapshot {
	planusage.AssignAccountViews(&snap, g.Accounts)
	if ferr != nil {
		snap.Err = planusage.ErrorCode(ferr)
		return snap
	}
	switch snap.Provider {
	case "xai":
		snap = applyQuotaGrokEstimate(ctx, cfg, snap)
	case "gemini":
		snap = applyQuotaGeminiEstimate(ctx, cfg, snap)
	case "clinepass":
		snap = applyQuotaClinePassEstimate(ctx, snap, magpieSrc, planusage.AccountIDFrom(g.Accounts[0]))
	}
	return snap
}

func runQuotaWith(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "输出 JSON")
	provider := fs.String("provider", "", "只显示这个 provider")
	noColor := fs.Bool("no-color", false, "强制关闭卡片配色（默认仅在 TTY 上配色）")
	explicit := fs.String("config", "", "config.yaml 路径")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `用法: prism quota [flags]

查询 SuperGrok 周池、Gemini 5小时/周限与 ClinePass 5小时/周/月限占用。
不依赖 prism 服务进程。这不是 prism usage 的本地词元账本。

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, _, err := loadCLIConfig(*explicit)
	if err != nil {
		return err
	}
	if !cfg.Quota.Enabled {
		return fmt.Errorf("quota 已关闭（config.yaml 的 quota.enabled 为 false）")
	}

	var accs []planusage.AccountView
	for _, a := range cfg.Accounts {
		if strings.EqualFold(a.Provider, "clinepass") {
			// clinepass accounts are discovered from magpie, not config.
			continue
		}
		accs = append(accs, cliAccount{name: a.Name, provider: a.Provider, base: a.BaseURL, key: quotaCredential(cfg, a), authHeader: a.AuthHeader})
	}
	// Discover the ClinePass accounts from magpie's provider file for CLI
	// quota. The CLI has no long-lived source to borrow, and it does not need
	// one: magpieusage.Source keeps no file handle, so one Source for this
	// invocation is exactly the "read the files as they are now" behaviour the
	// CLI wants (a newly created or replaced file is read as it is) while the
	// account discovery and the 总额 sums still share one scan of the log.
	magpieSrc := newMagpieSource()
	if magpieAccs, err := readClinePassAccounts(context.Background(), magpieSrc); err != nil {
		slog.Warn("read clinepass accounts from magpie failed", "error", err)
	} else if len(magpieAccs) > 0 {
		accs = append(accs, magpieAccs...)
	}
	if *provider != "" {
		var filtered []planusage.AccountView
		for _, a := range accs {
			if a.Provider() == *provider {
				filtered = append(filtered, a)
			}
		}
		accs = filtered
	}
	fetchers := planusage.DefaultFetchers()
	groups := planusage.GroupByKey(accs, fetchers)

	timeout := cfg.Quota.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var snaps []planusage.Snapshot
	failed := 0
	for _, g := range groups {
		ctx := context.Background()
		snap, ferr := planusage.FetchWithRetry(ctx, g.Fetcher, g.Accounts[0], timeout)
		if ferr != nil {
			failed++
		}
		snaps = append(snaps, buildQuotaSnapshot(ctx, cfg, g, snap, ferr, magpieSrc))
	}

	if *jsonOut {
		if err := planusage.WriteJSON(out, snaps); err != nil {
			return err
		}
	} else {
		// The capsule cards carry ANSI colors; a pipe, a redirect or
		// --no-color must get the same layout without them.
		color := wantColor(out, *noColor)
		if _, err := io.WriteString(out, planusage.RenderCards(snaps, time.Now(), planusage.CardOptions{NoColor: !color})); err != nil {
			return err
		}
	}
	if len(groups) > 0 && failed == len(groups) {
		return fmt.Errorf("全部 %d 个套餐查询失败", failed)
	}
	return nil
}

// loadCLIConfig loads prism YAML for CLI subcommands (auth, quota, …).
// Explicit --config uses that path only. Otherwise it tries
// <cwd>/config.yaml then usageConfigFallbackPath
// (/var/lib/prism/config.yaml), matching systemd WorkingDirectory.
func loadCLIConfig(explicit string) (*config.Config, string, error) {
	var candidates []string
	if explicit != "" {
		candidates = []string{explicit}
	} else {
		candidates = []string{
			filepath.Join(usageConfigDir(), "config.yaml"),
			usageConfigFallbackPath,
		}
	}
	var last error
	for _, p := range candidates {
		cfg, err := config.LoadConfig(p)
		if err == nil {
			return cfg, p, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("找不到 config.yaml")
	}
	return nil, "", fmt.Errorf("无法加载配置: %v", last)
}
