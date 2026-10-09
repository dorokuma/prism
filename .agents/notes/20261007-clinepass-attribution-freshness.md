---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: magpieusage, cmd/prism, config
---

# ClinePass 归属表新鲜度：读不到 providers.json 即停用尾号归属（+ 订阅级求和死代码删除）

## 一句话结论
- 归属（Track 2，掩码尾 → 账号）用的尾号表**只有「本次真的读到 providers.json」才算确认**：发现失败
  （读/解析错误）或**文件不存在**时，上一份表被标为**陈旧**（`markAttributionStale`），
  `attributionTable()` 对陈旧表返回 nil ⇒ **任何求和都不再用它归属**，keyless 行一律落回未归属，
  直到下一次成功的发现（`installRoster`）——避免用「换 key 之前的尾号表」把新 key 的行归到旧账号（**串账**，
  且这种错配是静默的：分子偏大/偏小都不会报错）。
- 「文件不存在」与「文件存在但没有 clinepass 账号」**不是同一件事**：前者保留上一份 roster 但**不确认**
  归属表；后者是本次发现的答案（空表），照旧安装（空表本来就归属不了任何行，且不冒充恢复行）。
- 陈旧状态**只在求和侧**推进状态机并计数：新增 `clinepass_usage_attribution_stale_total`
  （单位＝**每次求和**，与 `clinepass_usage_account_unmatched_total` 同单位、与按行的
  `clinepass_usage_unattributed_rows_total` 不同单位），WARN 每状态转换一次；**恢复行由成功的发现打印**，
  所以「又能归属了」永远不会在表未被确认时出现。发现侧保持沉默契约（无 magpie 的主机不刷日志）。
- 缓存失效条件因此有两个：文件身份（dev+inode / size / mtime）**或**已安装的尾号表变了。
  「发现换了 key 尾」或「归属表转陈旧/恢复」会让下一次求和重扫日志——**即使日志本身没动**。
- 顺带删除上一轮遗留的订阅级 `SumClinePassTokens`（deprecated、无调用方）连同它在 Source / 索引里的
  求和入口：删后全仓只剩按账号的 `SumClinePassTokensByAccount`，「唯一求和入口是按账号的」恢复成立
  （见 `20261007-clinepass-attribution-followups.md` L4/L5.5）。

## 背景
- 触发：`[MARK-PRISM-REWORK3-ATTRIB]`（换源 + 双轨归属的复审轮）。
- 上游事实：magpie 的 providers.json 是**换 key 时会被改写**的文件，prism 的 roster 只在启动 / SIGHUP 重读；
  两次发现之间，prism 手里的 roster 就是它**轮询用的**那份（`providerKeyId` 由 prism 与 magpie 各自从 key 算出）。
  因此「属谁」的答案必须与「谁在被轮询」一致，否则分子与百分比来自两套 key。

## 变更点（文件 → 语义）
- `internal/magpieusage/providers.go`
  - `clinePassAccounts(path) (accounts, found, err)`：第二个返回值报告**文件是否真的被读到**（无文件 =
    `found=false`、非错误，保持「无 magpie 的主机不出声」）。
  - `ListClinePassAccounts`：**错误**与**无文件**两条路径都 `markAttributionStale()`，然后照旧返回
    （错误 → 返回 err 让调用方保留上一份 roster；无文件 → 返回 nil,nil 让调用方看到空 roster）。
  - 新增 `markAttributionStale()` / `attributionIsFresh()`；`attributionTable()` 增加新鲜度门
    （陈旧 ⇒ nil）。陈旧时**保留**表本身不清空：下一次成功发现整份替换，且碰撞 WARN 要能点名 id 集。
- `internal/magpieusage/usage.go`
  - `indexFor()` 的复用条件：`index.matches(info) && indexAttribution.same(attribution)`，其中
    `attribution` 取自 `attributionTable()`（陈旧 ⇒ nil ⇒ 与已缓存的表不同 ⇒ 重扫一次，之后 nil==nil 即不再重扫）。
  - 删除订阅级（整份日志 / 全订阅）求和的旧入口（deprecated、无调用方）及其实现。
- `internal/magpieusage/source.go`
  - 新增 expvar `clinepass_usage_attribution_stale_total` + `observeStaleAttribution()`（每求和一次 +1，
    并在**首次**出现时打一次 WARN，原因 `attribTableStale`）；`unattributedRows` 的注释补「按行 × 按扫描
    累计、只当旗标看、不要按增长率设阈值」。
- `internal/magpieusage/providers_test.go` / `usage_test.go`：`clinePassAccounts` 的 `found`、挂起用例、
  重扫用例（见「验证」）。
- 订阅级求和入口删除后**调用方无需改动**：`cmd/prism` 两个调用点（`cmd/prism/magpie.go` 的
  `clinePassTokenSum`、`cmd/prism/main.go` 的 poller 适配）走的都是按账号的
  `SumClinePassTokensByAccount`；删后全仓 `grep -rn SumClinePassTokens` 只剩它的这个按账号形态。
- 文档：README 的 `/admin/quota` 行（重扫条件、roster 新鲜度、行计数语义）、`config.yaml.example` 的
  ClinePass 段（时间窗上界、打满窗口的总额、新鲜度、三个归属侧计数）、
  `20261007-clinepass-{usage-source-magpie,value-drift-signals,attribution-followups}.md` 的对应订正。

