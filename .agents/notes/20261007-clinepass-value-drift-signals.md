---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: magpieusage, planusage, cmd/prism
---

# ClinePass 估算两条「求和成功但值错了」的路径补观测：账号 id 零命中 + 窗口起点早于日志起点（分子是下界）

## 一句话结论
- 补的是**可观测性**，不是口径：求和返回值、`/admin/quota` JSON 字段与语义、卡片版式与列、`prism quota`
  输出**一律未动**；`clinepass_usage_source_errors` / `clinepass_usage_source_status` 的语义也**未动**
  （它们仍只表示「源读不到」）。
- 新增两个计数（同一 `clinepass_usage_` 前缀、同一「指标名就是部署契约」的规矩）：

  | 指标名 | 触发条件 | 口径 |
  | --- | --- | --- |
  | `clinepass_usage_account_unmatched_total` | 该账号 id 在**整份** usage log 里**一行都没有** | **每次求和 +1**（与 `clinepass_usage_source_errors` 同为「每次调用」） |
  | `clinepass_usage_window_lower_bound_total` | 窗口 `from` **早于**日志里最早一条可归属行 | **每次求和 +1** |

- 配套三条日志（沿用本模块既有 slog 风格：小写消息 + 结构化 kv，转换各一次）：

  | 级别 | 消息 | kv |
  | --- | --- | --- |
  | WARN | `clinepass account matched no usage row, total estimate unavailable` | `path`, `account` |
  | Info | `clinepass account matches usage rows again` | `path`, `account` |
  | WARN | `clinepass window starts before the usage log, numerator is a lower bound` | `path`, `from`, `log_start` |

- **A（静默零）**：`sumTokens` 对「id 非空但匹配 0 行」一直返回 `(0, nil)` —— 返回值**保持不变**
  （调用方不该因为账号空闲而开始失败），只是不再静默：计数 + 每账号一次 WARN。
- **B（下界被当成总量）**：日志起点晚于窗口起点时该窗口的分子只是**下界**，估计值偏小；这是**过渡态**，
  **随窗口重置自愈**；现在从 `/metrics` 与 WARN 能看出「这个窗口的分子是下界」。

## 背景
- oracle 复审出的两条，用户已批准修（任务书 `[MARK-PRISM-FIX-DRIFT]`）：
  - **A**：`internal/magpieusage/usage.go` 的 `sumTokens` 在 `providerKeyId` 非空但匹配 0 行时返回
    `(0, nil)`，完全静默；`cmd/prism/magpie.go` 的 `applyQuotaClinePassEstimate` 只在 id **为空**时才
    增加 `clinepass_quota_estimate_skipped_total`。后果：magpie 侧 `providerKeyId` 算法一旦漂移
    （例如改成 16 hex）或 key 轮换留下旧行，prism 会把总额静默算成错值，而
    `clinepass_usage_source_status` 仍是 `ok`。
  - **B**：`/root/.config/magpie/usage.jsonl` 的日志起点（**2026-10-07 只读实测首行 `t` =
    `2026-10-06T15:55:33.923381777+08:00`**，文件 9236 行 / 约 26 小时 / 仍在追加）**晚于**当前
    周/月窗口的 `period_start`，所以周分子只是**下界**；而 `internal/planusage/estimate.go` 的
    `ApplyClinePassEstimates` 又用周分子反推池大小 L 并写月 = 2×L，于是**卡片显示值也偏小**
    （用户已同意把「下界」作为已知偏差写进文档，不改显示）。
- 已知偏差本身**不修**（修不了：日志里就是没有更早的流量），修的是「它现在有信号」。

## 决策

### 信号一：账号 id 零命中（A）
- 判据放在 `Source.sumByAccount`：拿到索引后看 `len(idx.byKey[accountID]) == 0` —— 即该 id 在**整份文件**
  （不只是本窗口）里一行都没有。这就是「magpie 的 id 推导漂移」与「key 轮换后新 id 没有自己的行」的形状。
- **明确不是这条**：bucket 存在、只是本窗口内没有记录 —— 那是**合法空窗口**（新窗口第一分钟必然如此，
  账号这周没跑也如此）。按整份文件判断而不是按窗口判断，就是为了不天天误报。
- 计数按**每次求和** +1（一轮 = 一账号 3 个窗口 = +3），仍在坏的状态计数器持续增长而不是平掉，便于
  「还在坏吗」与「坏了几次」两个问题一起回答。
- WARN **每账号一次转换**：`Source.unmatched map[string]bool`（`s.mu` 保护）；该 id 重新有行时删除并
  打一次 Info。用 per-account 集合而不是单一全局 bool —— 两个 ClinePass 账号是**并发**求和的
  （poller 一个 key 组一个 goroutine），单一 flag 会被一个账号的成功清掉、又被另一个账号的失败置上，
  变成每轮 WARN。
