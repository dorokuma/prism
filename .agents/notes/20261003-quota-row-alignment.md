---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage
---

# quota 卡片行内对齐：百分比与指标段改左对齐，模块间距恒 1 空格、卡片常规宽度回到 58

## 一句话结论
- `clineRowLine` 的**百分比与指标段不再右对齐进定宽字段**（原 `render.PadLeft(pctLabel(...), 4)` / `render.PadLeft(clineMetricField(...), 13)`）——两段都按**自身自然宽度左对齐**，模块之间恒为 **1 个空格**；被删的 `clineMetricGap = 4` 一并退场。
- 行宽改由**行尾补齐**收口：`fill = width − DisplayWidth(row) − 2`（clamp ≥ 0）补在「最后一个段」与右边框 `│` 之间，所以**每行显示宽度恒 = 当次渲染的卡片宽度**、右边框永不漂移。
- 连带结果：`clineRowFixed` **48 → 45**（`1 + clineCapCells + 1 + clinePctWidth + 1 + clineNumberWidth + 2`），卡片常规宽度 **61 → 58**（`4 + clineRowFixed + clineNameColMin = 4 + 45 + 9 = 58`）；**进度条仍 23 格、名字列仍 9 列下限、进度条起始列仍为第 15 列（其前恒 14 列）、账号名仍不截断**。
- 用户可见变化：`%` 与估算值之间的可见空格由「**4 ~ 16 格随值长浮动**」变为**恒 1 格**；代价是**数值较短的行走右端到边框的留白更长**（这是本笔明确接受的取舍，见下「背景」）。

## 背景
- 用户实测（线上 v0.36.2，卡片 61 列）发现 **SuperGrok 与 ClinePass 的行里「进度条与估算值之间的间距不一样」**：`%` 与估算值之间的可见空格**从 4 到 16 格不等**。
- 根因：`pct = render.PadLeft(pctLabel(...), clinePctWidth)`（4 列右对齐）与 `metric = render.PadLeft(clineMetricField(...), clineNumberWidth)`（**13 列右对齐**）——值越短越被推到右边，于是 pct 右端与 metric 左端之间的可见间距随**值长**变化（`100%`+`1.7B/1.7B` 只剩 1 格；`0%`+`-` 则拉开到 16 格）。
- 用户明确要求**间距固定 + 边框宽度固定**（上一版的「4 空格」方案已被否掉）。物理上不可能同时做到「左右两端可见空白都固定 + 数值右端贴齐」——数值长度不同，右对齐必然把短值推向右侧、把间距交给值长决定；本笔的取舍是 **模块间距恒 1 空格 + 边框宽度恒 58**，代价是短值行右端留白更长。

