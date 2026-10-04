---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: usage, render, planusage
---

# quota 卡片收尾三项：windowUsedFraction 表驱动单测、极小池两位小数、usage 报告缩到 45 列

## 一句话结论
- **① 补 `TestWindowUsedFraction`**（加进既有 `internal/planusage/estimate_test.go`，未新开测试文件）：表驱动纯函数单测，把「所有池反推的分母口」的各分支与异常边界逐行钉死——`Percent=0 → 0`（中点例外）、`Percent 1/2/50/99 → 0.015/0.025/0.505/0.995`、`Percent>=100 → clamp 到 1`、负 `Percent → 0`、`UsedFraction>1 → 0`、`UsedFraction<=0 → 回落整数中点`、`UsedFraction>0 → 小数路径逐字优先`。
- **② 极小池与无数据在界面上分开**：`render.FormatTokensYi` 在**一位小数会塌成 0** 且值非零时改用**两位小数**（`2.2M → 0.02亿`、`500k → 0.01亿`），`0亿` 只留给真正的零（以及 `<0.005亿`，两位小数仍为 0 时保持原状）。13 列预算**未被撑破**：`0.02亿` 与 `9999亿` 同为 6 显示列，最坏组合 `0.02亿/9999亿` = 13。
- **③ usage 报告 56 → 45 列**，与 quota 卡同宽：`reportWidth = 45`（`reportInner` / `tableWidth` 随之 41），边框、分组标题、列预算全部按新宽度重算，`internal/usage` 测试不变量同步为「每行恰好 45 显示列」。quota 卡本轮未动。
- **三条均由用户拍板「当场修掉、不要遗留条目」**：`20261004-quota-card-5h-two-metrics.md` 的「双审观察项」小节整段删除，②③ 及整数百分比相关的未决条目改为「已修复」，本文档承接全部细节。

## 背景
- 双审（reviewer + oracle）放行后留下三条观察项：① 分母函数只被端到端用例间接覆盖；② 小池 `0亿/0亿` 与无数据 `-` 撞车；③ usage 56 列与 quota 45 列差 11 列。
- 用户第一次要求把它们落进遗留清单（commit `22b7308`），随后改为**直接修复**，并要求把遗留条目清理干净。
- ③ 会推翻 `20261003-usage-width-stays-56.md` 的「usage 维持 56 列不动」定案——该定案由用户在 **quota 58 列**时代拍下，本轮 quota 已改为 45 列，差值从 2 变成 11，「不统一」的代价已大到用户改主意。

## 决策

### ① `TestWindowUsedFraction`（纯函数表驱动单测）
- 位置：`internal/planusage/estimate_test.go`（`estimate_test.go` 的 `^func Test` **26 → 27**；**未新开测试文件**）。
- 覆盖矩阵（15 个子用例，浮点按 `1e-12` 容差比较）：
  - `Percent=0`（frac=0）→ `0`：中点例外。若中点作用于 0，一个 token 的流量会反推出 200 倍的池。
  - `Percent=1/2/50/99`（frac=0）→ `0.015/0.025/0.505/0.995`：区间中点 `(P+0.5)/100`。
  - `Percent=100/101/150`（frac=0）→ `1`：clamp；上游把 `percentUsed` 钳在 100%，区间顶端塌缩到 1，耗尽窗口仍走 T/T。
  - `Percent=-5`（frac=0）→ `0`：负百分比不得进入中点分支、不得产生负分母。
  - `Percent=34, frac=-0.2` → `0.345`：非正 fraction 回落到整数中点。
  - `Percent=34, frac=0.34` → `0.34`：小数路径逐字优先（Gemini `remainingFraction` 反转）。
  - `Percent=0, frac=0.004` → `0.004`：小数路径连 `Percent==0` 也覆盖。
  - `Percent=100, frac=1` → `1`；`Percent=34, frac=1.5` → `0`（guard）。
- 该函数被 `ApplyClinePassEstimates`（5h/weekly/monthly）、`ApplyWeekEstimate`（weekly/5h）、`ApplyGrokWeekEstimate` 共用，**一份单测守住三个 provider 的分母**。

### ② 极小池两位小数
- `render.FormatTokensYi`：先按一位小数渲染并去尾零；**若结果是 `"0"` 且原值非零**，改用两位小数再去尾零。因此
  - `0 → 0亿`（只有真正的零）
  - `<0.005亿`（`499_999`）→ `0亿`（两位小数仍为 0，保持现状）
  - `500_000 → 0.01亿`、`2_000_000 → 0.02亿`、`4_000_000 → 0.04亿`、`4_999_999 → 0.05亿`
  - `>=0.05亿`（`5_000_000`）起回到一位小数：`0.1亿`、`0.3亿`、`13亿`、`9999亿`
