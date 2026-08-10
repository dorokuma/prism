package config

import (
	"os"
	"strings"
	"testing"
)

// --- api_keys / auth_token backward-compat expansion ---

// TestLoadConfig_AuthTokenOnlyExpandsToDefaultKey: legacy auth_token alone
// must still authenticate — it expands into a single {name: default} key.
func TestLoadConfig_AuthTokenOnlyExpandsToDefaultKey(t *testing.T) {
	content := `
auth_token: legacy-token-123
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if len(cfg.APIKeys) != 1 {
		t.Fatalf("APIKeys len = %d, want 1", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "default" {
		t.Errorf("APIKeys[0].Name = %q, want default", cfg.APIKeys[0].Name)
	}
	if cfg.APIKeys[0].Token != "legacy-token-123" {
		t.Errorf("APIKeys[0].Token = %q, want legacy-token-123", cfg.APIKeys[0].Token)
	}
}

// TestLoadConfig_PrismAuthTokenEnvExpandsToDefaultKey: PRISM_AUTH_TOKEN env
// var expands into the same single default key.
func TestLoadConfig_PrismAuthTokenEnvExpandsToDefaultKey(t *testing.T) {
	t.Setenv("PRISM_AUTH_TOKEN", "env-token-456")
	content := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if len(cfg.APIKeys) != 1 {
		t.Fatalf("APIKeys len = %d, want 1", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "default" || cfg.APIKeys[0].Token != "env-token-456" {
		t.Errorf("APIKeys[0] = %+v, want {default env-token-456}", cfg.APIKeys[0])
	}
}

// TestLoadConfig_APIKeysOnlyPreserved: api_keys alone is used as-is, with no
// default entry injected.
func TestLoadConfig_APIKeysOnlyPreserved(t *testing.T) {
	content := `
api_keys:
  - name: ci-bot
    token: sk-ci-111
  - name: human
    token: sk-human-222
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("APIKeys len = %d, want 2", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "ci-bot" || cfg.APIKeys[0].Token != "sk-ci-111" {
		t.Errorf("APIKeys[0] = %+v, want ci-bot/sk-ci-111", cfg.APIKeys[0])
	}
	if cfg.APIKeys[1].Name != "human" || cfg.APIKeys[1].Token != "sk-human-222" {
		t.Errorf("APIKeys[1] = %+v, want human/sk-human-222", cfg.APIKeys[1])
	}
	for _, k := range cfg.APIKeys {
		if k.Name == "default" {
			t.Errorf("unexpected default key injected when api_keys alone is set: %+v", k)
		}
	}
}

// TestLoadConfig_BothConfiguredMergesDefault: auth_token + api_keys with
// distinct tokens merge — api_keys entries keep their names, auth_token is
// appended as {name: default}.
func TestLoadConfig_BothConfiguredMergesDefault(t *testing.T) {
	content := `
auth_token: legacy-token-999
api_keys:
  - name: ci-bot
    token: sk-ci-111
  - name: human
    token: sk-human-222
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if len(cfg.APIKeys) != 3 {
		t.Fatalf("APIKeys len = %d, want 3 (2 api_keys + default)", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "ci-bot" || cfg.APIKeys[1].Name != "human" {
		t.Errorf("api_keys names = %q,%q, want ci-bot,human (order preserved)", cfg.APIKeys[0].Name, cfg.APIKeys[1].Name)
	}
	last := cfg.APIKeys[2]
	if last.Name != "default" || last.Token != "legacy-token-999" {
		t.Errorf("appended key = %+v, want {default legacy-token-999}", last)
	}
}

// TestLoadConfig_BothConfiguredDedupAuthToken: when auth_token's value
// duplicates an api_keys token, the api_keys entry's name wins and NO extra
// default entry is injected.
func TestLoadConfig_BothConfiguredDedupAuthToken(t *testing.T) {
	content := `
auth_token: sk-human-222
api_keys:
  - name: ci-bot
    token: sk-ci-111
  - name: human
    token: sk-human-222
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("APIKeys len = %d, want 2 (duplicate auth_token must NOT add a default entry)", len(cfg.APIKeys))
	}
	for _, k := range cfg.APIKeys {
		if k.Name == "default" {
			t.Errorf("unexpected default key injected for duplicate auth_token: %+v", k)
		}
	}
	// The api_keys entry owning the duplicate token keeps its name.
	for _, k := range cfg.APIKeys {
		if k.Token == "sk-human-222" && k.Name != "human" {
			t.Errorf("token sk-human-222 owned by %q, want human", k.Name)
		}
	}
}

// TestLoadConfig_DuplicateAPIKeyTokenRejected: two api_keys entries sharing
// one token must be rejected at load time — the auth loop keeps the LAST
// matching name, so a duplicate would silently attribute every request (and
// every usage row) to the wrong key. The error names the two conflicting
// keys but must NEVER echo the token itself.
func TestLoadConfig_DuplicateAPIKeyTokenRejected(t *testing.T) {
	token := "sk-shared-secret-777"
	content := `
api_keys:
  - name: ci-bot
    token: ` + token + `
  - name: human
    token: ` + token + `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = LoadConfig(f.Name())
	if err == nil {
		t.Fatal("expected error for duplicate api_keys token, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ci-bot") || !strings.Contains(msg, "human") {
		t.Errorf("error must name the two conflicting keys, got: %q", msg)
	}
	if strings.Contains(msg, token) {
		t.Errorf("error must never echo the token itself, got: %q", msg)
	}
}

// TestLoadConfig_DuplicateTokenStillAllowsAuthTokenDedup: the known-good
// backward-compat case — auth_token equal to an api_keys token — must NOT be
// rejected by the duplicate check (the expansion dedups it before the check
// runs).
func TestLoadConfig_DuplicateTokenStillAllowsAuthTokenDedup(t *testing.T) {
	content := `
auth_token: sk-ci-111
api_keys:
  - name: ci-bot
    token: sk-ci-111
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content) // must load without error
	if len(cfg.APIKeys) != 1 {
		t.Fatalf("APIKeys len = %d, want 1 (auth_token deduped against api_keys)", len(cfg.APIKeys))
	}
}

