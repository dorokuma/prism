---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage, metapiusage, cmd/prism, render
---

# ClinePass 总额估算改「周锚定单向派生」：分子剔除 cache、月 = 2×周 单点赋值（D1–D5）

## 一句话结论
- 上游只给每个窗口的 `percentUsed`，从不给绝对池子；同时拿三个窗口各自反推会得到三个互相矛盾的池（5h 1.6G / 周 4.0G / 月 5.2G）。计划里唯一被保证的关系是 **月限额 = 2×周限额**，于是只派生 **一个** 池 `L`，并把月窗口写成 `2×L`，让 `月 = 2×周` **由构造成立**而非靠算术巧合。
- `L` 优先取**周窗口**反推（`L = tokens_w / frac_w`）；周窗口不可用（刚重置无流量、或已打满）时由**月窗口兜底**（`L = tokens_m / (2·frac_m)`，即月反推值的一半）。
- 分子口径必须**剔除 cache**（`prompt_tokens + completion_tokens`，**不再**用 `total_tokens`）：metapi 的 `total_tokens` 含 `cache_read_tokens`，而 cache 在周窗口里占比远高于月窗口，含 cache 会把 `月/周` 反推比从 2 压到 **1.28**。
- 5h 窗口**不参与估算**（滚动限速、自带 percent，反推只会多出一个无关的池），其卡片数字段改显示**重置倒计时**。
- 反推仍是**近似**：上游是黑箱，没有绝对真值；残差来源与幅度见「已知残差」。

## 背景
- ClinePass 三窗口（5h / 周 / 月）接入与「总额」估算的历史见 `20260925-clinepass-quota-total-estimate.md`（只读 metapi 求和源）、`20260929-clinepass-multi-account-metapi.md`（合卡版式）、`20260930-clinepass-multi-account-review-followups.md`（按账号隔离 / active 过滤 / SIGHUP 重发现）。
- 用户提供的**硬约束**：Cline 计划的「月限额 = 2×周限额」。实测反推必须落在这个关系上，否则三张卡片互相打架。
- 上一版（本分支改动前）的实现是**逐窗口独立反推**：`LimitTokensEstimate = consumed ÷ (percentUsed/100)`，每个窗口用自己的分子、自己的分母 ⇒ 生产上出现三个互不一致的池（5h 1.6G / 周 4.0G / 月 5.2G），且都随整数 percent 抖动。
- 本轮把估算改成**周锚定单向派生**，并同步调整分子口径与显示契约。

## 决策

### D1. 分子剔除 cache：只用 prompt + completion
- **依据（metapi 列语义）**：`proxy_logs.total_tokens` 在 metapi 里**包含** `cache_read_tokens`。生产库核对：在 `prompt_tokens_include_cache = 0` 的 **21265 行**里，**全部**满足 `total_tokens = prompt_tokens + completion_tokens + cache_read_tokens`（即 `total_tokens` 是含 cache 的总量）。
- **依据（cache 占比不对称）**：cache 命中占**周窗口**分子的 **49%**，但只占**月窗口**分子的 **13%**。cache 高度集中在最近的热点请求上，所以它对短窗口的污染远大于长窗口。
- **后果（含 cache 时的比值崩坏）**：含 cache 时，月/周的**水平法**反推比只有 **1.28**，而硬约束要求 **2**。偏差方向与幅度都不可接受。
- **实现**：`internal/metapiusage/store.go` 的 `SumClinePassTokensByAccount` 单行分子改为 `COALESCE(prompt_tokens,0) + COALESCE(completion_tokens,0)`，SQL 里去掉 `total_tokens` 分支（NULL 行与含 cache 行现在走同一条口径）。