- **13 列预算核算**：`亿` 是宽字符（2 列），故 `9999亿` = 4+2 = 6 列、`0.02亿` = 4+2 = 6 列、`0.04亿` = 6 列。两位小数形态的前缀恒为 `0.NN`（4 列），不会出现 `NN.NN亿`；因此**最坏组合 `0.02亿/9999亿` = 6+1+6 = 13 列**，与原来的 `9999亿/9999亿` 相同，`clineNumberWidth = 13` 不动，卡片几何零变化。
- `numbers.go` 注释同步（说明两位小数兜底、`0亿` 的归属、以及「不会撑破 13 列」的推导）；`numbers_test.go` 的 `TestFormatTokensYi` 扩到 17 个用例并新增「最坏组合恒 13 列」的成对断言。
- **效果**：卡片上「`0.02亿/0.02亿`」是**有小池**，「`-`」是**无数据**，二者不再撞车；用户否掉的 `<0.1亿` 兜底文案仍不引入。

### ③ usage 报告 56 → 45 列
- `internal/usage/report.go`：`reportWidth 56 → 45`，`reportInner`/`tableWidth` 随之 52 → 41（均为派生常量，无第二处硬编码）；`handler.go` 的 56-column 注释同步；版式注释、ASCII 卡片图、Geometry 段、`titleLine` 的 `descMax` 注释（50 → 39）全部重写。
- 固定列未动（`reqWidth`/`cacheWidth` = 6，`hitWidth` = 10+1+6 = 17）——它们分别由 `FormatTokens` 的进位链宽度与「命中率胶囊 10 格」的既定设计决定，缩它们会截断真实数值。
- **代价（ accepted，用户知情）**：单分组键的列预算从 20 塌到 **9 列**（41 − 6 − 6 − 17 − 3），`modelMaxWidth = 20` 从此**不再成为约束**（常量保留为「将来卡片变宽时的上限」，注释已写明它现在是 no-op）。多分组键更紧：2 键 → **4/4**，3 键 → 3/2/2，4 键 → 2/2/1/1，5 键 → 1/1/1/1/1；`供应商` 表头在 2 键视图里会显示成 `供…`。这是 45 列换来的同宽代价。
- `internal/usage` 测试同步（只改断言，`^func Test` 计数不变）：
  - `TestRenderUsageReportExact` / `TestHandlerTableFormat` 的整卡逐字节断言改为 45 列版本（标题 fill、├/╰ 边框 54 → 43 dash、`deepseek…`、`glm-5.2` 行的列位）。
  - `TestRenderUsageReportStructure` 的 ├/╰ 字面量同步。
  - `TestRenderUsageReportModelColumnTruncation`：`claude-sonnet-4`（16 列）→ 现截断为 `claude-s…`；`very-long-model-name-xyz` → `very-lon…`；CJK 名 → `自定义超…`。
  - `TestRenderUsageReportMultiGroupKeys`：2 键预算 19(10/9) → 8(4/4)，值显示 `gpt…`/`ope…`/`glm…`/`z-ai`、表头 `供…`。
  - `TestRenderUsageReportGroupColumnBudget`：模型列断言从「cap == modelMaxWidth(20)」改为「width/cap == 9/9 且 modelMaxWidth >= 9」。
  - `TestRenderUsageReportTitleTextIsPlainText` 注释里的 `3 + 10 + 1 + 41 + 1 = 56` → `3 + 10 + 1 + 30 + 1 = 45`。
  - `TestReportColumnsFillTheTableArea` 注释 52 → 41（断言本身用 `tableWidth`，无需改）。
- 连带两处 CLI/HTTP 测试的模型名断言（`internal/usage/handler_test.go` 的 `gemini-3.7-flash`、`cmd/prism/agy_test.go` 的同名）改为 9 列截断形态 `gemini-3…`。

