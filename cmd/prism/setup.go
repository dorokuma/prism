package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dorokuma/prism/internal/config"
)

// builtinProviders lists the providers that prism ships with.
var builtinProviders = []struct {
	Name    string
	Label   string
	BaseURL string
}{
	{Name: "opencode-go", Label: "OpenCode Go", BaseURL: "https://opencode.ai/zen/go/v1"},
	{Name: "opencode-zen", Label: "OpenCode Zen", BaseURL: "https://opencode.ai/zen/v1"},
	{Name: "ollama-cloud", Label: "Ollama", BaseURL: "https://ollama.com/v1"},
}

// builtinTools maps tool names to their config file path relative to $HOME.
var builtinTools = map[string]string{
	"pi": ".pi/agent/models.json",
}

type providerConfig struct {
	Name     string
	BaseURL  string
	Accounts []accountConfig
}

type accountConfig struct {
	Name string
	Key  string
}

type detectedTool struct {
	Name string
	Path string
}

// serviceUserName is the account the generated unit runs prism as; tool
// config files (models.json) are normalized to this owner during setup so
// the service can rewrite them without CAP_CHOWN (the unit runs with an
// empty CapabilityBoundingSet and prism is deliberately never granted
// CAP_CHOWN — see scripts/prism.service.example).
const serviceUserName = "prism"

// serviceGroupName is the group tool config files are normalized to when it
// exists (prism is a member, so group-write lets it atomically replace the
// file without owning it). When the group is absent the current group is
// kept — the owner normalization alone already makes the atomic replace
// work (the temp-file chown becomes a uid no-op).
const serviceGroupName = "pi-sync"

// minToolConfigMode is the minimum mode for tool config files: group must
// be able to read+write (prism writes through the group) and others must be
// able to read (pi reads the file as the invoking user). Missing bits are
// added, existing bits (e.g. extra read bits) are never removed.
const minToolConfigMode = os.FileMode(0664)

// fixToolConfigOwnership normalizes an existing tool config file so the
// prism service user can atomically rewrite it without CAP_CHOWN:
//   - owner → wantUser (serviceUserName): syncPIModelsJSON preserves the
//     deployed owner via chown on the temp file; when the temp file (created
//     by the prism process) is chowned to its OWN uid the chown is a uid
//     no-op and needs no privilege, so a prism-owned models.json syncs
//     cleanly under the unit's empty CapabilityBoundingSet, while a
//     root-owned one would abort with EPERM.
//   - group → wantGroup IF the group exists (otherwise the current group is
//     kept).
//   - mode → at least minToolConfigMode (missing bits added, nothing
//     removed).
//
// A non-existent file is NOT an error: the first sync creates it as a temp
// file owned by the prism process, so there is nothing to normalize.
// chown/chmod only run when something actually differs; failures return an
// explicit error and setup aborts (the file is left as-is — the failed
// syscall is atomic). Setup runs as root (it writes /etc, /var/lib/prism
// and the unit), so chown succeeds; it grants no capability anywhere.
func fixToolConfigOwnership(path, wantUser, wantGroup string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: owner info unavailable", path)
	}
	curUID, curGID := int(st.Uid), int(st.Gid)

	u, err := user.Lookup(wantUser)
	if err != nil {
		return fmt.Errorf("service user %q not found (cannot normalize %s owner): %w", wantUser, path, err)
	}
	wantUID, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("service user %q uid %q not numeric: %w", wantUser, u.Uid, err)
	}
	wantGID := curGID // group normalization is best-effort: keep current group when wantGroup is absent
	if g, err := user.LookupGroup(wantGroup); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			wantGID = gid
		}
	}

	if curUID != wantUID || curGID != wantGID {
		if err := os.Chown(path, wantUID, wantGID); err != nil {
			return fmt.Errorf("chown %s to %s:%s (uid/gid %d/%d): %w", path, wantUser, wantGroup, wantUID, wantGID, err)
		}
	}

	perm := fi.Mode().Perm()
	if perm&minToolConfigMode != minToolConfigMode {
		newMode := perm | minToolConfigMode
		if err := os.Chmod(path, newMode); err != nil {
			return fmt.Errorf("chmod %s to %04o: %w", path, newMode, err)
		}
	}
	return nil
}