### D2. 周锚定单向派生 + 月 = 2×周 单点赋值
- **为什么是周**：周窗口是**唯一同时满足**「有足够流量使 `consumed/frac` 稳定」和「窗口长度可控使窗口起点反推误差小」的窗口；5h 太短（占比小、percent 量化误差占比大），月太长（窗口起点靠 `AddDate(0,-1,0)` 反推，日历月归一化会带偏分子）。
- **为什么只算一个池**：三个 percent 各自可信度不同、误差方向不同；分别反推必然三值不一致。既然唯一被保证的关系是 `月 = 2×周`，就只算一个 `L`，月写成 `2L` —— 这条关系**由构造成立**，无论两个 percent 各自说了什么。
- **实现（`internal/planusage/estimate.go` 的 `ApplyClinePassEstimates`）**：先只对 `weekly` / `monthly` 两个窗口求和并取 `frac = windowUsedFraction(w)`；`pool` 的选取是严格优先级：
  1. 周可用（`0 < frac_w < 1` 且 `tokens_w > 0`）⇒ `pool = reversePool(tokens_w, frac_w)`；
  2. 否则月可用（`0 < frac_m < 1` 且 `tokens_m > 0`）⇒ `pool = reversePool(tokens_m, 2·frac_m)`（**除以 2 倍 frac**，因为月池是周池的两倍）。
  写回：`weekly.LimitTokensEstimate = L`、`monthly.LimitTokensEstimate = 2L`（哪个窗口存在就写哪个）。

### D3. 周窗口重置时用月兜底
- 场景：周窗口刚滚动，新周**尚无流量**（`tokens_w = 0` 或 `frac_w = 0`），此时周无法锚定；但月窗口仍是「部分已用」，仍在同一带上。
- 策略：用 `L = tokens_m / (2·frac_m)` 兜底（**月反推值的一半**），于是月卡片继续读 `2L`、新周卡片也不再是空的。
- 实现：见 D2 的优先级第 2 条；兜底路径**不是**「无估算」，所以不会落到 `L/L` 或 `T/T`。

### D4. 打满窗口按派生池渲染 `L/L`（月为 `2L/2L`）；首轮即打满退回 `T/T`
- 打满（`frac >= 1`，即上游把 `percentUsed` 钳到 100）窗口**不能用自己反推**（分母被钳位，必系统性低估），所以一律用**派生出来的池**渲染：周 `L/~L`、月 `2L/~2L`（`~` 见 D5）。
- **唯一退回 `T/T` 的情形**：**没有任何窗口可派生 `L`**（典型是「首轮即已打满」：周打满、月也打满/无数据 ⇒ `pool = 0`）。此时已耗尽窗口用**自己的实测消耗**当总额显示 `T/T`——这是数据能给出的最好答案，比空字段或 `-` 诚实。
- 两个窗口都不可用 ⇒ **不写任何估算**（字段留空），卡片回落 `-`（或 5h 的倒计时）。
- 实现：`pool > 0` 分支写回后即返回；`pool == 0` 时遍历 `{weekly, monthly}`，只对 `tokens > 0 && frac >= 1` 的窗口写 `LimitTokensEstimate = MeasuredTokens = tokens`。

### D5. 显示契约变化
- **5h**：数字段由 token 对改成**重置倒计时**（`2h 15m 后重置` / 已重置），因为该窗口没有池，`-` 对「上游确实报告了的窗口」什么都没说。
- **周/月总额加 `~`**：上游从不报告绝对池，`~` 标出这个数是**推断值**而非上报事实。标记条件 = `LimitTokensEstimate > 0 && MeasuredTokens == 0 && 窗口 ∈ {weekly, monthly}`（实测值不加 `~`）。
- **标题加「估算池」**：周/月卡片标题追加 ` · 估算池` 段（5h 不加，它没有池）。
- **账号名保留数字后缀**：`clineDisplayName` 返回账号**全名**（`Cline2` 不再折叠成 `Cline`）。metapi site 49 的两个账号是**两个独立订阅/两个池/两把 api_token**，折叠成一个名字会把两个额度读成一个。行身份本来就键在「全名 + 指纹」，显示与身份现在一致。
- 实现：`internal/planusage/report.go` 的 `clineNumberField`/`clineCountdownField`/`clineCardTitle`/`clinePoolWindow`/`formatTokenPair`/`clineDisplayName`（删 `stripNumericSuffix`）。

## 实测依据（本机 metapi 生产库，只读核对）
- **acct 34（Cline）**：周 no-cache **水平法** = **2,051,449,120**；月 no-cache 水平法 = **4,509,119,690** ⇒ 比值 **2.198**。
  - 同一组数据**含 cache** 时比值为 **1.280** ⇒ 这正是 D1 剔除 cache 的直接原因。
