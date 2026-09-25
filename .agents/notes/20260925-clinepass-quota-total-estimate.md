---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: "planusage, metapiusage, cmd/prism"
---

# ClinePass 三窗口「总额」估算接入 metapi 只读消耗源

## 一句话结论
- ClinePass 的 5小时/周/月三个窗口在百分比之外显示「总额 X 词元」，与 SuperGrok/Gemini 的既有反推行为对齐：consumed ÷ 已用占比。
- 新增内部包 `internal/metapiusage`（`DefaultDBPath = /var/lib/metapi/data/hub.db`）：以 `mode=ro` + `busy_timeout(5000)` 只读 metapi 生产库，`SUM(proxy_logs)` 按 `model_requested LIKE 'cline-pass/%'` 与 `[fromUnix, toUnix]`（UTC 文本 `created_at` 比较）求和，单行口径 `total_tokens`，为 NULL 时回落 `COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0)`。
- 周期起点由 ResetsAt 反推（`clinepassPeriodStart`）：5h → −5h；weekly → −7d；monthly → `AddDate(0,-1,0)`（日历月回推，Go 语义是**归一化**而非钳位：2026-05-31 → 2026-05-01、2026-03-31 → 2026-03-03）。三窗口统一应用 `planusage.ApplyClinePassEstimates`（`0 < 占比 < 1 且 token > 0` 才产出）。
- 服务端 poller 按 provider 分派 `SetClinePassEstimate`，CLI `prism quota` 走 `applyQuotaClinePassEstimate`，两条路径共用同一反推函数，输出一致。
- 降级铁律：metapi 库不存在/不可读/`proxy_logs` 缺失/`created_at` 格式漂移 ⇒ 该窗口无总额（记 WARN/跳过），**不影响**快照获取、缓存、展示与失败标记；不新增配置键、不新增落盘文件。
- （加固，2026-09-25 oracle 第二意见后）服务侧求和源改为**每次求和短连接自愈**（`metapiusage.Source`：open→created_at 形状自检→SUM→close，不持有常驻句柄），启动时库不可用/metapi 换库/文件替换/数据回滚都会在后续轮次自动恢复；降级可观测：expvar `clinepass_usage_source_errors`/`clinepass_usage_source_status` + 状态转换各一次 WARN、恢复记 Info；`created_at` 文本格式增加运行时形状自检与字面量契约测试（M11 教训）。详见「加固」节。

## 背景
- ClinePass（cline.bot）套餐窗口接入见 `20260924-clinepass-quota-display-order.md`：当时只显示 `percentUsed` 百分比。用户要求对齐 Gemini/SuperGrok 的「总额」展示。
- ClinePass 流量全部经 metapi 转发（prism 是 metapi 的上游），prism 自己的 `usage.db` 里没有 cline-pass 消耗；消耗量唯一可得来源是 metapi 生产库的 `proxy_logs`。
- 既有反推模式（`internal/planusage/estimate.go`）：`LimitTokensEstimate = consumed / usedFraction`；xai 用 usage.db `grok-*`，gemini 用 usage.db `gemini-*` + agy 本地索引。ClinePass 是第三家，缺的只是「消耗量求和源」。

## 决策
- **新增 `internal/metapiusage` 包（只读求和源）**：
  - `Open(path)` 用 `file:<PathEscape(path)>?mode=ro&_pragma=busy_timeout(5000)`（与 `internal/usage` 的 `roDSN` 同构；`mode=ro` 保证 prism 永远不可能写 metapi 库），`Ping` 失败即报错，交由调用方降级。
  - `SumClinePassTokens(ctx, from, to)`：窗口边界转 UTC `2006-01-02 15:04:05` 文本与 `created_at`（metapi 以 `datetime('now')` 写入，UTC 秒精度）做包含式比较；`fromUnix <= 0` 返回 0（与 `usage.SumTokensLike` 一致）；表缺失返回错误而非 panic/空值。
  - `DefaultDBPath` 是代码常量（对齐 `agyusage.DefaultIndexPath` 的既有模式），wiring 侧 `cmd/prism/metapi.go` 的 `metapiUsageDBPath` 为包变量（供测试重定向）。
