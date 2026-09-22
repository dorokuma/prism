---
status: active
superseded_by: ""
supersedes: 20250923-quota-tui-card-format.md
模块: planusage, render, cmd/prism
---

# 配额卡片改「流光胶囊」（RenderCards 条形重做）

## 一句话结论
- `RenderCards` 的进度条由 18 格 `█`/空格/`┈` 改为**全内宽 49 格「流光胶囊」**：已用 `▰`、未用 `▱`，已用格逐格按占用水平在 绿→黄→红 插值；卡片由「一账号一张多行」改为**一（账号×窗口）一张三段式卡片**（标题含窗口标签 / 胶囊行 / 明细行）；同时新增 `CardOptions{NoColor}` 与 `prism quota --no-color` + TTY 自动剥离配色。`RenderTable`、JSON、`types.go` 逐字未动。

## 背景
- 主代理实机验收旧卡片时投诉两点：**边框被顶出**（行宽偶发超过 60 列）与**表格错位**（多窗口挤在一张卡的密排行里，窗口多一列就串行）。
- 任务书 [MARK-WORKER-PRISM-QUOTA-CAPSULE] 要求条形改为流光胶囊（`▰`/`▱`、渐变、非 TTY 去色、任意百分比不错位）。

## 几何（以 DisplayWidth 去色实测为准，每行恒定 60 列）
- **一张卡片 = 一个（账号 × 窗口）**：`标题行 / 胶囊行 / 明细行 [/ ⚠错误] [/ ! limit reached] / 底边框`。窗口标签从此进标题（`5h window`/`weekly window`/`monthly window`，未知名原样、空名 `--`），旧卡片的 短期/中期/长期 只保留在 legacy 表格。
- 胶囊行：`Dim("│ ") + 2sp + capsule(49) + 1sp + pct(4) + Dim(" │")` = 60。`barCells = cardInner(56) − barIndent(2) − pctWidth(4) − barGap(1) = 49`——**先扣百分比标签列再给条长**，这就是「放不下标签时按列宽收缩条长」的实现方式（条长永远够，标签永远在）。
- 明细行：`Dim("│ ") + 2sp + detail(36) + reset(18) + Dim(" │")` = 60。左右两列固定宽度，左 `已用 34% / 总额 3.5M tok`，右 `resets in 3h 12m`。
- 标题：`Dim("╭─ ") + Brand(服务名) + " " + Dim(账号[+旧]) + Dim(" · ") + Brand(窗口标签) + Dim(" " + ─*fill + "╮")`；body 上限 54 列（fill≥1），超长**先截账号、再截服务名，窗口标签永不截**（`bodyMax = cardWidth − 3 − 1 − 2`）；行尾另有 `DisplayWidth(line) > cardWidth → Truncate` 兜底。
- 倒计时改为 `2h 01m`（分量间一空格，分钟补零），与规格示例 `resets in 3h 12m` 对齐；`formatRemain` 逐字未动。

## 颜色（ANSI 24-bit，沿用 render/color.go 约定）
- 标题服务名 + 窗口标签 Brand 青 #00B4D8；账号/边框/补线 Dim #666666；未用 `▱` 全条 Dim #666666。
- **已用 `▰` 逐格渐变**：格 i 的占用水平 = `(i+1)*100/49`（2 %…100 %），`capsuleRamp` 分段线性插值：**≤40 % 平绿 (82,183,136)**、40→60 % 插值到 #F4A261 黄 (244,162,97)、60→100 % 插值到耗尽红 (230,57,70)。健康窗口整段平绿，接近上限才逐格转暖，量变而非跳档。
- **耗尽**（`windowExhausted`：`Percent>=100 || used up || rate-limited`）= **整条 49 格纯红实心 `▰`** + `#F4A261 ! limit reached` 行。
- 相邻同色格合并为一段 ANSI（0 %/100 % 各只发一个转义），低占用条不产生 49 段色码。

## 明细行口径（与 upstream 的有意分歧）
- **百分比标签与胶囊长度共用同一个口径** `displayPercent`（`UsedFraction>0` 时以其提精度），旧卡片标签用 `Percent`、条长用 `UsedFraction`，会出现「标签 94 %、条却按 94.4 % 画」的不一致；现在两者永远一致（`formatRemain`/表格的占用列仍逐字未动）。
- **模型没有「已用绝对量」字段**：`Window` 只有占用比例（`Percent`/`UsedFraction`）和总额（`LimitTokensEstimate`/`LimitUSDEstimate`），没有已耗 token/美元数。因此**不编造** `1.2M / 3.5M tok` 的左值：左值固定为**占用百分比**，右值给出已知总额——`已用 34% / 总额 3.5M tok`（token 池）或 `额度 12% / $60.00`（美元估算，美分精度仍降级为 `$%d.00`），无估算概念时只留 `已用 34%`。「用量/重置」信息完整且全部来自现有字段。
- 美元估算窗口沿用旧口径把状态词定为 `额度`（`col2Label`），token 池走 `已用`；两种总额不重复标注。

