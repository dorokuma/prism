package main

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReadWritePathList pins the item-3 path helper: relative entries are
// resolved against the unit working directory, filepath.Clean is applied,
// and exact duplicates collapse to one line.
func TestReadWritePathList(t *testing.T) {
	// The second tool shares the usage.db parent (/var/lib/prism) and the
	// third is an exact duplicate of the first; a relative tool path is
	// resolved against /var/lib/prism.
	tools := []detectedTool{
		{Name: "pi", Path: "/root/.pi/agent/models.json"},
		{Name: "pi2", Path: "/var/lib/prism/models2.json"},
		{Name: "pi3", Path: "/root/.pi/agent/other.json"},
		{Name: "pi4", Path: "tools/pi/models.json"},
	}
	got := readWritePathList(tools)

	want := []string{
		"/var/lib/prism/model_cache",
		"/var/lib/prism",
		"/root/.pi/agent",
		"/var/lib/prism/tools/pi",
	}
	if len(got) != len(want) {
		t.Fatalf("readWritePathList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (full list: %v)", i, got[i], want[i], got)
		}
	}
}

// TestReadWritePathList_CleanAndRelative: a messy relative path (with ".."
// and "." segments and no leading slash) is cleaned and absolutized against
// the working directory (the cache dir and usage.db parent are always
// present too).
func TestReadWritePathList_CleanAndRelative(t *testing.T) {
	tools := []detectedTool{
		{Name: "pi", Path: "foo/../.pi/agent/./models.json"},
	}
	got := readWritePathList(tools)
	want := []string{"/var/lib/prism/model_cache", "/var/lib/prism", "/var/lib/prism/.pi/agent"}
	if len(got) != len(want) {
		t.Fatalf("readWritePathList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (full list: %v)", i, got[i], want[i], got)
		}
	}
}

// TestReadWritePathList_DangerousPathsRejected pins the final-review safety
// gate: an abnormal tools configuration must NEVER widen ReadWritePaths to
// "/" (which would make the whole root filesystem writable and defeat
// ProtectSystem=strict). Absolute root paths, relative paths that escape
// into / or outside the working directory, and empty tool paths are all
// skipped; the mandatory cache/usage entries remain.
func TestReadWritePathList_DangerousPathsRejected(t *testing.T) {
	tools := []detectedTool{
		{Name: "root-abs", Path: "/"},                       // 绝对根路径 → filepath.Dir = / → 跳过
		{Name: "root-nested", Path: "/var/lib/prism/../.."}, // Clean 后 = / → 跳过
		{Name: "escape-to-root", Path: "../../../.."},       // join+Clean 后 = / → 跳过
		{Name: "escape-out", Path: "../escape"},             // join+Clean 后 = /var/lib（逃出工作目录）→ 跳过
		{Name: "empty", Path: ""},                           // 空路径 → 跳过
	}
	got := readWritePathList(tools)
	want := []string{modelCacheDir, filepath.Dir(unitUsageDBPath)}
	if len(got) != len(want) {
		t.Fatalf("readWritePathList(dangerous) = %v, want %v (no dangerous entry may survive)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (full list: %v)", i, got[i], want[i], got)
		}
	}
	for _, p := range got {
		if p == "/" {
			t.Errorf("readWritePathList must never contain the root path, got %v", got)
		}
	}
}

// TestReadWritePathList_MixedSafeAndDangerous: dangerous entries are
// skipped while safe entries (absolute tool dirs and in-working-dir
// relative dirs) survive — one broken path cannot fail or poison the rest.
func TestReadWritePathList_MixedSafeAndDangerous(t *testing.T) {
	tools := []detectedTool{
		{Name: "pi", Path: "/root/.pi/agent/models.json"},
		{Name: "root-abs", Path: "/"},
		{Name: "rel-ok", Path: "tools/pi/models.json"},
		{Name: "escape-out", Path: "../escape"},
	}
	got := readWritePathList(tools)
	want := []string{
		modelCacheDir,
		filepath.Dir(unitUsageDBPath),
		"/root/.pi/agent",
		"/var/lib/prism/tools/pi",
	}
	if len(got) != len(want) {
		t.Fatalf("readWritePathList(mixed) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (full list: %v)", i, got[i], want[i], got)
		}
	}
}

