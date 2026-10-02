---
status: active
superseded_by: ""
supersedes: ""
模块: planusage, metapiusage, cmd/prism
---

# ClinePass 多账号双审收尾（F1 指纹接线 / F3 active 过滤 / F2 SIGHUP 重发现 / O5 死代码）+ 本轮遗留清单

## 一句话结论
- **F1**：CLI 与服务侧共用同一个装配函数 `planusage.AssignAccountViews`（账号名 + 每账号 key 指纹一次成型），生产 `prism quota` 恢复「色点 + 同色账号名 + 指纹行身份」，不再是裸 `·` + 位置键。
- **F3**：metapi 账号只取 `status = 'active'`（SQL 单点过滤），disabled 账号不轮询、不渲染。
- **F2**：账号发现在**启动与每次 SIGHUP** 各跑一次，复用 `internal/metapiusage.Source` 的账号列举（服务与 CLI 同一实现）；发现失败**保留上一轮 ClinePass 视图**，不再让整块卡片静默消失。
- **O5**：删除 `accountFP`、`metapiAccountView.fp/status`、`Store/Source.ListClinePassAccounts`（仅指纹版）、`poller.accountIDFrom`（与 `registry.AccountIDFrom` 重复）与 `keyFingerprint/TokenFingerprint`（与 `planusage.KeyFingerprint` 重复实现）。
- **consider①**：`stripNumericSuffix` 对纯数字账号名兜底返回原名（不再剥成空串、行只剩色点）。
- 遗留/观察项（O1/O2/O3/O4/O6 + consider②）与**发版必须显式 supersede 的 README 条目**见下。

## 背景
- 本轮是 `feature/clinepass-multi-sub-quota` 的双审收尾：reviewer 放行、oracle 条件放行，指出 F1（reviewer 漏项，复核确认）等 4 项 should fix 与若干观察项（O1–O6、consider①/②）。
- F1 的事实链：`RenderCards` 的唯一生产调用点是 `cmd/prism/quota.go` 的 `runQuotaWith`，而 `SetAccountFPs` 此前只在 `internal/planusage/poller.go`（服务侧）与测试里被调用 ⇒ 生产 CLI 的快照指纹为空：色点退化成裸 `·`、账号名无色、行身份退化成位置键，「同名账号靠色点区分」在生产 CLI 不可达。
- F3 事实：`accounts` 表（本机生产库实测）site 49 有 2 行：`id=34 Cline status=disabled`、`id=38 Cline2 status=active`；`status` 取值域为 `active`/`disabled`，列默认 `'active'`，无 NULL/空串（`SELECT count(*) FROM accounts WHERE status IS NULL OR trim(status)=''` = 0）。原 SQL 不筛 status，`cmd/prism` 读到的 `status` 又只写不读。
- F2 事实：账号发现只在 `cmd/prism/main.go` 启动时读一次，`readClinePassAccounts` 出错返回 `(nil, nil)` 只留一条 WARN，SIGHUP 只 `SetOptions` 不重新发现 ⇒ 启动期 metapi 不可用会让 ClinePass 整块卡片消失（比改动前更差，因为账号已不再来自 config）。

## 决策
### 1. 指纹装配收敛成一个函数（F1）
- `internal/planusage/types.go`：新增 `AssignAccountViews(s *Snapshot, accounts []AccountView)`：按 `accounts` 顺序一次写入 `Accounts` 与 `accountFPs`（`KeyFingerprint(a.Key())`），两片永远不会错位。
- 两个生产调用点都用它：服务侧 `poller.go` 的 `fetchOne`，CLI 侧 `cmd/prism/quota.go` 的新函数 `buildQuotaSnapshot`（`runQuotaWith` 的每组收尾：装配账号名/指纹 + 按 provider 应用总额估算）。（限定：服务侧的**失败写入路径**曾把这份装配结果抹掉——`Cache.StoreFailed` 用 `Snapshot` 字面量重建快照、不带 `accountFPs`，已随 O7 修复，见「遗留清单」O7。）
- 指纹口径 = `planusage.KeyFingerprint(AccountView.Key())`：metapi 账号的 `Key()` 就是 `api_token`，与服务侧完全一致（同一实现，不是「同值的第二份实现」）。
- 回归用例 `cmd/prism.TestCLIAssemblyCarriesAccountFingerprints` 走真实 CLI 形态：`readClinePassAccounts`（真读 metapi fixture）→ `GroupByKey` → `buildQuotaSnapshot` → `RenderCards`，断言「色点与账号名同色」「两行同名账号颜色不同」「指纹对齐且非空」「重复 roster 仍是 2 行（行身份走指纹而非位置）」。原 `TestRenderCardsClinePassDegradesWithoutFingerprint` 保留（锁降级语义）。

