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

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/planusage"
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

func runQuotaWith(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "输出 JSON")
	provider := fs.String("provider", "", "只显示这个 provider")
	explicit := fs.String("config", "", "config.yaml 路径")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `用法: prism quota [flags]

查询上游套餐用量（当前：OpenCode Go 的短期 / 中期 / 长期窗口）。
不依赖 prism 服务进程，直接用 config 里的账号 key 请求上游。
这不是 prism usage 的本地词元账本。

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

	cfg, err := loadQuotaConfig(*explicit)
	if err != nil {
		return err
	}
	if !cfg.Quota.Enabled {
		return fmt.Errorf("quota 已关闭（config.yaml 的 quota.enabled 为 false）")
	}

	var accs []planusage.AccountView
	for _, a := range cfg.Accounts {
		accs = append(accs, cliAccount{name: a.Name, provider: a.Provider, base: a.BaseURL, key: a.Key, authHeader: a.AuthHeader})
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
		cancel()
		snap.Accounts = names
		if ferr != nil {
			failed++
			snap.Err = planusage.ErrorCode(ferr)
		}
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

func loadQuotaConfig(explicit string) (*config.Config, error) {
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
			return cfg, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("找不到 config.yaml")
	}
	return nil, fmt.Errorf("无法加载配置: %v", last)
}
