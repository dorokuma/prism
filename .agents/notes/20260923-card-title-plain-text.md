---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: usage, planusage, render
---

# usage/quota 卡片标题与表内中文纯文本化（去掉「用量 ·」前缀与品牌青加粗）

## 一句话结论
- `prism usage` 与 `prism quota` 胶囊卡片标题与表内中文去色去粗体（**排除式口径**）：usage 卡片标题去掉 `用量 · ` 前缀（`╭─ 用量 · 按模型分组 ─…╮` → `╭─ 按模型分组 ─…╮`）、表头列名（`模型`/`请求`/`缓存`/`命中率` 等）不再「粗体+品牌青」；quota 卡片标题的 service 名 / 账号 / ` · ` 分隔 / 窗口标签去色去粗体。本轮**不**套 `Brand`/`BrandBold`/`Dim` 的文本是：usage 描述（`groupDesc` 文本）与中文列名（表头），以及 quota 的 service / 账号 / ` · ` / 窗口标签。quota 卡内的「已用/总额」等中文明细（`cardDetailRow`）原本就是无色普通文本、本轮未动。**仍在上色**（排除项，勿误读为已去色）的是：usage 侧框线「╭─ ╮」、dash fill 与 `│` 边线仍走 `pal.dim`（`Dim("╭─ ") + desc + Dim(" " + fill + "╮")`）、命中率胶囊渐变；quota 侧 dim 边框与 dash、窗口胶囊流光渐变、**耗尽时纯红胶囊**（`pal.red`，`internal/planusage/report.go` `capsuleBar`）与 `! limit reached` **黄色 footer**（`pal.yellow`，`internal/planusage/report.go:376` `cardFooter`）。后两者是**带色的卡片文本**、不是非文本元素：因此「卡片文本全部无色」「两模式逐字节同色同重」「彩色版残留转义全部落在非文本元素上」这类全称判断均不成立。成立的只有去彩不变量：`render.StripANSI(colored) == plain` 逐字节相等（彩色 / 无彩的上述元素 StripANSI 后一致），而彩色渲染中上述元素本身仍带转义。颜色边界是**排除式**的（列出的文本项不再上色、列出的胶囊与 footer 除外），不是「全卡去色」这一刀。
- **文本契约变更**：`prism usage` 与 `GET /admin/usage/summary?format=table` 的标题行不再含 `╭─ 用量 · `，按该前缀刮取者升级需适配；**另有一处超长标题契约变化**——`titleLine` 的 `descMax` 由 47 增到 54（去掉 `用量` 4 列 + ` · ` 3 列共 7 列），超长键组合要多占 7 列才触发截断，长标题的截断点整体右移，按原 47 列截断点断言长度的刮取方同样需适配。
- 兼容红线**不变**：JSON 默认输出、`WriteJSON`/`Response`、`types.go` JSON tag、`/admin/quota` 的 JSON 与 `?format=table`（仍走未动的 `RenderTable`/`RenderTableAt`/`formatRemain` legacy 表格路径）、`internal/render` 公共函数（`Brand`/`BrandBold`/`Dim`/`CapsuleBar` 实现逐字未动，`BrandBold` 自此无调用方、仅作通用原语保留）、CLI flag、SQL 与 store 层。
- 行宽仍恒 60 显示列；`fill` dash 数仍按实测 `DisplayWidth` 重算（usage 侧为删除 `headW`/`sepW` 后的剩余预算公式，quota 侧 `cardTitleLine` 公式未动），不硬编码。

## 背景
- 用户要求表内中文视觉大小统一：**无色、无粗体**；标题去掉「用量 ·」前缀。诉求是「文本本身不靠样式强调」，边框 / 胶囊等非文本装饰才上色——这样粘贴到纯文本、`--no-color`、非 TTY 采集（π 面板）时观感完全一致。
- 自 v0.31.0 卡片形态起：usage 标题是 `Brand("用量") + Dim(" · ") + Dim(desc)`，表头走 `BrandBold`（`ESC[1m`+品牌青）；quota 标题是 `Brand(service) + Dim(account) + Dim(sep) + Brand(window)`。文本与视觉元素同样上色 / 加粗，彩印与去彩两版观感不一致——这正是用户要抹平的差异。
- AGENTS.md 铁律 #4：对外 API 契约变更必须记 `.agents/notes/`。本轮 usage 文本输出（CLI 与 `?format=table`）真的变了、且明确破坏按 `╭─ 用量 · ` 前缀的刮取，故留痕。