### 2. 只取 active 账号（F3）
- `internal/metapiusage/store.go`：`SELECT id, username, api_token FROM accounts WHERE site_id = 49 AND status = 'active'`。
- 过滤放在 SQL 单点：调用方（`Source`/`cmd/prism`）不再持有 `status`，避免出现第二个判定点。
- **限制**：精确匹配小写 `'active'`。若 metapi 将来引入第三种状态值或改变大小写，该账号会**静默**地从卡片上消失（不会误轮询）。这是有意的方向选择（宁可少列也不轮询无订阅账号），但巡检时需注意。

### 3. 账号自愈 = SIGHUP 重发现（F2，评审允许的替代方案）
- `cmd/prism/main.go`：新增 `refreshQuotaAccounts(p, quotaPoller, metapiSource)`，启动时与**每次 SIGHUP**（与 config reload 成败无关）各调用一次；`cmd/prism/metapi.go`：新增纯规则函数 `nextQuotaViews(configViews, discovered, prev, err)`。
- 失败策略：发现**报错** ⇒ 保留上一轮 clinepass 视图（瞬时读失败不得让整块卡片消失）；发现**成功**（含空结果）⇒ 以新结果为准（账号被 disable/删除就是这样退出 roster 的）；本机无 metapi 库 ⇒ `readClinePassAccounts` 返回 `(nil, nil)`，静默、不 WARN、不报错。
- `readClinePassAccounts` 不再吞错：库存在但列举失败时把错误上抛（旧实现 `(nil, nil)` 让调用方无法区分「没有账号」与「读不到」）。
- **限制（写在明处）**：**没有**周期性重发现——poller 每轮不重新发现账号，poller 账号列表也不随 config reload 变化（`p.AllAccounts()` 是启动时的快照，SIGHUP 也不重建，属既有行为）。运行期 metapi 恢复、或账号新增/禁用，都要靠一次 SIGHUP（或重启）才生效。评审允许此替代方案，故不改 poller 的回调面。
- 回归用例 `cmd/prism.TestNextQuotaViews`（成功/空/失败三态 + 不给 config 视图重复）。

### 4. O5 死代码清理（含与评审字面要求的一处偏差）
- 删除：`report.go:accountFP`（无调用方）、`metapiAccountView.fp`/`.status`（只写不读；status 的职责已由 SQL 过滤接管）、`Store.ListClinePassAccounts`（仅指纹版）、`poller.accountIDFrom`（`registry.AccountIDFrom` 已是同一实现，服务侧改调后者）、`metapiusage.keyFingerprint` + `clinePassAccountWithToken.TokenFingerprint`（`planusage.KeyFingerprint` 是同一算法的唯一实现）。
- **偏差（必须记录）**：评审 F2 要求「复用 `internal/metapiusage.Source.ListClinePassAccounts`（仅指纹版）」。源码事实是：轮询 `cline.bot` 需要 `Authorization: Bearer <api_token>`（`internal/planusage/clinepass.go` 的 `Fetch` 用 `acc.Key()`），仅指纹版**无法驱动轮询**。因此把 `Source` 的**带 token 列举**提升为公开的 `Source.ListClinePassAccounts`（服务与 CLI 共用的唯一账号发现实现），并删除仅指纹版；若保留它，就会多出一条谁也用不上的列举路径。`Store.ListClinePassAccountsWithToken` 仍是底层实现（被 `Source` 调用）。
- `metapiAccountView` 只剩 `accountID/name/apiToken`；`cmd/prism/refreshQuotaAccounts` 是服务侧唯一 roster 装配点。

