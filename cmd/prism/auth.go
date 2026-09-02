package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/oauth"
	"github.com/dorokuma/prism/internal/oauth/google"
	"github.com/dorokuma/prism/internal/oauth/xai"
)

func runAuth(args []string) error {
	return runAuthWith(args, os.Stdout)
}

func runAuthWith(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, `prism auth — OAuth login for built-in providers

Usage:
  prism auth xai [--account NAME] [--config PATH] [--dir PATH]
  prism auth google [--account NAME] [--from PATH] [--config PATH] [--dir PATH]

xai: RFC 8628 device flow against auth.x.ai (SuperGrok / Grok-CLI client).
google: import Antigravity CLI token (agy) and store under oauth_dir.

Tokens are stored under oauth_dir (default /var/lib/prism/oauth/<account>.json)
and refreshed on the request path. YAML must not contain the access token.

--config defaults to ./config.yaml, then /var/lib/prism/config.yaml.

A running prism process picks up a new login from the file without restart.
`)
		return nil
	}
	switch args[0] {
	case "xai":
		return runAuthXAI(args[1:], out)
	case "google":
		return runAuthGoogle(args[1:], out)
	default:
		return fmt.Errorf("unknown auth provider %q (supported: xai, google)", args[0])
	}
}

func pickOAuthAccount(cfg *config.Config, loadedPath, provider, accountFlag string) (config.AccountConfig, error) {
	var oauthAccounts []config.AccountConfig
	for _, a := range cfg.Accounts {
		if strings.TrimSpace(a.OAuth) == provider {
			oauthAccounts = append(oauthAccounts, a)
		}
	}
	if len(oauthAccounts) == 0 {
		hint := "add an account (no key) with oauth: " + provider
		switch provider {
		case "xai":
			hint += " and base_url https://api.x.ai/v1"
		case "google":
			hint += " and base_url https://cloudcode-pa.googleapis.com"
		}
		return config.AccountConfig{}, fmt.Errorf("no account with oauth: %s in %s; %s", provider, loadedPath, hint)
	}
	chosen := oauthAccounts[0]
	if accountFlag != "" {
		found := false
		for _, a := range oauthAccounts {
			if a.Name == accountFlag {
				chosen = a
				found = true
				break
			}
		}
		if !found {
			return config.AccountConfig{}, fmt.Errorf("account %q is not an oauth: %s account", accountFlag, provider)
		}
	} else if len(oauthAccounts) > 1 {
		var names []string
		for _, a := range oauthAccounts {
			names = append(names, a.Name)
		}
		return config.AccountConfig{}, fmt.Errorf("multiple oauth: %s accounts (%s); pass --account", provider, strings.Join(names, ", "))
	}
	return chosen, nil
}

func runAuthXAI(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("auth xai", flag.ContinueOnError)
	fs.SetOutput(out)
	account := fs.String("account", "", "oauth account name (required when more than one xai oauth account exists)")
	configPath := fs.String("config", "", "config file (default: ./config.yaml, then /var/lib/prism/config.yaml)")
	dir := fs.String("dir", "", "token directory (default: config oauth_dir)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, loadedPath, err := loadCLIConfig(*configPath)
	if err != nil {
		return err
	}
	chosen, err := pickOAuthAccount(cfg, loadedPath, "xai", *account)
	if err != nil {
		return err
	}
	storeDir := *dir
	if storeDir == "" {
		storeDir = cfg.OAuthDir
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	xcfg := xai.Config{HTTP: &http.Client{Timeout: 20 * time.Second}}
	device, err := xai.RequestDevice(ctx, xcfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "在浏览器打开: %s\n输入代码: %s\n等待授权…\n", device.VerificationURI, device.UserCode)
	tok, err := xai.PollToken(ctx, xcfg, device)
	if err != nil {
		return err
	}
	if err := oauth.Save(storeDir, chosen.Name, "xai", tok); err != nil {
		return err
	}
	fmt.Fprintf(out, "已写入 %s/%s.json\n后续请求会自动 refresh。把账号配成 oauth: xai、base_url: https://api.x.ai/v1。\n", storeDir, chosen.Name)
	return nil
}

func runAuthGoogle(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("auth google", flag.ContinueOnError)
	fs.SetOutput(out)
	account := fs.String("account", "", "oauth account name (required when more than one google oauth account exists)")
	configPath := fs.String("config", "", "config file (default: ./config.yaml, then /var/lib/prism/config.yaml)")
	dir := fs.String("dir", "", "token directory (default: config oauth_dir)")
	from := fs.String("from", "", "Antigravity CLI token file (default: ~/.gemini/antigravity-cli/antigravity-oauth-token)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, loadedPath, err := loadCLIConfig(*configPath)
	if err != nil {
		return err
	}
	chosen, err := pickOAuthAccount(cfg, loadedPath, "google", *account)
	if err != nil {
		return err
	}
	storeDir := *dir
	if storeDir == "" {
		storeDir = cfg.OAuthDir
	}
	src := strings.TrimSpace(*from)
	if src == "" {
		src = google.DefaultAgyTokenPath()
	}
	tok, err := google.LoadAgyToken(src)
	if err != nil {
		return fmt.Errorf("read antigravity token %s: %w", src, err)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if tok.Refresh == "" {
		return fmt.Errorf("antigravity token %s has no refresh_token", src)
	}
	refreshed, rerr := google.Refresh(ctx, google.Config{HTTP: client}, tok.Refresh)
	if rerr != nil {
		if tok.Access == "" || (!tok.ExpiresAt.IsZero() && time.Now().After(tok.ExpiresAt)) {
			return fmt.Errorf("google oauth refresh: %w", rerr)
		}
		fmt.Fprintf(out, "refresh 失败，沿用文件里未过期的 access token: %v\n", rerr)
	} else {
		tok = refreshed
	}
	if err := oauth.Save(storeDir, chosen.Name, "google", xai.Tokens{
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.ExpiresAt,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "已写入 %s/%s.json（从 %s 导入）\n后续请求会自动 refresh。把账号配成 oauth: google、base_url: https://cloudcode-pa.googleapis.com。\n", storeDir, chosen.Name, src)
	return nil
}