- **acct 38（Cline2）**：周窗口**已打满**，比值 **2.000**（周打满 ⇒ 由月兜底派生 `L`，月恒为 `2L`，比值由构造等于 2）。
- **「水平法」vs「边际法」**：水平法 = 累计消耗 ÷ 当前占比；边际法 = 相邻两轮 Δ消耗 ÷ Δ占比。两者比值都能验证 `≈2`，但边际法整体偏低（见残差）。

## 已知残差（未完全闭环）
- **边际法比水平法低 7%（周）～15%（月）**：说明窗口内**消费速率不均**，用累计水平法会把早期低速段的消耗按当前速率放大。
- **周锚定约高估 7%**：以周锚定、月写 `2L` 时，月窗口相对水平法实测约高 7%（周锚定的系统性偏置被 2 倍放大到月）。
- **来源（三条，均为结构性而非可修 bug）**：
  1. **`percent` 是整数**：周 ±2.5%、月 ±0.8% 的量化误差直接进分母（`1/frac` 放大），窗口占比越小误差越大；
  2. **窗口起点靠 `ResetsAt` 反推**：上游只给重置时刻，`PeriodStart` 由 `−5h / −7d / AddDate(0,-1,0)` 反推，边界误差直接落进分子；月还用日历月归一化（如 5-31 → 5-01），起点可能偏早而分子偏大；
  3. **消费速率不均**：窗口内速率随负载变化，水平法假设「当前速率代表全窗口」。
- **上游是黑箱**：没有绝对真值可对齐，因此本方案**不宣称**数字是「真值」，只宣称（a）周/月关系**结构性正确**，（b）5h 不再贡献一个无关的池。任何「精确到百分比」的期望都不成立。

## 运维注意
- **roster 只在启动 / SIGHUP 重建**：账号发现（metapi site 49、`status='active'`）只在进程启动时与每次 SIGHUP 各跑一次，poller 每轮**不**重新发现。**部署新版本后需要一次 SIGHUP**（或重启）才会出现全部账号；运行期新增/禁用账号同理。
- 本机 metapi 库为 `/var/lib/metapi/data/hub.db`（默认路径常量，**不是**配置键）；服务以 root 运行时可读，若回落 `User=prism` 需只读 ACL（目录 `0700`）。
- 降级可观测：`/metrics` 的 `clinepass_quota_estimate_skipped_total`（本次新增：无 account_id 被丢弃的估算数）、`clinepass_quota_accounts`、`clinepass_quota_roster_drops_total`、`clinepass_usage_source_status/errors`。

## 被放弃的方案（必填）
- **逐窗口独立反推（上一版行为）**：三窗口各自 `consumed/frac`，生产上给出 5h 1.6G / 周 4.0G / 月 5.2G 三个互相矛盾的池，且月/周关系随机漂移；否决。
- **分子继续用 `total_tokens`（含 cache）**：cache 在周/月占比不对称（49% vs 13%），含 cache 使月/周反推比掉到 1.28（要求 2）；否决。
- **用 `total_tokens − cache_read_tokens` 现算**：等价于改完的 prompt+completion，但需要额外依赖「total 含 cache」这一列语义在每行都成立（NULL/异常行会算出负数），直接在 SQL 里取 prompt+completion 更稳；否决。
- **5h 也参与估算 / 用 5h 反推**：5h 是滚动限速，其 percent 与周/月池无固定容量关系，反推只会多出一个无关的池；否决（改为显示倒计时）。
- **月窗口自己独立反推**（不写 `2L`）：会让月与周再次脱钩，违反硬约束；否决。
- **打满窗口显示 `-`**：上游确实报告了该窗口，`-` 会让「读不到 metapi」与「已打满」不可区分；否决（打满显示 `X/~X`）。
- **打满窗口用 `tokens/1` 反推总额**：percent 被钳位，`tokens/1` 系统性低估；否决（沿用派生池，无池时才 `T/T`）。
- **保留账号名去尾数字后缀（`Cline1`→`Cline`）**：会把两个独立订阅读成一个额度；否决（显示全名）。