- 返回值保持 `(0, nil)`，估计值该空还是空（现有的「无总额」路径）。

### 信号二：窗口起点早于日志起点 ⇒ 分子是下界（B）
- `usageIndex` 新增 `earliest`（**最早一条可归属行**的纳秒时间；`scanUsageLog` 扫描时取 min，`0` = 文件
  里没有任何可归属行）。判据：`earliest > 0 && fromUnix > 0 && fromUnix*1e9 < earliest`。
- 计数按每次求和 +1；WARN **每个窗口起点一次**：`Source.lowerBoundFroms map[int64]bool`（`s.mu` 保护），
  键是 `from`，文案带 `from` 与 `log_start` 两个 RFC3339 —— 只有这两个数同时可见，操作者才知道缺多少。
- 为什么是「每个窗口起点一次」而不是像 A 那样「每次转换一次」：窗口只会**向前滚动**，起点就是它的身份
  （`sumByAccount` 只拿得到 from/to，看不到窗口名）。同一窗口每轮都会重新观测到，但只 WARN 一次。
  集合有天然上界：只有「起点早于日志起点」的窗口才会被记，而任何窗口最多一个窗口跨度后就滚过日志起点，
  之后**再也不新增**（实测场景下封顶十几个条目，之后永久停止增长）。
- 为什么**不**复用 `stale` / `degraded`：下界不是「读不到日志」——日志可用、估计照样出，只是值小。
  把 `clinepass_usage_source_status` 置 `degraded` 会把「可用但偏小」和「不可用」混成一档，而且
  **已经 degraded 时新的真不可用不会再 WARN（同一 flag ⇒ 掩盖）**。故用**独立**指标 + 独立状态。

### 月总额由周分子反推 ⇒ 下界会被同样放大到月窗口
- `ApplyClinePassEstimates` 只求**一个**池大小 L：周窗口可用（`0 < frac < 1` 且 tokens > 0）时
  `L = tokens_w / frac_w`，月窗口写 `2L`（月 = 2×周 由构造保证）；只有周**不可用**（新周无流量 / 周已打满）
  才退回 `L = tokens_m / (2·frac_m)`。
- 所以：周分子是下界 ⇒ L 是下界 ⇒ 月的 `2L` 也是下界（偏差同样放大）。周窗口打满（`frac ≥ 1`）走
  「T/T 实测消费」兜底，一样偏小。
- 这条**不改**（属显示/估算口径与对外契约），只在本笔记与 README 里写明为已知偏差。

### 该状态随窗口重置自愈
- 日志起点是**固定**的（magpie 的日志无限追加、永不轮转、永不回填），窗口起点则**每次重置向前滚**：
  一旦某窗口的 `period_start ≥ 日志最早一行`，该窗口就不再触发下界信号（WARN 不再出现、计数不再增长）。
  周窗口最快自愈（下一个周重置），月窗口最慢（下一次月重置）；两者都不需要人工干预、不需要重启、
  不需要 SIGHUP。
- 零命中信号的自愈路径**不同**：换 key 后新 id 一旦开始在日志里出现（SIGHUP 重发现 + magpie 写了新行），
  下一次求和即恢复（一次 Info）；而**算法漂移不会自愈** —— 这正是它必须**持续可见**（每轮计数）的原因。

## 对外契约
- 未动：`/admin/quota` 的 JSON 字段与语义、卡片版式与列、`prism quota` 输出、
  `clinepass_usage_source_errors` / `clinepass_usage_source_status` /
  `clinepass_quota_accounts` / `clinepass_quota_roster_drops_total` /
  `clinepass_quota_estimate_skipped_total` 的名字与语义。
- 新增的只有 `/metrics` 上两个**新** expvar 名字（上表）与三条日志，已同步进 README 的 `/admin/quota`
  行（那里的 metrics 说明就是 ClinePass 指标的登记处）。

## 被放弃的方案（必填）
- **零命中直接返回 error（复用 `clinepass_usage_source_errors` + `source_status=degraded`）**：否决。① 会把
  「这个账号在日志里一行都没有」记成「日志读不到」，稀释 `source_status` 的语义；② 已经 `degraded` 时新的
  **真**不可用不会再 WARN（同一 flag）⇒ **掩盖**；③ 账号整天没跑（或新账号还没第一条流量）也会被计成
  `degraded`，而 `0` 本来就是它诚实的答案。代价是两个新指标名，已在 README 登记。
- **只加 WARN 不加计数**：否决。WARN 只在转换时出现一次，操作者无法回答「现在还在坏吗」「坏了多久」——
  计数（每次求和口径）才是持续状态。