- **周期起点推导（`clinepass.go` 的 `clinepassPeriodStart`）**：上游只给 `resetsAt`，按窗口时长/日历月反推 `PeriodStart` 并写进 `Window`（与 Gemini weekly 在 fetcher 内推导的既有先例一致）；无 `resetsAt` 的窗口无 PeriodStart、无估算。JSON 输出因此新增 `period_start` 字段（纯增量）。
- **反推应用（`estimate.go` 的 `ApplyClinePassEstimates`）**：逐窗口取 `frac = windowUsedFraction(w)`（优先未取整的 `UsedFraction`，否则 `Percent/100`），`from = PeriodStart`、`to = min(now, ResetsAt)`；仅 `0 < frac < 1` 且 `tokens > 0` 时写 `LimitTokensEstimate = round(tokens/frac)`。占比 0（新窗口）无消耗可反推；占比 ≥ 100% 是上游钳位后的值，反推会系统性低估池子，故耗尽窗口不显示总额。求和错误只 WARN 并留空，绝不写 `Snapshot.Err`、不标记 fetch 失败。
- **双路接线**：poller 新增 `SetClinePassEstimate(sum)`，`fetchOne` 按 provider 分派（xai/gemini 走原 `ApplyWeekEstimate`，clinepass 走 `ApplyClinePassEstimates`，互不串路）；CLI `runQuotaWith` 对 `clinepass` 快照调 `applyQuotaClinePassEstimate`（打开只读库、defer Close、共用同一反推函数）。生产 `main.go` 在 gemini 估算之后接 `metapiusage.Source`（每次求和按次短连接，无常驻句柄；见「加固」节）；CLI 侧 `openMetapiUsage()` 每次调用打开、退出即关。
- **展示零改动**：卡片 `totalPart`（`已用 N% / 总额 X 词元`）与 legacy 表的 `formatEstimate` 已消费 `LimitTokensEstimate`，本次不改渲染代码（也就不动 xai/gemini 渲染与冻结文件的任何行为）。
- **权限与部署**：prism.service 基线 `User=prism`，但现网 drop-in `root-service.conf` 覆盖为 `User=root`，故默认路径可读；metapi 数据目录为 `0700 root:root`，若服务回落 `User=prism`，需给 prism 只读访问（ACL/绑定）——本次不改系统权限、不动 `/var/lib`，只在本笔记与交付报告登记。
- **测试**：`internal/metapiusage`（临时 sqlite fixture 的求和/边界/NULL 回落/表缺失/只读拒绝/DSN 断言）；`planusage`（三窗口起点推导与月历边界、三窗口反推与卡片/表格渲染含「总额」、未取整占比、亚百分比、求和错误/零占比/100%/无 PeriodStart 守卫、poller 生效与 provider 不串路）；`cmd/prism`（CLI 路径与共享函数结果一致、缺库/坏库降级）。

## 被放弃的方案（必填）
- **metapi 库路径做成 `quota.*` 配置键**：引入配置格式变更、默认值/校验/热重载语义（poller 的 sum 是启动期接线，路径变更需重启，得再补 restart-warning），而 agy 外挂库的既有模式就是代码常量；本机部署用默认值开箱即生效，无 live 配置更新需求；否决。
- **估算结果落盘 `clinepass-estimate.json`（对齐 grok/gemini 估算文件）**：该文件对 grok/gemini 还有 `StoredPeriodStart` 回退与检查用途；ClinePass 的 PeriodStart 恒由 live `resetsAt` 反推、也不参与 `prism usage` 默认窗口，落盘没有读取方，只增加锁/chown/IO 失败面；否决。
- **占比 ≥ 100% 也产出总额（tokens/1）**：上游把 `percentUsed` 钳在 100，真实消耗可能远超反推分母，会给出系统性偏低的「总额」，误导性强；耗尽窗口只显示限流态；否决。
- **在 `planusage` 内直接打开 metapi 库 / 依赖其 schema**：破坏「planusage 只接受 `GrokTokenSum` 函数」的既有分层，外部库 schema 会渗进配额渲染包；保持 schema 归口 `internal/metapiusage`；否决。
- **复用 `usage.SumTokensLike` 读 metapi**：时间列格式不同（`ts_unix` INTEGER vs `created_at` TEXT UTC），且 `internal/usage` 是 prism 自有账本，不应认识外部库；否决。
- **给 clinepass 单写一个 weekly-only 估算函数**：三窗口共享同一反推语义，缺的只是起点推导，复制粘贴会与 `windowUsedFraction`/`reversePool` 漂移；统一 `ApplyClinePassEstimates`；否决。

## 加固（oracle 第二意见后追加，2026-09-25）

oracle 结论「有条件放行」，两项应修落地如下。