// TestFixToolConfigOwnership_ModeUpgrade: an existing tool config with a
// weak mode (0600) is raised to at least 0664 while owner/group stay
// untouched when they already match the target.
func TestFixToolConfigOwnership_ModeUpgrade(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixToolConfigOwnership(path, cur.Username, cur.Username); err != nil {
		t.Fatalf("fixToolConfigOwnership: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0664 {
		t.Errorf("mode after fix = %o, want 0664", perm)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != os.Geteuid() {
		t.Errorf("owner changed to uid %d, want %d (same-user chown must be a no-op)", st.Uid, os.Geteuid())
	}
}

// TestFixToolConfigOwnership_AlreadyOk: a file already at 0664 (or better)
// with matching owner/group is left untouched.
func TestFixToolConfigOwnership_AlreadyOk(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{}"), 0664); err != nil {
		t.Fatal(err)
	}
	if err := fixToolConfigOwnership(path, cur.Username, cur.Username); err != nil {
		t.Fatalf("fixToolConfigOwnership: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0664 {
		t.Errorf("mode changed to %o, want 0664 kept", perm)
	}
}

// TestFixToolConfigOwnership_MissingFileIsNotError: setup may detect the
// tool directory before the file exists (first sync creates it as the
// prism process); the fix must be a no-op, not an error.
func TestFixToolConfigOwnership_MissingFileIsNotError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := fixToolConfigOwnership(path, "prism", "pi-sync"); err != nil {
		t.Fatalf("fixToolConfigOwnership on missing file: %v", err)
	}
}

// TestFixToolConfigOwnership_UnknownUserFails: a missing service user is an
// explicit setup-blocking error (the whole ownership normalization is
// meaningless without the account the unit runs as).
func TestFixToolConfigOwnership_UnknownUserFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	err := fixToolConfigOwnership(path, "prism-no-such-user-xyz", "pi-sync")
	if err == nil {
		t.Fatal("fixToolConfigOwnership must fail for an unknown service user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want a user-not-found message", err)
	}
}

// TestFixToolConfigOwnership_UnknownGroupKept: when the target group does
// not exist the current group is kept (best-effort group normalization) and
// the fix still succeeds.
func TestFixToolConfigOwnership_UnknownGroupKept(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixToolConfigOwnership(path, cur.Username, "pi-sync-no-such-group-xyz"); err != nil {
		t.Fatalf("fixToolConfigOwnership with unknown group: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0664 {
		t.Errorf("mode after fix = %o, want 0664", perm)
	}
}

// TestFixToolConfigOwnership_ChownDenied: when the target owner differs and
// the process lacks the privilege, the fix must return an explicit error
// (never silently continue with the wrong owner). Skipped when running as
// root (root can chown to anyone).
func TestFixToolConfigOwnership_ChownDenied(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Uid == "0" {
		t.Skip("running as root: chown to another user succeeds, cannot test denial")
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// "root" is assumed to exist and differ from the test runner.
	err = fixToolConfigOwnership(path, "root", cur.Username)
	if err == nil {
		t.Fatal("fixToolConfigOwnership must fail when chown is denied")
	}
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("error = %q, want a chown failure message", err)
	}
}

// TestGenerateSystemdUnit_SecurityHardening pins item 3: the generated unit
// carries every security directive of scripts/prism.service.example (the
// shared hardening set), the read-only base path, and ReadWritePaths for the
// model cache, the usage.db parent and the tool config dirs.
func TestGenerateSystemdUnit_SecurityHardening(t *testing.T) {
	unit := generateSystemdUnit([]providerConfig{
		{Name: "opencode-go", Accounts: []accountConfig{{Name: "go-1", Key: "k"}}},
	}, []detectedTool{{Name: "pi", Path: "/root/.pi/agent/models.json"}})

	for _, d := range unitSecurityHardening {
		if !strings.Contains(unit, d+"\n") {
			t.Errorf("generated unit missing hardening directive %q", d)
		}
	}
	for _, want := range []string{
		"Type=simple",
		"TimeoutStopSec=45",
		"KillMode=mixed",
		"ReadOnlyPaths=" + strings.Join(unitReadOnlyPaths, " "),
		"ReadWritePaths=" + modelCacheDir,
		"ReadWritePaths=" + filepath.Dir(unitUsageDBPath),
		"ReadWritePaths=/root/.pi/agent",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("generated unit missing %q", want)
		}
	}
}

