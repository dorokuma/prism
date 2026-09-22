---
status: active
superseded_by: ""
supersedes: ""
模块: usage, render, planusage, cmd/prism
---

# `prism usage` 报表改「A1·命中率胶囊」卡片（与 quota 流光胶囊同源）

## 一句话结论
- `usage.RenderUsageReport` 由「三行汇总 + 紧凑表格」改为**与 quota 卡片同宽同族的 60 列胶囊卡片**：`╭─ Usage · 按 <group> 分组 ─…╮` 标题行、单行汇总（`请求 N · 词元 X · 开销 $Y`）、`├─` 分隔、品牌青加粗表头 + dim 细分隔行、明细行（分组列 + 请求 + 缓存 + **10 格 ▰▱ 命中率胶囊** + 右对齐百分比）、`╰─` 底边框。**每行严格 60 列**（彩色/去色皆然）。
- 胶囊原语（字形 `▰▱`、逐格渐变 ramp、格数算术、同色合并）从 `internal/planusage` **下沉到 `internal/render`**（`capsule.go`），planusage 改为调用共享实现，usage 复用同一套；`render.RenderSummary`（三行汇总）随之改为 `render.SummaryLine`（单行汇总）。数据查询/store 层、quota 渲染语义、JSON 输出**逐字未动**。
- 命中率渐变方向与 quota **相反**：调用方传给共享 ramp 的是**严重度 level**，usage 传 `100 - CapsuleLevel(i)`（缺失的命中份额），因此「命中率越高越绿、冷缓存发红」；quota 仍传 `CapsuleLevel`（占用越高越红）。

## 背景
- 任务书 [MARK-WORKER-PRISM-USAGE-CAPSULE] 要求按已确认的 A1 设计重做 `prism usage` 报表，与上一轮 quota「流光胶囊」卡片风格统一；上一轮的胶囊实现散落在 planusage 内（`capsuleRamp`/`lerpRGB`/`capsuleSeg`/`capsuleBar`），usage 侧若再抄一份必然漂移，故先下沉再复用。

## 共享原语下沉方式（internal/render/capsule.go）
- 导出：`CapUsed`/`CapEmpty`（▰/▱）、`CapsuleRamp(level)`（严重度→颜色，≤40 % 平绿 (82,183,136)、40→60 % 插值到 #F4A261、60→100 % 插值到耗尽红 (230,57,70)）、`CapsuleLevel(i, cells)`、`CapsuleUsedCells(pct, cells)`、`CapsuleSeg`、`CapsuleBar(cells, used, level func(i int) int, color bool) string`。
- **方向由 level 参数决定，ramp 本身不区分方向**：`CapsuleBar` 对 `i < used` 的实心格逐格取 `CapsuleRamp(level(i))`，剩余格 dim 灰；相邻同色格合并为一段 ANSI（全绿/全 dim 各只发一个转义）。quota：`level = CapsuleLevel(i, barCells)`；usage：`level = 100 - CapsuleLevel(i, hitCells)`。
- `render.color.go` 增 `BrandBold`（`\x1b[1m` + 品牌青，单 reset）供表头用；`Fg`/`PadRight`/`PadLeft`/`Truncate` 沿用上一轮实现。
- planusage 侧只剩三个绑定 `barCells` 的薄封装（`capUsed/capEmpty` 常量别名、`capsuleUsedCells(pct)`、`capsuleLevel(i)`）与新增的 `capsuleBar` 调用；`capsuleRamp`/`lerpRGB`/`capsuleSeg`/`cardPalette.run` 删除，其边界测试（`TestCapsuleRampBoundaries`）随实现一起搬进 `internal/render/capsule_test.go`，quota 既有测试**零改动即保持全绿**。

