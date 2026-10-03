---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage
---

# quota 卡片「百分比 ↔ 指标值」的间隔 1 → 4 空格，卡片常规宽度随之 58 → 61

## 一句话结论
- `clineRowLine` 里「百分比字段」与「指标段」之间的间隔由 **1 个空格加宽到 4 个空格**（新增常量 `clineMetricGap = 4`），两段文本读起来是两个字段而不是一整句。
- 连带必然结果：每行 **+3 列**，`clineRowFixed` 45 → 48，卡片常规宽度 **58 → 61**（`4 + clineRowFixed + clineNameColMin = 4 + 48 + 9 = 61`）；最小舒适终端宽度随之由 58 变为 **61 列**（56 / 57 列终端早已会折行，这条沿用 v0.36.1 的结论）。
- 其余几何逐项照旧：进度条仍 **23 格**（不缩）、名字列仍 **9 列**下限、名字与进度条之间的间隔不变、百分比字段仍 4 列右对齐、指标段仍 13 列右对齐、账号名仍不截断；进度条起始列前的列数 = `width − clineRowFixed + 1 = 14`，**改前改后相同**。

## 背景
- 用户要求：quota 卡片数据行里「百分比」（如 `97%`）与其右侧的「指标值 / 倒计时」（如 `180.2M/185.8M`、`3h 45m`）之间的间隔拉开——「1 个空格 → 4 个空格」，让两个字段的分隔一眼可见。
- 改前 `clineRowLine` 的行拼接是 `… capsule + " " + pct + " " + metric + " │"`：pct（4 列右对齐）与 metric（13 列右对齐）之间只有 1 列，紧邻的右对齐数字与百分比看上去连成一段。
- 卡片几何是**单一来源**：行宽、卡宽、名字列填充与进度条起始列全部由 `clineRowFixed` 推导（`clineRowLine` 的 `pad = width − 4 − clineRowFixed − DisplayWidth(cell)`；`clineCardWidth` / `cardWidth` 的 `4 + clineRowFixed + …`）。因此「间隔 +3」不是一处字符串改动，而是**一个几何常量的改动**，所有推导式与注释一并跟着走。
- 几何常量改动属「用户可见文本契约变更」（`AGENTS.md` 要求留痕：卡片列宽是人眼可见的输出契约，逐字节断言卡宽/列位的刮取式调用方会受影响）。

## 决策
- **只加间隔，不缩进度条**：新增 `clineMetricGap = 4`（`internal/planusage/report.go` 的几何常量块），行拼接改为 `capsule + " " + pct + strings.Repeat(" ", clineMetricGap) + metric`；`clineRowFixed = 1 + 3 + clineMetricGap + clineCapCells + clinePctWidth + clineNumberWidth = 48`（dot 1 + 3 个单列间隔 + pct/metric 间隔 4 + 胶囊 23 + pct 4 + metric 13）。
- **卡宽 58 → 61**：`clineNameColMin = 9` 与所有推导式（`clineCardWidth` / `cardWidth` / `pad`）**未动**，`4 + 48 + 9 = 61` 成为常规输出的新宽度；这是「每行 +3 列」的必然结果，不是另设的目标值。
- **进度条起始列不变**：该列前的列数 = `width − clineRowFixed + 1 = 61 − 48 + 1 = 14`，与改前（`58 − 45 + 1 = 14`）相同——名字列与「名字 ↔ 进度条」的间隔都没动，三家 provider 卡片仍逐列对齐。
- **注释同步**（`internal/planusage/report.go`）：顶部卡片几何示例（`gemini-acct` 11 列 → 63 列卡）与统一卡片节的示例（`cline-user` 10 列 → 62 列卡）重算为「4 空格间隔」下的真实宽度；行格式说明由「every element separated by one space」改为「除 pct→metric 间隔（4 列）外每个元素之间一个空格」；`Row: … = clineRowFixed + n + 4 columns`、`4 + clineRowFixed + 9 = 61`、`clineRowFixed` 的分解式一并更正；`clineRowLine` 的文档注释写明该间隔为 `clineMetricGap = 4` 列。
- **测试同步（只改断言，不新增用例/新文件）**：`internal/planusage/report_test.go` 的 `rowTail` helper（各行尾断言的公共来源）里的 `" "` 改为 `strings.Repeat(" ", clineMetricGap)`；`TestRenderCardsUniformWidthAcrossProviders` 的宽度期望与注释 58 → 61；`wantCardWidth` 注释里的 58 → 61。**未新增任何 `func Test`，未新增测试文件**（本次是纯版式常量改动，不含新逻辑分支）。
- **门禁与取证**：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`scripts/test_*.sh`、`python3 scripts/test_generate_mcp_tools.py` 全绿，`gofmt -l` 对触碰文件无输出；实拍 `prism quota --no-color` 9 行数据「百分比后 4 列空白」逐行成立、9 条边框行 display 宽度全 = 61、进度条起始列全 = 14；变异自证：把 `clineMetricGap` 临时改回 1，`TestRenderCardsUniformWidthAcrossProviders` 立即 FAIL（`width 58, want 61`），改动确实被测试守护。