func runSetup() error {
	reader := bufio.NewReader(os.Stdin)
	home := os.Getenv("HOME")
	if home == "" {
		return fmt.Errorf("HOME environment variable not set")
	}

	fmt.Println()
	fmt.Println("=== 服务配置 ===")
	fmt.Println()
	listen := promptDefault(reader, "监听地址", "127.0.0.1:18790")
	fmt.Printf("  → 使用 %s\n\n", listen)

	// 非回环监听直接拒绝生成配置：setup 生成的配置没有 TLS、没有 api_keys，
	// 把服务暴露在非回环地址上等于裸奔。回环判断复用 config.IsLoopbackListen
	// （localhost 大小写不敏感视为回环；空 host、0.0.0.0、:: 及其他地址
	// 仍非回环），与 LoadConfig 的 TLS/鉴权检查用同一实现。
	if err := validateSetupListen(listen); err != nil {
		return err
	}

	fmt.Println("=== 上游选择 ===")
	fmt.Println()
	for i, p := range builtinProviders {
		fmt.Printf("  %d. %-15s — %s\n", i+1, p.Label, p.BaseURL)
	}
	fmt.Println("  4. 自定义")
	fmt.Println()
	choice := prompt(reader, "选编号，逗号分隔（如 1,3），或 all：")
	if choice == "all" {
		choice = "1,2,3"
	}
	selected := parseIndices(choice)
	if len(selected) == 0 && choice != "" {
		// "4" alone or custom
	}

	var providers []providerConfig

	// 账号名全局注册表：跨 provider 拒绝重名与 LB_KEY_* 折叠冲突（与
	// LoadConfig 同一规则），保证生成的配置一定能被加载。
	registry := newAccountNameRegistry()

	for _, idx := range selected {
		if idx >= 1 && idx <= 3 {
			bp := builtinProviders[idx-1]
			fmt.Printf("\n=== %s ===\n", bp.Label)
			accts, err := promptAccounts(reader, bp.Name, registry)
			if err != nil {
				return err
			}
			providers = append(providers, providerConfig{
				Name:     bp.Name,
				BaseURL:  bp.BaseURL,
				Accounts: accts,
			})
		}
	}

	// Check if custom was selected
	if choice == "4" || containsIndex(selected, 4) {
		fmt.Println("\n=== 自定义 ===")
		name := prompt(reader, "名称（用于 provider 标识）:")
		baseURL := prompt(reader, "接口地址:")
		accts, err := promptAccounts(reader, name, registry)
		if err != nil {
			return err
		}
		providers = append(providers, providerConfig{
			Name:     name,
			BaseURL:  baseURL,
			Accounts: accts,
		})
	}

	if len(providers) == 0 {
		return fmt.Errorf("至少选择一个上游")
	}

	// Tool detection
	fmt.Println("\n=== 工具检测 ===")
	fmt.Printf("  扫描 %s ...\n", home)
	var tools []detectedTool
	for toolName, relPath := range builtinTools {
		fullPath := filepath.Join(home, relPath)
		parentDir := filepath.Dir(fullPath)
		if _, err := os.Stat(parentDir); err == nil {
			fmt.Printf("    %-12s ✓  %s\n", toolName+".", fullPath)
			tools = append(tools, detectedTool{Name: toolName, Path: fullPath})
			// 安装期所有权归一化：已存在的 models.json 改为 prism 服务用户
			// 所有、pi-sync 组（组存在时）、mode ≥ 0664，使服务在无
			// CAP_CHOWN 下也能原子重写；失败则中止 setup（明确报错，
			// 不静默降级）。文件不存在时跳过（首次同步由 prism 进程
			// 创建，owner 天然正确）。
			if err := fixToolConfigOwnership(fullPath, serviceUserName, serviceGroupName); err != nil {
				return fmt.Errorf("归一化 %s 所有权/权限: %w", fullPath, err)
			}
			fmt.Printf("      owner=%s group=%s mode≥0664（未授予 CAP_CHOWN）\n", serviceUserName, serviceGroupName)
		} else {
			fmt.Printf("    %-12s ✗  （未检测到）\n", toolName+".")
		}
	}

	if len(tools) == 0 {
		return fmt.Errorf("未检测到任何支持的工具——至少需要一个才能继续")
	}

	// Preview
	fmt.Println("\n=== 预览 ===")
	fmt.Println()
	credstoreDir := "/etc/credstore/prism"
	for _, pv := range providers {
		for _, acct := range pv.Accounts {
			fmt.Printf("  %s/%s  新\n", credstoreDir, config.CredentialEnvName(acct.Name))
		}
	}
	fmt.Println("  /var/lib/prism/config.yaml                    新")
	fmt.Println("  /var/lib/prism/model_cache/                   新目录")
	fmt.Println("  /etc/systemd/system/prism.service            新")
	fmt.Println()
	fmt.Println("  提示：工具 models.json（如 ~/.pi/agent/models.json）由 setup 归一化为")
	fmt.Println("  prism 服务用户所有、pi-sync 组（组存在时）、mode ≥ 0664；服务在")
	fmt.Println("  无 CAP_CHOWN（unit 的 CapabilityBoundingSet 为空）下也能原子重写。")
	fmt.Println("  文件尚不存在时由 prism 首次同步创建（owner 天然为 prism）。")
	fmt.Println()
	confirm := promptDefault(reader, "确认？", "Y")
	if !strings.HasPrefix(strings.ToUpper(confirm), "Y") {
		fmt.Println("已取消。")
		return nil
	}

	// Generate credstore files
	fmt.Println("\n生成中...")
	if err := os.MkdirAll(credstoreDir, 0700); err != nil {
		return fmt.Errorf("创建 credstore 目录: %w", err)
	}
	for _, pv := range providers {
		for _, acct := range pv.Accounts {
			envName := config.CredentialEnvName(acct.Name)
			credPath := filepath.Join(credstoreDir, envName)
			if err := os.WriteFile(credPath, []byte(acct.Key+"\n"), 0600); err != nil {
				return fmt.Errorf("写入 %s: %w", credPath, err)
			}
			fmt.Printf("  ✓ %s\n", credPath)
		}
	}

	// Generate config.yaml
	if err := os.MkdirAll("/var/lib/prism", 0755); err != nil {
		return fmt.Errorf("创建 prism 目录: %w", err)
	}
	if err := os.MkdirAll("/var/lib/prism/model_cache", 0755); err != nil {
		return fmt.Errorf("创建 model_cache 目录: %w", err)
	}
	configPath := "/var/lib/prism/config.yaml"
	configYAML := generateConfigYAML(listen, providers, tools)
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		return fmt.Errorf("写入 %s: %w", configPath, err)
	}
	fmt.Printf("  ✓ %s\n", configPath)

	// Generate systemd unit
	unitPath := "/etc/systemd/system/prism.service"
	unit := generateSystemdUnit(providers, tools)
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("写入 %s: %w", unitPath, err)
	}
	fmt.Printf("  ✓ %s\n", unitPath)

	fmt.Println("\n✓ 完成。启动服务：")
	fmt.Println("  systemctl daemon-reload")
	fmt.Println("  systemctl enable --now prism")
	return nil
}

