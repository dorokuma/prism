---
status: active
superseded_by: ""
supersedes: "20250929-legacy-test-failure.md"
模块: planusage, metapiusage, cmd/prism, render
---

# ClinePass 合卡渲染（一 (套餐,窗口) 一卡、一行一账号）+ 多账号直读 metapi + 打满 X/X + 固定亮色调色板

## 一句话结论
- ClinePass 卡片定稿为**合卡版式**：按（套餐/profile，窗口）一张卡，一行一个账号；标题只放套餐显示名 + 窗口名（不出现账号名），行内只有「色点 + 账号名 + 23 列胶囊 + 已用百分比 + 13 列数字段」。
- 打满的唯一信号 = 胶囊整条走红 + 数字段 `X/X`（实测消耗 / 该窗口总额），**绝不显示 `-`**；卡片上不再有 detail 文案行、倒计时、`已达限额` footer、状态字后缀。
- 账号名与色点同色，颜色 = 固定亮色板按账号指纹取模；指纹为空或短于 8 位退回无色 `·`，不越界读取。
- 其它 provider（Gemini / SuperGrok / Opus / 未收录 provider）**版式零改动**：一账号一卡、标题带账号、胶囊行、detail 行（`已用 N% / 总额 X 词元`，34 列不截断）、倒计时、打满 `已达限额` footer、固定 56 列——用 HEAD 归档树对同一 fixture 逐字节比对验证（见「验证」）。
- 账号与消耗按 account_id 隔离，账号/密钥直读 metapi hub DB（只读）；旧笔记 `20250929-legacy-test-failure.md` 已 superseded（其记录的失败在本分支修复，`TestRunUsageMergesAgyGemini` 实测 PASS）。

## 背景
- 旧版式是「一账号一卡」：标题 `service account · window`，第 2 行胶囊，第 3 行 `已用 N% / 总额 X 词元` + 倒计时，打满再加 `已达限额`，整卡恒 56 列。ClinePass 一个套餐下有多个账号（metapi `accounts.site_id=49`），该版式让卡片数随账号数线性增长、同套餐账号无法横向对比，且长账号名会被标题截断（`…`）。
- ClinePass 的消耗量只能从 metapi 生产库 `proxy_logs` 求和（prism 侧没有 cline-pass 账本），账号与 api_token 也只能从 metapi `accounts` 表读；一个账号一把 key，`GroupByKey` 因此给出「一个账号一个快照」，合卡必须跨快照聚合。

## 决策

### 1. metapi 直读账号与密钥（只读，零新增凭据）
- `internal/metapiusage/store.go`：`SELECT id, username, status, api_token FROM accounts WHERE site_id = 49`（`ListClinePassAccounts` / 包内 `ListClinePassAccountsWithToken`）。（**20260930 后续（只加注，不改本条）**：SQL 已追加 `AND status = 'active'`（disabled 账号不轮询），仅指纹版 `ListClinePassAccounts` 已作为死代码删除，公开列举改为带 token 的 `Source.ListClinePassAccounts`，指纹统一由 `planusage.KeyFingerprint` 计算；详见 `20260930-clinepass-multi-account-review-followups.md`。）
- api_token 仅在包内用于计算 SHA-256 指纹（前 8 字节 hex）；**永不**进日志、报告、卡片或测试输出。prism 侧不新增任何 ClinePass 凭据，也不新增配置键。
- `cmd/prism` 用 `metapiAccountView`（`internal/metapiusage` → `AccountView`）轮询 `cline.bot`；`cmd/prism/main.go` 与 `cmd/prism/quota.go` 都跳过 config 里的 clinepass 账号，改为 metapi 发现。

### 2. 按 account_id 隔离统计
- `internal/planusage/poller.go`：`clinepassSumFactory func(accountID int64) GrokTokenSum`，每账号独立求和；`internal/metapiusage/store.go` 提供 `SumClinePassTokensByAccount`。
- CLI 侧 `applyQuotaClinePassEstimate(ctx, snap, accountID)` 与服务侧走同一个 `planusage.ApplyClinePassEstimates`，两条路径数字一致。
- 库缺失/不可读/表缺失/created_at 漂移 ⇒ 该窗口无总额（降级），不影响快照获取与展示。

