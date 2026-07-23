package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	for _, idx := range selected {
		if idx >= 1 && idx <= 3 {
			bp := builtinProviders[idx-1]
			fmt.Printf("\n=== %s ===\n", bp.Label)
			accts := promptAccounts(reader, bp.Name)
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
		accts := promptAccounts(reader, name)
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
			fmt.Printf("  %s/LB_KEY_%s  新\n", credstoreDir, strings.ToUpper(strings.ReplaceAll(acct.Name, "-", "_")))
		}
	}
	fmt.Println("  /var/lib/prism/config.yaml                    新")
	fmt.Println("  /var/lib/prism/model_cache/                   新目录")
	fmt.Println("  /etc/systemd/system/prism.service            新")
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
			envName := "LB_KEY_" + strings.ToUpper(strings.ReplaceAll(acct.Name, "-", "_"))
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

func promptAccounts(reader *bufio.Reader, providerName string) []accountConfig {
	nStr := promptDefault(reader, "  几个账号？", "1")
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 1 {
		n = 1
	}
	var accounts []accountConfig
	for i := 1; i <= n; i++ {
		defaultName := fmt.Sprintf("%s-%d", providerName, i)
		name := promptDefault(reader, fmt.Sprintf("    账号 %d 名称", i), defaultName)
		key := prompt(reader, fmt.Sprintf("    账号 %d API Key:", i))
		accounts = append(accounts, accountConfig{Name: name, Key: key})
	}
	return accounts
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

func generateSystemdUnit(providers []providerConfig, tools []detectedTool) string {
	var sb strings.Builder
	sb.WriteString("# Prism systemd unit — generated by `prism setup`\n")
	sb.WriteString("[Unit]\n")
	sb.WriteString("Description=Prism - LLM Load Balancer\n")
	sb.WriteString("After=network-online.target\n")
	sb.WriteString("Wants=network-online.target\n\n")
	sb.WriteString("[Service]\n")
	sb.WriteString("User=prism\n")
	sb.WriteString("Group=prism\n")
	sb.WriteString("WorkingDirectory=/var/lib/prism\n")
	sb.WriteString("ExecStart=/usr/local/bin/prism\n")
	sb.WriteString("ExecReload=/bin/kill -HUP $MAINPID\n")
	sb.WriteString("Restart=always\n")
	sb.WriteString("RestartSec=3\n\n")
	sb.WriteString("# 安全加固\n")
	sb.WriteString("ProtectSystem=strict\n")
	sb.WriteString("ProtectHome=true\n")
	sb.WriteString("PrivateTmp=true\n")
	sb.WriteString("CapabilityBoundingSet=\n")
	sb.WriteString("MemoryMax=1G\n")
	sb.WriteString("TasksMax=256\n\n")
	sb.WriteString("# 写入权限\n")
	sb.WriteString("ReadWritePaths=/var/lib/prism/model_cache\n")
	for _, t := range tools {
		parentDir := filepath.Dir(t.Path)
		sb.WriteString(fmt.Sprintf("ReadWritePaths=%s\n", parentDir))
	}
	// Avoid duplicates for tools sharing parent dirs
	sb.WriteString("\n# Credential 注入\n")
	for _, pv := range providers {
		for _, acct := range pv.Accounts {
			envName := "LB_KEY_" + strings.ToUpper(strings.ReplaceAll(acct.Name, "-", "_"))
			sb.WriteString(fmt.Sprintf("LoadCredential=%s:/etc/credstore/prism/%s\n", envName, envName))
		}
	}
	sb.WriteString("\n[Install]\n")
	sb.WriteString("WantedBy=multi-user.target\n")
	return sb.String()
}
