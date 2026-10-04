---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage
---

# quota 卡片版式重做（10 格胶囊 + 倒计时上标题 + 亿单位）与 5h 窗口 token 池反推

## 一句话结论
- **版式**：胶囊 `clineCapCells` **23 → 10**（每格 10%，只作粗量感，精确读数交给百分比）；`clineRowFixed` **45 → 32**（`1 + 10 + 1 + 4 + 1 + 13 + 2`），卡片常规宽度 **58 → 45**（`4 + clineRowFixed + clineNameColMin = 4 + 32 + 9`）。**进度条起始列、名字列下限（9）、账号名不截断、一次渲染一个宽度、无窗口失败行占位、耗尽整条纯红——全部逐字保持**（进度条起始列仍为第 15 列：名字列 9 列不变）。
- **倒计时上标题**：标题由 `<Provider> · <窗口>` 改为 `<Provider> · <窗口> · <倒计时>`（第三个分隔符同为 ` · `），倒计时是**窗口级属性**（`clineGroup.resetsAt`，取该组第一个非空 reset），**不再逐行重复**。文案中文化：`2小时01分`（分钟补零）/ `6天23小时` / 整天 `3天` / `45分` / `不足1分` / `已重置`。
- **行内只剩两段**：百分比 + 「已用量/估算量」token 对，**两段都右对齐进各自的预留宽度**（4 列 / 13 列）；**行内不再有倒计时**（`clineCountdownField` 与 `resetText` 一并删除），拿不到 pool/实测的窗口在 token 对位置读 `-`。
- **token 值中文单位**：只到「亿」，一位小数，整数不写 `.0`（`13亿`、`2.2亿`、`0.3亿`），**不做 `<0.1亿` 兜底**（低于 0.05 亿就读 `0亿`）。`render.FormatTokensOneDecimal`（K/M/B 进位链）的唯一调用方就是本卡片的 token 对，故**整体替换**为 `render.FormatTokensYi`。
- **5 小时窗口也反推 token 池**：`ApplyClinePassEstimates` 撤掉 `default: continue`，5h 与 weekly/monthly 一样按 `[PeriodStart, min(now, ResetsAt)]` 求和、按自身 used fraction 反推，**写回自己的 `LimitTokensEstimate`**（不并入周锚定池 L，不参与 月 = 2×周 的单向派生）；`clineTokenPairWindow` 纳入 `"5h"`；`ApplyWeekEstimate` 泛化为按窗口名处理 `"weekly"`（仍冻结估算文件）+ `"5h"`（**不落估算文件**），Gemini 5h 补 `PeriodStart = ResetsAt − 5h`。SuperGrok 只有 weekly，**不无中生有 5h**；opencode-go 的 `rolling` 是 USD 窗口，**不是** token 窗口。

## 背景
- 用户要求卡片「数字更密、重复信息更少」：58 列的卡里胶囊占 23 格是主体，而倒计时在同一张卡内**逐账号重复**同一个窗口级事实；token 对用 K/M/B 英文缩写，读不出量级感。
- 5 小时窗口此前被刻意排除在反推之外（`estimate.go` 的 `default: continue`），卡片只能显示倒计时；用户要求 5h 行也显示 token 对。
- 硬约束（用户逐条拍板，不得妥协）：版式按 A1–A6 实施；账号名显示规则（去尾部纯数字后缀 + 色点/同色名）**保持原样**，禁止新增任何编号或后缀；`RenderTable` / `RenderTableAt` / `WriteJSON` / `Response` / `types.go` 的 JSON 行为零改动。