### 3. ClinePass 合卡版式（本轮定稿）
- **分组键** `clineKey{provider, window}`：provider 取快照 provider 归一化（`strings.ToLower(strings.TrimSpace(...))`），仅当 provider 命中 ClinePass（`isClinePass`，与 `ClinePassFetcher.Match` 同款判定：`EqualFold(TrimSpace(provider), "clinepass")`）才走合卡；其余 provider 走原路径。
- **卡顺序**：快照先按既有 `accountSortKey` 稳定排序；合卡在其第一个贡献快照的位置输出，Gemini/SuperGrok 的相对顺序不变；卡内行按快照顺序追加（= 账号名序）。
- **标题**：`╭─ ClinePass · 5小时限额 ────…╮`，只有套餐显示名 + `windowTitle(name)`，**不含账号名**；沿用既有标题机制（纯文本、右侧 dash fill、超限先缩可变段）。
- **行格式**（元素间各一个空格）：`│ ` 色点(1) 账号名(n) 胶囊(23) 已用百分比(4 右对齐) 数字段(13 右对齐) ` │`。
  行宽 = 45+n 列，卡宽 = 4+45+n，n = **卡内最长账号显示名**（20 是标题需求下限）。账号显示名**永不截断**：名字长则整卡加宽；同一张卡内所有行等宽，因此各列对齐。
- **账号显示名去掉尾部数字后缀**（`Cline1` → `Cline`、`Cline2` → `Cline`）：`stripNumericSuffix` 经 `clineDisplayName` 接线到合卡行，`clineRowLine` 显示与 `clineCardWidth` 量宽都用显示名，所以被去掉的后缀不会白留一列。**行身份不受影响**：`clineRowID` 仍用**全名** + 指纹，`Cline1`/`Cline2` 依旧是 2 行，同名靠色点 + 同色名区分。
- **行身份** = （窗口, 账号名, 指纹），账号名取**全名**（不因显示名去后缀而变化）。指纹非空时以（名, 指纹）去重：两个**同名不同指纹**账号 = 2 行；同一账号出现在本组多个快照 = 1 行（防止重名/倍增行）；指纹为空时以（名, 快照位次, 账号下标）区分，保证同名账号仍恰好 2 行可见。
- **数字段口径**（`clineNumberField`）：`total = LimitTokensEstimate`，为 0 时回落 `MeasuredTokens`；两项都为 0（metapi 读不到）才显示 `-`。非打满 `used = total × displayPercent%`；**打满（`Percent>=100` / `used up` / `rate-limited`）强制 `X/X`**（= 该账号该窗口实测消耗与总额，`ApplyClinePassEstimates` 在耗尽窗口把 `LimitTokensEstimate` 设为实测值）。
- **打满唯一信号** = 胶囊整条 solid red + 数字段 `X/X`。行内不出现 detail 文案（`已用 …/总额 …`）、倒计时、footer、状态字后缀，也不在账号名后**追加** emoji 或数字后缀：指纹带数字后缀的账号显示名**去掉**该后缀（`Cline1`/`Cline2` 都显示 `Cline`），同名账号靠色点 + 同色名区分（行身份仍用全名 + 指纹，所以仍是 2 行；失败归因 note 行仍用全名）。

### 4. 固定亮色调色板与命名规则
- `accountColor`（`internal/planusage/report.go`）用 6 色高对比亮色板：`00FFFF` 青 / `FF00FF` 洋红 / `FFFF00` 黄 / `00FF00` 绿 / `FF8800` 橙 / `8800FF` 紫；索引 = 指纹前 8 个 hex 字符按 16 进制累加后取模，稳定不变。
- `accountDot` 与 `accountNameText` 共用同一 `accountColor`，因此账号名与色点必然同色；无色模式（`CardOptions{NoColor:true}`）只留裸 `·` + 素色名字。
- 指纹为空或长度 < 8 时返回空色并退回 `·`，不越界读（`accountColor` 有长度守卫，`FgFromHex` 对非 6 位串原样返回）。

