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
	"github.com/dorokuma/prism/internal/oauth/xai"
)

func runAuth(args []string) error {
	return runAuthWith(args, os.Stdout)
}

func runAuthWith(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, `prism auth xai — SuperGrok / xAI device-code login

Usage:
  prism auth xai [--account NAME] [--config PATH] [--dir PATH]

Opens the RFC 8628 device flow against auth.x.ai (same public Grok-CLI
client Pi uses). After you approve in a browser, tokens are stored under
oauth_dir (default /var/lib/prism/oauth/<account>.json) and refreshed on
the request path. YAML must not contain the access token.

--config defaults to ./config.yaml, then /var/lib/prism/config.yaml.

A running prism process picks up a new login from the file without restart.
`)
		return nil
	}
	if args[0] != "xai" {
		return fmt.Errorf("unknown auth provider %q (supported: xai)", args[0])
	}
	fs := flag.NewFlagSet("auth xai", flag.ContinueOnError)
	fs.SetOutput(out)
	account := fs.String("account", "", "oauth account name (required when more than one xai oauth account exists)")
	configPath := fs.String("config", "", "config file (default: ./config.yaml, then /var/lib/prism/config.yaml)")
	dir := fs.String("dir", "", "token directory (default: config oauth_dir)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, loadedPath, err := loadCLIConfig(*configPath)
	if err != nil {
		return err
	}
	var oauthAccounts []config.AccountConfig
	for _, a := range cfg.Accounts {
		if strings.TrimSpace(a.OAuth) == "xai" {
			oauthAccounts = append(oauthAccounts, a)
		}
	}
	if len(oauthAccounts) == 0 {
		return fmt.Errorf("no account with oauth: xai in %s; add an account (no key) with oauth: xai and base_url https://api.x.ai/v1", loadedPath)
	}
	chosen := oauthAccounts[0]
	if *account != "" {
		found := false
		for _, a := range oauthAccounts {
			if a.Name == *account {
				chosen = a
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("account %q is not an oauth: xai account", *account)
		}
	} else if len(oauthAccounts) > 1 {
		var names []string
		for _, a := range oauthAccounts {
			names = append(names, a.Name)
		}
		return fmt.Errorf("multiple oauth: xai accounts (%s); pass --account", strings.Join(names, ", "))
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