// validateSetupListen is the setup-side listen gate: only loopback
// addresses (via config.IsLoopbackListen — the same implementation
// LoadConfig uses for its TLS/auth gates) may be generated into a config.
// A listen without a port (e.g. a bare "127.0.0.1") is rejected with an
// explicit host:port error BEFORE the loopback classification, so it is
// never misreported as "not a loopback address".
func validateSetupListen(listen string) error {
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("监听地址 %q 不是 host:port 形式（如 127.0.0.1:18790 或 [::1]:18790），缺少端口: %v", listen, err)
	}
	if !config.IsLoopbackListen(listen) {
		return fmt.Errorf("拒绝为非回环监听地址 %q 生成配置：setup 只生成本地回环配置（127.0.0.1 / ::1 / localhost）；如需对外提供服务，请手动配置 TLS 与 api_keys", listen)
	}
	return nil
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Printf("%s ", label)
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(input)
}

func promptDefault(reader *bufio.Reader, label string, defaultVal string) string {
	fmt.Printf("%s [%s]: ", label, defaultVal)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func parseIndices(s string) []int {
	parts := strings.Split(s, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && n >= 1 {
			out = append(out, n)
		}
	}
	return out
}

func containsIndex(arr []int, target int) bool {
	for _, v := range arr {
		if v == target {
			return true
		}
	}
	return false
}

// accountNameRegistry tracks the account names already entered across ALL
// providers during one setup run. It mirrors the two account-name gates of
// LoadConfig — exact duplicates and folded LB_KEY_* credential collisions
// ("a-b" vs "a_b") are rejected GLOBALLY, not per provider — so the
// generated config is guaranteed to load: LoadConfig rejects a duplicate
// name or a folded credential collision anywhere in the flattened account
// list.
type accountNameRegistry struct {
	names       map[string]bool
	foldedCreds map[string]string
}

func newAccountNameRegistry() *accountNameRegistry {
	return &accountNameRegistry{
		names:       make(map[string]bool),
		foldedCreds: make(map[string]string),
	}
}

// check validates a new account name against every name entered so far
// (across providers) and records it on success. The credential name uses
// config.CredentialEnvName — the same public conversion LoadConfig uses —
// so the setup-side conflict rule can never drift from the load-side rule.
func (r *accountNameRegistry) check(name string) error {
	if r.names[name] {
		return fmt.Errorf("账号名 %q 重复：账号名必须在所有 provider 间唯一（它是审计账号标签、expvar 键和 LB_KEY_* 凭据名）", name)
	}
	envName := config.CredentialEnvName(name)
	if prev, ok := r.foldedCreds[envName]; ok {
		return fmt.Errorf("账号名 %q 与 %q 冲突：凭据名 %s 相同（连字符折叠为下划线）；请改名使 LB_KEY_* 名称不同", prev, name, envName)
	}
	r.names[name] = true
	r.foldedCreds[envName] = name
	return nil
}

func promptAccounts(reader *bufio.Reader, providerName string, registry *accountNameRegistry) ([]accountConfig, error) {
	nStr := promptDefault(reader, "  几个账号？", "1")
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 1 {
		n = 1
	}
	var accounts []accountConfig
	for i := 1; i <= n; i++ {
		defaultName := fmt.Sprintf("%s-%d", providerName, i)
		name := promptDefault(reader, fmt.Sprintf("    账号 %d 名称", i), defaultName)
		// 交互校验复用 config.ValidateAccountName（与 LoadConfig 同一规则）：
		// 生成阶段就拒绝非法账号名（fail fast），保证生成的配置一定能被加载。
		if err := config.ValidateAccountName(name); err != nil {
			return nil, fmt.Errorf("账号名 %q 不合法（账号名仅允许 ASCII 字母/数字/_/-，且必须以字母或数字开头）: %w", name, err)
		}
		// 跨 provider 的全局唯一性与凭据名折叠冲突（与 LoadConfig 同一规则，
		// 凭据名转换复用 config.CredentialEnvName）：
		// “opencode-go” 的默认名与自定义 provider 同名时，或某 provider 的
		// “a-b” 与另一 provider 的 “a_b” 折叠到同一 LB_KEY_* 时，生成的配置
		// 会被 LoadConfig 拒绝——在这里就拦住。
		if err := registry.check(name); err != nil {
			return nil, err
		}
		key := prompt(reader, fmt.Sprintf("    账号 %d API Key:", i))
		accounts = append(accounts, accountConfig{Name: name, Key: key})
	}
	return accounts, nil
}

func generateConfigYAML(listen string, providers []providerConfig, tools []detectedTool) string {
	var sb strings.Builder
	sb.WriteString("# Prism config — generated by `prism setup`\n")
	sb.WriteString(fmt.Sprintf("listen: \"%s\"\n", listen))
	sb.WriteString("probe_interval: 1m\n")
	sb.WriteString("wire_api: both\n\n")
	sb.WriteString("# 关掉 Codex 虚拟模型名映射（PI 用真实模型名）\n")
	sb.WriteString("model_remap_enabled: false\n\n")
	sb.WriteString("providers:\n")
	for _, pv := range providers {
		sb.WriteString(fmt.Sprintf("  %s:\n", pv.Name))
		sb.WriteString("    accounts:\n")
		for _, acct := range pv.Accounts {
			sb.WriteString(fmt.Sprintf("      - name: %s\n", acct.Name))
			sb.WriteString(fmt.Sprintf("        base_url: %s\n", pv.BaseURL))
		}
	}
	sb.WriteString("\ntools:\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("  %s: \"%s\"\n", t.Name, t.Path))
	}
	sb.WriteString("\n# Codex 兼容（model_remap_enabled: true 时生效）\n")
	sb.WriteString("model_tiers:\n")
	sb.WriteString("  frontier: deepseek-v4-pro\n")
	sb.WriteString("  standard: deepseek-v4-flash\n")
	sb.WriteString("  glm-standard: glm-5.2\n")
	sb.WriteString("default_tier: standard\n\n")
	sb.WriteString("strip_fields:\n")
	sb.WriteString("  glm-standard:\n")
	sb.WriteString("    - prompt_cache_retention\n\n")
	sb.WriteString("model_remap:\n")
	sb.WriteString("  gpt-5.5: frontier\n")
	sb.WriteString("  gpt-5.5-pro: frontier\n")
	sb.WriteString("  gpt-5.6-luna: frontier\n")
	sb.WriteString("  gpt-5.6-sol: frontier\n")
	sb.WriteString("  gpt-5.6-terra: frontier\n")
	sb.WriteString("  gpt-5.4: standard\n")
	sb.WriteString("  gpt-5.4-mini: standard\n")
	sb.WriteString("  gpt-5.4-nano: standard\n")
	sb.WriteString("  gpt-5.3-codex: standard\n")
	sb.WriteString("  gpt-5.2: standard\n")
	sb.WriteString("  gpt-5.2-codex: standard\n")
	sb.WriteString("  gpt-5.1-codex-mini: standard\n")
	sb.WriteString("  gpt-5.1-codex-max: standard\n")
	sb.WriteString("  codex-auto-review: standard\n")
	sb.WriteString("  gpt-4.1-mini: standard\n")
	sb.WriteString("  gpt-4.1-nano: standard\n")
	sb.WriteString("  o4-mini: standard\n")
	sb.WriteString("  glm-5.2: glm-standard\n\n")
	sb.WriteString("mcp_tools_json: \"/var/lib/prism/mcp_tools.json\"\n")
	return sb.String()
}