### 5. 纯数字账号名兜底（consider①）
- `report.go:stripNumericSuffix`：剥到头（整个名字都是数字，如 `12345`）时返回原名。否则行会只剩色点，显示信息被抹掉。
- 顺带修正该函数文档里「`Cline-1` → `Cline-1`」的错误示例（真实行为是 `Cline-1` → `Cline-`：只剥尾部数字串，连字符不是数字，但保留）。
- 回归用例 `internal/planusage.TestStripNumericSuffix`（含 `12345`/`0` 与一条端到端渲染断言）。

## 遗留清单（本轮遗留 / 已知限制；其中 O7 与 O10 的服务侧 per-account 一项已在本笔修复，其余未修）
- **O1 stale 快照的行不带旧标记**：ClinePass 合卡行没有任何 stale 标记，只有抓取失败才会出现 `⚠` note ⇒「数据是旧的但抓取本身成功」这一状态在卡片上**看不出来**（`Snapshot.Stale` 只进入 JSON）。区分依赖 `⚠` note 能否出现。
- **O2 调色板只有 6 色**：`accountColor` 的调色板（青/洋红/黄/绿/橙/紫）按指纹前 8 hex 取模 6 ⇒ 账号数 > 6 必然撞色；同名账号（显示名相同）对撞色概率约 1/6，撞色时同名同行**无法区分**（行身份仍是两条，但视觉上看不出差异）。
- **O3 「打满」两个信号在 99.5%–100% 窄区间不一致**：`displayPercent` 对 `UsedFraction` 向上取整（`math.Ceil`），而 `windowExhausted` 只看整数 `Percent >= 100` / status。一个 `Percent=99` 且 `UsedFraction=0.9951…0.9999` 的窗口：占比列显示 `100%`、数字段算出 `X/X`，但 `clineBar` 走非耗尽分支 ⇒ 胶囊是**逐格渐变**（首格偏绿、末格红）而不是整条纯红。`≥100%`（或 `used up`/`rate-limited`）才是整条纯红的唯一充分条件。
- **O4 `MeasuredTokens` 在生产路径与 `LimitTokensEstimate` 恒等**：`ApplyClinePassEstimates` 只在耗尽分支写这两个字段，且写同一个 `tokens` ⇒ 当前是实现冗余（保留是为了「打满时用实测消耗当总额」显式可读，且为上游将来只给实测值留位）；JSON 因此新增 `measured_tokens`（`omitempty`，只在耗尽窗口出现）。
- **O6 数字段 13 列无余量但成立（已核实为非问题）**：`formatTokenPair` 最长形如 `999.9E/999.9E` = 13 列，正好占满；`clineRowFixed` 与之相加的几何已被 `TestRenderCardsClinePass*` 的宽度断言锁定。
- **consider②（`accountColor` 逐字符 `ParseUint`）判定为不改**：该循环对 8 个 hex 字符各调一次 `hexValue`，非 hex 字符按 0 计入（错误被有意丢弃）。改成一次 `ParseUint(fp[:8], 16, 64)` 需要新增「非法 hex 怎么退化」的分支，而 `accountColor` 的既有契约（长度 < 8 或空 ⇒ 无色）与用例都按逐字符语义锁定；每行 8 次 `ParseUint` 的开销与渲染无关紧要，也不存在第二份实现需要收敛。故维持现状。
- **O7 服务侧失败轮丢指纹（本笔已修）**：poller.go:219 已装配指纹，但失败分支
  poller.go:224 走 cache.StoreFailed(fp, provider, names, code)，cache.go:32-38
  以 Snapshot 字面量重建快照、不带 accountFPs ⇒ 抓取失败的那一轮，ClinePass
  合卡行退化为裸 ·、无色账号名、位置键身份（CLI 侧 quota.go:135 在失败早返回
  之前装配，不受影响）。不丢行、不多行，仅降级态观感。**本笔已修**：
  `Cache.StoreFailed` 改收整个 `Snapshot`（cache.go:36，不再用字面量重建，`Accounts` 与
  `accountFPs` 原样带过，因此两片不会错位），`poller.go:227` 的失败分支改传 `snap`。
  新增用例 `TestPollerFailedRoundKeepsAccountFingerprints`（失败轮缓存快照的 `AccountFPs`
  非空、与 `Accounts` 对齐、两账号两枚指纹）与 `TestCacheStoreFailedKeepsFingerprints`
  （缓存边界 + 指纹随账号换序对齐）。原来那句「故『两个生产调用点都用
  AssignAccountViews』只对装配成立，服务侧失败写入路径会把结果抹掉」已随本次修复失效
  （见「决策 1」新加的限定）。