## 决策
### 1. 新列预算与几何常量
| 常量 | 旧 | 新 | 说明 |
| --- | --- | --- | --- |
| `clineCapCells` | 23 | **10** | 每格 10%，`CapsuleUsedCells(pct, 10)` = `ceil(pct/10)` |
| `clinePctWidth` | 4 | 4 | 不变，`PadLeft(pctLabel, 4)` 右对齐 |
| `clineNumberWidth` | 13 | 13 | 不变，`PadLeft(Truncate(pair, 13), 13)` 右对齐；最长形态 `9999亿/9999亿` |
| `clineRowFixed` | 45 | **32** | `1 + clineCapCells + 1 + clinePctWidth + 1 + clineNumberWidth + 2` |
| `clineNameColMin` | 9 | 9 | 不变（= 生产 roster 最长名 `SuperGrok`，部署事实） |
| 卡片常规宽度 | 58 | **45** | `4 + clineRowFixed + clineNameColMin = 4 + 32 + 9` |
| 进度条起始列 | 第 15 列 | 第 15 列 | 其前恒 14 列（`4 + 名字列 9 + 1`），公式 `width − clineRowFixed + 1` 逐字不变 |

- 行 = `"│ "` + 色点(1) + `" "` + 名字段(n) + `" "` + 胶囊(10) + `" "` + pct(4, 右对齐) + `" "` + pair(13, 右对齐) + `" │"` = `clineRowFixed + n + 4`。**两段都右对齐进预留宽度后，行的显示宽度恒等于卡宽，行尾 fill 退化为 0 的防御性 guard**（保留 `fill = width − DisplayWidth(row) − 2`，防止将来某段超出预留把右边框顶出去）。
- 低用量段**不做任何补偿**：3% → `ceil(0.3) = 1` 格，不四舍五入到 0，也不为「太小看不见」加格。
- 标题 shrink 顺序：窗口标签 → 倒计时 → service 名（`cardTitleLineAt` 的 `shrink` 循环，`titleWidth(svc, win, cd, sepW)` 同步参与 `clineCardWidth`，所以正常路径永不触发 shrink）。
- 无窗口失败快照的卡片（`g.window == ""`）**没有倒计时段**、pct/pair 仍是 4/13 列空格占位，列数守恒不变。
- `clineGroup.resetsAt` 取该组**第一个非空** reset 且不再覆盖：同组多个 snapshot 只要有一个给了 reset，标题就不会因为另一个没给而被抹空；同一渲染内不会抖动。

### 2. 中文文案规则
- 倒计时（`cardCountdown`，标题专用）：`≤0 → 已重置`；`<1min → 不足1分`；`<1h → "%d分"`；`<24h → "%d小时%02d分"`（分钟补零便于成列，`2小时00分` 也显示分钟）；`≥24h → "%d天%d小时"`，整整天读 `"%d天"`（`3天`）。
- token 对（`formatTokenPair`）：两侧都走 `render.FormatTokensYi` = `FormatFloat(n/1e8, 'f', 1, 64)` 后去尾零与小数点，加 `亿`。`13亿` 不写 `13.0亿`；`0.3亿` 保留；**没有** `<0.1亿`、`万`、`千万`。
- legacy `?format=table` 的 `formatRemain` / `formatEstimate`（含 `render.FormatTokens` 的 K/M/B 口径）**逐字未动**——中文单位只进卡片。