// modelCacheDir is the on-disk model cache directory used by main.go; the
// generated unit must make it writable (ReadWritePaths) under
// ProtectSystem=strict.
const modelCacheDir = "/var/lib/prism/model_cache"

// unitWorkingDir is the unit's WorkingDirectory; relative ReadWritePaths
// entries are resolved against it.
const unitWorkingDir = "/var/lib/prism"

// unitUsageDBPath is the usage database default path (see internal/config);
// its parent directory must be writable for usage recording.
const unitUsageDBPath = "/var/lib/prism/usage.db"

// unitSecurityHardening lists the security hardening directives emitted by
// the generated unit. scripts/prism.service.example mirrors the same set (a
// test pins the two in sync) — keep both in lockstep when changing.
var unitSecurityHardening = []string{
	"NoNewPrivileges=true",
	"ProtectSystem=strict",
	"ProtectHome=true",
	"PrivateTmp=true",
	"ProtectKernelTunables=true",
	"ProtectKernelModules=true",
	"ProtectControlGroups=true",
	"LockPersonality=true",
	"RestrictSUIDSGID=true",
	"RestrictNamespaces=true",
	"SystemCallArchitectures=native",
	"CapabilityBoundingSet=",
	"AmbientCapabilities=",
	"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	"MemoryMax=1G",
	"TasksMax=256",
}