// TestSetupUnitSecurityMatchesExample pins the example↔generator sync: every
// directive in the example's [Service] section (except the documented
// placeholders that legitimately differ — credential names, description,
// network ordering) must appear in the generated unit with the SAME value.
// The full hardening set is checked both ways.
func TestSetupUnitSecurityMatchesExample(t *testing.T) {
	exampleBytes, err := os.ReadFile(filepath.Join("..", "..", "scripts", "prism.service.example"))
	if err != nil {
		t.Fatalf("read prism.service.example: %v", err)
	}
	example := string(exampleBytes)
	unit := generateSystemdUnit([]providerConfig{
		{Name: "opencode-go", Accounts: []accountConfig{{Name: "go-1", Key: "k"}}},
	}, []detectedTool{{Name: "pi", Path: "/root/.pi/agent/models.json"}})

	exampleDirs := parseUnitDirectives(t, example)
	unitDirs := parseUnitDirectives(t, unit)

	// The hardening set must be present in BOTH files (the generator emits
	// it verbatim; the example mirrors it).
	for _, d := range unitSecurityHardening {
		k, v, _ := strings.Cut(d, "=")
		if !exampleDirs[k][v] {
			t.Errorf("example missing hardening directive %q — keep scripts/prism.service.example in sync", d)
		}
		if !unitDirs[k][v] {
			t.Errorf("generated unit missing hardening directive %q", d)
		}
	}

	// Every remaining example directive must be present in the generated
	// unit with the SAME value (multi-valued keys compare as sets).
	// LoadCredential/Description/After/Wants are the documented placeholders
	// (credential names are deployment-specific and the example keeps a
	// minimal network ordering).
	skip := map[string]bool{"LoadCredential": true, "Description": true, "After": true, "Wants": true}
	for k, vs := range exampleDirs {
		if skip[k] {
			continue
		}
		for v := range vs {
			if !unitDirs[k][v] {
				t.Errorf("generated unit missing example directive %s=%s", k, v)
			}
		}
	}
}

// parseUnitDirectives extracts "Key=Value" directives from a unit file text
// as key → set-of-values (comments and blank lines ignored). Multi-valued
// keys (e.g. ReadWritePaths) keep every value.
func parseUnitDirectives(t *testing.T, text string) map[string]map[string]bool {
	t.Helper()
	out := make(map[string]map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue // section headers like [Service]
		}
		k = strings.TrimSpace(k)
		if out[k] == nil {
			out[k] = make(map[string]bool)
		}
		out[k][strings.TrimSpace(v)] = true
	}
	return out
}

// TestGenerateSystemdUnit_SystemdAnalyzeVerify validates the generated unit
// text with systemd-analyze verify (read-only; the binary is only executed
// on a temp file). Skipped when systemd-analyze is not installed. A unit
// that fails verification fails the test.
func TestGenerateSystemdUnit_SystemdAnalyzeVerify(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not installed")
	}
	unit := generateSystemdUnit([]providerConfig{
		{Name: "opencode-go", Accounts: []accountConfig{{Name: "go-1", Key: "k"}}},
	}, []detectedTool{{Name: "pi", Path: "/root/.pi/agent/models.json"}})

	path := filepath.Join(t.TempDir(), "prism.service")
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("systemd-analyze", "verify", "--man=no", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify failed: %v\n%s", err, out)
	}
}

// TestSetup_NonLoopbackListenRefused pins the setup safety gate: a
// non-loopback listen address is refused outright (before any config is
// generated) — setup only ever produces a local-loopback config, and the
// loopback classification is the exported config.IsLoopbackListen (localhost
// case-insensitive counts; empty host / 0.0.0.0 / :: and other addresses
// do not).
func TestSetup_NonLoopbackListenRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:18790", "[::]:18790", ":18790", "192.168.1.5:18790", "example.com:18790"} {
		err := validateSetupListen(addr)
		if err == nil {
			t.Errorf("validateSetupListen(%q) = nil, want a refusal", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:18790", "[::1]:18790", "localhost:18790", "LOCALHOST:18790", "LocalHost:18790"} {
		if err := validateSetupListen(addr); err != nil {
			t.Errorf("validateSetupListen(%q) = %v, want nil (loopback listen is accepted)", addr, err)
		}
	}
}