// TestLoadConfig_NonLoopbackRequiresCredential: non-loopback listen passes
// when any credential exists (post-expansion) and fails only when the
// expanded APIKeys set is empty.
func TestLoadConfig_NonLoopbackRequiresCredential(t *testing.T) {
	t.Run("non-loopback with api_keys passes", func(t *testing.T) {
		content := `
listen: 0.0.0.0:8080
api_keys:
  - name: ci-bot
    token: sk-ci-111
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if len(cfg.APIKeys) != 1 {
			t.Fatalf("APIKeys len = %d, want 1", len(cfg.APIKeys))
		}
	})

	t.Run("non-loopback with PRISM_AUTH_TOKEN env passes", func(t *testing.T) {
		t.Setenv("PRISM_AUTH_TOKEN", "env-token-456")
		content := `
listen: 0.0.0.0:8080
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Name != "default" {
			t.Fatalf("APIKeys = %+v, want single default key from env", cfg.APIKeys)
		}
	})

	t.Run("non-loopback with api_keys + auth_token dup passes", func(t *testing.T) {
		content := `
listen: 0.0.0.0:8080
auth_token: sk-ci-111
api_keys:
  - name: ci-bot
    token: sk-ci-111
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if len(cfg.APIKeys) != 1 {
			t.Fatalf("APIKeys len = %d, want 1 (dedup)", len(cfg.APIKeys))
		}
	})

	t.Run("non-loopback without any credential rejected", func(t *testing.T) {
		content := `
listen: 0.0.0.0:8080
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, err = LoadConfig(f.Name())
		if err == nil {
			t.Fatal("expected error for non-loopback listen without any credential, got nil")
		}
		if !strings.Contains(err.Error(), "auth_token") {
			t.Errorf("error = %q, want it to mention auth_token", err.Error())
		}
	})
}

// TestReloadConfig_AuthKeysHotReloadNoWarning: auth_token / api_keys changes
// must be hot-reloadable — no restart-required warning, and the new
// credential set must be visible through the atomic holder.
func TestReloadConfig_AuthKeysHotReloadNoWarning(t *testing.T) {
	t.Run("auth_token change is hot-reloadable without restart warning", func(t *testing.T) {
		content1 := `
listen: 127.0.0.1:8080
auth_token: token-v1
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content1)); err != nil {
			t.Fatal(err)
		}
		f.Close()

		cfg1, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		holder := NewConfigHolder(cfg1)

		content2 := `
listen: 127.0.0.1:8080
auth_token: token-v2
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
			t.Fatal(err)
		}
		warnings, err := ReloadConfig(holder, f.Name())
		if err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "auth") && strings.Contains(w, "restart") {
				t.Errorf("auth change must not produce a restart warning, got: %q", w)
			}
		}
		got := holder.Load().APIKeys
		if len(got) != 1 || got[0].Token != "token-v2" {
			t.Errorf("after reload APIKeys = %+v, want [{default token-v2}]", got)
		}
	})

	t.Run("api_keys change is hot-reloadable without restart warning", func(t *testing.T) {
		content1 := `
listen: 127.0.0.1:8080
api_keys:
  - name: ci-bot
    token: sk-ci-111
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content1)); err != nil {
			t.Fatal(err)
		}
		f.Close()

		cfg1, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		holder := NewConfigHolder(cfg1)

		content2 := `
listen: 127.0.0.1:8080
api_keys:
  - name: ci-bot
    token: sk-ci-222
  - name: ops
    token: sk-ops-333
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
			t.Fatal(err)
		}
		warnings, err := ReloadConfig(holder, f.Name())
		if err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "auth") && strings.Contains(w, "restart") {
				t.Errorf("api_keys change must not produce a restart warning, got: %q", w)
			}
		}
		got := holder.Load().APIKeys
		if len(got) != 2 || got[0].Token != "sk-ci-222" || got[1].Name != "ops" {
			t.Errorf("after reload APIKeys = %+v, want updated set", got)
		}
	})
}

// TestLoadConfig_ApiKeyTokenTooLongRejected: the constant-time auth
// comparison pads tokens to a fixed length (middleware.authPadLen = 256); a
// longer token would be truncated and a prefix match could pass. Such
// configurations must be rejected at load time, never silently weakened.
func TestLoadConfig_ApiKeyTokenTooLongRejected(t *testing.T) {
	long := strings.Repeat("a", 300)
	content := `
api_keys:
  - name: ci-bot
    token: "` + long + `"
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := LoadConfig(f.Name()); err == nil {
		t.Fatal("expected error for api key token longer than the auth pad, got nil")
	} else if !strings.Contains(err.Error(), "longer than 256") {
		t.Errorf("error = %q, want it to mention the 256-byte pad limit", err.Error())
	}

	// Same guard applies to the legacy auth_token expansion path.
	content2 := `
auth_token: "` + long + `"
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(f.Name()); err == nil {
		t.Fatal("expected error for auth_token longer than the auth pad, got nil")
	}
}

// loadCfgString writes content to a temp file and loads it via LoadConfig.
func loadCfgString(t *testing.T, content string) *Config {
	t.Helper()
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}