// unitReadOnlyPaths lists the read-only base paths hardened on top of
// ProtectSystem=strict; the ReadWritePaths entries below take precedence for
// the specific writable subtrees.
var unitReadOnlyPaths = []string{
	"/var/lib/prism",
}

// readWritePathList builds the deduplicated ReadWritePaths list: the model
// cache dir, the usage.db parent dir, and every tool config parent dir.
// Relative entries are resolved against the unit working directory,
// filepath.Clean is applied to every entry, and exact duplicates are
// removed.
//
// Safety gate (final review): a broken usage/tools configuration must never
// widen ReadWritePaths to "/" — that would make the ENTIRE root filesystem
// writable and silently defeat ProtectSystem=strict. Dangerous inputs are
// rejected by SKIPPING the entry (the rest of the unit is generated
// unchanged, so one bad tool path cannot fail the whole setup):
//   - empty tool paths: filepath.Dir("") is "." and would silently mark the
//     working directory writable;
//   - anything resolving to "/": an absolute root tool path ("/"), a
//     nested path that climbs to the root ("/var/lib/prism/../../../.."),
//     or a relative path that escapes to the root ("../../.." →
//     /var/lib/prism/../../.. → "/");
//   - escape outside the working directory: a relative path whose
//     resolution leaves it (".." → /var/lib, "../escape" → /var/lib), and
//     an ABSOLUTE path containing ".." segments whose Dir result leaves it
//     ("/var/lib/prism/../.." → Dir "/var/lib"). Explicit absolute tool
//     paths WITHOUT ".." segments (e.g. /root/.pi/agent) are configuration
//     intent and stay.
func readWritePathList(tools []detectedTool) []string {
	paths := []string{modelCacheDir, filepath.Dir(unitUsageDBPath)}
	for _, t := range tools {
		if t.Path == "" {
			continue // 危险空路径：跳过，绝不把工作目录悄悄变成可写区
		}
		dir := filepath.Dir(t.Path)
		// 绝对路径含 ".." 段（异常配置）：filepath.Dir 返回前已按 Clean
		// 语义解析，逃逸在此处已经发生 —— Dir 结果必须落在工作目录内，
		// 否则跳过（"/var/lib/prism/../.." → /var/lib 逃逸）。无 ".." 段
		// 的绝对路径（/root/.pi/agent）是显式意图，保留。
		if filepath.IsAbs(t.Path) && containsDotDot(t.Path) && dir != unitWorkingDir && !strings.HasPrefix(dir, unitWorkingDir+"/") {
			continue
		}
		paths = append(paths, dir)
	}
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		rel := !filepath.IsAbs(p)
		if rel {
			// 相对路径：以工作目录为基准解析（Join 内部已 Clean，所以
			// "../escape" 在解析时就已得到 /var/lib 这样的逃逸结果）。
			p = filepath.Join(unitWorkingDir, p)
		}
		// 安全门 1：根路径（含相对路径逃逸到根 "../../.." → /）→ 跳过
		if p == "/" {
			continue
		}
		// 安全门 2：相对路径的解析结果必须落在工作目录内，逃逸 → 跳过。
		if rel && p != unitWorkingDir && !strings.HasPrefix(p, unitWorkingDir+"/") {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// containsDotDot reports whether a path contains a ".." segment — the
// marker of a potentially escaping path. The RAW path is inspected (NOT
// Clean-ed first: Clean would resolve the ".." segments away and hide the
// escape).
func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if seg == ".." {
			return true
		}
	}
	return false
}