## 不变清单（避免误读）
- **`resetText` 的可达路径仍是两条**：① 5 小时窗口行的指标段；② 周/月「拿不到 token 总额」时的回落倒计时。**无窗口失败快照（`r.win == nil`）的指标段是空格、不经过 `resetText`**。本次几何改动不改这三者中的任何一个，`已重置` / `-` 与周/月 token 对文案逐字未动，5 小时行仍无「后重置」后缀。
- 进度条 23 格（`clineCapCells`）、名字列 9 列下限（`clineNameColMin`）、名字与进度条之间的间隔、百分比 4 列（`clinePctWidth`）、指标段 13 列（`clineNumberWidth`）与 `PadLeft` 右对齐、「一次渲染一个宽度」的不变量、账号名不截断（含 `旧` 标记占名列宽）、`providerDisplayOrder`（Gemini < SuperGrok < ClinePass）全部未动。
- `--json` 字段与路径、`/admin/quota` JSON、`/metrics`、CLI flag、`Snapshot` / `Window` 模型、legacy `?format=table`（`RenderTable`/`formatRemain`）、`internal/usage` 的 56 列报告**均未动**。本次只动渲染层的间隔常量与其注释、以及同步的测试断言与文档。

## 与既有笔记的取代范围（部分取代）
按仓库既有的「部分取代」先例处理（先例见 `20261003-quota-card-uniform-width.md` 的「遗留清单」节：旧笔记保持 `status: active`、`superseded_by: ""`，由新笔记正文说明取代了哪一部分、哪一部分仍成立；本篇 frontmatter 的 `supersedes` 因此保持 `""`，**不整篇标 superseded**，也**不改旧笔记的正文结论**）：
- `20261003-quota-card-uniform-width.md`：其**「常规输出恒 58 列」这一部分被本笔取代**（58 → 61）；其**机制层面结论全部仍成立**——「一次渲染只产出一个宽度」、「`clineNameColMin` 是下限不是上限」、「长名不截断、所有卡一起变宽」、「`clineCardWidth` 只是单卡需求」。该笔记的「遗留清单」（`internal/usage` 仍 56 列、未决是否统一）**仍 active**。
- `20261003-quota-countdown-text-and-order.md`：其**「每张卡仍 58 列」那一句被本笔取代**（按 `.agents/notes/README.md` 的「旧笔记只加 superseded 链接、不改写旧结论」，该旧笔记正文保持入库原状、仍写作 58 列的历史事实）；其**倒计时去后缀文案与 `providerDisplayOrder` 展示序的结论仍 active**，`resetText` 两条路径与「无窗口失败快照不经过 `resetText`」的表述仍成立。