### 5. 其它 provider 维持旧版式（本轮零改动）
- 渲染分派只新增了 ClinePass 分支：`RenderCards` 的非 ClinePass 循环、`renderWindowCard`、`cardBarRow`、`cardDetailRow`、`cardFooter`、`cardNote`、56 列几何（`detailWidth = 34`）全部与 HEAD 一致。
- detail 行保持 34 列：`已用 N% / 总额 X 词元` **完整显示不截断**；本分支上一版把 detail 挤成 21 列 + 13 列数字段导致 detail 被 `…` 截断，本轮已还原（数字段只属于 ClinePass 合卡行）。
- 新增的宽度参数化（`cardTitleLineAt` / `cardBodyAt` / `joinCardAt`）在 `cardWidth` 下与旧实现逐字节等价，旧调用点全部走薄包装。

## 验证（可复现）
- **其它 provider 零改动**：`git archive HEAD` 出归档树 + 同一份 golden 打印测试（gemini/xai/opencode-go/未收录 provider，覆盖 0/1/34/59/94/99/100%、used up、rate-limited、美元额度、词元池（含 4.6B 进位）、无窗口错误卡、stale+error、超长 ASCII/CJK 账号名与超长窗口名、无色+彩色两种模式、legacy 表格）→ 两棵树输出 `diff` 完全一致。
- **ClinePass 三张真样例**（走真实 `RenderCards`）：一个打满 + 一个未满 / 两个都打满 / 单账号，均为一张卡 + 每账号一行，打满行数字段 `4.2M/4.2M`、`7.7M/7.7M`，未满行 `3.4M/10.0M`；无 `-`。
- **同窗口两账号 = 恰好 2 行**：2 个快照（各 1 账号、同名 "cline-user"、指纹不同、同窗口 "5h"）→ 1 张卡 2 行；名单在两快照上重复出现时（naive 逐快照×账号展开 = 4 行）仍为 2 行；两个快照完全不携带指纹的同名账号仍为 2 行。回归用例 `TestRenderCardsClinePassRowIdentity`。
- **显示名去后缀 + 行数不变**：`Cline1` + `Cline2`（指纹不同、同窗口）→ 1 张卡 2 行，两行行首都是 `│ · Cline `，卡内不出现 `Cline1`/`Cline2`，两行同文本不同色（紫 `8800FF` / 绿 `00FF00`，点与名同色），卡宽 = 4+45+5（后缀不占列）；指纹为空时同为 2 行且都显示 `Cline`。回归用例 `TestRenderCardsClinePassDisplayNameDropsNumericSuffix`；反向变异（`clineDisplayName` 返回全名）时该用例按预期 FAIL。
- **其它 provider 仍是零改动（本轮复跑）**：`git archive HEAD` 出归档树 + 同一份 golden 打印程序（6 个非 ClinePass provider × 8 个账号名 × 10 个窗口状态 + 多窗口/错误/无窗口/stale/无账号/空表，彩色与无色两种模式，共 2019 行输出）→ 两棵树 `cmp` 一致，`md5sum` 均为 `810c1c8da2fdfc06a3ba052c3e2b8656`；同一程序加入 `clinepass` fixture 后 diff 672 行，证明该比对确实能测出差异。
- **门禁**：`go build -o prism ./cmd/prism`、`go test ./...`、`go vet ./...`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全绿。