func generateSystemdUnit(providers []providerConfig, tools []detectedTool) string {
	var sb strings.Builder
	sb.WriteString("# Prism systemd unit — generated by `prism setup`\n")
	sb.WriteString("[Unit]\n")
	sb.WriteString("Description=Prism - LLM Load Balancer\n")
	sb.WriteString("After=network-online.target\n")
	sb.WriteString("Wants=network-online.target\n\n")
	sb.WriteString("[Service]\n")
	sb.WriteString("Type=simple\n")
	sb.WriteString("User=prism\n")
	sb.WriteString("Group=prism\n")
	sb.WriteString("WorkingDirectory=" + unitWorkingDir + "\n")
	sb.WriteString("ExecStart=/usr/local/bin/prism\n")
	sb.WriteString("ExecReload=/bin/kill -HUP $MAINPID\n")
	sb.WriteString("Restart=always\n")
	sb.WriteString("RestartSec=3\n")
	sb.WriteString("TimeoutStopSec=45\n")
	sb.WriteString("KillMode=mixed\n\n")
	sb.WriteString("# 安全加固（与 scripts/prism.service.example 保持一致）\n")
	for _, d := range unitSecurityHardening {
		sb.WriteString(d + "\n")
	}
	sb.WriteString("ReadOnlyPaths=" + strings.Join(unitReadOnlyPaths, " ") + "\n\n")
	sb.WriteString("# 写入权限：model cache、usage.db 父目录、工具配置目录（相对路径以 WorkingDirectory 为基准，已 Clean + 去重）\n")
	for _, p := range readWritePathList(tools) {
		sb.WriteString("ReadWritePaths=" + p + "\n")
	}
	sb.WriteString("\n# Credential 注入\n")
	for _, pv := range providers {
		for _, acct := range pv.Accounts {
			envName := config.CredentialEnvName(acct.Name)
			sb.WriteString(fmt.Sprintf("LoadCredential=%s:/etc/credstore/prism/%s\n", envName, envName))
		}
	}
	sb.WriteString("\n[Install]\n")
	sb.WriteString("WantedBy=multi-user.target\n")
	return sb.String()
}