// TestSetup_ListenWithoutPortRejected pins the port-less listen gate: a
// bare host ("127.0.0.1") must be rejected with an explicit host:port
// error — never misreported as "not a loopback address" (127.0.0.1 IS
// loopback; the problem is the missing port, and the error must say so).
func TestSetup_ListenWithoutPortRejected(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "localhost", "::1", "0.0.0.0", "example.com"} {
		err := validateSetupListen(addr)
		if err == nil {
			t.Errorf("validateSetupListen(%q) = nil, want a host:port refusal", addr)
			continue
		}
		if !strings.Contains(err.Error(), "host:port") {
			t.Errorf("validateSetupListen(%q) error = %q, want it to say the listen must be host:port (missing port)", addr, err)
		}
	}
}

// TestSetup_PromptAccountsRejectsInvalidName pins the interactive reuse of
// config.ValidateAccountName: a name that LoadConfig would reject (e.g.
// with a space) fails the setup prompt with an explicit error instead of
// generating a config that can never be loaded.
func TestSetup_PromptAccountsRejectsInvalidName(t *testing.T) {
	// 1 account, invalid name, then a valid key (never reached).
	input := "1\nbad name\nsk-abc\n"
	reader := bufio.NewReader(strings.NewReader(input))
	_, err := promptAccounts(reader, "prov", newAccountNameRegistry())
	if err == nil {
		t.Fatal("promptAccounts must reject an account name with a space (same rule as LoadConfig)")
	}
	if !strings.Contains(err.Error(), "不合法") && !strings.Contains(err.Error(), "name") {
		t.Errorf("error = %q, want it to mention the invalid name", err)
	}
}

// TestSetup_PromptAccountsAcceptsValidName guards the happy path: valid
// names are accepted and the key prompt is consumed.
func TestSetup_PromptAccountsAcceptsValidName(t *testing.T) {
	input := "2\nacc-1\nsk-1\nacc_2\nsk-2\n"
	reader := bufio.NewReader(strings.NewReader(input))
	accounts, err := promptAccounts(reader, "prov", newAccountNameRegistry())
	if err != nil {
		t.Fatalf("promptAccounts with valid names: %v", err)
	}
	if len(accounts) != 2 || accounts[0].Name != "acc-1" || accounts[1].Name != "acc_2" {
		t.Errorf("accounts = %+v, want [acc-1 acc_2]", accounts)
	}
	if accounts[0].Key != "sk-1" || accounts[1].Key != "sk-2" {
		t.Errorf("keys = [%s %s], want [sk-1 sk-2]", accounts[0].Key, accounts[1].Key)
	}
}

// TestSetup_PromptAccountsRejectsCrossProviderDuplicate pins the global
// (cross-provider) duplicate-name gate: the same account name in two
// different providers must be rejected at the second prompt — LoadConfig
// rejects duplicate account names anywhere in the flattened account list,
// so a per-provider-only check would generate a config that can never
// load.
func TestSetup_PromptAccountsRejectsCrossProviderDuplicate(t *testing.T) {
	reg := newAccountNameRegistry()

	// Provider A: 1 account named "acc-1".
	readerA := bufio.NewReader(strings.NewReader("1\nacc-1\nsk-a\n"))
	if _, err := promptAccounts(readerA, "provA", reg); err != nil {
		t.Fatalf("first provider's account must be accepted: %v", err)
	}

	// Provider B: 1 account with the SAME name → must be rejected.
	readerB := bufio.NewReader(strings.NewReader("1\nacc-1\nsk-b\n"))
	_, err := promptAccounts(readerB, "provB", reg)
	if err == nil {
		t.Fatal("the same account name in a second provider must be rejected (account names are globally unique)")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Errorf("error = %q, want it to mention the duplicate name", err)
	}
}