- **应修 1：生命周期（自愈）**：服务侧求和源由「启动时 open 一次、常驻句柄」改为 `metapiusage.Source`——**每次求和一条短连接**（open→created_at 形状自检→SUM→close），不缓存句柄、不做 dev/ino 校验。
  - 选 (a) 短连接而非 (b)「保留句柄 + 每轮校验 dev/ino」的理由：短连接对换库/文件被替换/数据面回滚天然自愈（每次 open 都解析当前路径指向的文件），对启动时库不可用也不需要任何额外状态（每轮重新尝试 open 即自动恢复）；(b) 需要处理 stat 与 open 之间的竞态，还要区分「句柄过期」与「表缺失/权限/格式漂移」等失败原因才能决定是否重开，代码面更大且仍要保留兜底重开路径。成本：配额刷新间隔默认 120s、每轮 3 个窗口各一次 open+ping（毫秒级，远小于那次 SUM 查询本身），可忽略。
  - CLI 路径（`prism quota`）本来就是「每次调用 open→3 窗口求和→close」，按次自愈，本次核对后无行为改动（仅同步注释）。
  - 服务不再持可 Close 的句柄，main.go 的 shutdown 分支删除了对 Store 的 Close。
- **应修 2：可观测降级**：`Source` 维护 healthy/degraded 状态：失败按 incident 计入 expvar **`clinepass_usage_source_errors`**，当前状态由 **`clinepass_usage_source_status`**（ok/degraded）暴露；**每次 healthy→degraded 转换记一次 WARN**（连续多轮/多窗口失败不重复刷屏），**degraded→healthy 记一次 Info 恢复日志**。降级语义不变：只影响「该窗口无总额」，绝不写 `Snapshot.Err`、不影响快照获取/缓存/展示与 fetch 失败标记（有 CLI 级断言）。
- **应修 3：created_at 契约钉**：
  - 独立测试 `TestCreatedAtLayoutLiteral` 用**字面量** `"2026-09-25 21:41:03"` 钉住 `formatCreatedAt` 的输出与「19 字符、第 11 位空格、字典序随时间递增」，不复用生产常量 `metapiCreatedAtLayout`；测试 fixture 也改用字面量布局（`createdAtFixtureLayout`），不再由生产常量自证。
  - 运行时形状自检 `Store.checkCreatedAtShape`：每次求和前取样 `MAX(created_at)`（metapi schema 带 `proxy_logs_created_at_idx`，走索引非扫全表），非空且第 11 字符不是空格（含短于 11 字符的样本，如 epoch 秒文本）⇒ 返回 `ErrCreatedAtShape`，**该轮所有窗口降级不产出估算**（并经 Source 记 WARN/计数）；空表（样本 NULL/空）跳过检查。
  - **M11 教训**：窗口边界是与 metapi `datetime('now')` 文本强耦合的**文本范围比较**，此前既没有独立契约钉也没有运行时校验；格式漂移（RFC3339 / epoch 秒）不会报错，而是把文本比较悄悄带偏（同日/跨日边界错窗）→ 静默少算且测试全绿（fixture 与断言都用同一个生产常量自证）。现在三层：字面量测试（静态钉）+ 形状自检（运行时拒算）+ 既有 SUM 窗口测试。
- **变异自证（沙盘，改完即还原、sha256 校验）**：
  - 去掉 `checkCreatedAtShape` 调用 → `TestCheckCreatedAtShape`（rfc3339/epoch/新旧混合 3 例）与 CLI 端到端 `TestApplyQuotaClinePassEstimateCreatedAtDrift` 失败；后者 fixture 特意带 `prompt_tokens/completion_tokens` 列，确保失败原因是漂移而非列缺失——无守卫时漂移行在旧格式窗口下会被文本比较算成 200 词元，期望的「无总额」被打破。
  - `Source` 改为缓存句柄（模拟常驻句柄）→ `TestSourcePicksUpReplacedFile`（替换文件后仍读旧值 100）与 `TestSourceTransitionLogging`（删除文件后旧句柄仍可读）失败。
  - 去掉 WARN-once 守卫 → `TestSourceTransitionLogging`（3 条 WARN ≠ 1）失败；改 `sync.Once` 只开一次不重试 → `TestSourceStartupUnavailableThenRecovers`（恢复轮仍返回启动时错误）失败。

对应实现：`internal/metapiusage/source.go`（Source/状态机/expvar）、`store.go`（`checkCreatedAtShape`/`ErrCreatedAtShape`）、`cmd/prism/main.go`（接线）、测试 `internal/metapiusage/{store,source}_test.go`、`cmd/prism/metapi_test.go`。

## 运维前提登记（部署/排障须知）