## 遗留清单（未决 / 需后续处理）
- **`SumClinePassTokens` 口径不一致（本笔只加 `Deprecated:` 注释，未改行为）**：它是**全订阅**口径（SQL 无 `account_id` 过滤）且分子**仍含 cache**（仍用 `total_tokens`），与生产在用的 `SumClinePassTokensByAccount`（已剔 cache、按账号）在**账号维度**与**cache 维度**上双重不一致；当前**无生产调用者**（服务侧走 `Source.SumClinePassTokensByAccount`，CLI 走 `Store.SumClinePassTokensByAccount`，`Source.SumClinePassTokens` 也只被测试调用）。**未决**：对齐口径（改为 per-account + 剔 cache）还是直接删除；连同它的 `Source` 包装一起处置。
- **反推残差未闭环**（见「已知残差」）：需要上游提供更高精度的 percent（或直接给池子）才可能收窄；在此之前周锚定 ≈ 高估 7% 是**已知偏置**，不改。
- **月窗口起点用 `AddDate(0,-1,0)` 归一化**：`5-31 → 5-01`、`3-31 → 3-03` 这类起点偏早会让月分子偏大；是否改成「按天钳位」或改为按周锚定的 2 倍直接外推月分子，未决。
- **5h 与周/月容量关系未查证**：若上游未来明确 5h 容量，可再考虑接入 5h 估算（当前有意不接）。
- **上游打满的判定沿用 `Percent>=100` / status**：`displayPercent` 用 `math.Ceil` 而 `windowExhausted` 只看整数 `Percent`，在 99.5–100% 窄区间两信号可能不一致（胶囊渐变 vs `X/X`），见 `20260930` 的 O3，未修。
- **roster 无周期性重发现**（只靠 SIGHUP / 重启，见「运维注意」）：未决，评审此前允许该替代方案。
- **`User=prism` 回落需 ACL**：metapi 目录 `0700 root:root`，服务回落非 root 用户时必须补只读 ACL，否则持续 degraded 无总额。运维前提，未改系统权限。
- **REST 契约**: 本次 JSON 增量为 `period_start`（既有）与估算字段语义变化；`measured_tokens` 仍只在打满窗口出现（含 cache 口径修正后的数值）。发版 changelog 需覆盖「逐窗口独立反推 → 周锚定」与「分子剔 cache」两条**行为变更**。
- **本笔未做的测试项（第 4 条之外）**：5h 倒计时文案的宽度边界（>13 列被 `PadLeft` 截断）、月兜底路径下 5h 行的渲染、`SumClinePassTokens` 若被删除后的调用面清理，均无用例。
- **合并卡数字段仅 13 列在 `X/~X` 形态下的截断隐患**：`clineNumberWidth = 13`，**任意量级档的 `100.0X–999.9X`、且两侧文本同为 6 列宽时都会溢出**（实测 `100.0K/~100.0K` 与 `100.0B/~100.0B` 均为 14 列；一侧 6 列 + 另一侧 5 列恰 13 列不溢出）；超出 13 列定宽时**值被省略号截断、行宽不变**；当前测试用例（如 5.0M/10.0M）刻意规避了该区间，后续需评估调整列宽或紧凑格式化。
- **服务侧缺对称可观测**：`cmd/prism` 侧 `account_id <= 0` 有 WARN + `clinepass_quota_estimate_skipped_total`，但服务侧 `internal/planusage/poller.go` 的 `AccountIDFrom` ≤0 时仍静默产出空估算；生产上 metapi 账号 id 恒 >0，属防御纵深缺口，后续补对称告警。
- **T/T 兜底时月/周不满足 2×**：两窗口都不可锚、且某窗口打满时，各自写入自身实测值，此时「月 = 2×周」不作为展示关系；这是有意的最后手段（避免展示 0/0），需在文档中明确点破。
- **旧笔记未标 superseded**：`.agents/notes/20260929-…` 中「显示名去掉尾部纯数字后缀」一段已被本次反转（改为保留全名），该旧笔记未加 superseded 交叉标注，后续补。