### 3. 5h 反推放开的理由与口径
- **ClinePass**（`ApplyClinePassEstimates`）：`5h` 成为第三个 `observed`，与 weekly/monthy 同一段代码同一条 SQL 口径（metapi `proxy_logs`、`model_requested LIKE 'cline-pass/%'`、按 `account_id`、分子 `prompt_tokens + completion_tokens` **剔 cache**、区间 `[PeriodStart, min(now, ResetsAt)]`、`windowUsedFraction` 取未取整 frac、`reversePool` 舍入**逐字沿用**）。写成 `snap.Windows[fiveHour.idx].LimitTokensEstimate = reversePool(tokens, frac)`；`frac >= 1`（上游把 100%  clamp 掉）时置 `MeasuredTokens`，显示 T/T——与 weekly 打满语义一致。
- **为什么不让 5h 并入 L**：`L` 的存在意义是「月 = 2×周」这条 plan 硬约束**由构造成立**；5h 是另一个分母、另一段时间窗，折进 L 会重新制造 v0.35.0 之前的「5h 1.6G / weekly 4.0G / monthly 5.2G」三池互相矛盾。所以 5h **各自反推、各自显示**。
- **`ApplyWeekEstimate` 泛化**：按 `[]string{"weekly", "5h"}` 顺序处理同名窗口；两者都反推，**只有 weekly 写估算文件**（`period_start` 是 `GeminiWeekStartUnix` 的周锚，写进 5h 起点会把 usage 默认查询区间搬走）。窗口没有 `PeriodStart` 就跳过（与旧行为一致）。
- **Gemini 5h**：`geminiBucketWindow` 补 `PeriodStart = ResetsAt − 5h`（weekly 仍 −7d）；分子来自 prism 自有 `usage.db` 的 `gemini-*` 求和（**Gemini 流量不经 metapi，绝不能用 `proxy_logs`**）；`UsedFraction` 由 `remainingFraction` 反转而来，因此 5h 用的是未取整占比。
- **SuperGrok**：只有 weekly，`ApplyWeekEstimate` 的 5h 分支是 no-op，**不新增窗口、不改 roster**。
- **`clineTokenPairWindow`**：`weekly || monthly || 5h`。`rolling`（opencode-go，USD 信用窗口，无 token 分母）**不纳入**，其行读 `-`、倒计时在标题。
- **5h 无数字的如实表现**：frac = 0（刚重置没流量）或 sum 失败 → `LimitTokensEstimate` 留空 → 卡片该行读 `-`，**不用假值填充**；标题仍给倒计时。

## 被放弃的方案（必填）
- **① 行内保留倒计时作为「无 pool 时的兜底」**：与 A2/A3 直接冲突（倒计时是窗口级属性、行内只剩两段），且会让 5h 卡在「有 pool / 无 pool」两种状态间版式跳动。否决；无 pool 就读 `-`。
- **② 5h 并入周锚定池 L（或按 L/月比例缩放）**：5h 与周/月不是同一个池（滚动限流 vs 订阅额度），缩放系数没有任何上游依据，会把 v0.35.0 已经修掉的「三池互相矛盾」带回来。否决。
- **③ 为 5h 单独建估算文件（仿 `grok-week-estimate.json` / `gemini-week-estimate.json`）**：5h 周期只有 5 小时，文件的意义是「跨周期冻结」，对 5h 毫无价值，还会让 `GeminiWeekStartUnix` 的周锚来源多一个错误候选。否决；5h 每次现场反推。
- **④ token 对改用「万/亿」两级或加 `<0.1亿` 兜底**：用户明确「单位只到亿、不要万、不要千万、不要 `<0.1亿` 兜底」。否决；小值就读 `0亿`。
- **⑤ 胶囊改成 20 格（每格 5%）保留一点精度**：用户拍板 10 格 = 10%，并明确「不要为低用量段做任何特殊补偿」。否决。
- **⑥ 把倒计时放进「行内 + 标题」两处（标题新增强、行内保留）**：正是本次要消除的逐行重复。否决。
- **⑦ 保留 `render.FormatTokensOneDecimal` 并在 planusage 里另写中文格式化**：其唯一生产调用方就是本卡片的 token 对，留着一份死代码不如就地替换。否决，改为替换为 `FormatTokensYi`。