## 版式几何（DisplayWidth 去色实测，每行恒定 60 列）
- 通用：`body = Dim("│ ") + PadRight(content, 56) + Dim(" │")`；分隔线 `├`+`─`×58+`┤`、底边框 `╰`+`─`×58+`╯`；标题 `╭─ `(3) + `Brand("Usage")`(4) + `Dim(" · ")`(3) + `Dim(desc)` + `Dim(" " + fill + "╮")`，`fill = 60-3-1-4-3-descW-1`，desc 截断上限 47 列（保证 fill≥1），行尾另有 `DisplayWidth>60 → Truncate` 兜底。
- 表区 = `reportInner(56) − 缩进(2) = 54` 列；固定列：`请求 6`、`缓存 6`、`命中率 17`（10 格胶囊 + 1 空格 + 6 列百分比，**先扣标签再给条长**，与 quota「reserve the label」同规则）；列间距 1 列；分组列均分剩余预算 `54 - 29 - (n+2)`（余数给前几列），因此 n=1→22、n=2→21、n=3→20。**列宽之和 + 间距恒等于 54**，由 `TestReportColumnsFillTheTableArea` 逐 n 锁死。
- model 列沿用既有 20 列 `MaxWidth` 省略号截断（比 22 列预算更窄时取 20，再补齐到列宽）；其余分组列也按预算截断——**固定宽度卡片必须截断**，这是与旧「非 model 列不截断」的有意分歧（见「被放弃的方案」）。
- 汇总行走 `render.SummaryLine`：`请求 1,783 · 词元 2.23M · 开销 $0.836`（`FormatInt`/`FormatTokens`/`FormatCost`，无单价 `-`）。

## 命中率口径与渐变映射
- 分母沿用 `CachedTokens / cacheHitInput()`（OpenAI-form prompt + Anthropic assembled input），百分比仍由 `cacheHitRate` 输出一位小数（该函数零分母仍稳定输出 `0.0%`，被 agy/query 测试复用，**未改动**）。
- **分母为 0 或缺失：显示 `-` 且不画条**——0 格实心会被读成「命中率 0 %」，满条会被读成「100 %」，两者都是编造数据；同理 `0/100` 仍画 10 格全空 `▱` + `0.0%`（这是真实测量值）。
- 渐变映射：第 i 格的命中率水平 = `CapsuleLevel(i, 10)`（10 %…100 %），喂给 ramp 的是 `100 - 该值`。故 5 % 命中率首格即暖红、95 % 命中率末段进入平绿带、100 % 全条绿；**条长即命中率**（ceil(frac×10)），与 quota「条长即占用」一致。

## 颜色与去色（一个出口）
- `reportPalette{color}` 统一 brand/dim/brandBold 三个出口，胶囊颜色由 `render.CapsuleBar(..., color)` 决定；格子数、padding、字形、条长在任何分派前已定死，因此 **no-color = 彩色版剥掉转义序列后逐字节相同**（测试对两种模式做等值断言）。
- `cmd/prism/usage.go` **零改动**：`runUsageWith` 早已 `color := wantColor(out, o.noColor)` 并传入 `ReportOptions{Color}`，本次只把该布尔接到新卡片上。非 TTY/管道/`--no-color` 自动去色；**输出永不依赖 COLUMNS**（`TestRunUsageDefaultCompactNonTTY` 原样通过）。

## 对齐保证手段
- 所有宽度计算走 `render.DisplayWidth`（ANSI 计 0、CJK/全宽计 2）；所有填充/截断走 ANSI-aware 的 `render.PadRight`/`PadLeft`/`render.Truncate`（省略号收缩，不会撑破边框）；每行再由 `pal.body` 二次 `PadRight(56)` 收口，行宽**恒 60** 与数据形态无关。
- 覆盖形态：超长 ASCII/CJK 模型名、命中率缺失、0 % 命中、空结果（`(no data)` 保留）、多分组键（1/2/3 键）、时间桶视图、无 group_by（标题 `未分组`、无分组列，行由 `body` 补齐）。

