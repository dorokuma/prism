---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: usage, planusage
---

# usage 报告维持定宽 56 列，不与 quota 卡片的 58 列统一

## 一句话结论
- `internal/usage` 的报告**维持定宽 56 列**（`internal/usage/report.go` 的 `reportWidth = 56`），**不与** `internal/planusage` quota 卡片的常规 **58 列**统一；两处相差 **2 列**是有意保留的现状，由用户拍定。

## 背景
- quota 卡片的宽度在 v0.36.2 / v0.36.3 连续变动（61 → 58，见 `20261003-quota-metric-gap.md`、`20261003-quota-row-alignment.md`），usage 报告侧未跟进，于是两处宽度由「同宽 56」（`20260924-card-width-60-to-56.md`）先拉到 56 vs 61（差 5），现为 **56 vs 58（差 2）**。
- 两条命令**不同屏**：`prism usage` 与 `prism quota` 各自独立输出、不会被并排比对，也没有「同一屏内左右对齐」的诉求；宽度差异不触及任何调用方契约（两侧的 `--json` 字段与路径、legacy 表格路径都与卡片宽度无关）。

## 决策
- **维持现状**：`internal/usage` 的 `reportWidth = 56` **不动**，`internal/planusage` quota 卡片维持 58 列；不为「两卡同宽」的旧约定再付一轮改动成本。
- 该定案由**用户拍定**：用户在 v0.36.3 上线后明确表态「usage 不动」。

## 关闭的遗留
- **`20261003-quota-row-alignment.md` 的「遗留（未决 / 需后续处理）」节**：「`internal/usage` 的 `reportWidth = 56` 与 quota 卡 58 是否统一仍待用户决策」一项，自此关闭。
- **`20261003-quota-card-uniform-width.md` 的「遗留清单（未决 / 需后续处理）」节中关于 `reportWidth = 56` 的那一项**（「`internal/usage` 的 `reportWidth = 56` 未跟进 quota 侧的 58……是否统一待用户决策」，后又被 `20261003-quota-metric-gap.md` 的「遗留」节沿用）：该项自此关闭。
- 上述两处**均不改旧笔记正文**——按本批同车补写的维护规矩（`.agents/notes/README.md` 的「部分取代」条），关闭/取代关系由本笔记正文声明，旧笔记保持入库原状。

## 被放弃的方案（必填）
- **① 把 usage 一起提到 58 列**：纯审美改动——两条命令不同屏、无并排对齐诉求；却要再动 `internal/usage` 一个模块的**文本契约**（`reportWidth` 及其派生的 `reportInner` / `tableWidth` / `descMax` / 规则行 dash 数，外加 `handler.go` 的 56-column 注释与相关测试期望），并各走一轮双审与发布。收益与代价不成比例。否决。
- **② 把 quota 压回 56 列**：quota 卡片 58 列（`4 + clineRowFixed(45) + clineNameColMin(9)`）是 v0.36.2/v0.36.3 刚定稿的版式结论（见 `20261003-quota-row-alignment.md`），压回 56 要么缩进度条/名字列、要么推翻刚落地的 58 列结论，等于回退刚上线的决定，只为对齐一个无人并排看的宽度。否决。
- **③ 让 usage 走「按内容 + 名字列下限」的动态宽度**：这是旧笔记遗留清单里与「提到 58 列」并列的另一条出路（见 `20261003-quota-card-uniform-width.md` 的「遗留清单」节），同样要动 `internal/usage` 一个模块的文本契约，还会引入「宽度随 roster 变化」的新抖动面。既然定案是 usage 维持定宽 56 列，该方案**也随上述遗留一并关闭**。否决。

## 来源
- 用户当次明确表态「usage 不动」（v0.36.3 上线后拍定）。
- 遗留项来自 `20261003-quota-row-alignment.md` 的「遗留（未决 / 需后续处理）」节与更早的 `20261003-quota-card-uniform-width.md`「遗留清单」节（后者又在 `20261003-quota-metric-gap.md` 的「遗留」节被沿用）。
- 相关实现：`internal/usage/report.go`（`reportWidth = 56`）、`internal/usage/handler.go`（56-column 注释）；相关笔记：`20260924-card-width-60-to-56.md`（两卡同宽 56 的由来，其「两卡同宽」结论已被 quota 侧的后续宽度变动取代）、`20261003-quota-row-alignment.md`（quota 侧现为 58 列）。
