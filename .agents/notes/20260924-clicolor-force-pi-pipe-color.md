---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: "cmd/prism, usage, planusage"
---

# CLI 颜色决策新增 CLICOLOR_FORCE/FORCE_COLOR 强制开关（Pi 扩展管道捕获仍要彩色卡片）

## 一句话结论
- `cmd/prism` 的 `wantColor`（usage.go，quota.go 复用）新增第二优先级：`CLICOLOR_FORCE` 或 `FORCE_COLOR` 为非空且 ≠ `"0"` 时强制着色，覆盖管道场景；`--no-color` 仍为最高优先级，原有 TTY（`ModeCharDevice`）检测逻辑不变。
- Pi 扩展 `/root/.pi/agent/extensions/prism.ts` 配套：`runPrism` spawn 时注入 `CLICOLOR_FORCE=1`；`reportLines` 不再整体剥 ANSI，改为只保留以 `m` 结尾的 CSI（SGR 颜色），OSC/APC/光标/清屏等其余控制序列照旧剥离，颜色交由 pi-tui 的 ANSI-aware 渲染。
- 跨仓库改动（prism 仓 + Pi 扩展），且属 CLI 颜色输出的对外行为契约变化，按铁律 4 留痕。

## 背景
- 用户诉求：Pi 的 `/quota`、`/usage` 斜杠命令要显示彩色卡片。根因两处：
  1. Pi 扩展 `spawn("/usr/local/bin/prism", ...)` 用 `stdio: ["ignore","pipe","pipe"]` 捕获 stdout（非 TTY），prism 的 `wantColor` 仅做 `ModeCharDevice` 检测，管道下判定不着色，输出全素色；
  2. 扩展渲染前调用 pi-tui 的 `stripTerminalSequences(data.text)`，把 ANSI 一律剥掉——即使 prism 着色也被二次抹除。
- 两处互为前提：不去掉扩展的全量剥离，prism 着色无意义；不给 prism 强制开关，去掉剥离也无颜色可留。

## 决策
- `wantColor(out, noColor)` 优先级（从上到下）：
  1. `--no-color` 显式关闭，最高优先；
  2. `envForcesColor()`：`CLICOLOR_FORCE` / `FORCE_COLOR` 任一为非空且 ≠ `"0"` → 强制开启（管道/重定向下也着色）；
  3. 原有 TTY 检测不变（`out.(*os.File)` + `ModeCharDevice`，非 TTY 关闭）。
- 两个环境变量是等价别名，不设相互优先级：`CLICOLOR_FORCE=0` + `FORCE_COLOR=1` 仍强制开启；`"0"` 与 `""` 都视为「不强制」（回落 TTY 检测），`"1"`/`"true"`/`"2"` 等其余值均强制。
- 只影响 usage/quota 彩色渲染路径；`--json` 输出不含颜色、不受影响。`--watch` 重绘沿用同一 `wantColor`。
- Pi 扩展侧：
  - `spawn(bin, argv, { stdio, env: { ...process.env, CLICOLOR_FORCE: "1" } })`——保留原有 env（含 PATH）继承，仅追加强制变量；父进程若显式设了 `CLICOLOR_FORCE=0` 也被覆盖为 `"1"`（扩展的要求就是彩色）。
  - `reportLines` 改用新函数 `keepSgrSequences`：保留 `ESC [ <参数/中间字节> m` 的 CSI 原文（含 `38;2`/`38;5`/`0m`），剥离 OSC（`ESC ]…BEL/ST`）、APC（`ESC _…BEL/ST`）、非 `m` 结尾 CSI（光标/清屏等）与其它 ESC 序列；截断的 CSI 也整体丢弃。超时、`MAX_CHARS` 截断、usage 缩进、组件结构、命令注册一律未动。
- 测试：`TestWantColor` 扩为 env 情形表（CLICOLOR_FORCE/FORCE_COLOR 强制、`--no-color` 压过、`"0"` 不强制、别名组合）；新增 `TestRunUsageForceColorEnv` 走 `runUsageWith`（bytes.Buffer 即管道）做 CLI 级四情形断言，并断言剥色后可见文本与素色输出逐字一致。

## 被放弃的方案（必填）
- **只改 Pi 扩展、不加强制着色**：prism 在管道下根本不输出 ANSI，少了剥离也无颜色可留，治标不治本；否决。
- **扩展用 PTY（node-pty 之类）伪造 TTY**：引入原生依赖、改变超时/退出码/信号行为，风险与代价都大；否决。
- **扩展自行解析 prism 输出重建颜色**：重复实现渲染与调色板，易与 prism 的真实配色漂移；否决。
- **在扩展里给 `FORCE_COLOR` 单独更高优先级**：无实际需求，等价别名最简，规则越少越好；否决。
- **只认 `CLICOLOR_FORCE=1`（其余值不认）**：`FORCE_COLOR` 已是事实标准（chalk/CI），只认一个变量会在别的调用方那里失效；按「非空且非 0」统一处理，否决仅认 `"1"` 的写法。
- **直接放宽 TTY 检测（管道也默认着色）**：会污染所有管道消费方（grep/文件重定向），必须显式 opt-in；否决。

## 来源
- 主代理任务说明 MARK-PRISM-FORCECOLOR-7D2A（Pi `/quota`、`/usage` 彩色卡片配套改动：prism 强制着色 + 扩展保留 SGR）。
- 相关约定：CLICOLOR 规范（`CLICOLOR_FORCE` 非 0 强制）、`FORCE_COLOR` 生态惯例（非空且非 0 强制）；pi-tui `Text` 注释写明行内 ANSI 可渲染，`wrapTextWithAnsi`/`truncateToWidth` 均 ANSI-aware。
- 相关笔记：`20260922-quota-capsule-bar.md`、`20260922-usage-capsule-report.md`（卡片配色的两条消费路径）。