## 与既有笔记的取代范围（部分取代）
按仓库既有先例处理（旧笔记保持 `status: active`、`superseded_by: ""`，由本篇正文声明取代范围；本篇 `supersedes` 保持 `""`，**不改旧笔记正文**）：
- `20261003-quota-row-alignment.md`：其**「百分比与指标段左对齐 + 行尾 fill 收口、卡宽 58」的核心结论被本笔取代**（两段回到 `PadLeft` 右对齐进预留宽度，行宽恒等由「两段定宽右对齐」保证，fill 退化为 0 的 guard；卡宽公式改为 45）。其**「进度条起始列公式 `width − clineRowFixed + 1`」「一次渲染一个宽度」「`clineNameColMin` 是下限不是上限」「账号名永不截断」「legacy table 不动」全部仍成立**。
- `20261003-quota-countdown-text-and-order.md`：其**「倒计时显示在指标段（`resetText`）+ `2h 01m` 英文格式」被本笔取代**：倒计时移到标题、改中文、`resetText`/`clineCountdownField` 删除、`cardCountdown` 改中文格式。其**「倒计时不带后重置后缀」的结论仍成立**（本笔 `已重置` 亦无后缀）。
- `20260924-quota-card-cn-text.md`：其**倒计时/错误码中文化的一般原则仍 active**；但具体词表被本笔扩充（`2小时01分` / `6天23小时` / `不足1分` / `已重置`）。
- `20261003-clinepass-quota-estimate-derivation.md`：其**「5 小时窗口不参与估算、5h 卡显示倒计时」这一条被本笔取代**（5h 改为各自反推并显示 token 对）；其**周锚定单向派生（月 = 2×周）、分子剔 cache、`account_id` 隔离、`SumClinePassTokensByAccount` 口径、打满 T/T 语义全部仍 active**。
- `2026-10-03-unified-quota-cards-spec.md`：其**「智能单指标（5h 显示倒计时 / 周月显示 token 对 / 回落倒计时）」整节被本笔取代**（改为「行内 token 对或 `-`，倒计时只在标题」）；其**单行式版式、标题不含账号、无详情行/无 footer、版式不按 provider 分叉仍 active**。
- `20260925-token-format-carry-chain.md`：其 **K/M/B/T/P/E 进位链本身仍 active**（`render.FormatTokens` 仍服务 legacy 表格与 usage 报告）；本笔只是让 quota 卡片的 token 对走中文「亿」单位，**不是**修改该进位链。

## 返修（同一分支第二个 commit）：整数百分比的中点修正

### 实测证据
- 用户只读 GET 了 ClinePass `plan/usage-limits` 的原始响应：`five_hour=2`、`weekly=51`、`monthly=75`，**三个值全是整数，无小数点**。即 `Percent` 是上游 `percentUsed` 的**向下取整**，真实 used fraction 落在区间 `[Percent/100, (Percent+1)/100)` 内（51% 可能是 51.0% ~ 51.99…%）。

### 决策
- `windowUsedFraction` 回落到 `Percent/100` 的分支改为**区间中点**：`Percent >= 1` 时取 `(Percent + 0.5) / 100`。这是该区间的无偏点估计，把最坏相对误差减半（`P=1`：原来的 50T~100T 两倍区间收窄成以 66.7T 为中心的 1.5 倍区间），并消除整套 roster 的系统性高估。
- **`Percent == 0` 的例外不动**：仍返回 0。中点会凭空造出 0.5%，一个 token 的流量就能反推出 200 倍的池——那是「自信的假数字」。0 继续表示「无流量 / 未知」：不写估算、卡片该行读 `-`，与 Gemini 交真实 `UsedFraction` 的小数路径在语义上一致。
- **中点 clamp 到 1**：上游把 `percentUsed` 钳在 100%，区间顶端塌缩到 1，因此**耗尽窗口仍走 T/T 兜底**（`reversePool(tokens, 1) = tokens`），不会因为这次修正反而丢了估算。
- **小数路径逐字不动**：`UsedFraction > 0`（Gemini 的 `remainingFraction` 反转）不进这个分支，v0.35.0 的未取整口径不受影响。

### 数值影响（T = 该窗口实测词元；新池 = 旧池 × `P/(P+0.5)`）
| 窗口 / Percent | 旧分母 | 新分母 | 旧池 | 新池 | 变化 |
| --- | --- | --- | --- | --- | --- |
| 5h，P=2（线上实测） | 0.02 | 0.025 | 50T | 40T | **−20.0%** |
| 5h，P=5 | 0.05 | 0.055 | 20T | 18.18T | −9.1% |
| 5h，P=7 | 0.07 | 0.075 | 14.29T | 13.33T | −6.7% |
| 5h，P=10 | 0.10 | 0.105 | 10T | 9.52T | −4.8% |
| weekly，P=8 | 0.08 | 0.085 | 12.5T | 11.76T | −5.9% |
| weekly，P=51（线上实测） | 0.51 | 0.515 | 1.960T | 1.940T | −1.0% |
| monthly，P=75（线上实测） | 0.75 | 0.755 | 1.333T | 1.325T | −0.7% |
| monthly，P=25 | 0.25 | 0.255 | 4T | 3.92T | −2.0% |
| 任何 P=100（打满） | 1.0 | 1.0（clamp） | T | T | 0% |
| 任何 P=0 | — | — | 无估算 | 无估算 | 0% |
- 百分比列与「已用」一侧同比例缩小（`used = total × Percent/100`），因此**百分比读数不变**，只有 token 对的绝对值下移；「月 = 2 × 周」的构造关系不受影响（两侧分母同时变）。
- 低占比窗口收益最大（P=2 的 5h 降 20%，P=8 降 5.9%），高占比窗口几乎无感（P=51/75 降 1% 上下）——这正是想要的：**修正只发生在原来偏差最大的地方**。