## 被放弃的方案（必填）
- **①「只补端到端数字断言、不补纯函数单测」**：上一轮的实际状态——边界回归只能从「某个窗口的总量变了」反推，且三个 provider 共用同一分母，定位成本高。否决（本笔补纯函数单测）。
- **② 两位小数兜底扩展到所有小值（取消 `0亿`）**：`<0.005亿` 显示两位小数仍是 `0.00`，硬显示 `0.00亿` 只会把「没有数据」伪装成「有 0.00 亿」。否决；两位小数仍为 0 时保持 `0亿`。
- **②b 用 `<0.1亿` / `<1M` 之类文案**：用户在上一轮已明确否掉。否决。
- **③ 把 `reqWidth`/`cacheWidth`/`hitCells` 一并缩小来给分组列让路**：`FormatTokens` 的进位链最宽 6 列（`999.9T`）、命中率胶囊 10 格是 `20260922-quota-capsule-bar.md` 的既定设计，缩它们会截断真实数值或改掉另一处已审设计。否决；只缩卡宽，让分组列承担代价。
- **③b 只缩 usage、不动 `modelMaxWidth` 的注释**：留着「模型名超过 20 列截断」的旧注释会说谎（9 列就截断了）。否决，注释改写为「上限已不可达」。
- **③c 同时把 quota 卡改宽以迁就 usage**：用户明确「quota 卡本轮保持不动」。否决。

## 与既有笔记的取代范围（部分取代）
按仓库既有先例处理（旧笔记保持 `status: active`、`superseded_by: ""`，由本篇正文声明取代范围；本篇 `supersedes` 保持 `""`，**不改旧笔记正文**）：
- `20261003-usage-width-stays-56.md`：其**「usage 报告维持定宽 56 列、不与 quota 卡片同宽」的核心定案被本笔整体取代**（usage 现为 45 列，与 quota 卡同宽）。按其「被放弃的方案①②③」的理由复盘：①「两条命令不同屏」仍成立，但用户本轮以「同宽」为目标明确要求统一；②「quota 58 列是刚定稿的版式结论」中的 58 已不存在（quota 现为 45）；③ 动态宽度方案仍未采用。**该笔记的 frontmatter 由本笔按仓库维护规矩标记为 `status: superseded` + `superseded_by: 20261004-quota-card-followups.md`**（只加链接、不改写其历史结论）。
- `20260924-card-width-60-to-56.md`：其**「52 列实测不可行（分组列预算塌到 16 列）、56 为 52..60 的最小可行宽度」的结论被本笔取代**——45 列比 52 更窄且已上线，分组列预算为 9 列（单键）。其「usage/quota 两卡同宽」的初衷反而由本笔重新达成（同为 45）。
- `20261003-quota-card-uniform-width.md` / `20261003-quota-metric-gap.md`：其**「usage 56 与 quota 卡是否统一」的未决项由本笔关闭**（统一到 45）；其余宽度机制结论（一次渲染一个宽度、名字列下限、长名不截断）仍 active。
- `20261004-quota-card-5h-two-metrics.md`：其「遗留」节中**「`亿` 粒度的读不出」「`reportWidth` 56 与 quota 卡 45 的差值」「整数百分比的精度风险」三条改为「已修复」**，并由 `22b7308` 追加的**「双审观察项（收尾）」整节删除**（三条已当场修掉，不再作为遗留）；该笔记其余结论（版式、倒计时、5h 反推、中点修正）全部仍 active。

## 遗留（未决 / 需后续处理）
- **多分组键视图已到可用性下限**：2 键 4/4、3 键 3/2/2、5 键 1/1/1/1/1，`供应商` 表头会变成 `供…`。45 列是用户拍的同宽目标，若后续要多键视图可用，方向是「多键视图另给一版布局」或「窄终端下提示用 `--json`」，而不是回退宽度。
- **`<0.005亿` 仍读 `0亿`**：与「真正的 0」在视觉上仍同形。这是用户认可的原状（两位小数也分不出），若要区分只能上「整数词元 + 单位」的结构性改法。
- **`modelMaxWidth = 20` 现为不可达上限**：保留是为将来卡片变宽时省一次改动；若认为死常量应删，可单开一笔清理（会连带删 `reportColumns` 的 `capWidth` 分支与一条断言）。

## 来源
- 用户指令：三条双审观察项「当场修掉、不要遗留条目」，其中 ③ 明确「usage 从 56 缩到 45 与 quota 同宽，quota 本轮不动」。
- 相关实现：`internal/render/numbers.go`（`FormatTokensYi` 两位小数兜底）、`internal/render/numbers_test.go`、`internal/planusage/estimate_test.go`（`TestWindowUsedFraction`）、`internal/usage/report.go`（`reportWidth` 45 与全部布局注释）、`internal/usage/handler.go`（注释）、`internal/usage/report_test.go`、`internal/usage/handler_test.go`、`cmd/prism/agy_test.go`。
- 相关笔记：`20261004-quota-card-5h-two-metrics.md`（清理对象）、`20261003-usage-width-stays-56.md`（被本篇取代）、`20260924-card-width-60-to-56.md`（「52 列最小可行宽度」结论被本篇取代）、`20260922-quota-capsule-bar.md`（命中率胶囊 10 格的由来，未动）。
