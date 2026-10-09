---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: magpieusage, planusage, cmd/prism, config
---

# ClinePass 用量源由 metapi hub.db 换成 magpie usage.jsonl（对外契约不变）

## 一句话结论
- prism 的 ClinePass「总额」估算的消耗量来源由 **metapi 生产库** `/var/lib/metapi/data/hub.db`
  的 `proxy_logs`（SQL）整体换成 **magpie 的逐调用日志** `/root/.config/magpie/usage.jsonl`
  （流式逐行 JSONL），账号与 provider key 由同目录 `providers.json` 的 `clinepass` 条目发现。
- 新包 `internal/magpieusage`（usage.go / providers.go / source.go）；旧包 `internal/metapiusage`
  与 `cmd/prism/metapi.go` 整体删除（换源后零调用方），新文件 `cmd/prism/magpie.go`。
- 对外契约**除账号显示名外零变更**：`/admin/quota` JSON 字段与语义、卡片版式、`prism quota` 输出、
  metrics 名（`clinepass_usage_source_errors` / `clinepass_usage_source_status` /
  `clinepass_quota_accounts` / `clinepass_quota_roster_drops_total` /
  `clinepass_quota_estimate_skipped_total`）逐字未动。可见变化只有三条：ClinePass 账号显示名由
  metapi `username` 改为 `clinepass#<providerKeyId>`（卡片上再经既有 `stripNumericSuffix` 规则削掉
  尾部数字串，如 `clinepass#d0c4d7aa83` → 显示 `clinepass#d0c4d7aa`，行身份仍是完整名 + 色点，
  不合并）；`clinePassRosterDelta` 的一档原因文案由 `metapi database absent` 改为
  `magpie providers absent`（仅 WARN 文本，指标名不变）；服务侧/CLI 的一条 WARN 文案
  `read clinepass accounts from metapi failed` → `...from magpie failed...`。
- 账号 id 的类型由 `int64`（metapi `accounts.id`）改为 `string`（magpie `providerKeyId`，
  sha256(key) 十六进制前 10 位），连带 `planusage.AccountView.AccountID() string`、
  `AccountIDFrom`、`Poller.SetClinePassEstimate(func(string) GrokTokenSum)`。
- **求和口径由单轨改为双轨（本笔记后续修订，重要）**：旧的「`provider == "clinepass"` + `model`
  前缀」会丢掉 magpie 未归属（无 `providerKeyId`）但确属同一个订阅的 921 行；现改为**模型门槛 +
  两条归属轨**（`providerKeyId` 优先；keyless `cline` 行按掩码账号标签的 key 末 4 位对 roster
  唯一匹配）。数值会变：`ae4c5650bd` 的分子由 3 352 085 → 17 659 654（5.3 倍）；契约（字段/
  文案/指标名）仍不变。详见下文「双轨归属（Track 1 + Track 2）的实测依据」。

## 背景
- ClinePass 流量全经 magpie 网关转发，prism 自己的词元账本（usage.db）里没有这部分消耗，
  所以「总额 X 词元」的分子必须取自网关侧。原实现读 metapi 的 `proxy_logs`，而 metapi 正在
  被下线：`/var/lib/metapi/**` 可能随时消失，且 metapi 的数据面本身也可能被换掉。
- magpie **没有数据库**，用量明细只有一个无限追加、无轮转、无 schema 版本的 0600 JSONL
  （2026-10-07 实测 8885 行 / 约 24 小时 / 0 行不可解析，其中 2494 行为非 clinepass）。
- 实测（只读核对，2026-10-07）：`providers.json` 的 `clinepass` 条目 = `key`（1 把）+
  `keys[]`（1 个元素），**`keys[]` 元素只有 `key` 一个字段**——没有 `name`/`label`/`email`/`remark`
  之类可读标签，所以「用 key 派生 id 兜底」不是退路而是唯一口径。两把 key 的 `providerKeyId`
  实测为 `d0c4d7aa83` 与 `ae4c5650bd`（与 magpie 自己写在 `usage.jsonl` 里的 id 一致）。