## 去色（render / cmd 的最小改动）
- `planusage.CardOptions{NoColor bool}` + `RenderCards(snaps, now, opts...)`（变参，旧两参调用与既有测试不变，默认上色）。所有颜色经 `cardPalette{brand/dim/red/yellow/run}` 一个出口，`NoColor` 时原样返回字符串——**版式的格子数、padding、字形在任何分派前已定死**，因此「去色 = 有色彩渲剥掉转义序列」逐字节成立（测试对两种模式做等值断言）。
- `cmd/prism/quota.go` 仅两处：新增 `-no-color` flag、`wantColor(out, *noColor)`（复用 usage 命令的 TTY 判定，不改 usage.go）决定 `NoColor`。非 TTY/管道/`--no-color` 自动去色。

## render 包新增（最小实现 + 单测）
- `Fg(r,g,b,s)`：通用 24-bit 真彩色包裹（`Brand/Red/Yellow/Dim` 的插值版），供胶囊渐变使用；`render.Green` 因胶囊改走插值后**无调用方**，按仓库惯例（参见前序笔记删 `Orange` 的处理）连同 `colorGreen` 一并删除。
- `PadRight/PadLeft`：ANSI-aware 填充（转义序列计 0 宽），超宽按 `Truncate` 省略号收缩——**这正是「条放不下就收缩、绝不撑破边框」的填充原语**；planusage 内旧的私有 `padRight/padLeft` 删除，改调 render 版（单一实现，避免两处漂移）。

## 测试（新增/更新）
- 逐行 `DisplayWidth == 60`：pct = 0/1/34/59/99/100 + `used up`/`rate-limited` + `UsedFraction` 亚百分比 + token 池/美元估算 + 无窗口/错误/旧标记 + 长账号截断，**彩色与 no-color 两种模式各跑一遍**。
- 渐变色档边界：`capsuleRamp` 在 0/34/40 = 平绿、60 = #F4A261、100 = 红、中点 50/80 精确值、且 g/b 通道单调不增；渲染层断言 34 % 只有绿、99 % 首格绿末格红且颜色段数 > 1。
- 去色模式：输出无 `\x1b` 且与彩染去色后**逐字节相等**。

## 被放弃的方案（必填）
- **沿用 18 格短条 + 右侧大空白**：胶囊行会变成「短条 + 30 列空白 + 百分比」，视觉断裂；且仍然解释不了用户的错位投诉。改为一卡一窗口、条占满内宽减去标签列。
- **耗尽用全红 `▱`（空心）串**：空心语义是「没用/空」，与「已耗尽=用满」相反，容易误读；定案纯红实心 `▰` + `! limit reached` 兜底。
- **整段一色（按总占用率取色，不逐格插值）**：可用但接近上限时没有「流光」渐变了，视觉平滑度差；定案逐格插值 + ≤40 % 平绿。
- **沿用 █/空格/┈ 只改色**：不满足规格的 `▰`/`▱` 与胶囊造型。
- **把绝对已用 token 当作已用量渲染**（如 `UsedFraction × LimitTokensEstimate`）：推导值会被误读为上游实测，违背「不得编造字段」红线。
- **改 `windowLabel` 的 短期/中期/长期 输出**：legacy 表格快照被测试锁死，只让卡片用新的窗口标签，`RenderTableAt` 零改动。
- **卡片仍按账号聚合（多窗口多行）**：那样窗口标签只能挤进行内，规格要求的「标题含窗口标签」无法满足，且旧密排正是错位投诉源。

## 兼容红线
- `RenderTable`/`RenderTableAt`/`formatRemain`/`WriteJSON`/`Response`/`types.go` JSON tag、`go.mod` 零改动；`format=table` 与 `/admin/quota` JSON 行为不变。
- `RenderCards` 旧签名仍可用（变参默认上色），`cmd/prism/quota.go` 之外无其它调用方。

## 来源
- 任务书 [MARK-WORKER-PRISM-QUOTA-CAPSULE]；前序笔记 20250923-quota-tui-card-format.md（本笔记 supersede 其卡片版式部分，其 table/兼容红线结论仍有效）。
- `internal/render`（`DisplayWidth`/`StripANSI`/`Truncate` + 新增 `Fg`/`PadLeft`/`PadRight`）作为 ANSI 对齐与去色基础。