## 决策
- **删 `clineMetricGap`**（连同其注释）：行拼接回到 `capsule + " " + pct + " " + metric`，两个模块边界各 1 个空格。
- **百分比左对齐**：`pct = pctLabel(*r.win)`（自然长度 3~4 列），不再 `PadLeft(..., clinePctWidth)`。
- **指标段左对齐**：`metric = render.Truncate(clineMetricField(*r.win, now), clineNumberWidth)`——自然宽度，只用 `Truncate` 保留「超宽即截断、绝不顶出边框」的硬保证（真实窗口最长 token 对 = 13 列，`Truncate` 是防御性 guard，不是补齐）。
- **常量语义变更（保留而非删除）**：`clinePctWidth = 4` / `clineNumberWidth = 13` 不再是「补齐宽度」，改为**段宽预留**——经过 `clineRowFixed` 决定卡宽必须为「最宽的一行」留足列数（否则 `100%` + 13 列 token 对的行会顶破 58 列边框），并作为**无窗口失败行**的空格占位宽度。`clineRowFixed` 改为 `1 + clineCapCells + 1 + clinePctWidth + 1 + clineNumberWidth + 2 = 45`（分隔空格的计数方式随之重写）。
- **行尾补齐**：`clineRowLine` 先拼出 `│ ` + 色点 + 名字段（`pad+1` 填到渲染全局名字列）+ 胶囊 + 空格 + pct + 空格 + metric，再按 `fill = width − DisplayWidth(row) − 2`（`fill < 0` 时 clamp 到 0）补空格，最后接 ` │`。`pad` 公式（`width − 4 − clineRowFixed − DisplayWidth(cell)`）逐字未动，只是 `clineRowFixed` 变小，故**进度条起始列 = `width − clineRowFixed + 1 = 14` 列之前**（第 15 列）保持不变。
- **无窗口失败行**：`r.win == nil` 时 pct 与 metric 各输出 `clinePctWidth` / `clineNumberWidth` 列空格占位，使该行的可见结构与有数据的行一致（与「各自留空 + 行尾补齐」在字节上等价——都是空格；显式占位让「段边界」在代码层面可读、也让预留常量有第二个真实用途）。
- **测试同步（只改断言，未新增用例/新文件）**：`rowTail` helper 由「`PadLeft` 两个右对齐字段」改为「自然宽度 + 行尾 fill」（`fill = clinePctWidth + clineNumberWidth − DisplayWidth(pct) − DisplayWidth(metric)`，即预留列数减去两段实际占用）；`TestRenderCardsCapsuleGeometry` 的 `wantPct` 去掉右对齐前导空格（`"  0%"` → `"0%"` 等 7 处）；`TestRenderCardsClinePassExhaustedShowsXOverX` 的 `want` 去前导空格并改走 `rowTail("100%", …)`；`TestRenderCardsUniformWidthAcrossProviders`、`wantCardWidth` 注释与卡片用例节头的行格式说明 61 → 58。**`^func Test` 计数 HEAD 与工作区一致（report_test.go 50 / metapi_test.go 8）**。
- **门禁与取证**：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`scripts/test_*.sh`（5 个脚本 rc=0）、`python3 scripts/test_generate_mcp_tools.py` 全绿，`gofmt -l` 对触碰文件无输出；实拍本机 `prism quota --no-color` 六卡（当前工作区构建，产物在 `/tmp`）：21 行（9 数据行 + 12 边框行）display 宽度**全 = 58**、进度条**恒 23 格**、起始列**恒第 15 列**、`进度条→百分比` 与 `百分比→指标段` **逐行恒 1 空格**（测量脚本 python `east_asian_width`，与 `internal/render.DisplayWidth` 同口径）；**变异自证**：临时删掉行尾补齐后 `TestRenderCardsBasic` 立刻 FAIL（`line 1: width 52, want the render-wide 60`），恢复后 sha256 与 `diff -q` 逐字节相同。

## 被放弃的方案（必填）
- **① `clineMetricGap = 4` 的四空格方案（v0.36.2 已上线，被用户否掉）**：它只把「pct→metric」这一处拉宽，**没有**解决「间距随值长变化」——右对齐下 `100%`+13 列值的可见间距仍是 1 格、`0%`+`-` 仍是 16 格，只是把基准从 1 提到 4；且它让卡宽涨到 61（终端宽度代价）却没换来定值间距。否决。
- **② 只把指标段左对齐、百分比保持右对齐**：`进度条→百分比` 的间距仍随百分比值长变化——百分比右对齐进 4 列字段时，值在字段内被推向右，胶囊→`%` 的可见空格在 1~3 格间浮动（`100%` 1 格、`0%` 3 格），不满足「模块间距恒 1 空格」。否决。
- **③ 保留两段右对齐（右端贴齐）**：右端齐了，但**间距随值长变化**正是用户报的 bug；「左右两端可见空白都固定 + 数值右端贴齐」在数值定宽前提下物理不可同时成立。否决。
- **④ 让卡片宽度随内容变（不预留 pct/metric 宽度）**：短值行会连边框一起缩，破坏「一次渲染一个宽度 / 三家卡片逐列对齐」的既有不变量（`20261003-quota-card-uniform-width.md`），也会让同一条命令的卡宽随取到的值跳动。否决。
- **⑤ 把 pct/metric 各自 `PadRight` 到定宽（左对齐 + 定宽字段）**：与最终实现**字节等价**（补的空格落在同一位置），但它把「百分比→指标段」的空格计入 pct 字段，`34%` 后会出现 2 格（1 个分隔 + 1 个补齐），与「模块间距恒 1 空格」的实测口径不符，也让「无定宽字段」的实现说明不成立。否决，采用「两段自然宽度 + 行尾 fill」。

