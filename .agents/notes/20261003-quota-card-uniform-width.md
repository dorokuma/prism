---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage
---

# quota 卡片宽度由「每卡各算」改为「一次渲染全局统一 + 名字列 9 列下限」（三家 provider 卡片等宽 58）

## 一句话结论
- `internal/planusage` 的卡片宽度**不再按卡各自计算**：`RenderCards` 在分组后调用新增的 `cardWidth(groups)` 求**本次渲染所有 (provider, window) 卡**的最大需求宽度，向下取 `max(所有卡 clineCardWidth, 4 + clineRowFixed + clineNameColMin)`，然后把**同一个 width** 交给 `renderGroupCard`，标题行、每行数据、失败 note 行与上下边框全部按它排布。
- 新增常量 `clineNameColMin = 9`（名字列 display 列下限 = 当前已知最长账号名 `SuperGrok` 的长度），`4 + clineRowFixed + 9 = 58`，故**常规输出恒为 58 列**：三家都在场是 58，只有短名 provider 也是 58，不再忽宽忽窄。
- 用户可见变化：同一份数据下 ClinePass 卡 54 / Gemini 卡 55 / SuperGrok 卡 58 → **三张（六卡）全 58**，进度条起始列、右内边距、右边框列全部对齐。
- 名字**永不截断**：下限只是**下限**，出现比 9 列更长的名字时所有卡**一起变宽**（仍严格同宽），`clineRowLine` 的 `pad` 依旧按实测 `DisplayWidth` 计算、`pad < 0` 的 clamp 只是防御性 guard，不承担截断。
- 兼容红线**不变**：`--json` 字段与路径、`RenderTable`/`?format=table` legacy 表格、标题/窗口段规则（`windowTitle`、标题超长先缩窗口再缩 service）、失败卡语义（`StoreFailed`/`ErrorNote`/`⚠` note 行）、颜色与胶囊与倒计时逻辑、`internal/usage` 报告模块**均未动**；`clineCardWidth` 的语义收窄为「**这张卡自己的需求**」，跨模块复用为 0（`clineRowFixed`/`clineCardWidth`/`renderGroupCard`/`cardTitleLineAt`/`cardBodyAt`/`joinCardAt` 仅被 `internal/planusage` 自身引用）。

## 背景
- 用户实测（display 列数）：ClinePass 卡 = 54、Gemini 卡 = 55、SuperGrok 卡 = 58，三张卡左右边框参差。根因是宽度取自「该卡内最长账号名单元格」——`clineCardWidth = 4 + clineRowFixed + max(卡内 clineRowNameCell 宽度)`，卡与卡之间各算各的：账号名 `Cline`(5) / `Gemini`(6) / `SuperGrok`(9) 三档名字直接变成三档卡宽。
- 名字列若直接取全局长名，短名 provider 单独出现时卡会跟着窄下去（同一条命令不同运行之间宽度跳动）；若给名字列加固定下限，又必须保证长名不被截断，因此需要「全局统一 + 下限 + 允许一起变长」三者同时成立。

## 决策
- **宽度算法**（`internal/planusage/report.go`）：
  - `clineCardWidth(g)`：语义改为「**这一张卡自己的需求宽度**」，体不变（`4 + clineRowFixed + 卡内最长 name cell`，再与 `titleWidth(profile, window) + 6` 取大，保留标题下限以防短名把标题挤成省略号）。
  - 新增 `cardWidth(groups map[clineKey]*clineGroup) int`：`width := 4 + clineRowFixed + clineNameColMin`，遍历所有分组取 `max(width, clineCardWidth(g))`。取最大值与 map 迭代顺序无关；`groups` 恰好等于本次渲染要输出的卡集合（`RenderCards` 用同一批 key 发射卡片）。
  - `RenderCards` 在 `cardGroups` 之后算一次 `width := cardWidth(groups)`，`renderGroupCard(g, width, now, pal)`（新增 width 形参），卡内 `cardTitleLineAt(width, …)`、`clineRowLine(r, width, …)`、`cardBodyAt(width, note, pal)`、`joinCardAt(lines, width, pal)` 全部沿用同一 width。
