package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAuthHelp(t *testing.T) {
	var buf bytes.Buffer
	if err := runAuthWith(nil, &buf); err != nil {
		t.Fatal(err)
	}
	help := buf.String()
	if !strings.Contains(help, "prism auth xai") {
		t.Fatalf("help = %s", help)
	}
	if !strings.Contains(help, "prism auth google") {
		t.Fatalf("help missing google: %s", help)
	}
	if !strings.Contains(help, "/var/lib/prism/config.yaml") {
		t.Fatalf("help missing fallback path: %s", help)
	}
}

const authTestStaticYAML = `
listen: 127.0.0.1:18790
accounts:
  - name: a
    key: k
    provider: p
    base_url: https://api.example.com
`

func withCLIConfigLookup(t *testing.T, cwd, fallback string) {
	t.Helper()
	oldDir, oldFB := usageConfigDir, usageConfigFallbackPath
	usageConfigDir = func() string { return cwd }
	usageConfigFallbackPath = fallback
	t.Cleanup(func() {
		usageConfigDir = oldDir
		usageConfigFallbackPath = oldFB
	})
}

func TestRunAuthFallbackConfig(t *testing.T) {
	cwd := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(fallback, []byte(authTestStaticYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	withCLIConfigLookup(t, cwd, fallback)
	err := runAuthWith([]string{"xai"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no account with oauth: xai") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), fallback) {
		t.Fatalf("err = %v, want fallback path %s", err, fallback)
	}
}

func TestRunAuthMissingConfig(t *testing.T) {
	withCLIConfigLookup(t, t.TempDir(), filepath.Join(t.TempDir(), "no-such-config.yaml"))
	err := runAuthWith([]string{"xai"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "无法加载配置") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAuthCwdWinsOverFallback(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte(authTestStaticYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(fallback, []byte(`
listen: 127.0.0.1:18790
accounts:
  - name: a1
    oauth: xai
    provider: xai
    base_url: https://api.x.ai/v1
  - name: a2
    oauth: xai
    provider: xai
    base_url: https://api.x.ai/v1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	withCLIConfigLookup(t, cwd, fallback)
	err := runAuthWith([]string{"xai"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no account with oauth: xai") {
		t.Fatalf("err = %v", err)
	}
	cwdCfg := filepath.Join(cwd, "config.yaml")
	if !strings.Contains(err.Error(), cwdCfg) {
		t.Fatalf("err = %v, want cwd config %s", err, cwdCfg)
	}
	if strings.Contains(err.Error(), "--account") {
		t.Fatalf("fallback oauth config was used: %v", err)
	}
}

func TestRunAuthExplicitConfigSkipsFallback(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(fallback, []byte(authTestStaticYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	withCLIConfigLookup(t, t.TempDir(), fallback)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	err := runAuthWith([]string{"xai", "--config", missing}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "无法加载配置") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want explicit path %s", err, missing)
	}
}

func TestRunAuthUnknownProvider(t *testing.T) {
	err := runAuthWith([]string{"openai"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown auth provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAuthNoOAuthAccount(t *testing.T) {
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(`
listen: 127.0.0.1:18790
accounts:
  - name: a
    key: k
    provider: p
    base_url: https://api.example.com
`)
	f.Close()
	err = runAuthWith([]string{"xai", "--config", f.Name()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no account with oauth: xai") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAuthMultipleAccountsNeedFlag(t *testing.T) {
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(`
listen: 127.0.0.1:18790
accounts:
  - name: a1
    oauth: xai
    provider: xai
    base_url: https://api.x.ai/v1
  - name: a2
    oauth: xai
    provider: xai
    base_url: https://api.x.ai/v1
`)
	f.Close()
	err = runAuthWith([]string{"xai", "--config", f.Name()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--account") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAuthGoogleNoAccount(t *testing.T) {
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(authTestStaticYAML)
	f.Close()
	err = runAuthWith([]string{"google", "--config", f.Name()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no account with oauth: google") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAuthGoogleMissingFrom(t *testing.T) {
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(`
listen: 127.0.0.1:18790
accounts:
  - name: Gemini
    oauth: google
    provider: gemini
    base_url: https://cloudcode-pa.googleapis.com
`)
	f.Close()
	missing := filepath.Join(t.TempDir(), "no-agy-token")
	err = runAuthWith([]string{"google", "--config", f.Name(), "--from", missing}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "read antigravity token") {
		t.Fatalf("err = %v", err)
	}
}
