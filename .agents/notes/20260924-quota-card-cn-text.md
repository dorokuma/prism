---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage, render
---

# quota 胶囊卡片三处英文文案中文化（已达限额 / 后重置 / ⚠ 错误码映射）

## 一句话结论
- `internal/planusage` 的 `RenderCards` 卡片三处英文改中文：耗尽 footer `! limit reached` → `已达限额`（仍走 `cardFooter` 的 `pal.yellow` #F4A261 包装，文本以外零变化）；明细行右侧倒计时 `resets in 3h 12m` / `resets now` → `3h 12m 后重置` / `已重置`（`-` 不变；`resetWidth` = 18 列右对齐、`cardCountdown` 的时间格式 `2h 01m`/`3d 4h` 都不动）；拉取失败 note `⚠ <code>` 新增渲染层映射 `cardErrorNote`：`unauthorized` → 未授权、`no_subscription` → 无订阅、`unexpected_status` → 上游状态异常、`fetch_failed` → 拉取失败，未映射码原样透传且保留 `⚠` 前缀。
- 明细行总额单位 `tok` → `词元`（`totalPart`；与 usage 卡片/汇总行的 `词元` 统一），显示宽度 3 列 → 4 列仍在 `detailWidth` 34 内，每行仍恒 56 列。
- 数据层零改动：`ErrorCode` / `Cache.StoreFailed` / `Snapshot.Err` / `/admin/quota` JSON 的 code 仍全是英文原值；legacy 表格 `RenderTable`/`formatRemain` 逐字未动。

## 背景
- 卡片中文化（v0.31.0 系列，见 `20260922-usage-quota-cn-text.md`）后卡片上仍留三处英文。其中 `! limit reached` 曾按规格注释被明确「保留英文」（见 `20260923-quota-tui-card-format.md`），本轮任务书 [MARK-PRISM-I18N-9E4C] 要求改中文，属对既有决策的修订，故留痕。

## 决策
- **错误码映射放渲染层**（`cardErrorNote`）：报告/日志与 HTTP JSON 都消费英文 code，`cache.clearsQuotaWindows("unauthorized"/"no_subscription")` 也按英文 code 决定是否清窗口——映射若放数据层会破坏这些契约。
- **映射覆盖 `ErrorCode` 能产出的全部四个码**（`unauthorized`/`no_subscription`/`unexpected_status`/`fetch_failed`），其余未知码（如测试构造的 `timeout`）原样透传、仍带 `⚠` 前缀。
- **文案定案**：`已达限额`（无感叹号、无前导符号）、`3h 12m 后重置` / `已重置`。新文案显示宽度 ≤ 13 列 < `resetWidth` 18，每行恒 56 列、边框与右对齐列宽不变量不变（`cardLines` 断言未放松）。
- **测试同步**：`planusage/report_test.go` 相关断言全部改新文案，并新增 `TestRenderCardsLocalizedCardText`（新文案出现 + 旧英文串必须消失 + 未知码透传 + 列宽）；`render/color_test.go` 的 `Yellow` 样例串同步为 `已达限额`；`render/color.go` 注释同步。无任何放宽。
- **门禁**：`go build -o prism ./cmd/prism`、`go test ./...`、`scripts/test_*.sh` 全跑、`python3 scripts/test_generate_mcp_tools.py` 全绿；另 `go vet ./...` 干净，`gofmt -l` 对**本批改动文件**干净（仓库整体另有 4 个与本批无关的存量未格式化文件，gofmt -l 会一并标出：`internal/cache/routing_test.go`、`internal/proxy/aggregate_test.go`、`internal/proxy/models_test.go`、`internal/usage/writer_test.go`）。

## 被放弃的方案（必填）
- **只映射 unauthorized/no_subscription、fetch_failed 保持英文**：`fetch_failed` 是卡片上最高频的失败码，留英文与「警告行中文化」的目标矛盾，否决。
- **在 `ErrorCode`/`Snapshot.Err` 直接产出中文**：污染日志与 `/admin/quota` JSON 契约，且 `clearsQuotaWindows` 依赖英文码，否决。
- **把「后重置」拼进 `cardCountdown`**：`cardCountdown` 只管时间格式（其 `已到` 分支仍被单测直接覆盖），词尾放进 `resetText` 更贴近职责边界；改 `formatRemain` 更不可行（legacy 表格输出冻结），否决。

## 来源
- 任务说明 [MARK-PRISM-I18N-9E4C]（worker 派发）。
- 相关笔记：`20260922-quota-capsule-bar.md`（卡片几何/耗尽 footer）、`20260923-quota-tui-card-format.md`（`! limit reached` 原为规格英文原文）、`20260922-usage-quota-cn-text.md`（上一轮中文化范围）、`20260924-card-width-60-to-56.md`（56 列宽）。