- **名字列填充**：`clineRowLine` 原有 `pad = width - 4 - clineRowFixed - DisplayWidth(cell)` 逻辑不变，但 `width` 现在是渲染全局值，因此每行名字单元格自动**填到统一的（渲染全局）名字列**（width 58 → 名字列 9 列；`pad+1` 的空格是名字段尾随空格）。胶囊起始列 = `width - clineRowFixed + 1`（58 时为第 14 列），三家卡片逐行一致。
- **常量**：`clineNameColMin = 9`，注释写明来源（`SuperGrok`）与「下限不是上限」。`clineRowFixed`(45)、`clineCapCells`(23)、`clinePctWidth`(4)、`clineNumberWidth`(13) 未动。
- **测试**（`internal/planusage/report_test.go`，仅改本文件）：
  - `cardLines` helper 从「每张卡各自恒宽（空行重置期望）」改为「**整次渲染所有卡恒宽**」——空行只分隔卡片，不再重置，跨卡宽度不一致立即 FAIL（这把等宽契约锁到所有既有卡片用例上）。
  - 新增 helper `wantCardWidth(nameCells...)`（独立复述契约，不调用生产 `cardWidth`）。
  - 新增 2 个用例：`TestRenderCardsUniformWidthAcrossProviders`（三家混合，短名/短名/9 列，六卡边框 display 宽度全 = 58；含「只有短名时仍 58」的下限断言、每行胶囊起始列一致断言、名字完整未截断断言）、`TestRenderCardsNameColumnFloorWidensEveryCard`（25 列超长名 → 所有卡一起变宽到 `4+clineRowFixed+25` 且长名完整）。
  - 既有断言按新契约更新（仅宽度期望值，不放松）：`cardLines` 注释与实现、`TestRenderCardsBasic`、`TestRenderCardsTitleTextIsPlainText`、`TestRenderCardsTitleNeverCarriesAccount`、display-name/stale-marker 用例的 `want` 改为 `wantCardWidth(...)`。
- **反向验证（防假绿）**：把实现临时改成 ① 每卡各自宽度（`renderGroupCard(groups[key], clineCardWidth(groups[key]), …)`）② 去掉 `clineNameColMin` 下限 ③ `max` 改成永不因长名变宽，三种变异分别被上述两个新用例捕获 FAIL（②由「只有短名时仍 58」那条捕获），恢复后与备份逐字节相同。

## 被放弃的方案（必填）
- **把 `clineNameColMin` 直接等于 9 并只保留每卡取 max（不加全局统一）**：短名卡仍会各自变窄，用户诉求「三家等宽」不成立，否决。
- **只加全局统一、不加名字列下限**：没有 9 列下限时，全局宽度 = 最长名字列，短名 provider 独占一次渲染时卡宽随名字缩水（同命令宽度跳动），且宽度会随 roster 变化，否决。
- **给名字列设上限 + 超长名截断（省略号）**：名字必须完整（用户明确要求），截断还会破坏 `clineRowNameCell`/`旧` 标记与胶囊起始列的对齐契约，否决。
- **把宽度写死成常量 58（不比 `clineCardWidth`/标题需求）**：超长账号名或超长窗口名时会把边框顶出去或把标题挤成省略号（`cardTitleLineAt` 的 shrink 只是兜底），否决。
- **顺手把 `internal/usage` 的 `reportWidth` 一起对齐到 58**：超出本轮范围（用户只要求 quota 卡片），且 usage 侧宽度与 56 列不变量在既有笔记中有硬约束，否决。

## 遗留清单（未决 / 需后续处理）
- **`internal/usage` 的 `reportWidth = 56` 未跟进 quota 侧的 58**：本次只动 quota 卡片（`internal/planusage`），usage 报告仍是定宽 56 列，与 quota 的常规 58 列**不再相等**（`20260924-card-width-60-to-56.md` 的「两卡同宽」已被本笔取代）。两者是否统一（把 usage 也改成「按内容 + 名字列下限」的动态宽度，或把 quota 压回固定 56）**待用户决策**，本次不动。
- **「部分取代」在笔记维护规矩里没有规范写法**：`.agents/notes/README.md` 的维护规矩只给了 `active` / `superseded` 二元状态（方案更新时旧笔记标 `superseded` + `superseded_by`、新笔记填 `supersedes`），**没有**「旧笔记结论仍部分有效、只有一部分被新笔记取代」时的写法。本次按仓库既有先例处理：旧笔记保持 `status: active`、`superseded_by: ""`，由新笔记正文说明取代了哪一部分（quota 侧宽度算法）、哪一部分仍成立（`internal/usage` 的 `reportWidth = 56`）。是否把「部分取代」写法正式补进笔记维护规矩 = **待用户决策**，本次不动。

## 来源
- 任务说明（worker 派发，[MARK-PRISM-WIDTH-49]）：quota 卡片三家宽度不一致（54/55/58），要求全局统一 + 9 列下限 + 名字完整不截断。
- 实测证据：本机 `GET /admin/quota`（运行中的旧构建，loopback 内存缓存，未联网取数）取到真实 4 个快照（ClinePass `Cline`/`Cline2`、xai `SuperGrok`、gemini `Gemini`），同一份 JSON 分别用 HEAD 渲染器与新渲染器输出：HEAD = 54/54/54 + 55/55 + 58，新实现 = 六卡全 58；测量脚本（python `east_asian_width`）与 `render.DisplayWidth` 两套独立口径一致。
- 相关笔记：`2026-10-03-unified-quota-cards-spec.md`（统一卡片版式与 `clineCardWidth` 由来）、`20260929-clinepass-multi-account-metapi.md`（宽度参数化 `cardTitleLineAt`/`cardBodyAt`/`joinCardAt`）、`20260924-card-width-60-to-56.md`（旧的整卡宽度定宽时代，已被本次「无固定卡宽、按内容 + 下限」取代）。