- **O8 库文件不存在 ≠ 读取失败**：metapi.go:95-97 + metapiUsageDBPathIfPresent
  （metapi.go:49-53）把「库文件不存在」当成功空结果，而 nextQuotaViews 的成功
  分支以新结果为准 ⇒ 运行中 metapi 库文件短暂消失（换库/恢复/rename 窗口、
  挂载抖动、父目录权限）时来一次 SIGHUP，会**无 WARN、无计数**地清空 roster 里
  的 ClinePass 视图，且库恢复后不自动回来（需再发一次 SIGHUP）。建议：库不存在
  但上一轮有 clinepass 视图时归入「保留上一轮」（或至少补 WARN），并给 roster
  规模加 expvar（如 clinepass_quota_accounts）。
- **O9 三条静默收缩路径**（共同后果：账号数掉到 0 与「本机没部署 metapi」在
  卡片上无法区分）：① status = 'active' 精确匹配（metapi 第三态/改大小写即静默
  丢账号）；② id/username/api_token 任一为 NULL 的行被 Go 侧 continue 静默跳过
  （metapi DDL 允许 NULL；status='active' 但 api_token='' 的非 NULL 行会进
  roster 并固定报未授权）；③ site_id=49 硬编码（metapi 删站重建会拿到新 id ⇒
  静默空 roster）。建议用 expvar/计数或一次 WARN 让收缩可见。
- **O10 测试缺口**：服务侧 per-account 接线**本笔已修**（`fakeAcc.accountID`
  已赋不同值，factory 按传入的 accountID 返回不同消耗，`TestPollerClinePassPerAccountEstimate`
  断言每个账号的消耗写进自己的快照、不串号，`TestPollerClinePassEstimateApplied` 的 factory
  也改为断言收到本账号 id；原缺口：poller_test.go:147,199 的 factory 忽略 accountID，
  fakeAcc.accountID（go_test.go:20,29）加了却从未赋值/断言，poller 传 0 或取错账号仍全绿）；accounts 表缺失的 roster
  错误路径无用例；refreshQuotaAccounts（F2 真正接线点）与「无库 ⇒ (nil,nil)」
  语义无用例；`Accounts[i]↔AccountFPs()[i]` 多元素对齐本笔只在
  `TestCacheStoreFailedKeepsFingerprints` 覆盖了 2 元素 + 换序的缓存边界，成功轮的渲染路径仍缺多元素用例；
  失败轮不丢指纹（O7）已随本笔补用例。
- **（本笔已加可观测，直接覆盖 O8/O9 的静默收缩）**：`cmd/prism` 的 
  `refreshQuotaAccounts` 在「上一轮 clinepass 视图数 > 0 且本轮 = 0」时记一条 WARN，
  带原因（`metapi database absent` / `no clinepass accounts discovered` / `discovery failed`），
  并新增 expvar `clinepass_quota_accounts`（当前 roster 的 ClinePass 规模）与
  `clinepass_quota_roster_drops_total`（掉到 0 的轮数），同时上 /metrics；判定与计数是纯函数
  `clinePassRosterDelta`（cmd/prism/metapi.go:187），用例 `TestClinePassRosterDelta`。
  **语义未改**：库文件不存在仍视为「本机无 metapi」（不报错、不 WARN），只把它当作「上一轮>0
  而本轮=0」的原因写出来。故上面 O8/O9 里的「建议加 expvar/一次 WARN」已落地，其余描述
  （触发窗口、库恢复后需再发一次 SIGHUP 才回来、status 精确匹配）仍成立。

