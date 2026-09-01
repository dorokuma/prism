package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/render"
)

var defaultModelCacheDir = "/var/lib/prism/model_cache"

// runModels executes the 'prism models' CLI command.
func runModels(args []string) error {
	subcommand := "refresh"
	var flagArgs []string

	if len(args) > 0 {
		first := args[0]
		if first == "refresh" || first == "status" {
			subcommand = first
			flagArgs = args[1:]
		} else if strings.HasPrefix(first, "-") {
			// default to refresh if options start immediately
			subcommand = "refresh"
			flagArgs = args
		} else if first == "-h" || first == "--help" || first == "help" {
			printModelsUsage(os.Stdout)
			return nil
		} else {
			return fmt.Errorf("unknown subcommand %q. Usage: prism models [refresh|status] [flags]", first)
		}
	}

	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	var (
		provider   string
		direct     bool
		configPath string
		serverURL  string
		adminToken string
		jsonOutput bool
	)

	fs.StringVar(&provider, "provider", "", "Target provider to refresh or query status (default all)")
	fs.BoolVar(&direct, "direct", false, "Execute directly via local cache instead of HTTP API")
	fs.StringVar(&configPath, "config", "config.yaml", "Path to config file")
	fs.StringVar(&serverURL, "url", "", "Prism server base URL (e.g. http://127.0.0.1:18790)")
	fs.StringVar(&adminToken, "token", "", "Prism admin token (defaults to PRISM_ADMIN_TOKEN env)")
	fs.BoolVar(&jsonOutput, "json", false, "Output response in JSON format")

	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	if direct {
		return runModelsDirect(subcommand, configPath, provider, jsonOutput)
	}
	return runModelsHTTP(subcommand, serverURL, configPath, adminToken, provider, jsonOutput)
}

func printModelsUsage(out io.Writer) {
	fmt.Fprint(out, `prism models — Manage and query upstream model cache

Usage:
  prism models [refresh|status] [flags]

Subcommands:
  refresh    Trigger model cache refresh (default)
  status     Query current model cache status (read-only)

Flags:
  --provider string   Target provider (default: all configured providers)
  --direct            Execute directly against local cache (offline/service stopped)
  --config string     Path to config file (default: config.yaml)
  --url string        Prism server base URL (e.g. http://127.0.0.1:18790)
  --token string      Admin token (defaults to PRISM_ADMIN_TOKEN env)
  --json              Output in JSON format
`)
}

func runModelsDirect(subcommand, configPath, provider string, jsonOutput bool) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", configPath, err)
	}

	if provider != "" && !cfg.HasProvider(provider) {
		return fmt.Errorf("provider %q not found in config %q", provider, configPath)
	}

	p := pool.NewPool(cfg.Accounts)
	cacheDir := defaultModelCacheDir
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("create cache dir %q: %w", cacheDir, err)
	}

	mc, err := cache.New(cacheDir, p, cfg)
	if err != nil {
		return fmt.Errorf("init model cache: %w", err)
	}
	mc.LoadFromDisk()

	if subcommand == "status" {
		// Read-only: no Fetch or RefreshAll
		snapshot := mc.Snapshot()
		if provider != "" {
			if snap, ok := snapshot[provider]; ok {
				snapshot = map[string]cache.ProviderSnapshot{provider: snap}
			} else {
				snapshot = map[string]cache.ProviderSnapshot{}
			}
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{
				"status":    "ok",
				"provider":  provider,
				"providers": snapshot,
			})
		}
		printSnapshotTable(snapshot)
		return nil
	}

	// subcommand == "refresh"
	if provider != "" {
		if err := mc.Fetch(provider); err != nil {
			return fmt.Errorf("fetch provider %q: %w", provider, err)
		}
	} else {
		mc.RefreshAll()
	}

	mc.SyncTools(cfg)
	snapshot := mc.Snapshot()

	if provider == "" {
		var failed []string
		for p, s := range snapshot {
			if s.Error != "" {
				failed = append(failed, fmt.Sprintf("%s (%s)", p, s.Error))
			}
		}
		if len(failed) > 0 {
			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(map[string]any{
					"status":    "error",
					"provider":  provider,
					"providers": snapshot,
					"error":     fmt.Sprintf("刷新失败 %d 个: %s", len(failed), strings.Join(failed, ", ")),
				})
			} else {
				printSnapshotTable(snapshot)
			}
			return fmt.Errorf("刷新失败 %d 个: %s", len(failed), strings.Join(failed, ", "))
		}
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"status":    "completed",
			"provider":  provider,
			"providers": snapshot,
		})
	}

	fmt.Println("刷新成功。")
	printSnapshotTable(snapshot)
	return nil
}