## 决策
### 账号发现与显示名
- 只取 `id == "clinepass"` 的条目（**精确匹配**：两个文件都由 magpie 写，大小写折叠只会把
  magpie 侧改名藏成「部分命中」），key 顺序 = `key` 后 `keys[]` 文件序，空 key 跳过
  （`sha256("")` 会派生出一个看着像真 id 的值），同一把 key 重复出现只算一个账号。
- `providerKeyId = hex(sha256(key))[:10]`（`magpieusage.KeyID`）；显示名 `clinepass#<id>`
  = magpie 自己的账号标签形式。**provider key 原文只在内存里用于 Bearer，绝不进日志/渲染/测试
  fixture/报告**，对外可见的身份只有 id 与 `planusage.KeyFingerprint`（色点用）。

### 求和口径（与 metapi 原口径逐条对照）
| 维度 | metapi（原） | magpie（新） |
| --- | --- | --- |
| 时间字段 | `created_at`（TEXT `YYYY-MM-DD HH:MM:SS` UTC） | `t`（RFC3339 带 +08:00 / 纳秒） |
| 窗口 | SQL `created_at >= ? AND created_at <= ?`（**闭区间**） | 逐行比对 `t` ∈ [from, to]（**闭区间**，按纳秒全精度比较，不做秒级截断） |
| 模型门槛 | `model_requested LIKE 'cline-pass/%'` | `model` 前缀 `cline-pass/`（含斜杠，`cline-pass-extra/x`、`xcline-pass/x` 不命中；`cline-free/*` 天然不命中） |
| 账号归属 | `proxy_logs.account_id = <metapi account id>` | **双轨**（Track 1 优先）：① `providerKeyId` 非空 → 按该 id（**不看 `provider`**，实测 1 行 `provider=metapi` 也按 id 归属）；② keyless 且 `provider == "cline"` → 掩码账号标签（`providerAccount`，空则回落 `host`）的 key 末 4 位对 roster 的 key 末 4 位做**唯一**匹配：命中 1 个则归属，0 个不算，≥2 个拒绝归属（建表时发现尾部碰撞则整条 Track 2 禁用）；③ 其余一律不归属 |
| 分子 | `COALESCE(prompt_tokens,0)+COALESCE(completion_tokens,0)`（**cache 不计入**） | `in + out`（**`cache_read` 不计入**，与上游百分比口径一致） |
| 失败请求 | 计入（无 status 过滤） | 计入（同样无 status 过滤，日志里失败行也写） |
- 只读：两个 magpie 文件全程只读，prism 不写、不改、不新建；测试有「内容+size+mode+mtime 前后
  一致」的断言（`TestSourceNeverWritesTheLog`）。
- 流式：`bufio.Reader.ReadBytes('\n')` 逐行，不把文件读进内存；**残缺末行容忍**（magpie 正在追加时
  读到的半行不解析即跳过，下一次扫描补上；半行但**能**解析则计为完整记录）。
- 行形状自检：`t`/`provider`/`model`/`in`/`out` 任一缺失（含 JSON null）、`t` 不能按
  RFC3339 解析、JSON 本身坏 ⇒ `ErrUsageLine`，**整个窗口不出总额**（不静默按错窗口少算）；
  一条坏行会否掉整份文件的求和，即使其余行有效（宁可无总额，不可少算）。文件存在但无任何
  可用行 ⇒ `ErrNoUsageLines`（空/全空白），同样不出总额。
- 文件版本缓存：`dev+ino`（`os.SameFile`）+ `size` + `mtime` 三者全同才复用上次解析结果；
  追加（size 变）、截断（size 变）、替换（inode 变）都会重扫。一次扫描答完所有账号与所有窗口
  （poller 每账号 3 个窗口共享），空文件不缓存（下一轮重试）。**不常驻文件句柄**。