### 行为变更（发版 changelog 与 commit message 必须写）
1. **config 里的 clinepass 账号被忽略；账号改由 metapi site 49 发现**（`accounts` 表只取 `status='active'`）：`accounts:` 里写 `provider: clinepass` 的账号不再进 quota 轮询与卡片，账号名/密钥改为只读 metapi 生产库取得；metapi 不可用 ⇒ 该 provider 无卡片（启动期不可用可用 SIGHUP 恢复）。
2. **必须显式 supersede README v0.33.0 的这条**：`占比为 0 或 ≥100% 的窗口不产出总额（≥100% 仍置 rate-limited）`（README「发版记录」v0.33.0 段落）。现行为：0% 窗口仍不产总额，但 **≥100%（打满）窗口改为用实测消耗当总额**，卡片显示 `X/X`（不再显示 `-`），这一条必须在 changelog 里点名覆盖，否则 README 与实现互相矛盾。

### 唯一离线不可证项（需上线后实机冒烟）
- **`cline.bot` 配额端点是否接受 `api_token` 作 `Authorization: Bearer`**（即 metapi `accounts.api_token` 是否可直接轮询 `GET /api/v1/users/me/plan/usage-limits`）：离线无法证明，测试全程不联网（fixture 直读 metapi 库、快照由测试构造）。建议上线后对每个 active 账号做一次实机冒烟，确认返回 `200` 且 `data.limits[]` 三个窗口可解析，而不是 `401 unauthorized`（`clinepass.go` 会把 401 归为 `unauthorized`、403 归为 `no_subscription`，届时卡片会显示对应错误码）。

## 被放弃的方案（必填）
- **复用 `Source.ListClinePassAccounts`（仅指纹版）做账号发现**：无法轮询（Bearer 需要 `api_token`），保留它等于多一条死列举路径；改为公开带 token 的 `Source.ListClinePassAccounts`（见「决策 4」）。
- **周期性重发现账号（poller 每轮回调）**：改动面扩到 `internal/planusage` 的轮询接口与接线，收益只是「不必 SIGHUP」；评审允许 SIGHUP 替代，故不做，并在本笔记写明限制。
- **在 `cmd/prism` 侧过滤 disabled 账号**：会出现两个判定点（SQL + cmd），且远端将来改 status 枚举时两处必然漂移；否决，只留 SQL 一处。
- **保留 metapiusage 的 `TokenFingerprint`/`keyFingerprint`**：与 `planusage.KeyFingerprint` 同算法同值，属于评审明确要求收敛的重复实现；否决，指纹统一由 `planusage` 计算。
- **发现失败即清空 roster（更「干净」的实现）**：会重现本轮要修的故障——metapi 瞬时读失败让整块 ClinePass 卡片静默消失；否决，改为保留上一轮视图。
- **把 `MeasuredTokens` 从 JSON 去掉**：它已随本分支进入 JSON 契约（`measured_tokens`），删字段会破坏本轮已交付的接口；保留并在 O4 记录冗余来源。

## 验证（本轮）
- `go build -o prism ./cmd/prism`、`go test ./...`、`go vet ./...` 均绿；`internal/planusage`、`internal/metapiusage` 另跑 `-race`；`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 绿。
- **非 ClinePass 渲染逐字节零改动**：`git archive HEAD` 出归档树 + 同一份 golden 打印程序（4 provider × 8 账号名 × 12 窗口形态 + 多窗口/无窗口/错误/stale/无账号/空表；卡片彩色与无色、legacy 表格、JSON 三段）⇒ 两棵树 13593 行输出 `diff` 为空、`md5sum` 同为 `623dd76fd4dd2987fc58bfea30fadb57`；加 `-clinepass` fixture 的灵敏度对照 ⇒ 两棵树 30 行差异（全部落在 ClinePass 卡片段）。
- **反向变异**：F1 装配、consider① 兜底、F3 过滤、F2 回退保留、F4 每账号隔离各做一次变异 ⇒ 对应用例全部 FAIL，复原后 md5 一致。
- 样例（CLI 装配路径、两个同名 active 账号）：两行显示名都是 `Cline`，色点分别 `#FFFF00`/`#00FF00` 且与各自账号名同色，数字段 `3.4M/10.0M` 与 `6.2M/10.0M`。

