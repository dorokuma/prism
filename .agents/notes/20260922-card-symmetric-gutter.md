---
status: active
superseded_by: ""
supersedes: ""
模块: usage, planusage, render
---

# 两张卡片内部左缩进去掉：卡片内左右空隙 1:1 对称化

## 一句话结论
- `prism usage` 汇总卡片与 `prism quota` 胶囊卡片的**正文行统一去掉卡片内部的 2 格左缩进**：usage 删除 `reportIndent`（`tableWidth` 54→56，模型列 22→24），quota `barIndent` 2→0（`barCells` 49→51，`detailWidth` 36→38）。每行左右空隙现在恒为 1 格（仅 `│ ` / ` │` 边框呼吸位），行宽仍恒 60 列；legacy 表格、JSON、CLI flag、标题行、配色、胶囊渐变逻辑零改动，未执行 git commit。

## 背景
- 任务书 [MARK-WORKER-PRISM-SYMMETRIC-GUTTER]：卡片内正文左侧 2 格缩进 + 右侧 1 格呼吸位 = 左右空隙 2:1，视觉上内容偏左、右侧空一块；要求两侧对称 1:1，范围限于两卡片的渲染与其连带测试/留痕。
- 明确排除项：`planusage.RenderTable`/`RenderTableAt`（HTTP `format=table` 的 legacy 路径仍用它自身的 2 格缩进）、JSON 输出、CLI flag、标题行、颜色、胶囊渐变方向。

## 决策
### usage 卡片（internal/usage/report.go）
- 删除 `reportIndent` 常量及所有引用：汇总行、表头行、`strings.Repeat("─", …)` 细分隔行、明细行（`joinCells`）、no-data 行均不再前置 2 格；`pal.body(...)` 仍是 `Dim("│ ") + PadRight(content, 56) + Dim(" │")`。
- `tableWidth = reportInner = 56`（原 `reportInner - len(reportIndent)`）。group 列预算公式 `tableWidth − req(6) − cache(6) − hit(17) − gaps(3)` 不变，单键模型列因此 22→24，多键均分也 +2（两键 11/10→12/12）。
- 模型列 20 列上限（`modelMaxWidth`）保持：22 格预算本来就宽于上限，改动前后截断行为一致。
### quota 卡片（internal/planusage/report.go）
- `barIndent` 2→0：胶囊行与明细行去掉前导 2 格；`cardBarRow`/`cardDetailRow` 的 `strings.Repeat(" ", barIndent)` 随之删除。
- 几何常量公式不变、取值自动更新：`barCells = cardInner(56) − barIndent(0) − pctWidth(4) − barGap(1) = 51`（49→51）、`detailWidth = cardInner(56) − barIndent(0) − resetWidth(18) = 38`（36→38）。
- `cardBody`（footer/note 行）本就无缩进，未动。卡片几何 doc 注释的示例图与 49/36 数字同步更新为新值。

### barCells 49→51 的取值原因（留痕）
- **先扣标签列再给条长**的口径不变：胶囊长度 = 内宽 − 缩进 − 百分比标签 − 条与标签的间隔。缩进归零后这 2 格全部让给了胶囊，条长 49→51，百分比列（4 格 `pctWidth`）仍是**第一个被预留**的列，因此「标签永远在、条长永远够」的取舍没有变。
- 取 51 而非「条长 +2 同时把 pct 加宽」：标签宽度由 ` 34%`/`100%` 的最宽值钉死（4 列），没有理由为 2 格余量改标签列；把余量给胶囊，单位格的占比从 100/49≈2.04 % 变为 100/51≈1.96 %，渐变更细。
- 仍是**奇数**个格子（49→51），`CapsuleLevel(last) = 100`（`(i+1)*100/cells` 整除）的既有性质保持；健康平绿段（`level ≤ 40`）仍是 20 格（`(i+1)*100/51 ≤ 40` → i ≤ 19），渐变节点位置不变，只是尾部多 2 格暖色空间。
- 每行宽度不受影响：`2 + 51 + 1 + 4 + 2 = 60`，`2 + 38 + 18 + 2 = 60`，与缩进版 `2+2+49+1+4+2 = 60`、`2+2+36+18+2 = 60` 等价——这是一次纯「左右再分配」，不是宽度改动。

## 被放弃的方案（必填）
- **保留 2 格缩进、只把右侧呼吸位加宽到 3 格**：否决。行内容会进一步右移，卡片可用内宽反而更少，且「右侧留白变大」并不解决内容偏左的观感；1:1 对称的正确做法是拿掉多余的那一侧。
- **缩进归零后把 2 格加到百分比标签列**：否决。`pctWidth=4` 由最宽标签 `100%` 钉死，加宽只会让 ` 34%` 更偏左；且会改变 `barGap`/`pctWidth` 的既有语义与全部几何注释，收益仅是「看起来没变」。
- **两张卡片改成左对齐到边框、右侧呼吸位保留 1 格（只改 usage）**：否决。两张卡片同源同几何（同一 `cardWidth`、同一边框词表），只改一张会造成 usage/quota 并列输出时缩进不一致。

## 连带测试改动（定性：位置/格数断言同步，未放松任何断言）
- `internal/usage/report_test.go`：`TestRenderUsageReportExact` 整卡精确串（标题行 dash 数不变，正文行整体左移 2 列、列宽 +2）、`missing denominator` 的 `-` 占位串、多键预算注释 21→24、单键预算注释 22→24、`54`→`56` 的表面积注释。
- `internal/usage/handler_test.go`：406 行 `TestHandlerTableFormat` 整卡精确串同步；新增 `hasModelRow` 助手替代原先靠 `"  cur"` 双空格缩进定位数据行的写法（`TestHandlerDefaultFromWeek`/`TestHandlerToOnlyRange`/`TestHandlerTableOverviewAllHistoryDefaulted`）——现在从 `tableDataRows` 取行后按**首列全等**判模型，比原来的「正文含双空格+模型名」更严格，不是放宽。
- `internal/planusage/report_test.go`：`cardBarCells` 49→51；胶囊行/明细行前缀断言 `"│   "`→`"│ "`；`TestRenderCardsCapsuleGeometry`、`TestRenderCardsBasic`、`TestRenderCardsWindowCards`、`TestCapsuleGradientInBar` 的格数（34 %→18、59 %→31、94 %→48、99/100 %→51）与 `TestCapsuleUsedCells`（`ceil(pct/100*51)`）、`TestCapsuleLevel(0)=1` 同步。
- `internal/render/capsule_test.go`：共享原语测试里作为示例的 quota 49 格几何改为 51 格（`CapsuleUsedCells`/`CapsuleLevel`/字形宽度三处）；`assertCardWidth`/`cardLines`/`DisplayWidth` 的 60 列断言一处未动。

## 验证
- `go build -o prism ./cmd/prism`、`go vet ./...`、`go clean -testcache && go test ./... -count=1`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全过。
- `./prism usage`、`./prism quota` 管道输出实测：正文行左右各 1 格空隙（`│ ` + 内容 + ` │`），列列对齐，每行 60 列。

## 来源
- 任务书 `[MARK-WORKER-PRISM-SYMMETRIC-GUTTER]`（worker 实拉，未执行 git commit）；前置几何见 `20250923-quota-tui-card-format.md`、`20260922-quota-capsule-bar.md`、`20260922-usage-capsule-report.md`、`20260922-usage-quota-cn-text.md`。