## 验证
- 新增/加固测试（均在既有文件内）：
  - `TestAttributionSuspendedUntilDiscoveryConfirms`
    - 「a failed discovery suspends attribution until the next success」：发现失败后求和只剩**可归属**的行
      （7，不是 18）、`unattributed_rows_total` 增加、`attribution_stale_total` **每次求和 +1**（两次求和 = +2）、
      挂起期间不重复 WARN、下一次成功发现后**恢复**（回 18）且恢复行由发现打印、恢复后不再计 stale。
    - 「a missing provider file does not confirm a roster」：文件消失 → 同样挂起，且**发现侧不出声**
      （无 magpie 的主机契约：无 WARN、无 error、空 roster）。
    - 「a file that names no account is a success that attributes nothing」：读到了但里面没有 clinepass 条目
      ⇒ 空表是**确认过的**（stale 计数不动），但它归属不了任何行（自带一次原因 WARN、**不得**冒充恢复行）；
      下一次读到真 roster 才打恢复行并恢复归属。
  - `TestSumAttributionDualTrack` 新增「a rescan uses the discovered table, never a re-read of the file」：
    轮换 key **且日志增长**（缓存必然未命中）后，重扫仍用**发现安装的**表 ⇒ 该账号总额不变（5，不是 0）。
- **变异验证**（证明用例真的守着行为，不靠空跑；三处变异各自被抓）：
  - M1 `attributionTable` 去掉新鲜度门（陈旧表仍归属）⇒ FAIL：`sum after a failed discovery = (18), want (7)`。
  - M2 `!found` 分支仍 `installRoster(accounts)`（空发现冒充确认）⇒ FAIL：挂起用例的缺失文件子用例。
  - M3 `indexFor` 在缓存未命中时**重读 providers.json**（本轮明确否决的替代实现）⇒ FAIL：
    `sum after the rescan = (0), want (5)`。
- 本机只读实测（2026-10-07，`/root/.config/magpie/`，真日志）：
  - 日志 `cline-pass/` 行 8628、**keyless 921 行**，掩码尾（`API key …326d`）**唯一命中** `ae4c5650bd`；
    另有 1 行 `providerKeyId=b5e5862607`（不在当前 roster）——Track 1 **不看 roster**，符合文档口径。
  - `prism quota --json --provider clinepass`：两个账号 6 个窗口的 `limit_tokens_estimate` 与**独立重算**
    （Python 复刻过滤/双轨归属/时间窗/周锚定池/5h 独立反推）**逐字相符**，含月 = 2×周
    （65836996 = 2×32918498、227866504 = 2×113933252）与 `percent=0` 的 5h 窗口**无总额**。
  - 低频账号（`ae4c5650bd`）三次求和的数字在两次读数之间**完全不动**（该账号 17659654 稳定），
    说明「缓存 + 一次扫描回答所有窗口」确实生效。
  - 运行日志只有「窗口早于日志起点 ⇒ 分子是下界」的一次性 WARN，无归属类 WARN（尾号唯一、表新鲜）。

## 被放弃的方案
- **读不到 providers.json 就把归属表清空成空表**：空表会被下一次求和当成「本次确认过的空答案」，
  空表与「未确认」不可区分，且会让恢复行被误打印（上一轮修正过的静默假象）。否决。
- **只标陈旧、不计数**：归属挂起本身是「总额偏小」的静默形态，必须在 `/metrics` 上看得见。否决。
- **每次求和重读 providers.json**：会把「轮询用旧 key、归属指向新 key」的错配引入求和侧（变异 M3 即此），
  也放弃「一次扫描回答所有账号 × 所有窗口」的缓存。否决。
- **把归属挂起计入 `clinepass_usage_source_status=degraded`**：会把「可用但偏小」和「读不到日志」混成一档，
  并让一个与源读取无关的条件抖掉 lifecycle 状态。否决（沿用前几轮的同一条边界）。

## 遗留
- 挂起窗口内 ClinePass 总额是**下界**（该窗口内 keyless 行不计入）：这是换 key / 文件暂时不可读期间的
  **有意**行为，自愈条件是「下一次成功的发现」（服务侧 = 启动 / SIGHUP；CLI = 每次调用各自发现一次）。
- `clinepass_usage_attribution_stale_total` 是**每次求和**单位，服务侧一轮 = 账号 × 窗口，不要按轮比对。

## 与既有笔记的关系
- `20261007-clinepass-usage-source-magpie.md`（active）：不取代。本篇补其「归属与轮询同源」一节的新鲜度边界
  （原文写「调用方保留上一份 roster 与归属表」——归属表现在**保留但停用**，已按代码订正）。
- `20261007-clinepass-attribution-followups.md`（active）：落实其 L4（删除订阅级 `SumClinePassTokens`）
  与 L5 第 5 条（误用面随删除消失）；L5 第 2 条的「碰撞整表失效」结论不变。
- `20261007-clinepass-value-drift-signals.md`（active）：其「过滤口径」「独立指标」两句已按双轨与三个
  归属侧计数订正；其余结论不变。

## 来源
- 任务书 `[MARK-PRISM-REWORK3-ATTRIB]`（本轮复审要求：新鲜度契约 + 三处文档订正 + 删死代码 + 变异验证）。
- 本机只读实测（真日志 + 独立重算，见「验证」）。