- 空账号 id 报错而不是退回「订阅级求和」：两个 ClinePass 账号是两套独立池，共用分子会把一个
  账号的流量加进另一个账号的百分比（串账）。这同时关掉了 20261003 笔记里「服务侧无对称告警」的
  遗留：服务侧 `AccountIDFrom` 返回 `""` 时，求和源会返回 `empty account id` 错误 → 该轮不出
  总额 + 一次状态转换 WARN + `clinepass_usage_source_errors` 计数。

### wiring
- `cmd/prism/metapi.go` → `cmd/prism/magpie.go`（改名并改符号：`magpieUsagePath` /
  `magpieProvidersPath` / `newMagpieSource()` / `magpieAccountView` / `magpieProvidersPathIfPresent`），
  服务端（`refreshQuotaAccounts`，启动 + SIGHUP）与 CLI（`prism quota`）共用
  `readClinePassAccounts` 与一个 `magpieusage.Source`。
- **config 里 `provider: clinepass` 的账号仍然跳过**（规则保留）：账号与 key 的唯一来源是
  magpie 的 `providers.json`，config 里的 clinepass 账号既拿不到 provider key（无法轮询上游、
  也无法按 `providerKeyId` 归属消耗），又会在 roster 里与 magpie 发现的同名账号重复。
- 路径依旧是**代码内常量**（`magpieusage.DefaultUsagePath` / `DefaultProvidersPath`），不新增
  配置键；`cmd/prism/magpie.go` 里的两个包变量只为测试重定向而存在。

### 双轨归属（Track 1 + Track 2）的实测依据（重要）
日志里另有 932 行 `provider == "cline"`，其中 921 行 model 是 `cline-pass/deepseek-v4.1-flash`
（其余 11 行是 `cline-free/*`）。**旧实现把它们全丢掉，实测证明这是漏算**（2026-10-07 只读核对）：

| | provider `clinepass` | provider `cline` |
| --- | --- | --- |
| 行数（`cline-pass/*`） | 6885（keyed；另有 1 行 `provider=metapi` 带 id） | 921（**全部 keyless**）；另 11 行 `cline-free/*` |
| `providerKeyId` | 100% 都有 | 100% 都没有 |
| `host` | `api.cline.bot`（真实上游） | `cline as API key …326d` 之类的**合成标签**，不是 URL |
| `providerAccount` | 无 | `API key …326d`：magpie 自己的**掩码账号标签**，末 4 位 = 某把 provider key 的末 4 位 |
| providers.json 条目 | `key`(67) + `keys[]`(1)，`preset: clinepass`，`keysUrl: app.cline.bot` | `key` **为空** + `keys` 为空，字段是 `hidden/quiet/routing/sink`（无凭据 sink 条目） |

关键证据是**掩码的末 4 位与 roster 自己的 key 末 4 位对应**：这 921 行的掩码尾**统一是 `326d`**
（即 key `ae4c5650bd`），且**每一行都唯一命中**（0 行歧义、0 行落空）——即 magpie 自己写明了这些
调用是用那把 ClinePass provider key 打的；而 `cline` 条目本身**没有 key**，这个掩码不可能是它自己的。
据此按掩码归属到对应账号，而不是当成「非订阅来源」。

事实订正（2026-10-07 复核）：这 921 行**只出现 `326d` 一个尾号**；另一把 key `d0c4d7aa83` 的尾号
`5f27` 在这 921 行里**一次都没出现**（它只出现在 1 条 `cline-free/*` 行上，0 词元）。`326d` 另
出现在 10 条 `cline-free/*` 行上（合计 77 928 词元）。这 11 条 `cline-free/*` 行是被**模型前缀
门槛**（`cline-pass/`）排除的，不是被尾号排除的：归属的可靠性来自「模型前缀 + 掩码唯一命中」，
尾号只回答「归哪一把 key」，不构成过滤条件。

对照实测量（同一份日志 9534 行，2026-10-07 18:41）：