## 被放弃的方案（必填）
- **① 把进度条从 23 格缩到 20 格，以维持 58 列卡宽**：用户要的是「把间隔拉开」，不是「在别处省出同样的列数」；缩进度条会降低读数精度（23 格约合每格 4.3 %，20 格每格 5 %）并改变 `clineCapCells = 23` 这个已被实拍与断言锁定的刻度——为一个纯间距诉求牺牲一个已定稿的读数刻度，代价与收益不成比例。否决。
- **② 把指标段改成别的对齐方式（左对齐 / 不定宽）来「腾」出视觉间隔**：指标段的 13 列右对齐是「数字右对齐才好比较数量级」的既定契约（`clineNumberWidth` + `PadLeft`），左对齐会让 `52m` / `2h 31m` / `390.7M/685.4M` 这些值左缘参差、跨行难以对读；且「每个字段从固定列开始」正是三家卡片逐列对齐（`cardLines` 断言）的前提，改对齐方式会一并破坏它。否决。
- **③ 从名字列省列（把 `clineNameColMin` 由 9 降到 5–6）以维持 58 列**：名字列下限是「一次渲染只产出一个宽度」不变量的一部分（见 `20261003-quota-card-uniform-width.md`），降下限会让生产 roster 里的 `SuperGrok`（9 列）重新变成「比下限更长」的名字，短名 provider 独占一次渲染时卡宽又随名字缩水（同一条命令宽度跳动），等于把间距诉求转嫁给账号名的可读性。否决。
- **④ 把间隔做成可配置（CLI flag / 环境变量）**：本轮范围是「一个版式常量」，引入配置面要连带定义默认值、文档、测试与兼容口径，远超本次改动，而用户要求的是定值 4。否决。

## 遗留（未决 / 需后续处理）
- **`internal/usage` 的 `reportWidth = 56` 与 quota 卡 61 的差距拉大到 5 列**：本次只动 quota 侧（`internal/planusage`），usage 报告仍是定宽 56 列，「两卡同宽」是否统一（把 usage 也改成「按内容 + 名字列下限」的动态宽度，或把 quota 压回固定 56）**仍待用户决策**，本次未推进。沿用 `20261003-quota-card-uniform-width.md` 的遗留清单——该笔时为 56 vs 58（差 2），本笔加宽后为 56 vs 61（差 5），结论与未决状态不变。
- **`.agents/notes/README.md` 是否补写「部分取代」的规范写法**：仓库维护规矩目前只给 `active` / `superseded` 二元状态，没有「旧笔记结论仍部分有效、只有一部分被新笔记取代」的写法；本篇与 `20261003-quota-card-uniform-width.md` 都按既有先例处理（旧笔记保持 `status: active`、`superseded_by: ""`，由新笔记正文声明取代范围），oracle 建议把该写法正式补进维护规矩。本次**未改** `.agents/notes/README.md`，**待用户决策**。

## 来源
- 用户要求：quota 卡片「百分比与数值之间」的间隔由 1 个空格加宽到 4 个空格，并把这笔改动并入当前 v0.36.2 批次（含 README 条目与留痕）。
- 相关实现：`internal/planusage/report.go`（`clineMetricGap` / `clineRowFixed` / `clineRowLine` / 卡片几何注释）、`internal/planusage/report_test.go`（`rowTail` / `TestRenderCardsUniformWidthAcrossProviders` / `wantCardWidth` 注释，仅断言）。
- 实拍证据：本机 `/var/lib/prism/config.yaml` 下 `prism quota --no-color`（当前工作区的构建，产物在 `/tmp`）——9 行数据百分比后均为 4 列空白、9 条 `╭`/`╰` 边框行 display 宽度全 = 61、进度条起始列全 = 14；测量脚本（python `east_asian_width`）与 `internal/render.DisplayWidth` 同口径。
- 相关笔记：`20261003-quota-card-uniform-width.md`（本笔取代其「常规输出恒 58 列」一部分；统一宽度 + 名字列下限 + 不截断机制仍 active）、`20261003-quota-countdown-text-and-order.md`（本笔取代其「每张卡仍 58 列」一句；倒计时文案与展示序结论仍 active）、`2026-10-03-unified-quota-cards-spec.md`（单行式几何与「版式不按 provider/窗口分叉」规范）。