## 测试（新增/更新）
- `internal/render/capsule_test.go`（新增）：ramp 停止位/中点/单调性、方向即严重度（含 usage 反转映射）、字形宽度、格数算术 clamp、`CapsuleBar` 两方向渲染与段合并、no-color 逐字节等值、非法入参 clamp。
- `internal/usage/report_test.go`（重写卡片相关用例）：逐字节精确输出、结构（边框/汇总/表头/胶囊）、彩色与去色等值、命中率单元格（96.8 %/99.8 %/93.3 %、零分母 `-` 无字形、零分子全空条 + `0.0%`）、命中率渐变方向（95 % 绿 / 5 % 无绿且冷端暖、50 % 半条）、10 种形态 × 彩色/去色的宽度恒 60、多分组键列预算、model 20 列截断与 CJK 安全、列宽填充算术。
- `internal/usage/handler_test.go`：format=table 期望值改为同一张卡片（HTTP 与 CLI 同源不变），`tableDataRows` 跳过 `│` 内的 dim 细分隔行与边框；汇总断言改为单行。
- `cmd/prism/usage_test.go`：汇总行断言改为 `请求 N · 词元 X · 开销 $Y`。
- quota（`internal/planusage`）既有测试**未新增失败**，仅搬走 ramp 边界测试。

## 被放弃的方案（必填）
- **保留三行汇总 + 紧凑表格，只把命中率列换成胶囊**：与 A1「胶囊卡片、与 quota 统一」不符，且表格无边框时列位随内容漂移；定案整卡重做。
- **分组列按内容实测宽度（沿用 render.Table 的 max(title, cells)）**：短模型名会让胶囊列左移到不同列位，卡片失去固定骨架；定案固定列宽 + 分组列均分预算（余数给前列）。
- **非 model 分组列不截断（沿用旧规则）**：60 列固定卡片下会顶破边框；定案按预算省略号截断，model 额外保留 20 列历史上限。
- **多分组键时缩小胶囊格数（10→6）**：会让命中率列宽随分组数变化，测试与视觉都不稳；定案胶囊恒 10 格，压缩只发生在分组列（n=7 时每列 2~3 列，仍有省略号兜底）。
- **零分母沿用 `0.0%` 并画空条**：把「没有分母」伪装成「命中率 0」，属编造；定案 `-` 且不画条。`cacheHitRate` 本身不动（被 agy/query 测试复用），由 `hitCell` 决定该口径。
- **在 usage 侧另写一份胶囊（最小复用）**：两处漂移风险已被上一轮笔记点名；下沉后 usage 侧只多一个取反的 `level` 闭包，代价远小于收益。
- **把 `render.Summary` 三行版保留为死代码**：仓库惯例是删死代码（同上一轮删 `render.Green`）；改为 `SummaryLine` 单行版并重写其测试。

## 兼容红线
- `internal/usage` 数据层（`store.go`/`query.go`/`writer.go`/`handler.go`）、`SummaryRow`/`Overview` 结构、JSON 输出、`FilterBlankModelRows`、`DescribePeriod`、`FormatModelName`、`formatGroupValue` 零改动；`FormatTokens`/`FormatCost` 口径不变。
- `RenderUsageReport(ov, rows, groupBy, ReportOptions)` 签名不变，`handler.go` 与 `cmd/prism/usage.go` 调用点不变（CLI 侧无需接线改动，`wantColor` 既有逻辑原样生效）。
- quota 卡片 60 列几何、配色、`RenderTable`/`formatRemain`/JSON 行为不变；不执行任何 git commit。

## 来源
- 任务书 [MARK-WORKER-PRISM-USAGE-CAPSULE]；前序笔记 20260922-quota-capsule-bar.md（胶囊实现与去色结论由本笔记扩展为跨 usage 共享）。
- `internal/render`（新增 `capsule.go`、`color.go` 增 `BrandBold`、`summary.go` 改 `SummaryLine`）作为 ANSI 对齐、去色与胶囊基础。