- **每轮都 WARN（不做转换去重）**：否决。一轮 = 一账号 3 个窗口，会把日志刷满；违背本模块「WARN 一次转换 /
  恢复一次 Info、never once per round」的既有契约。
- **单一全局 bool 表达下界**：否决。两个账号并发 + 三种窗口并存 ⇒ flag 在同一轮内反复翻转，每轮都 WARN。
- **把「窗口内 0 行」也算成信号**：否决。账号有 bucket 但当窗口没流量是**正常空闲**，计它等于天天误报。
- **用 gauge（如 `clinepass_usage_log_start`）代替计数**：本轮不做。计数 + WARN（带两个时间戳）已能回答
  「这个窗口的分子是下界」；再加 gauge 是额外契约面。列为遗留。
- **把下界/零命中反映到 JSON 或卡片（例如加字段或把估算标灰）**：否决。属对外契约，任务书明令不动；
  真需要表达也该先停手上报，不做自行扩权。

## 测试
- 只加在既有文件 `internal/magpieusage/usage_test.go`（不新开文件），两个最小用例：
  - `TestSumUnmatchedAccountIsObservable`：漂移 id（16 hex）仍返回 `(0, nil)`，但计数 +1/次、WARN 恰好一次；
    匹配账号（含窗口内空闲）**不**动计数；交错求和不让 WARN 每轮复现；该 id 重新有行时一次 Info 并静默。
  - `TestSumWindowBeforeLogStartSignalsLowerBound`：`from` 早于日志首行 ⇒ 计数 +1/次、WARN 一次（且文案含
    窗口起点与日志起点两个时间戳）；同一窗口第二轮只计数不再 WARN；`from` 落在日志首行/之后的窗口**不**触发。
- 计数在测试里按**名字**取（`expvar.Get`）：指标名是契约，改名必须让测试红而不是看不见。
- 反向变异核对：把 `sumByAccount` 里两个 observer 调用注释掉 ⇒ 两个用例双双红（`counter delta = 0`），
  还原后绿 —— 断言真的在守护这两条信号。

## 遗留
- 未加 gauge/`log_start` 导出（见「被放弃的方案」）；若将来需要「一眼看出还差多少」，再评估。
- 生产侧尚未部署观测：本改动只到代码 + 文档，`/metrics` 上的两个新计数要在下次发布后才能看到实际数值。
- magpie 侧 `providerKeyId` 一旦真的漂移，prism 只**报告**不**自愈**（不会去猜新算法）；修复路径仍是让
  magpie 与 prism 的 id 口径重新一致。

## 与既有笔记的关系
- `20261007-clinepass-usage-source-magpie.md`（active）：本文是**同一改动集**（未提交分支
  `feature/magpie-usage-source`）在复审后的**追加**，**不取代它任何结论**——「求和口径（`in + out`，剔除
  cache）」「过滤口径（模型前缀 `cline-pass/`；归属走双轨：带 `providerKeyId` 的行按 id 直归、不看
  provider，keyless 行限 `provider == "cline"` 且按掩码尾唯一命中账号）」
  「降级契约（读不到日志 ⇒ 不出总额、
  不影响快照与展示、状态转换各一次日志）」全部仍成立。本文显式补充一条边界：
  `clinepass_usage_source_errors` / `clinepass_usage_source_status` **仍只表示源读不到**，零命中、下界
  与归属侧条件（含后加的 `clinepass_usage_attribution_stale_total`）
  **都不计进它们**，各自走独立指标。
- `20261003-clinepass-quota-estimate-derivation.md`（active）：不取代。派生结论（周锚定单向、月 = 2×周、
  5h 独立反推、整数百分比按区间中点反推）不变；本文只指出「周分子是下界时，该偏差会被月窗口同样放大」。

## 来源
- 用户任务书（`[MARK-PRISM-FIX-DRIFT]`）：oracle 复审两条 + 用户批准修（含「下界作为已知偏差写进文档」）。
- 本机只读实测（2026-10-07）：`/root/.config/magpie/usage.jsonl` 首行 `t` =
  `2026-10-06T15:55:33.923381777+08:00`、末行 `t` = `2026-10-07T18:14:11+08:00`、共 9236 行
  （只读 `t` 字段；凭据类内容未读取、未记录）。
- 代码：`internal/magpieusage/usage.go`（`usageIndex.earliest`、`scanUsageLog`、`sumByAccount`）、
  `internal/magpieusage/source.go`（两个 expvar + `observeAccountMatch` / `observeWindowCoverage`）、
  `internal/planusage/estimate.go`（未改，`ApplyClinePassEstimates` 的月 = 2×周 派生）。