## 被放弃的方案（必填）
- **继续用 fingerprint bridge 读账号**：bridge 是单账号思路，无法自然扩展多账号；否决。
- **打满行继续显示 `-` / 用 `tokens/1` 反推总额**：前者与「用实测消耗当总额」冲突，后者在 percent 被上游钳到 100 时系统性低估池子；打满行改用实测消耗 `X/X`；否决。
- **按账号名哈希颜色**：同名账号颜色必然冲突；改用指纹哈希，稳定且唯一；否决。
- **保留旧测试的硬编码日期**：与 14 天 prune 窗口冲突；改用 `time.Now()`；否决。
- **账号名后加下标/emoji 区分同名账号**（如 `cline-user 1`、打满加 🔥）：行内后缀会让同行数据列错位，且同名账号本就应靠色点 + 同色名区分；否决（行内不追加任何后缀）。
- **合卡行继续按原样显示带尾部数字的账号名**（`Cline1`/`Cline2` 原样）：定稿要求显示名去掉尾部数字后缀，保留原样会让同套餐账号读起来是两个不同名字且白占宽度；否决。
- **ClinePass 沿用 56 列固定宽并截断长账号名**：与「账号名不截断」冲突；改为按最长账号名整卡加宽、同卡等宽；否决。
- **给非 ClinePass provider 也加 13 列数字段**：会把 detail 挤到 21 列并截断 `已用 …/总额 … 词元`，与「其它 provider 零改动」冲突；否决（数字段只属于 ClinePass 合卡行）。
- **合卡按 provider 一张卡、行 = 账号 × 窗口**：卡内会出现多窗口混排且账号重复出现，无法一眼看出某账号某窗口占用；按（provider, 窗口）分组才是定稿；否决。

## 与旧笔记的关系（旧笔记只加/保留 superseded 标记，不改旧结论）
- `20250929-legacy-test-failure.md`：`status: superseded`（本分支修复其记录的 `TestRunUsageMergesAgyGemini`，已验证 PASS），superseded_by 指向本笔记。
- `20260925-clinepass-quota-total-estimate.md`：估算接入（metapi 只读求和、周期起点反推、双路接线、降级铁律）仍 active；其「展示零改动：卡片 `totalPart`（`已用 N% / 总额 X 词元`）」一节**对 ClinePass 已不适用**（本轮 ClinePass 改合卡 + 数字段 `X/X`），对其它 provider 仍成立。未整篇标 superseded（该笔记的估算主体仍是当前实现）。
- `20260923-quota-tui-card-format.md`「一账号一卡」：对 ClinePass 已不适用（该笔记此前已 superseded）；对其它 provider 仍成立。
- `20260924-clinepass-quota-display-order.md`（Gemini < ClinePass < SuperGrok 展示序）、`20260924-quota-card-cn-text.md`（`已达限额` / `后重置` / 错误码中文化）、`20260922-quota-capsule-bar.md`（胶囊几何/ramp）对非 ClinePass 卡片仍 active 且未改动。

## 已知限制
- **无窗口的 ClinePass 快照**（抓取失败）走合卡的空窗口键：一张卡、每账号一行（只有色点 + 名字），失败按 `⚠ 账号名: 本地化错误码` 归因（合并后标题不再带账号名，失败归因只能落在 note 行）。
- `accountColor` 只接受 8+ 长度指纹；短指纹退回无色 `·`，不 panic。
- `TestRunUsageMergesAgyGemini` 虽已修复（日期改为 `time.Now()`），agy WAL 在测试中的可见性仍依赖 modernc/sqlite 行为；驱动升级时需复查。

## 来源
- 本轮任务 `[MARK-PRISM-QUOTA-9]`（账号显示名去尾部数字后缀 + 行身份不变）；上一轮 `[MARK-PRISM-QUOTA-8]`（ClinePass 合卡定稿版式重做 + 交付）；更早 `[MARK-WORKER-PRISM-CLINEPASS-TOTAL]`、`[MARK-WORKER-PRISM-CPA-HARDEN]` 的估算/降级加固。
- 相关实现：`internal/planusage/report.go`（合卡渲染、调色板、数字段）、`internal/planusage/{types,estimate,poller,clinepass}.go`、`internal/metapiusage/{store,source}.go`、`cmd/prism/{main,quota,metapi}.go`、`internal/render/{color,numbers,capsule}.go`。
