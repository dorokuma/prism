---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage, cmd/prism
---

# ClinePass 套餐窗口接入 quota 轮询，展示序定为 Gemini < ClinePass < SuperGrok

## 一句话结论
- 新增 `ClinePassFetcher`（internal/planusage/clinepass.go）：GET `https://api.cline.bot/api/v1/users/me/plan/usage-limits`（Bearer），把 `data.limits[]` 的 `five_hour` / `weekly` / `monthly` 映射为共享 Window（5h / weekly / monthly，固定短→长），401→`unauthorized`、403→`no_subscription`、其余状态→`unexpected_status`；由 `registry.go` 的 `DefaultFetchers()` 注册（OpenCode Go 照旧不参与轮询）。
- 展示序新增单一实现 `providerDisplayOrder`（internal/planusage/order.go）：`gemini < clinepass < xai`；排序键 = `%03d` rank + 首账号名，`RenderTableAt` 与 `RenderCards` 共用。未列出的 provider 共享尾 rank（=3）回退字典序——这是有意的展示契约调整：未列出的 provider 不再能按名字插到精选 provider 之前。
- `report.go` 两处配套：`providerDisplayName` 增加 `clinepass → "ClinePass"`；`accountSortKey` 改为 rank 前缀 + 首账号名。

## 背景
- 账号侧新增 ClinePass（Cline）订阅，`prism quota` 应与 Gemini / SuperGrok 并列展示其 5小时 / 周 / 月三窗口占用；线上 config 与凭据已就位（凭据不落仓）。
- 上游契约（实测，样例见 `clinepass_test.go` 的 `clinepassUsageBody`）：`GET https://api.cline.bot/api/v1/users/me/plan/usage-limits`，`Authorization: Bearer <token>`；响应 `data.limits[]` 每项 `type=five_hour|weekly|monthly`、`percentUsed` 为**已用**百分比（非剩余，无需反转）、`resetsAt` 为 RFC3339Nano。
- 展示侧问题：账号名 "ClinePass" 按字典序排在 "Gemini" 之前（C < G），若继续只按账号名排序，卡片会呈 ClinePass → Gemini → SuperGrok，与期望的阅读序 Gemini → ClinePass → SuperGrok 不符。

## 决策
- **归属判定**（Match）：provider 为 `clinepass`（大小写不敏感）或 base_url host 是 `cline.bot` 本体/子域（镜像 `config.isClineHost` 语义，`notcline.bot` / `evilcline.bot` 之类相似后缀不匹配）；quota 端点恒为 `api.cline.bot`，base_url 只用于识别账号。
- **窗口映射**：`five_hour`（及 `five-hour` / `5h` 别名）→ `5h`（复用 Gemini 的 5小时限额标题）、`weekly` / `monthly` 原样；未知 type 丢弃、绝不猜成已知窗口；一个已知窗口都没有时报 `missing clinepass usage limits`。输出顺序固定 5h → weekly → monthly。
- **百分比语义**：`percentUsed` 是已用值，不做反转；clamp 到 0..100；整数百分比只写 `Percent`（floor），**不写** `UsedFraction`——n/100 的 float64 乘回不总是精确（0.07*100 = 7.000000000000001），若写了 fraction，`displayPercent` 的 ceil 会把精确的 7% 显示成 8%；非整数百分比（含亚百分比 0.4）才保留 `UsedFraction`，让 `displayPercent` ceil 出 1%，不被 floor 掉。≥100% 置 `rate-limited`（卡片走红色实心胶囊 + 已达限额 footer）。
- **状态映射**：200 解析；401→`ErrUnauthorized`；403→`ErrNoSubscription`；其余→`ErrUnexpectedStatus`（含状态码）。`FetchWithRetry` 只重试 `fetch_failed`，所以 401 / 403 / 其它状态都只请求一次。
- **展示序**：`providerDisplayOrder = ["gemini","clinepass","xai"]` 是卡片/表格顺序的唯一实现（order.go）；`accountSortKey(s) = fmt.Sprintf("%03d%s", rank, 首账号名(缺省 provider))`，两个渲染入口共用，同一 provider 内仍按首账号名 tie-break，模块不交错。未列出 provider 的 rank 为 `len(providerDisplayOrder)`（=3），它们之间仍按账号名字典序，但整体排在三个精选 provider 之后——**不能再插到精选之前**，属有意收窄的展示契约。
- **配套**：`registry.go` `DefaultFetchers()` 追加 `ClinePassFetcher{}`；`report.go` 增加 `clinepass → "ClinePass"` 显示名与 rank 化 `accountSortKey`；CLI `prism quota` 帮助文案补 ClinePass。
- **测试**：`clinepass_test.go` 覆盖 Match（含相似域名拒绝）、200 三窗口映射与 Bearer/Accept 头、亚百分比/小数百分比、空 key→unauthorized、401/403/其它状态、缺 limits、100%→rate-limited、未知 type 丢弃；`order_test.go` 覆盖排序键的精选序、未列出回退与卡片级顺序。
- **观察项**：429 / 402 未特判，二者目前都落入 default → `unexpected_status`；待线上首次刷新后抽查上游真实限流/欠费形态，如确有需要再补分支。

## 被放弃的方案（必填）
- **维持纯账号名字典序**：无法让 ClinePass 落在 Gemini 与 SuperGrok（grok）之间；靠改账号名（如 "Gemini-2"）凑顺序则把展示序耦合进账号命名，脆弱且污染配置语义；否决。
- **逐快照展示序字段（Snapshot 加 rank/weight，由 fetcher 或调用方填）**：要动 Snapshot 模型、Cache 序列化与全部渲染调用链，而展示顺序是纯展示关注点；改动面不成比例，否决。
- **配置化展示序（config.yaml 增加 provider_order 之类）**：为固定三个 provider 的界面顺序引入新配置格式、默认值、校验与文档，收益不成比例；否决。
- **只在 cmd/prism 排序、不共享实现**：HTTP `/admin/quota` 的表格与 CLI 卡片共用 planusage 渲染，双实现必然漂移；否决。

## 来源
- ClinePass quota 接入改动（内部双审通过）；本轮 [MARK-PREP-PRISM-CLINE-QUOTA-240924-213431] 补齐决策留痕与文案同步。
- 上游契约实测样例：`internal/planusage/clinepass_test.go` 的 `clinepassUsageBody`（GET users/me/plan/usage-limits）。
- 相关笔记：`20260922-quota-capsule-bar.md`（卡片几何与耗尽态）、`20260924-card-width-60-to-56.md`（56 列宽）、`20260923-quota-tui-card-format.md`（卡片格式）、`20260922-usage-quota-cn-text.md`（文案中文化）。