### 验证（本笔补记）
- **为什么必须重跑**：本笔动了 `internal/planusage/cache.go`（O7）与 `poller.go`，上一节「非 ClinePass 逐字节零改动」的结论因此作废，下面是在本笔最终态上的重跑。
- **门禁**：`go build -o prism ./cmd/prism`、`go test -count=1 ./...`（24 包全 `ok`）、`-race` 于 `internal/planusage`/`internal/metapiusage`/`cmd/prism`、`go vet ./...`、`scripts/test_*.sh` ×5、`python3 scripts/test_generate_mcp_tools.py` 全绿（exit=0）。
- **非 ClinePass 渲染逐字节零改动（重跑）**：`git archive HEAD`（v0.33.0 `df8e699`）归档树 + 工作树副本，各放同一份 golden 打印程序（md5 `a5021f75982020a24d7919c8320dbe5f`，只用两树都有的标识符：不含 `accountFPs`/`MeasuredTokens`/`AssignAccountViews`），矩阵 5 provider × 6 账号名（含 CJK 超长、`12345`、空）× 8 窗口形态 × 5 error × stale 2 = 2400 快照（彩色/无色/`RenderTableAt`/JSON + 聚合四段）⇒ 两树 **88028 行、`diff` 为空、`md5` 同为 `43698c38d60624d6c9586f6b8b6d85e6`**。确定性对照：同树两次相隔 >1 分钟 `md5` 一致（`RenderTable()` 等价 `RenderTableAt(…, time.Now())`，已从 dump 中剔除，避免「同一分钟内一致」的假绿）。**灵敏度对照**：同一 harness 追加 3 个 ClinePass fixture ⇒ 两树 **126 行 unified diff，差异全部落在 ClinePass 段**（非 ClinePass 的 `### 0..2399` 段 32460 行仍逐字节相同）。
- **反向变异（三条；均在 /tmp 树副本内做，用后还原并核对 md5）**：① `cache.go` 恢复「字面量重建、不带 `accountFPs`」⇒ `TestCacheStoreFailedKeepsFingerprints` + `TestPollerFailedRoundKeepsAccountFingerprints` 双红；② `poller.go` 把 `AccountIDFrom(g.Accounts[0])` 换成 `0` ⇒ `TestPollerClinePassPerAccountEstimate` + `TestPollerClinePassEstimateApplied` 双红；③ `cmd/prism/metapi.go` 去掉 dbPresent 分支 ⇒ `TestClinePassRosterDelta` 三条子用例红。三条还原后文件 `md5` 与工作树一致（`155cd283…`/`fca87854…`/`3be45adf…`），golden dump `md5` 回到 `43698c38…`。
- **端到端（临时 harness，未入库）**：真实调 `refreshQuotaAccounts` 三情形全 PASS —— 库文件缺失（WARN `msg="clinepass quota accounts dropped to zero" previous=2 reason="metapi database absent"`、`clinepass_quota_roster_drops_total` +1、`clinepass_quota_accounts` = 0、roster 仍清零）、库在且 2 个 active 账号（gauge = 2、无 drop WARN）、发现成功但 0 账号（reason `no clinepass accounts discovered`）。

## 与旧笔记的关系
- `20260929-clinepass-multi-account-metapi.md`：合卡版式/调色板/数字段等**排版结论仍 active**；其中「账号列举」一处实现细节已被本轮改写（SQL 加 `status='active'`、删除仅指纹版列举、指纹统一由 `planusage.KeyFingerprint` 计算），本轮补充记录于本笔记，旧笔记相应位置只加指向本笔记的注释、不改旧结论。
- `20260925-clinepass-quota-total-estimate.md`：估算接入/降级铁律仍 active；其「占比为 0 或 ≥100% 的窗口不产出总额」的表述**对 ≥100% 已不适用**（见「行为变更 2」），发版时须在 README/changelog 显式 supersede。

## 来源
- 本轮任务 `[MARK-PRISM-QUOTA-12]`（双审收尾：F1/F2/F3/F4 + consider① + O5 + 遗留清单）；上一轮 `[MARK-PRISM-QUOTA-9]`（显示名去尾部数字后缀 + 行身份不变）、`[MARK-PRISM-QUOTA-8]`（合卡版式定稿）。
- 相关实现：`internal/planusage/{types,poller,report,registry}.go`、`internal/metapiusage/{store,source}.go`、`cmd/prism/{main,metapi,quota}.go`。