func runModelsHTTP(subcommand, serverURL, configPath, adminToken, provider string, jsonOutput bool) error {
	baseURL := serverURL
	if baseURL == "" {
		baseURL = resolveBaseURLFromConfig(configPath)
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	if adminToken == "" {
		adminToken = os.Getenv("PRISM_ADMIN_TOKEN")
	}

	reqURL := baseURL + "/prism/v1/models/refresh"
	if provider != "" {
		v := url.Values{}
		v.Set("provider", provider)
		reqURL += "?" + v.Encode()
	}

	method := http.MethodPost
	if subcommand == "status" {
		method = http.MethodGet
	}

	req, err := http.NewRequest(method, reqURL, bytes.NewReader(nil))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect to prism at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("未授权 (401)：需有效 PRISM_ADMIN_TOKEN")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("限流 (429)：10 秒内最多刷新一次")
	}
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("请求无效 (400): %s", string(bodyBytes))
	}
	if method == http.MethodGet && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态查询失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}
	if method == http.MethodPost && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("刷新失败 (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if jsonOutput {
		fmt.Println(string(bodyBytes))
		return nil
	}

	var refreshResp cache.RefreshResponse
	if err := json.Unmarshal(bodyBytes, &refreshResp); err == nil {
		if method == http.MethodPost {
			fmt.Println("刷新成功。")
		}
		if refreshResp.Provider != "" {
			fmt.Printf("目标供应商: %s\n", refreshResp.Provider)
		}
		printSnapshotTable(refreshResp.Providers)
	} else {
		if method == http.MethodPost {
			fmt.Printf("刷新成功: %s\n", string(bodyBytes))
		} else {
			fmt.Println(string(bodyBytes))
		}
	}

	return nil
}

func resolveBaseURLFromConfig(configPath string) string {
	defaultURL := "http://127.0.0.1:18790"
	cfg, err := config.LoadConfig(configPath)
	if err != nil || cfg == nil || cfg.Listen == "" {
		return defaultURL
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		// Handle cases like ::1:18790 where brackets might be omitted
		if idx := strings.LastIndex(cfg.Listen, ":"); idx > 0 {
			host = cfg.Listen[:idx]
			port = cfg.Listen[idx+1:]
		} else {
			return defaultURL
		}
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == ":" || host == "::" || host == "0.0.0.0" || host == "0:0:0:0:0:0:0:0" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	scheme := "http"
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%s", scheme, host, port)
}

func printSnapshotTable(snapshots map[string]cache.ProviderSnapshot) {
	if len(snapshots) == 0 {
		fmt.Println("无供应商")
		return
	}

	names := make([]string, 0, len(snapshots))
	for name := range snapshots {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		s := snapshots[name]
		updated := "-"
		if s.UpdatedAt != nil {
			updated = s.UpdatedAt.Format("01-02 15:04")
		}
		rows = append(rows, []string{
			name,
			strconv.Itoa(s.ModelsCount),
			updated,
		})
	}

	tbl := &render.Table{
		Columns: []render.Column{
			{Title: "供应商", Align: render.AlignLeft},
			{Title: "模型", Align: render.AlignRight},
			{Title: "更新时间", Align: render.AlignLeft},
		},
		Rows:   rows,
		Indent: "  ",
		Gap:    " ",
	}
	fmt.Print(tbl.Render())
}