- **单订阅假设**：`SUM(proxy_logs WHERE model_requested LIKE 'cline-pass/%')` 是「订阅级」口径，所有 cline-pass 流量都进当前展示窗口的分子，分母是该窗口的 percentUsed。现网只有一个 ClinePass 订阅（一个 prism clinepass 账号）时两者一致；**若出现第二个 cline-pass 订阅/账号（或独立 metapi 上游），这个求和不区分账号，必须复核口径后（按 metapi account_id 或给 sum 加账号维度）再依赖总额数字**。
- **「cline-pass 流量全经 metapi」运维证据**（2026-09-25 只读核对）：metapi 生产库 `proxy_logs` 有 8617 行 `model_requested LIKE 'cline-pass/%'`（总行 42769），而 prism 自己的 usage 账本（`/var/lib/prism/usage.db`）里没有对应消耗（记录的是 prism 自己代理的请求）。该结论是总额口径成立的前提；metapi 路由/账本规则变化时需重新核对。（只要 prism 不再作为 metapi 的唯一上游、或 metapi 改记法，口径即失效。）
- **metapi 换库/数据面回滚后的 prism 恢复手段**（按所选的短连接实现，如实描述）：**无需重启 prism**。每轮配额刷新（`quota.refresh_interval`，默认 120s）重新 open 当前路径的文件，替换/回滚在下一轮即生效；启动时库不可用也一样在后续轮次自动恢复。排障看 `/metrics` 的 `clinepass_usage_source_status`/`clinepass_usage_source_errors` 与日志里成对的 WARN（unavailable）/INFO（recovered）。
- **User=prism 回落需 ACL**：metapi 数据目录为 `0700 root:root`；prism.service 基线 `User=prism`，现网 drop-in `root-service.conf` 覆盖为 `User=root`，故默认路径目前可读。若去掉 drop-in 回落 `User=prism`，必须给 prism 对 `/var/lib/metapi/data/hub.db`（及同目录 WAL/SHM）只读访问（目录需要 +x，如 ACL `setfacl -m u:prism:r`＋目录 `x`，或组绑定），否则 Source 持续 degraded、ClinePass 窗口无总额。
- **开机顺序风险**：metapi 未起或未建库时 prism 先启动，ClinePass 窗口会先无总额（首轮 WARN + counter 计数），metapi 就绪后的下一轮自动恢复，不再有「永久禁用」风险。要求首轮即可用则保证 metapi 先于 prism 启动（否则接受一个刷新周期的降级）。

## 已知限制/残留（oracle 加固签字时登记的残留项）

- **N1 窗口级失败日志在持续降级时逐轮输出**：`Source` 的状态转换层只在进入/离开降级时各记一条（不刷屏，另有 expvar 计数），但 `planusage.ApplyClinePassEstimates` 的求和失败是**逐窗口**记 WARN——持续降级时约 3 行/轮（5h/weekly/monthly 各一条；默认 120s 刷新）。日志带窗口名便于定位；若后续嫌吵可在窗口级也做转换去重。
- **N2 打开/求和不随调用方 ctx 中断**：`Open` 的 ping 用独立 5s 超时（`context.Background()`），SQL 侧等待由 `busy_timeout(5000)` 封顶，调用方 ctx 取消不会提前打断这些等待；极端锁场景最坏 ~15s/轮（量级 = 3 窗口 × 5s busy 等待，有界且不累积）。WAL 常态下只读连接不被写者阻塞，无感。
- **N3 created_at 形状自检是粗粒度 tripwire**：`checkCreatedAtShape` 只验证样本第 11 位是空格（并把短于 11 字符的样本判为漂移，如 epoch 秒文本），可拦住 RFC3339/epoch 这类异形漂移；**同形语义漂移不检出**——分钟精度（`2006-01-02 15:04`）、带时区/偏移后缀、本地时间写入等「前 11 位形状不变」的漂移仍会静默带偏文本范围比较。此类变更依赖 metapi 侧改 `created_at` 写法时的人工复核（见「运维前提登记」的换库/改记法复核要求）。

## 来源
- 主代理任务说明 `[MARK-WORKER-PRISM-CLINEPASS-TOTAL]`（本仓 worker 派发）；加固由 `[MARK-WORKER-PRISM-CPA-HARDEN]` 派发（oracle 第二意见，两项应修）。
- 相关笔记：`20260924-clinepass-quota-display-order.md`（ClinePass 接入与展示序）、`20260922-usage-capsule-report.md`（估算列与卡片）、`20260923-quota-tui-card-format.md`（卡片明细行文案）。
- 上游契约实测：`data.limits[]` 的 `percentUsed`/`resetsAt`（`clinepass_test.go` 的 `clinepassUsageBody`）；metapi `proxy_logs.created_at` 为 UTC `datetime('now')` 文本、`model_requested = cline-pass/<model>`。