## 与旧笔记的关系
- `20260925-clinepass-quota-total-estimate.md`：**「只读 metapi 求和源 / 降级铁律 / 周期起点靠 ResetsAt 反推」仍 active**；其中「逐窗口独立反推、`0<占比<1` 才产出」的**反推规则**已被本笔替换（周锚定单向派生），且分子口径由 `total_tokens` 改为 prompt+completion。
- `20260929-clinepass-multi-account-metapi.md`：合卡版式仍 active；其中「显示名去掉尾部数字后缀」已被本笔**反转**（保留全名，见 D5）。
- `20260930-clinepass-multi-account-review-followups.md`：按账号隔离 / active 过滤 / SIGHUP 重发现 / 可观测仍 active；其「≥100% 用实测消耗当总额」在本笔细化为「有派生池则用池 `X/~X`，无池才退回实测 `T/T`」。

## 来源
- 本轮任务 `[MARK-PRISM-EST-27]`（reviewer 复审要求：补决策笔记 + expvar 增量断言 + `SumClinePassTokens` 标 Deprecated + 打满用例）。
- 相关实现：`internal/planusage/estimate.go`（`ApplyClinePassEstimates`）、`internal/planusage/report.go`（`clineNumberField`/`clineCountdownField`/`clineCardTitle`/`clineDisplayName`）、`internal/metapiusage/store.go`（`SumClinePassTokensByAccount` 口径、`SumClinePassTokens` Deprecated）、`cmd/prism/metapi.go`（`applyQuotaClinePassEstimate` + `clinepassEstimateSkipped`）。
- 实测数值来自本机 metapi 生产库只读核对（acct 34 / acct 38）；上游百分比的整数化与窗口起点反推均为结构性质，见「已知残差」。

## 修订（supersede）：账号显示名恢复「去尾部数字后缀」（显示回归修复，`[MARK-PRISM-NAME-39]`）
- **本笔反转 D5 的「账号名保留数字后缀」**：`clineDisplayName` 重新接上 `stripNumericSuffix`，显示名只剥**尾部连续数字**，`Cline` 与 `Cline2` 两行**都显示 `Cline`**；名字**中间**的数字不动，剥完为空则保留原名（`12345` → `12345`）。
- **不变的部分**：行身份仍键在**全名 + 指纹**（`clineRowID` 未动），**行数不变**（`Cline` / `Cline2` 仍是 2 行）；账号靠**颜色**区分——行首色点与同色名（紫 `8800FF` / 绿 `00FF00`）；卡宽量宽以**显示名**为基准，被去掉的后缀不占列。
- **为什么反转**：v0.35.0 把显示名改成全名属**显示回归**；`.agents/notes/20260929-clinepass-multi-account-metapi.md` 记录的「显示名去后缀 + 行数/身份不变」是用户反复强调的既定契约，两个独立订阅的信息由**颜色 + 行身份**承载，不依赖名字里的数字。
- **本次未动 v0.35.0 的其它行为**：分子剔 cache、周锚定单向派生（月 = 2×周）、`~` 标记、标题「估算池」、5h 重置倒计时、`account_id <= 0` 的 WARN + expvar、`SumClinePassTokens` 的 Deprecated——全部保持原样。
- **回归用例**：`internal/planusage/report_test.go` 的 `TestRenderCardsClinePassDisplayNameDropsNumericSuffix`（同一 provider、`Cline` + `Cline2` 两账号合并渲染 ⇒ 卡内无数字后缀、两行行首同为 `│ · Cline `、仍 2 行、两色不同、卡宽按显示名）；反向变异（`clineDisplayName` 返回全名）时该用例按预期 FAIL。同时恢复 `TestStripNumericSuffix`（尾部数字 / 中间数字 / 纯数字兜底）。
- **连带改动（超出「只改 internal/planusage」边界的说明）**：`cmd/prism/metapi_test.go` 的 `TestCLIAssemblyCarriesAccountFingerprints` 在 v0.35.0 同车把断言改成了「两行显示全名」（`sgrBefore(t, r, "Cline2")`），显示名回退后该断言必然红，故把这三处断言（注释 / 行筛选 / 颜色循环）还原为 v0.34.0 的「两行同名、靠色点 + 同色名区分」形态；**仅测试断言，无生产代码改动**。
- **同时消解「遗留清单」中的悬置项**：原「旧笔记未标 superseded」的前提（显示名保留全名）已被本修订反转 —— 20260929 与现行行为重新一致，**无需**再给它加 superseded 标注，该悬置项就此关闭。
