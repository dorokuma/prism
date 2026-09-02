package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"strings"

	"github.com/dorokuma/prism/internal/config"
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

func grokPriceFor(cfg *config.Config) func(string, int64) *usage.Price {
	return func(model string, contextTokens int64) *usage.Price {
		if cfg == nil {
			return nil
		}
		m := strings.TrimSuffix(model, "-build")
		meta, ok := cfg.LookupModelMetadata("xai", m)
		if !ok || meta.Cost == nil {
			meta, ok = cfg.LookupModelMetadata("", m)
		}
		if !ok || meta.Cost == nil {
			return nil
		}
		c := meta.Cost.EffectiveCost(contextTokens)
		if c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 {
			return nil
		}
		return &usage.Price{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}
	}
}

func applyQuotaGrokEstimate(ctx context.Context, cfg *config.Config, snap planusage.Snapshot) planusage.Snapshot {
	path := cfg.Usage.DBPath
	if path == "" {
		return snap
	}
	store := usage.NewSQLiteStore(path)
	if err := store.Open(); err != nil {
		return snap
	}
	defer store.Close()
	from, to := planusage.GrokBuildImportWindow(snap, time.Now())
	if _, err := usage.ImportGrokBuild(ctx, store, usage.DefaultGrokSessionsDir(), from, to, grokPriceFor(cfg)); err != nil {
		// estimate still runs on whatever is already in the database
	}
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

func runQuotaWith(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "输出 JSON")
	provider := fs.String("provider", "", "只显示这个 provider")
	explicit := fs.String("config", "", "config.yaml 路径")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `用法: prism quota [flags]

查询 SuperGrok 周池与 Gemini 5小时/周限占用。
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
		accs = append(accs, cliAccount{name: a.Name, provider: a.Provider, base: a.BaseURL, key: quotaCredential(cfg, a), authHeader: a.AuthHeader})
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
		names := make([]string, 0, len(g.Accounts))
		for _, a := range g.Accounts {
			names = append(names, a.Name())
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		snap, ferr := g.Fetcher.Fetch(ctx, g.Accounts[0])
		snap.Accounts = names
		if ferr != nil {
			failed++
			snap.Err = planusage.ErrorCode(ferr)
		} else {
			if snap.Provider == "xai" {
				snap = applyQuotaGrokEstimate(ctx, cfg, snap)
			}
			switch snap.Provider {
			case "gemini":
				path := cfg.Usage.DBPath
				if path != "" {
					if fi, serr := os.Stat(path); serr == nil && fi.Mode().IsRegular() {
						store := usage.NewReadOnlyStore(path)
						if serr := store.Open(); serr == nil {
							snap = planusage.ApplyWeekEstimate(ctx, snap, func(c context.Context, f, t int64) (int64, error) {
								return store.SumTokensLike(c, f, t, "gemini-%", "gemini")
							}, planusage.DefaultGeminiEstimatePath, time.Now())
							store.Close()
						}
					}
				}
			}
		}
		cancel()
		snaps = append(snaps, snap)
	}

	if *jsonOut {
		if err := planusage.WriteJSON(out, snaps); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(out, planusage.RenderTable(snaps)); err != nil {
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