## 与既有笔记的取代范围（部分取代）
按仓库既有的「部分取代」先例处理（先例见 `20261003-quota-card-uniform-width.md` 的「遗留清单」节与 `20261003-quota-metric-gap.md` 的同名节：旧笔记保持 `status: active`、`superseded_by: ""`，由新笔记正文说明取代了哪一部分、哪一部分仍成立；本篇 frontmatter 的 `supersedes` 因此保持 `""`，**不整篇标 superseded**，也**不改旧笔记正文**）：
- `20261003-quota-metric-gap.md`：其**「间距 1 → 4 空格、卡宽 58 → 61」这一整节被本笔取代**（`clineMetricGap` 已删除、卡宽回到 58）；其**「间隔必须一眼可见地分开两个字段」的问题陈述与「不缩进度条、不从名字列省列、不做成可配置」三个否决理由仍成立**（本笔同样不缩胶囊、不动名字列下限、不引入配置面）。
- `20261003-quota-card-uniform-width.md`：**卡宽公式 `4 + clineRowFixed + clineNameColMin` 回到本笔记的 58 列结论**（`clineRowFixed` 在此笔为 45、`4 + 45 + 9 = 58`）；其**机制层面结论全部仍成立**（「一次渲染只产出一个宽度」「`clineNameColMin` 是下限不是上限」「长名不截断、所有卡一起变宽」「`clineCardWidth` 只是单卡需求」）。
- `20261003-quota-countdown-text-and-order.md`：其**「指标段 13 列 `clineNumberWidth` + `PadLeft` 右对齐」「每张卡仍 58 列」两句被本笔取代**（改为左对齐自然宽度，58 列结论按新布局重新成立）；其**倒计时去后缀文案与 `providerDisplayOrder` 展示序结论仍 active**。
- `2026-10-03-unified-quota-cards-spec.md`：其**「百分比 4 列右对齐 / 指标段 13 列右对齐 + `PadLeft` 截断」的列对齐段被本笔取代**，其余（单行式版式、智能单指标规则、失败卡语义、版式不按 provider/窗口分叉）仍 active。

## 遗留（未决 / 需后续处理）
- **`internal/usage` 的 `reportWidth = 56` 与 quota 卡 58 是否统一仍待用户决策**：本次只动 quota 侧（`internal/planusage`），usage 报告仍是定宽 56 列，差 2 列。沿用 `20261003-quota-card-uniform-width.md` / `20261003-quota-metric-gap.md` 的遗留清单，结论与未决状态不变（本笔只把差值从 5 收敛回 2）。
- **`.agents/notes/README.md` 是否补写「部分取代」的规范写法**：仓库维护规矩仍只有 `active` / `superseded` 二元状态，没有「旧结论仍部分有效、只有一部分被新笔记取代」的写法；本笔又是**一例跨版本取代**（取代 `20261003-quota-metric-gap.md` 的间距+卡宽节，同时把宽度结论**回退**到更早的 `20261003-quota-card-uniform-width.md`），已按既有先例处理（旧笔记保持 `status: active`、`superseded_by: ""`）。本次**未改** `.agents/notes/README.md`，**待用户决策**。

## 来源
- 用户要求：quota 卡片「进度条与估算值之间的间距要固定、边框宽度要固定」，并明确指出 4 空格方案不再讨论；实测线上 v0.36.2 的可见间距为 4~16 格（随值长浮动）。
- 相关实现：`internal/planusage/report.go`（`clinePctWidth` / `clineNumberWidth` / `clineRowFixed` / `clineRowLine` / `pctLabel` / 卡片几何与统一卡片节注释）、`internal/planusage/report_test.go`（`rowTail` / `wantCardWidth` / `TestRenderCardsCapsuleGeometry` / `TestRenderCardsClinePassExhaustedShowsXOverX` / `TestRenderCardsUniformWidthAcrossProviders`，仅断言）。
- 实拍证据：本机 `/var/lib/prism/config.yaml` 下 `prism quota --no-color`（当前工作区的构建，产物在 `/tmp`，用完删除）——六卡 21 行 display 宽度全 = 58、进度条 23 格且起始列第 15 列、两处模块间距逐行恒 1 空格；测量脚本（python `east_asian_width`）与 `internal/render.DisplayWidth` 同口径；变异自证：删掉行尾补齐 → `TestRenderCardsBasic` FAIL（width 52 / want 60），恢复后 sha256 与 `diff -q` 逐字节相同。
- 相关笔记：`20261003-quota-metric-gap.md`（本笔取代其「间距 1 → 4 空格、卡宽 58 → 61」节）、`20261003-quota-card-uniform-width.md`（宽度结论回到其 58 列公式）、`20261003-quota-countdown-text-and-order.md`（取代其「指标段 13 列右对齐」「每张卡仍 58 列」两句）、`2026-10-03-unified-quota-cards-spec.md`（单行式几何规范与「版式不按 provider/窗口分叉」）。