| 口径 | `d0c4d7aa83` | `ae4c5650bd` | 其他桶 |
| --- | --- | --- | --- |
| 旧（`provider == "clinepass"`） | 6481 行 / 29 037 775 | 404 行 / 3 352 085 | — |
| 新（双轨） | 6481 行 / 29 037 775 | **1325 行 / 17 659 654** | `b5e5862607`（metapi 那 1 行，0 词元） |

即 `ae4c5650bd` 的分子被少算了 **14 307 569 词元（+921 行，5.3 倍）**，`d0c4d7aa83` 不受影响
（它没有任何 keyless 行）。两口径的**差值**与 `d0c4d7aa83` 的**零偏移**才是结论：日志会继续追加，
绝对数字随快照上移（planner 基线 6352 行 / 28 577 635 词元在本机可**逐字节复现**——按追加顺序
累计到第 6352 行为止正好是 28 577 635 词元，之后的差全是日志增长，不是口径差异）。

同一次核对还确认：两把 key 的末 4 位无碰撞（碰撞会禁用整条 Track 2，见 `newClineAttribution`）、
11 行 `cline-free/*` 被模型门槛挡住（免费档，无订阅池，共 77 928 词元均未计入）。

**provider 门槛今天的实作用**：实测 0 行 `cline-pass/*` 落在「keyless 且 provider 既不是
`clinepass` 也不是 `cline`」这一档，但这道门槛不能去掉：别的 provider 的 keyless 行即使掩码尾
碰巧撞上 roster 尾巴，也不得归到 ClinePass 账号（串账）。同理，43 行 `provider == "metapi"`
（host `panel.hotkids.eu`）里只有 1 行 model 是 `cline-pass/*`，靠 Track 1 按 id 归属（0 词元），
其余是别的模型家族；provider 为空串的 18 行全部 keyless 且**模型也不带 `cline-pass/` 前缀**
（13 行是 `cline/cline-pass/deepseek-v4.1-flash` + status 404，带前缀的 `cline/…` 不命中），
一律不归属（这 13 行合计 0 词元，计不计都不影响数字）。

**残留风险（新增）**：Track 2 依赖 magpie 的掩码形态（`…` + 末 4 位）与 roster 的 key 表未轮转。若掩码
位数/样式变化，或 key 轮转后归属表与轮询名单不一致，这些行会**静默**落回「不归属」，分子只会变小——
这个形状现已按**行**计数（用户已批准 D1）：`clinepass_usage_unattributed_rows_total` 在被**扫描**的
日志里把未归属的 keyless 行 +1，与按「账号 × 每次求和」计数的 `clinepass_usage_account_unmatched_total`
是两个单位，不得混用（前者今天的真实日志上为 0：921/921 全命中）。归属与轮询**同源**：归属表只随
**账号发现**（启动 / SIGHUP）一起更新，不再每次求和重读 providers.json，所以「换 key 窗口」里不会出现
「百分比来自旧 key、Track 1 只有旧 id、归属表却已指向没人轮询的新 id」这类静默少算。
当前归属层的 WARN（每状态转换一次，即每次发现至多一条）覆盖三种条件：**掩码碰撞**、**读到的表里没有
任何可用 key 尾**、以及**尾号表未获确认**；**providers.json 不可读**（或文件不存在）
是**账号发现**的失败：调用方保留上一份 roster 并打 WARN，归属表则**标记为陈旧、不再参与归属**
（keyless 行落回未归属，计入 `clinepass_usage_unattributed_rows_total`；用过未确认表的**求和**另计入
`clinepass_usage_attribution_stale_total`，见 `20261007-clinepass-attribution-freshness.md`），
两者都不计入 `clinepass_usage_source_errors`（那只表示「usage.jsonl 读不了」）。

## 被放弃的方案（必填）
- **把 `provider == "cline"` 的行整体算进 ClinePass 消耗（不按掩码归属）**：否决。这 921 行
  确实是订阅消耗（实测见上节），但「整体算」只能落到一个桶里：两个账号是两套独立池，入口错
  一个就是串账。必须按掩码尾唯一匹配归属，0 命中或 ≥2 命中宁可不算。