// TestSetup_PromptAccountsRejectsFoldedCredentialCollision pins the
// cross-provider CredentialEnvName fold gate: "a-b" in one provider and
// "a_b" in another both fold to LB_KEY_A_B — LoadConfig rejects that
// collision anywhere in the flattened account list, so setup must reject
// it at the second prompt too (same conversion function, same rule).
func TestSetup_PromptAccountsRejectsFoldedCredentialCollision(t *testing.T) {
	reg := newAccountNameRegistry()

	readerA := bufio.NewReader(strings.NewReader("1\na-b\nsk-a\n"))
	if _, err := promptAccounts(readerA, "provA", reg); err != nil {
		t.Fatalf("first provider's account a-b must be accepted: %v", err)
	}

	// Second provider, same name: must be rejected (exact duplicate).
	readerDup := bufio.NewReader(strings.NewReader("1\na-b\nsk-b\n"))
	if _, err := promptAccounts(readerDup, "provB", reg); err == nil {
		t.Fatal("exact duplicate a-b in a second provider must be rejected")
	}

	// Second provider, FOLDED collision a_b → LB_KEY_A_B: must be rejected
	// with the credential-name message.
	readerB := bufio.NewReader(strings.NewReader("1\na_b\nsk-b\n"))
	_, err := promptAccounts(readerB, "provB", reg)
	if err == nil {
		t.Fatal("a_b in a second provider must be rejected (folds to LB_KEY_A_B like a-b)")
	}
	if !strings.Contains(err.Error(), "LB_KEY_A_B") {
		t.Errorf("error = %q, want it to name the folded credential LB_KEY_A_B", err)
	}
}

// TestSetup_AccountNameRegistryAllowsDistinctFoldNames guards the
// converse: names that fold to DIFFERENT credential names stay accepted
// across providers (a-b vs a-c → LB_KEY_A_B vs LB_KEY_A_C).
func TestSetup_AccountNameRegistryAllowsDistinctFoldNames(t *testing.T) {
	reg := newAccountNameRegistry()
	for _, tc := range []struct{ name, provider string }{
		{"a-b", "p1"},
		{"a-c", "p2"},
		{"a_b", "p3"}, // folds to LB_KEY_A_B — collides with a-b!
	} {
		err := reg.check(tc.name)
		if tc.name == "a_b" {
			if err == nil {
				t.Error("a_b after a-b must be rejected (both fold to LB_KEY_A_B)")
			}
			continue
		}
		if err != nil {
			t.Errorf("reg.check(%q) = %v, want nil (distinct folded credential name)", tc.name, err)
		}
	}
}

func TestBuiltinProviders_ClinepassIsFourthAndMenuDerived(t *testing.T) {
	if len(builtinProviders) != 4 {
		t.Fatalf("len(builtinProviders) = %d, want 4", len(builtinProviders))
	}
	want := []string{"opencode-go", "opencode-zen", "ollama-cloud", "clinepass"}
	for i, name := range want {
		if builtinProviders[i].Name != name {
			t.Errorf("builtinProviders[%d].Name = %q, want %q", i, builtinProviders[i].Name, name)
		}
	}
	if got := builtinProviders[3].BaseURL; got != "https://api.cline.bot/api/v1" {
		t.Errorf("clinepass BaseURL = %q, want https://api.cline.bot/api/v1", got)
	}
	if got := customProviderIndex(); got != 5 {
		t.Errorf("customProviderIndex() = %d, want 5 (one past the last builtin)", got)
	}
	if got := allBuiltinIndices(); got != "1,2,3,4" {
		t.Errorf("allBuiltinIndices() = %q, want 1,2,3,4", got)
	}
}

func TestGenerateConfigYAML_DoesNotPrefillOptionalFlags(t *testing.T) {
	got := generateConfigYAML("127.0.0.1:18790", []providerConfig{
		{Name: "clinepass", BaseURL: "https://api.cline.bot/api/v1", Accounts: []accountConfig{{Name: "clinepass-1"}}},
		{Name: "opencode-go", BaseURL: "https://opencode.ai/zen/go/v1", Accounts: []accountConfig{{Name: "opencode-go-1"}}},
	}, nil)
	if strings.Contains(got, "dsml_guard") || strings.Contains(got, "skip_pi_sync") || strings.Contains(got, "probe_path") {
		t.Fatalf("setup YAML must not prefill dsml_guard/skip_pi_sync/probe_path:\n%s", got)
	}
	if !strings.Contains(got, "  clinepass:\n    accounts:\n      - name: clinepass-1\n        base_url: https://api.cline.bot/api/v1\n") {
		t.Fatalf("generated YAML missing clinepass name+base_url block:\n%s", got)
	}
}