### 测试同步与取证
- 只改断言，**未新增测试文件、未新增 `func Test`**：`estimate_test.go`（`TestApplyWeekEstimateEatsCombinedAgySum` 2000→1980、`TestApplyWeekEstimateFiveHourWindow` 周 200000→198020、`TestApplyClinePassEstimates` 三窗 1.0B/1.0B/2.0B → 909090909/941176471/1882352942 且卡片对 `0.5亿/9.1亿`、`0.8亿/9.4亿`、`0.8亿/18.8亿`、`TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly` 800000/1600000 → 784314/1568628、`TestApplyGrokWeekEstimateFirstPeriodUsesLive` 2000→1980、`TestApplyGrokWeekEstimateShowsLiveAfterRollover` 1000→667、`TestApplyGrokWeekEstimateLiveEveryPeriod` 10000→9913）、`poller_test.go`（5h 14286→13333、weekly 12500→11765、monthly 25000→23530、账号 9 37500→35294）、`cmd/prism/metapi_test.go`（12353→12174、20588→20290、5h 200000→190476、weekly 100000→99010、monthly 200000→198020）。
- **未受影响（小数路径 / 例外路径，断言逐字未动）**：`TestApplyWeekEstimateUsesUnflooredFraction`、`TestApplyWeekEstimateSubPercentStillInverts`、`TestApplyClinePassEstimatesUsesUnflooredFraction`、`TestApplyClinePassEstimatesSubPercentStillInverts`、`TestApplyClinePassEstimatesDrainedWeekAnchorsMonthly`（frac=0.999）、`TestApplyGrokWeekEstimateIgnoresNonGrokSumZeroPercent`（P=0）、`TestApplyClinePassEstimatesGuards`（P=0 与 P=100 两条例外）、`TestApplyClinePassEstimatesExhaustedUsesMeasured`（P=100 clamp）。
- **变异自证**：把 `w.Percent > 0` 条件临时去掉（让中点也作用于 P=0）→ `TestApplyClinePassEstimatesGuards` / `TestApplyGrokWeekEstimateIgnoresNonGrokSumZeroPercent` FAIL；去掉 clamp → 两个耗尽用例 FAIL；改回 `Percent/100`（旧口径）→ 上述 10 个断言 FAIL。恢复后 `go test -count=1 ./...` 全绿。

### 与既有笔记的取代范围（本次返修）
- 本篇上一节的「遗留 → 整数百分比的精度风险」条目**由本节修正**（从「已知未决」变为「已用中点修正、残余为区间内 ±25% 中心估计」），原文保留不改。
- `20261003-clinepass-quota-estimate-derivation.md`：其**整数占比口径被第二次修正**——v0.35.0 只给 Gemini 补了未取整 `UsedFraction`，ClinePass / SuperGrok 的整数 `Percent` 仍按地板值反推；本笔把它改为区间中点。其周锚定单向派生、分子剔 cache、T/T 语义全部仍 active。
- `20260925-clinepass-quota-total-estimate.md`：其「5h/weekly/monthly 各自反推」的历史描述属早期形态，已被 v0.35.0 与本笔两次迭代取代，本篇不再重复声明。