- **以 `model` 前缀为唯一过滤条件（去掉 provider 门槛）**：否决。keyless 行的 provider 门槛
  今天是空操作（实测 0 行），但它是防串账的最后一道：别的 provider 的 keyless 行即使掩码尾碰巧
  与 roster 尾巴相同，也不得被归到 ClinePass 账号。另一个被否决的写法是「把 `providerAccount`
  理解为名字/标签」——它是掩码（末 4 位），不是名字，用等于比较比用后缀比较更容易被前缀文案
  变化搞坏。
- **`bufio.Scanner` 逐行读**（任务书示例之一）：否决。`Scanner` 默认 64 KiB 单行上限，
  magpie 一行有 33 个字段（含 `response_id` / `session` / `timings`），超限时 `Scanner` 直接
  报 `token too long` 并**中断整轮求和**；改用 `bufio.Reader.ReadBytes('\n')`（无行上限，且能
  区分「带终止符的行」与「残缺末行」，后者正是需要容忍的半行）。`json.Decoder` 也不行：
  它按值流式解码，残缺末行会直接报 `unexpected EOF`，无法区分「末尾半行」与「中间坏行」。
- **原因文案 `magpie usage log absent`**（任务书示例）：改成 `magpie providers absent`。
  这一档的原判据是「账号发现（读 providers.json）成功返回空，而同轮发现因文件不在而不可能
  成功」——缺的是 **providers.json**，不是 usage log；usage log 缺失对应的是「账号在、只是本
  轮无总额」那条路径，不应报成 roster 掉零的原因。指标名不变。
- **改造 `internal/metapiusage` 保留包名**：包名与语义都是「metapi 的库」，换源后名不副实，
  且任务要求新增包与旧包同构；故新建 `internal/magpieusage` 并删除旧包（旧包此时已零调用方）。
- **保留 `internal/metapiusage` 作为死代码**：否决。prism 内不得再留 hub.db 读取路径，而红色
  边界明确 metapi 目录随时会被另一个任务删除——留着就是随时会烂的死代码。删除可逆（未提交，
  `git checkout -- internal/metapiusage` 即恢复）。
- **`total_tokens` / `total` 之类的「存储总量」字段**：否决。magpie 的 `total` 含 `cache_read`，
  与上游百分比口径不符（20261003 笔记已实测：含 cache 会把月/周反推比从 ≈2.2 压到 ≈1.3）。
- **整文件读进内存 + 全量 JSON 解析**：否决（日志无限追加、无轮转，体积无上界）。
- **`model != "" && strings.Contains(model, "cline-pass")`**：否决，用前缀 + 斜杠收窄。
- **watch/inotify 或常驻 `*os.File`**：否决。句柄会锁住旧 inode（替换/回滚后读到旧数据），
  且需要 goroutine 生命周期管理；`stat` + 版本缓存已足够（每账号 3 窗口共享一次扫描）。
- **缓存按「大小 + mtime（秒）」**：否决，改用 `SameFile + size + mtime(Nanosecond)`。
- **用 `username`（metapi）风格的显示名**：不可能——magpie 的 `keys[]` 元素没有标签字段（实测），
  只用 id 派生。
- **按 `t` 的秒级粒度比较窗口**：否决，按纳秒全精度比较（一条写在 `to+0.5s` 的行共享 `to` 的秒，
  秒级比较会把它算进来）。

## 与既有笔记的取代范围（部分取代）
- `20260925-clinepass-quota-total-estimate.md`：**取代**其中「消耗量来源 = metapi 生产库
  `/var/lib/metapi/data/hub.db` 的 `proxy_logs`」「每次求和短连接自愈（open→created_at 形状
  自检→SUM→close）」「`created_at` 文本格式运行时形状自检」这几节与新源相关的实现**描述**（自
  本文起失效）；**仍成立**：估算算法（窗口起点由 `resetsAt` 反推）、降级语义（不出总额、不影响
  快照获取/展示、不标 fetch 失败）、expvar 名与「状态转换各一次 WARN / 恢复一次 Info」的
  可观测契约、CLI 级断言的要求。