## 决策
- **标题与表内中文去色去粗体（排除式边界）**：无论 `ReportOptions.Color` / `CardOptions{NoColor}` 真假，usage 描述（`groupDesc` 文本）与中文列名（表头）、quota 标题的 service 名 / 账号 / ` · ` 分隔 / 窗口标签均为普通文本，不套 `Brand`/`BrandBold`/`Dim`；quota 卡内「已用/总额」等 `cardDetailRow` 中文明细原本无色、本轮未动。**仍保留颜色**的（排除项，勿误读为已去色）：usage 侧框线「╭─ ╮」、dash fill 与 `│` 边线仍走 `pal.dim`（`Dim("╭─ ") + desc + Dim(" " + fill + "╮")`）、命中率胶囊走渐变；quota 侧边框与 dash 走 dim、窗口胶囊走流光渐变、**耗尽时纯红胶囊**（`pal.red`，`capsuleBar`）与 `! limit reached` **黄色 footer**（`pal.yellow`，`cardFooter`）。其中耗尽纯红胶囊与黄色 footer 属**带色的卡片文本**、并非非文本元素——所以「彩色版转义全部落在非文本元素上」「卡片文本再无任何转义」「两模式逐字节同色同重」这类全称表述一律不写；可写的只有剥转义不变量 `render.StripANSI(colored) == plain`（彩色 / 无彩两版剥掉转义后逐字节一致），彩色渲染中上述元素本身仍带转义。
- **usage 标题去掉前缀**：`titleLine` 由 `Dim("╭─ ") + Brand("用量") + Dim(" · ") + Dim(desc) + Dim(" " + fill + "╮")` 改为 `Dim("╭─ ") + desc + Dim(" " + fill + "╮")`，描述裸置两段 dim 之间、其间无任何转义；`fill` 在删除 `headW`/`sepW` 后按实测 `DisplayWidth` 重算（`按模型分组` 描述 10 列 → 45 条 dash，3+10+1+45+1=60），公式本身不写死 dash 数。
- **usage 表头去加粗**：`headerContent` 的列名改普通文本（`c.cell(c.title)`，等宽填充同数据行），不再 `BrandBold`。`ReportOptions.Color` 的 godoc 从「(brand title, brand bold headers, dim borders and the hit-rate capsule ramp)」改为「(dim borders and the hit-rate capsule ramp)」。
- **超长标题文本契约变化（descMax 47→54）**：`titleLine` 的截断预算由 `reportWidth - prefixW - suffixW - headW - sepW - 2`（47）改为 `reportWidth - prefixW - suffixW - 2`（54），即去掉 `用量`（4 列）+ ` · `（3 列）共 7 列，标题描述可容纳 7 列更长的键组合才被截断。这是超长标题文本契约的实质变化（长标题截断点变宽），不只是视觉样式：按原 47 列截断长度做断言的调用方需重新校准。
- **quota 标题去色去粗体**：`cardTitleLine` 把 `pal.brand(svc)`/`pal.brand(win)`/`pal.dim(acc)`/`pal.dim(sep)` 全部改为直写 `svc`/`acc`/`sep`/`win`；`cardPalette` 删除 `brand` 方法，但**保留** `dim`/`red`/`yellow`——`dim` 仍供边框与 dash 填充，`red` 仍供耗尽窗口的纯红胶囊（`capsuleBar`），`yellow` 仍供 `! limit reached` footer（`cardFooter`），三者都在 `internal/planusage/report.go` 中继续使用，本轮未动。账号去重（账号 == service 时只显示一次，见 v0.31.1）逻辑不动。
- **`BrandBold` 保留为通用原语**：`internal/render/color.go` 的 `colorBold`/`BrandBold` 实现逐字未动，仅 doc 注释改为说明「usage 表头曾是唯一消费者，v0.31.2 起无调用方，仍作为想要粗体品牌标签的通用原语保留」。外部可见函数签名 / 行为不变——这是「内部 render 公共函数逐字未动」红线的落点。
- **测试锁死（新增 2 个契约用例）**：`TestRenderUsageReportTitleTextIsPlainText`（usage）逐字节锁定彩色渲染标题行 = `Dim("╭─ ") + desc + Dim(" " + fill + "╮")`、纯文本渲染同行无转义、卡片无 `ESC[1m`、无品牌青落在描述 / `按模型分组` 上、`StripANSI(colored)==plain`、`ReportOptions{}`（`Color: false` 的 Go 层无彩渲染）不含 `\x1b`；`TestRenderCardsTitleTextIsPlainText`（quota）同级把 `Claude claude-main · 5小时限额` 标题行按字节 pin 住、卡片无 bold、去彩逐字节相等。（旁注：CLI 层 `cmd/prism/usage.go` 的 `wantColor(out, o.noColor)` 让 `--no-color` 强制关闭 `Color`，方向与上述 Go 层锁定一致，但本轮未在 CLI 进程层新增锁定，故上文只写 `ReportOptions{}`。）既有 `TestRenderUsageReportColor`（`internal/usage/report_test.go`）与 `TestRenderCardsBasic`（`internal/planusage/report_test.go`）的断言从「含品牌青 / 粗体 / dim 账号」反转为「**不**含品牌青与粗体、但含 dim 边框」；`TestHandlerTableFormat`（`internal/usage/handler_test.go` 约 421 行）只是整卡精确串里标题行的同步（`╭─ 用量 · 按模型分组 …╮` → `╭─ 按模型分组 …╮`），属精确串同步，并非「品牌青/粗体断言反转」。`report_test`/`handler_test` 两处整卡精确串的标题行同步为新形态，`TestRenderUsageReportNoData` 前缀由 `╭─ 用量` 改 `╭─ `、`TestRenderUsageReportOverLongTitle` 前缀由 `╭─ 用量 · ` 改 `╭─ `（CJK 截断用例由 `键×20` 调到 `键×26`：新 `descMax`=54 = 60-3-1-2；`按模型/` 前缀宽 7 + 23 个键 46 + 省略号 1 = 54，即**截断结果宽 54**、并非 23 个键把 54 列键宽用满，属「超长截断」路径而非「截断少 1 列」；真正停在预算少 1 列——双宽 rune 放不下省略号那一列——的路径由同测试的单键 `键×30` 用例覆盖，见 `internal/usage/report_test.go` 约 691 行）。每行恒 60 列（`assertCardWidth`/`assertCardTitle`/`cardLines`）未放松。
- **文档同步**：README `/admin/usage/summary` 端点表 v0.31.2 起标注标题为纯文本 `╭─ 按模型分组 ─…╮`、明确「`╭─ 用量 ·` 前缀已不存在，按该前缀刮取者升级需适配」；顶部版本号 v0.31.1→v0.31.2、日期 2026-09-23；Changelog 增 v0.31.2 条目，逐条列明不变的兼容红线与变了的文本契约。
- **门禁**：`go build -o prism ./cmd/prism`、`go vet ./...`、`go test ./...`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全过。
- **发布状态**：代码已于 commit `1989a7e`（`fix(render): plain-text usage and quota card titles`）提交、打 tag `v0.31.2`，origin 已含该 commit 与 tag；README 版本号与 Changelog v0.31.2 为已发布文本，按 reviewer 意见保留不动。本笔记属**补交留痕**，未纳入 `1989a7e`（该 commit 只含代码 / 测试 / README 改动），作为后续提交单独落地。