## 遗留（未决 / 需后续处理）
- **5h 反推的语义风险（已知、未消除）**：5h 的「池」是把 5 小时滚动限流窗口内观测到的词元 ÷ 该窗口 used fraction 得到的**推断量**，它不是一个上游承诺的额度——窗口越短、流量越小、占比越低，反推值越不稳（见下「精度风险」）。是否需要按置信度（如占比 < 5% 不显示）留给后续决策；本笔如实显示，不加门槛。
- **整数百分比的精度风险**：ClinePass 上游 `percentUsed` 是整数，`windowUsedFraction` 回落到 `Percent/100`，真实 frac ∈ `[P/100, (P+1)/100)`，反推池落在 `(100T/(P+1), 100T/P]`——`P=1` 时是 **50T ~ 100T** 的 2 倍区间。5h 窗口的 T 本身就小，这个相对误差比周/月更显眼；`displayPercent` 的 ceil 也会让「已用」一侧略大于分子的实际占比。
- **`亿` 粒度的读不出**：`< 0.05亿`（50M 词元以下）的池在卡片上读 `0亿`，token 对比无（如测试里的 12500/25000 这种小 fixture）。这是单位选择的设计后果，不是数据缺失；若线上真实池子在 10^8 量级则无影响（ClinePass 周池生产实测在 10^9 量级）。
- **窗口边界是推断值**：5h 的 `PeriodStart = ResetsAt − 5h` 由 reset 反推，若上游 reset 与真实窗口边界不对齐，`proxy_logs` 的 TEXT 区间求和会漏/重（`created_at` 秒级精度对 5h 短窗也更敏感）。`ErrCreatedAtShape` 的形状自检仍在，但「reset 不对齐」这一类它查不到。
- **`internal/usage` 的 `reportWidth = 56` 与 quota 卡 45 的差值**：本笔把差值从 2 拉大到 11。是否统一仍待用户决策（沿用 `20261003-quota-card-uniform-width.md` / `20261003-quota-metric-gap.md` 的未决项）。
- **`不足1分` 与整天 `3天` 两个形态是本次自定**（用户只给了三个样例：5h / 跨天 / 已重置）；如用户想要别的写法，改 `cardCountdown` 一处即可（测试同步）。

## 来源
- 用户拍板的需求 A（版式 A1–A6）与需求 B（5h 反推 B1–B5），含「更深层约束优先」的协作约定与必须保持的不变量清单；门禁：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`scripts/test_*.sh`、`python3 scripts/test_generate_mcp_tools.py`、`gofmt -l`。
- 相关实现：`internal/planusage/report.go`（常量、`clineGroup.resetsAt`、`cardGroups`、`clineCardWidth` / `cardWidth` 带 `now`、`renderGroupCard`、`clineRowLine`、`clineMetricField`、`clineTokenPairWindow`、`cardTitleLineAt` / `titleWidth` / `titleCountdown`、`cardCountdown`、`formatTokenPair`）、`internal/planusage/estimate.go`（`ApplyWeekEstimate` 泛化、`ApplyClinePassEstimates` 5h）、`internal/planusage/gemini.go`（5h `PeriodStart`）、`internal/render/numbers.go`（`FormatTokensYi` 替换 `FormatTokensOneDecimal`）。
- 相关测试（只改断言 + 2 个新增 5h 反调用例 + 1 个新格式化用例，**未新增测试文件**）：`report_test.go` `^func Test` **50 → 50**（`TestResetText` / `TestCardCountdownZeroPadsMinutes` 换成 `TestCardCountdown` / `TestTitleCountdown`）、`estimate_test.go` **24 → 26**、`render/numbers_test.go` **5 → 6**、`poller_test.go` / `gemini_test.go` / `cmd/prism/metapi_test.go` 计数不变。
- 相关笔记：`20261003-quota-row-alignment.md`、`20261003-quota-countdown-text-and-order.md`、`20261003-clinepass-quota-estimate-derivation.md`、`2026-10-03-unified-quota-cards-spec.md`、`20261003-quota-card-uniform-width.md`、`20260925-clinepass-quota-total-estimate.md`、`20260925-token-format-carry-chain.md`。