- `20260929-clinepass-multi-account-metapi.md`：**取代**「账号与密钥直读 metapi `accounts`
  （site_id=49 + status='active'）」「按 `proxy_logs.account_id` 隔离求和」这两条来源口径（自
  本文起失效）；**仍成立**：一（套餐，窗口）一卡、一行一账号、行身份 =（窗口，账号名，key 指纹）、
  同名账号以「色点 + 同色账号名」区分、行显示名去掉尾部纯数字后缀、config 里 clinepass 账号
  不进 roster。
- `20260930-clinepass-multi-account-review-followups.md`：**取代**其中 O8「库文件不存在 ≠
  读取失败」的 metapi 实现细节与 `reason="metapi database absent"` 文案（自本文起失效）；
  **仍成立**：O5 的删除清单（除本笔新增删除 `internal/metapiusage` 外）、roster 可观测
  （`clinepass_quota_accounts` / `clinepass_quota_roster_drops_total` + 掉零 WARN）、
  `nextQuotaViews` 的「失败保旧、成功（含空）覆盖」规则、`clinepass_quota_estimate_skipped_total`。
- `20261003-clinepass-quota-estimate-derivation.md`：**取代**其中「分子 = `prompt_tokens +
  completion_tokens`（SQL 口径）」的**实现描述**（自本文起失效，改为 JSONL 的 `in + out`）；
  **仍成立**：剔除 cache 的口径结论、周锚定单向派生（月 = 2×周）、5h 独立反推、整数百分比按
  区间中点反推、以及「服务侧无对称告警」这条遗留（见上文「空账号 id 报错」——**该遗留由本文关闭**）。

## 来源
- 用户任务书（换源要求，`[MARK-PRISM-MAGPIE-SRC]`）：prism ClinePass 用量源 metapi → magpie。
- 用户任务书（双轨归属修订，`[MARK-IMPL-CLINE-DUALTRACK]`）：模型门槛 + Track 1 + Track 2
  找回被漏算的 921 行，碰撞则禁用 Track 2、0 命中不归属。
- 双轨归属的本机复核（2026-10-07，只读）：`/tmp/cline_recon/recon.py` 按旧/新两套规则重放同一份
  日志并逐账号对照（输出见上节表），另用新实现跑真实 `DefaultUsagePath` / `DefaultProvidersPath`
  （临时 `go run` probe，已删）：两个账号的分子与重放结果逐字节一致（29 012 255 / 17 659 654），
  全量求和 46 671 909。
- 回归：`internal/magpieusage/usage_test.go` 新增 `TestSumAttributionDualTrack`（三个子用例：
  掩码命中 + `cline-free` 与未命中行不入账 / `providerAccount` 空则回落 `host` 且 `providerAccount`
  优先 / 尾部碰撞拒绝归属、只 WARN 一次、不泄露 key 与尾字符），并把
  `TestSumFiltersProviderModelAndWindow`（keyID 在即按 id 归属、foreign provider 的 keyless 行不
  归属）与 `TestSumClinePassTokensByAccount`（keyless `cline` 行无 roster 时不归属）的 fixture 调
  到新口径；`go test ./...` 与 `go test -race ./internal/magpieusage` 全绿。
- 本机只读实测：`/root/.config/magpie/providers.json`（条目结构 + `keys[]` 仅有 `key` 字段）、
  `/root/.config/magpie/usage.jsonl`（8885 行 / 24 小时 / 0 行不可解析）、
  `prism quota --json --provider clinepass` 端到端输出（两个账号、每账号自己的总额）。
- magpie 的 `providerKeyId` 推导：`hex(sha256(key))[:10]`，与本机日志里实际出现的 id 逐一对上
  （`d0c4d7aa83` / `ae4c5650bd`）。
