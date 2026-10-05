---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: usage, render
---

# usage 卡片去掉「请求/词元/开销」汇总行与 ├ 分隔线

## 一句话结论
- `RenderUsageReport`（CLI `prism usage` 与 HTTP `?format=table` 共用）不再输出第一行汇总行「`请求 X · 词元 Y · 开销 $Z`」，其下的 `├─┤` 分隔线一并删除：卡片由「标题 / 汇总 / ├ / 表头 / 数据 / ╰」变为「标题 / 表头 / 数据 / ╰」，与 quota 卡「标题 / 行 / ╰」的结构同构（quota 卡本就无内部横线）。**卡宽仍 45，列结构与列预算逐字未动**。
- `RenderUsageReport` 签名去掉 `ov *Overview` 参数；HTTP `serveTable` 不再执行 Overview 查询（表路径少一次 SQL，Overview 查询失败的 503 分支随之消失，Summary 失败仍 503）。**Overview 数据管线整体保留**：CLI `--json` 的 `overview` 字段是汇总数字的唯一出口（`writeUsageJSON` 未动），`Store.Overview` / `AddOverview` / defaulted 时「Overview 跨全部历史而 rows 只在窗口内」的语义全部不变。
- `render.SummaryLine` / `render.Summary`（唯一生产调用方随汇总行消失）按 `FormatTokensOneDecimal` 先例整体删除（`summary.go` + `summary_test.go`，未新开文件）。

## 决策
- **├ 线随汇总行一起删**：├ 的存在意义是分隔汇总区与表格区；汇总行没了，├ 紧贴标题行形成 `╭─╮`/`├─┤` 双横线，且破坏与 quota 卡的结构同构。否决「保留 ├ 作表头分隔」。
- **Overview 管线保留、只断表格消费**：`cmd/prism/usage.go` 的 `render()` 仍查询 Overview 并 `AddOverview`（`--json` 需要）；`serveTable` 删除 `qOverview`/`Overview` 查询/`AddOverview` 三段。HTTP JSON 本来只有 `{"rows"}`，不受影响。
- **blank-model 对账契约迁移**：`AddOverview` 必须吃**未过滤** agy extra（空 model 计入总览、不计入表行）的契约，原由 `TestHandlerOverviewIncludesAgyExtraBlankModel` 的表格断言承载；汇总行删除后该面在 HTTP 侧不可观测，迁移为 `agy_test.go` 的 `TestAddOverviewIncludesBlankModelExtra` 单测 + `TestRunUsageGroupByModelFiltersBlankModel` 的 `--json overview.requests == 2` 端到端断言。HTTP 侧测试更名 `TestHandlerTableFiltersAgyBlankModel`，只钉表格过滤半边。
- **overview 窗口语义的 HTTP 断言删除**：`TestHandlerToOnlyRange`（to-only 窗口总数）、`TestHandlerTableOverviewAllHistoryDefaulted`（defaulted 全历史合计 4 vs 窗口 2，更名 `TestHandlerTableDefaultedVsExplicitRows`）中「header 计数」类断言随汇总行消失——该语义在 CLI 侧由 `TestRunUsageWeekDefaultOverviewAllHistory`（`--json`）继续钉住，HTTP 侧只保留行窗口断言。
- **死代码边界**：`render.FormatCost`（唯一生产调用方是 SummaryLine）与 `render.FormatCostCompact`（更早就没有生产调用方）**保留**——两者是同一主题的成本格式化原语、各有独立测试，清理应单开一笔，不混入本笔。

## 变了 / 不变
- **变了（刮取提示）**——CLI `prism usage` 与 HTTP `?format=table` 卡片文本：少两行（汇总行、├ 线），卡片行数 −2，按行号/行数/首行内容解析的刮取式调用方需适配。
- **不变**——卡宽 45 与 quota 卡同宽、`╭─/╰─` 边框、表头 `模型/请求/缓存/命中率` 列结构与列预算、命中率胶囊 10 格、无数据行 `（暂无数据）`、配色与 dim 边框、CLI flag、`--json` 字段与路径（CLI overview 字段逐字未动）、HTTP JSON `{"rows"}`、`/metrics`、quota 卡片与 quota `?format=table` 完全未动、SQL schema 未动。

## 被放弃的方案（必填）
- **保留 ├─┤ 作标题与表头的分隔**：紧贴 ╭ 边成双横线、quota 卡无内部横线，同构破坏。否决。
- **HTTP JSON 补 overview 字段以挽回可观测面**：JSON 是程序化契约，新增字段是接口变更，远超「删表格一行」的指令范围。否决，未做（若将来要补，按铁律另记笔记）。
- **连带删除 `Store.Overview`/`AddOverview`/`Overview` 整条管线**：CLI `--json` overview 是活契约，删了会破坏 `--json` 字段。否决。
- **连带删除 `render.FormatCost`/`FormatCostCompact`**：属独立的死代码清理主题（后者本就无生产调用方）。否决，见「遗留」。

## 遗留（未决 / 需后续处理）
- **`render.FormatCost` / `FormatCostCompact` 均无生产调用方**（前者由本笔造成，后者更早），当前保留为带测试的渲染原语；要清理可单开一笔（连带 `numbers_test.go` 两个用例）。
- **usage 汇总数字在 HTTP 侧完全不可见**：HTTP JSON 无 overview 字段、表格无汇总行；需要总览只能走 CLI `--json`。这是本笔的既定结果，不是疏漏。
- **按行数/行号解析输出的刮取方**（若有）需要适配卡片行数 −2；仓库内无此类调用方（`internal/usage` 与 `cmd/prism` 测试全部同步）。

## 来源
- 用户指令：「把 usage 模块表格第一行彻底去掉，就是请求 词元 开销 那行」。
- 相关实现：`internal/usage/report.go`（渲染体、版式注释、签名）、`internal/usage/handler.go`（serveTable 去 Overview 查询）、`cmd/prism/usage.go`（CLI 调用点）、`internal/render/summary.go`/`summary_test.go`（删除）、`internal/usage/agy.go`（AddOverview 注释口径）。
- 相关测试：`report_test.go`（Structure/Exact/NoData/Color/TitleText/NoCacheSegments 更名、OverLongTitle/CardWidth 去 ov）、`handler_test.go`（TableFormat 逐字节、FiltersAgyBlankModel 更名、MixedSources、MergesAgyGeminiRow、NoData、ToOnlyRange、DefaultedVsExplicitRows 更名）、`cmd/prism/usage_test.go`（Table、SplitCacheSegments、GroupByModelFiltersBlankModel 加 JSON overview 断言、day 视图断言）、`agy_test.go`（新增 `TestAddOverviewIncludesBlankModelExtra`）。**未新增测试文件**。