## 被放弃的方案（必填）
- **只把 `用量 · ` 前缀去色去粗体、保留该前缀字样**：用户要求的是「标题去掉『用量 ·」前缀」，即删掉这段字样、不是改其样式；保留前缀会继续误导按前缀解析的刮取方，否决。
- **保留表头「粗体 + 品牌青」、仅在无彩模式下去掉**：与「无色无粗体、两模式视觉统一」的诉求冲突——彩色终端仍会看到加粗表头，达不到表内中文大小统一；改为两模式都纯文本，否决。
- **从 `internal/render/color.go` 删除 `BrandBold`（连 `colorBold`）**：`BrandBold` 是通用原语，删它会破坏「内部 render 公共函数逐字未动」的兼容红线，且未来别处若要粗体品牌标签又得重造；改为保留 `internal/render/color.go` 的公共实现。实际删除的是**两处已无调用方的私有方法**：quota 侧 `cardPalette.brand`（`internal/planusage/report.go`）与 usage 侧 `reportPalette.brand`/`reportPalette.brandBold`（`internal/usage/report.go`），公共 `render.Brand`/`render.BrandBold` 一字未动。
- **把新标题的 dash 填充写成固定条数（如 45）**：`fill` 仍按实测 `DisplayWidth` 从剩余预算推导；写死 dash 数会在下次改文案时再次漂移（与 `20260922-usage-quota-cn-text.md`、`20260922-wide-truncate-pad-title-fill.md` 同一结论），否决。
- **顺带改 JSON 键名、列头底层值或 `RenderTable` legacy 路径**：JSON 输出与 `types.go` JSON tag 是兼容红线，诉求只针对视觉样式、不在本轮范围，拒绝（`/admin/quota?format=table` 的 `RenderTable`/`RenderTableAt`/`formatRemain` 逐字未动）。

## 来源
- commit `1989a7e` — `fix(render): plain-text usage and quota card titles`（tag `v0.31.2`，作者 dorokuma，2026-09-23，已推送 origin，origin 含该 commit 与 tag）。
- README.md Changelog v0.31.2（2026-09-23）与 `/admin/usage/summary` 端点表 v0.31.2 标注。
- 实现：`internal/usage/report.go`（`titleLine`/`headerContent`/`reportPalette`/`ReportOptions.Color` godoc）、`internal/planusage/report.go`（`cardTitleLine`/`cardPalette`）、`internal/render/color.go`（`colorBold`/`BrandBold` godoc）。
- 回归测试：`internal/usage/report_test.go`（`TestRenderUsageReportTitleTextIsPlainText`）、`internal/planusage/report_test.go`（`TestRenderCardsTitleTextIsPlainText`）、`internal/usage/handler_test.go`。
- 相关笔记：`20260922-usage-capsule-report.md`、`20260922-usage-quota-cn-text.md`、`20260922-wide-truncate-pad-title-fill.md`、`20260922-card-symmetric-gutter.md`、`20260923-quota-tui-card-format.md`、`20260922-quota-capsule-bar.md`。
