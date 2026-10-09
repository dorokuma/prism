---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: magpieusage, planusage, render, cmd/prism
---

# ClinePass 归因第二轮对抗审计：假恢复 / 一轮快照 / stale 语义 / 单位阶梯（F2 占比 0 已裁定：保留行为、收窄文档）

## 一句话结论
- 任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]` 的四条「必须修 / 应修」已落地：**F1 假恢复**（未读日志的调用
  不再 observe）、**S1 一轮快照**（一次抓取的三个窗口共用同一张归属表与同一份新鲜度结论）、**S2 单位阶梯**
  （token 对按总额在 亿 / 万 / 千 中选一个单位，分子分母同单位）、**S3 stale 计数语义**（「从未确认过尾号表」
  不计 stale）。四条各有一条能复现原 bug 的回归用例，都有「先 FAIL 后 PASS」实测原文。
- **F2（占比 0 仍写总额）没改**：按任务书的停止条件（「若现有测试或代码里存在明确依赖『周窗口占比 0 也写
  池』的断言：停下报我，不要自行翻转语义」），既有用例
  `TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly`（`internal/planusage/estimate_test.go:534-563`）
  正是这条断言 —— 周窗口 `Percent: 0`、求和 0，而它断言该窗口**拿到**月窗口锚定的池 `784314`（注释原文
  「the fresh week is not left blank」）。语义冲突未自行翻转，留待主代理裁定——**已裁定：保留现行代码
  行为、只收窄文档承诺**（证据与备选路径见文末「F2 裁定」节）。
- 三条判「不改」的项（`HasSuffix` 末 4 位匹配、`INDEX.md` 被 `.gitignore:17` 忽略、旧表新鲜时新 key 尾号
  碰撞误归属）已追加为 `.agents/notes/20261007-clinepass-attribution-followups.md` 的 **L7–L9**
  （含「被放弃的方案」三条），并按 AGENTS.md 重生成索引（`scripts/notes-index.sh`）。
- 文档同步：`config.yaml.example`（stale 计数语义 + 一轮快照）、`README.md` 的 `/admin/quota` 行
  （同两处）、`README.md:249` 的 changelog（「`SumClinePassTokens` Deprecated 全部保持」→ 该入口已随
  `internal/metapiusage` 删除）。

## 背景
- 触发：magpie 用量源换入 + 双轨归属收口后的**第二轮对抗审计**（2026-10-08），结论是一批「不阻断但必须修」
  的缺陷与两条应修项。四条修复的共同点是**都改了对外的行为契约**（source_status 的写入时机、一轮内
  attribution 的原子性、stale 计数的单位语义、卡片 token 对的单位），故按 AGENTS.md 落笔记。
- 关键事实（实测，见任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]` 背景锚点）：
  ① 非正 `from` 的调用会 `observe(nil)` 把 `degraded` 打回 `ok` 并打 recovered 日志（文件仍不存在）；
  ② 一轮的三次求和各自读一次活归属表，SIGHUP 抖动可让同一轮一半新表一半旧表（实测 11 / 0 / 7）；
  ③ `attribution_stale` 的单位是「求和」，没装 magpie 的主机每账号每窗口每轮 +1；
  ④ 卡片 token 对只走 亿，小池/小实测的打满窗口渲染成全零对（`0亿/0亿`），与 JSON 的真实值不符。

## 改动逐项

### F1 假恢复：没读日志的调用不得 observe（致命）
- **实现**：`internal/magpieusage/usage.go` 的 `Round.Sum`（:721）把 `fromUnix <= 0` 的守卫放在
  `observe` **之前**并且直接 `return 0, nil` —— 非正下界既不动 `source_status`、不打 recovered、也不动
  stale 计数；`observe` 只由真正读过日志的路径（含空 account id 的错误、以及 `sumByAccount` 的错误）触发。
- **调用方事实**：**没有生产调用方在传非正值**。求和回调只有两处（`internal/planusage/estimate.go` 的
  `ApplyClinePassEstimates` 与 `ApplyWeekEstimate`），两处都在 `w.PeriodStart == nil || w.PeriodStart.IsZero()`
  时 `continue`，因此 `from = PeriodStart.Unix()` 恒为真实时刻（≥1）。守卫是 API 自身的边界（历史
  metapi 时代 `SumClinePassTokens` 就有 `fromUnix <= 0` 返回 0 的口径），未改任何调用方语义。
- **用例**：`TestZeroWindowSumDoesNotRepublishHealthyStatus`（`internal/magpieusage/usage_test.go:852`）。
  形状：日志缺失 → 一次真实求和失败（status=degraded）→ 以 `from=0`、`from=-5` 各调一次 → 断言 status 仍
  `degraded`、日志里无 `recovered` 行、stale 计数不动、同一 round 换成真实窗口仍按原样失败。
- **先 FAIL 后 PASS 原文**（把守卫改回 `observe(nil)` 后）：
  `usage_test.go:884: source status after a sum that read nothing = "ok", want the failed read's "degraded" still standing`。

### S1 一轮快照：一次抓取的三窗口共用一张表与一份结论（应修，时序）
- **实现**：新增 `attributionSnapshot{table, fresh, confirmed}` 与
  `Source.snapshotAttribution()`（`usage.go:759`，单次持锁读齐三项）；新增
  `Source.BeginRound(accountID) *Round`（`usage.go:702`）与 `Round.Sum`（`usage.go:721`），后者只用
  round 的 `snap` 归属，`indexForAttribution`（`usage.go:588`）按「文件版本 + 该快照的表」决定缓存复用，
  **不再回调活状态**；`SumClinePassTokensByAccount` 退化为「自己取一轮再一次求和」的一次性形态。
- **调用方接线**：`cmd/prism/main.go:586`（服务侧 `SetClinePassEstimate` 工厂在 `fetchOne` 入口取一次
  round）与 `cmd/prism/magpie.go:229`（CLI 估算）各自在入口取一次 round；两处都**不在每个窗口重新取**。
- **无全局可变共享态**：`Round` 是值（快照三字段），`BeginRound` 之后不再读 `Source` 的归属字段；并发安全由
  `-race -count=20`（`internal/magpieusage`，含既有的并发求和用例）与 `-race -count=5`
  （planusage / cmd/prism / render）证明，0 条 `WARNING: DATA RACE`。
- **用例**：`TestRoundPinsAttributionAcrossRediscovery`（`usage_test.go:1595`）。形状：取 round → 一次
  失败发现把表转陈旧 → round 的求和仍按旧表（11），而**同一时刻新取的 round 与一次性形态**都按轮换后的表
  （0）；末尾再断言「从未确认过」不计 stale（S3）与「一轮三个窗口要么都报同一结论、要么都不报」。
- **先 FAIL 后 PASS 原文**（把 `Round.Sum` 改成每次读活状态后）：
  `usage_test.go:1625: round sum begun before the rediscovery = (0, <nil>), want (11, nil): a round keeps the table its fetch began with`。

### S2 单位阶梯：token 对按总额选 亿 / 万 / 千，两半同单位（应修，文案精度）
- **实现**：
  - `internal/render/numbers.go` 新增 `FormatTokensUnit(n, div, suffix)`（一位小数、整单位去小数、
    非零但一位小数会塌成 0 时改两位小数），`FormatTokensYi` 改为 `FormatTokensUnit(n, 1e8, "亿")` 的薄包装
    （行为逐字节不变）。
  - `internal/planusage/report.go`（现行 `clinePairUnits` 阶梯起、到 `formatTokenPair` 止；
    `clinePairUnitFor(total)` / `pairSideReadable(text, value)` 已随本节被取代而删除，见下方 superseded 注）：
    旧规则是 `formatTokenPair` 先按总额单位渲染，不可读则**向上一级**重试（一个落到粗档上界的总额因此
    改读更粗的单位，而非被字段截断），全部不可读才回落总额自己的单位。旧规则的具体产出与翻档见
    `TestRenderCardsTokenPairUnitLadder` 与 `.agents/notes/20261008-clinepass-c4-unit-ladder.md`，本笔记不复述。
- **契约要点**：① 单位只由**总额**决定，分子分母同单位（绝不写两个要读者自己换算的档）；
  ② 打满窗口（池=实测=T）仍是 **X/X 对等值**，只是换了单位，不再读成全零；
  ③ 一个单位下会塌成 0 的小值不再显示为 0；④ 卡片文案与 `/admin/quota` 的 JSON 对得上
  （这是原 bug 的本质：JSON 有真实值而卡片读全零）；⑤ 字段宽度不变（`clineNumberWidth`）仍成立，
  `pairSideReadable` 的整数位上界就是这个预算。
- **用例**：`TestRenderCardsTokenPairUnitLadder`（`internal/planusage/report_test.go`，表驱动：各档位起点、
  极小分子保持总额单位、低于最小单位走 千、宽度上界与边界上翻档）、`TestFormatTokensUnit`
  （`internal/render/numbers_test.go`，各单位的精度规则与负数/零/非正 div 的边界）、以及既有 `cmd/prism` 的
  `TestApplyQuotaClinePassEstimate`（断言改为带单位的对，并新增「出现全零对即失败」的反向断言）。
  具体期望串与数字见这些测试，本笔记不复述。
- **先 FAIL 后 PASS**：旧实现下这些用例 FAIL（打印出全零对），换成 `FormatTokensUnit` 后 PASS。
  **FAIL/PASS 原文见当时的交付回报**，本笔记不复述其中的数字。
- **文本契约变更（待发版登记）**：卡片 token 对是**人眼可见**的文本，由「恒 亿」改为按总额选单位属
  契约变更，依赖全零对字面量或「恒 亿」形态的刮取式调用方需适配；**changelog 条目与版本号属发版动作**
  （本次任务书未要求），留待主代理在发版轮补一条 `fix(quota)`。
  ［**本节已被取代（2026-10-08，oracle 证伪后）**：`clinePairUnitFor` / `pairSideReadable` / 「不可读则向上
  一级重试」这套**三档**阶梯已换成**四档（亿 / 万 / 千 / 原值）+ 逐档打分**
  （`internal/planusage/report.go` 的 `clinePairUnits` / `clinePairUnitStart` / `clinePairCandidates` /
  `clinePairCandidate.beats` / `clinePairPlain`）——oracle 证伪的正是本节这套规则：一 token 与四 token 的
  对仍读全零、极小分子对落到粗档上界时仍读全零，而某些打满对渲染出的粗档文本比字段更宽、卡片被截。
  本节「单位只由总额决定、两侧同单位」的契约仍成立，①②③④四条事实保留；⑤（`clineNumberWidth`）现由打分
  规则里的 `clinePairWidthBudget`（硬预算，超了只能舍精度）/ `clinePairSideBudget`（偏好的两侧分割）承担：
  硬规则是**卡片不得出现被 `Truncate` 截掉的数字**，「非零→0」优先于「超宽」。原文保留为当时结论，新行为见
  `.agents/notes/20261008-clinepass-c4-unit-ladder.md`。］

### S3 stale 计数语义：只有「曾确认过、后来读不到」才计（应修，计数语义）
- **实现**：`Source.attributionConfirmed`（`usage.go:539`）在**第一次** `installRoster` 成功（`providers.go:347`）
  置位且永不清零；`observeStaleAttribution(snap)`（`source.go:236`）改为 `if snap.fresh || !snap.confirmed { return }`
  —— 从未确认过尾号表的主机（没装 magpie、发现一直读不到 providers.json）**不计**该计数、也不打 WARN：那种
  情形没有 roster、没有「行被丢掉」的损失，信号由发现路径的 `!found` 承担，否则该计数会在健康部署上每账号
  每窗口每轮空涨。
- **与 status 分离的实测**：stale / `account_unmatched` / `unattributed_rows` 三者都不写
  `clinepass_usage_source_status`；F1 的用例同时断言「非正 from 不再能把 status 打回 ok」，两者自此真正解耦。
- **用例**：`TestStaleAttributionIsSilentBeforeTheFirstConfirmedTable`（`usage_test.go:1686`）：源从未确认过表
  → 求和前后 stale 计数 delta = 0；确认过之后转陈旧 → 每求和 +1；成功发现 → 表新鲜 → 不再涨（恢复行的
  日志由成功发现打，不由求和打）。插入在既有 `TestAttributionSuspendedUntilDiscoveryConfirms` 之后。
- **先 FAIL 后 PASS 原文**（去掉 `!snap.confirmed` 守卫后）：
  `usage_test.go:1712: stale counter delta on a source that never confirmed a table = 1, want 0: there is no roster whose rows went missing`。

### F2 占比 0 仍写总额（致命）— **未改，已裁定：保留行为、收窄文档承诺**
- **现象确认**：`internal/planusage/estimate.go:462-469`：`if pool > 0 { if weekly.set { 写 pool }; if monthly.set { 写 2*pool } }`
  —— 池可能来自**月**窗口，而**周**窗口只要 `set` 就写池，不看周窗口自身占比；同理月窗口被写时不看月窗口占比。
- **为什么没改（停止条件命中）**：任务书要求「若现有测试或代码里存在明确依赖『周窗口占比 0 也写池』的断言：
  停下报我，不要自行翻转语义」，而 `internal/planusage/estimate_test.go:534-563` 的
  `TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly` 正是这条断言：fixture 是「周窗口 `Percent: 0`、
  周窗口求和 0、月窗口 25% 且求和 400000」，它断言**周窗口** `LimitTokensEstimate == 784314`（= 月窗口反推
  的一半，注释写「the fresh week is not left blank」），并对月窗口断言 `2 × 784314`。实测该用例今天 **PASS**，
  即该行为是被**有意钉住**的（它也是本分支 in-flight 改动的一部分，不是历史遗留）。
- **两种自洽方向（供裁定）**：
  ① **按 F2 改代码**：`estimate.go` 的写池两处各加「本窗口占比 > 0」守卫（周：`weekly.set && weekly.frac > 0`；
     月：`monthly.set && monthly.frac > 0`），并同步改这条用例（周窗口期望 784314 → 0，月窗口不变），
     `config.yaml.example` 的「占比为 0 的窗口不产出总额（无从反推池）」即成为严格承诺，与 5h 现有行为一致；
  ② **按现状改文档**：把该句收窄为「占比为 0 的窗口**不由自己**反推池；能从另一窗口锚定出池时仍写池（`0/池`——左半按显示占比折算，占比 0 时就是 0）」，
     与已写明的「打满窗口能从另一窗口锚定就仍写池」并入同一条规则。
- **任务书里的一处引用需更正**：`README.md:177`（`/admin/quota` 行）**并没有**「占比为 0 的窗口不产出总额」
  的承诺（该行关于总额的表述只是「account 的 `cline-pass/` 消耗 ÷ 已用占比」），承担该承诺的是
  `config.yaml.example` 的估算段（本次未动该句）。
- **没有为 F2 新增用例**：S4 要求「为上面每条补回归测试」，F2 因停止条件变成待裁定项，**未**新增任何断言
  （否则等于替主代理做了语义决定）。

## 被放弃的方案（必填）
- **自行翻转 `TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly`（F2）**：否决。任务书明令该情形必须
  停下上报；两条语义（「占比 0 不产出总额」vs「刚重置的周窗口不留白」）都是本分支作者能自圆其说的设计，
  改哪一边都会动到 in-flight 的既有契约。
- **为一轮快照引入全局可变共享态（例如包级 current round / 读写锁外的原子指针）**：否决。`Round` 做成
  值语义（三字段快照）已经够用，全局态会让并发求和互相污染，且 `-race` 只能证明「没报」，证明不了「语义
  正确」，属更差的取舍。
- **给 token 对加「千万 / 十万」等更细的中文单位**：否决。阶梯只需保证「数值可读 + 对等值 + 宽度不溢出」，
  多一档就多一个必须逐档验证的翻档边界，收益为零。
- **在 F1 里顺手把非正 from 的调用方改成不调用**：否决。任务书要求「不要顺手改调用方语义」；且当前**没有**
  调用方传非正值，改调用方只会扩大改动面。

## 与既有笔记的关系
- `20261007-clinepass-attribution-followups.md`（active）：**本次追加 L7–L9**（三条「不改」+ 两条「被放弃
  的方案」），其余 L1–L6 结论不变、不取代。
- `20261007-clinepass-usage-source-magpie.md`（active）：**不取代**。本文的单轮快照（S1）与 stale 语义（S3）
  是那篇「双轨归属」契约的收口细化；求和口径（`in + out` 剔 cache）、过滤门槛、降级契约均未动。
- `20261003-clinepass-quota-estimate-derivation.md` / `20260925-clinepass-quota-total-estimate.md`（active）：
  **不取代**，但有一处需注意 —— 后者写「占比 0（新窗口）无消耗可反推；…故耗尽窗口不显示总额」属**旧行为**，
  本分支已改为「耗尽窗口写池（X/池）、无池才写实测（T/T）」；F2 若定为方向 ①，两篇的相应句子也需一并更新。
- 本分支的 in-flight 改动（`feature/magpie-usage-source`，31 个已暂存改动）未提交，本文与工作树改动同为
  待审产物。

## 来源
- 任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]`（第二轮对抗审计的四条修复 + 三条「不改」+ 文档同步）。
- 实测（2026-10-08，工作树）：`go build ./...` / `go vet ./...` / `go test -count=1 ./...`（24 个包 ok、
  1365 顶层用例 PASS、0 FAIL）、`scripts/test_*.sh` 5 个脚本、`python3 scripts/test_generate_mcp_tools.py`
  全部退出 0；`-race` 见 S1 节。

## F2 裁定（主代理拍定，2026-10-08）：保留现行行为，只收窄文档承诺

- **裁定**：**保留**「占比 0 的窗口仍写总额」的现行代码行为，改文档 —— 承诺句收窄、并把两个窗口的差异写明。
  不把行为改成「占比 0 一律不产出总额」。
- **依据**：`internal/planusage/estimate_test.go:534-563` 的
  `TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly` 明确断言「周窗口 `Percent=0`、求和 0 → 周窗口
  拿到月窗口锚定的池」：fixture 里周窗口 `Percent: 0`、求和返回 0，月窗口 25 % 且求和 400000，用例断言
  **周窗口** `LimitTokensEstimate == 784314`（= 月窗口反推的一半）、月窗口 `== 2 × 784314 = 1568628`，
  注释原文 “the fresh week is not left blank”。该用例在本次（2026-10-08，工作树）实测 **PASS**，且属本
  分支 in-flight 改动 —— 行为是被**有意钉住**的。按 F2 改码会抹掉卡片上「本周额度锚定」这条信息、推翻
  既有意图，弊大于利。
- **落地（文档侧，本次）**：`config.yaml.example` 估算段把「占比为 0 的窗口不产出总额（无从反推池）」
  收窄为三句 —— 占比 0 的窗口**不用它自己去反推池**（0 占比无法外推，不能拿它算限额）；池能从**别的
  窗口**锚定出来时该窗口**仍写这个池**并显示「0/池」；**（左半口径：按显示占比 `displayPercent` 折算、
  小数路径向上取整，不总等于 JSON 的 `measured_tokens`——见文末「补记：左半口径（R2）」节）
  **5h 窗口占比 0 时保持不产出总额**（现状，
  与前者不矛盾：它没有可用的锚定来源）。`README.md` 的 v0.36.0 条目里把 `SumClinePassTokens` Deprecated
  列入「不变」的句子同 v0.35.1 条目口径修正（该入口已随 `internal/metapiusage` 包删除，现行代码无此入口）。
- **备选路径（将来若要改成严格版）**：
  ① 动 `internal/planusage/estimate.go` 的**两处写池守卫** —— `if weekly.set` → `if weekly.set && weekly.frac > 0`、
     `if monthly.set` → `if monthly.set && monthly.frac > 0`（池仍可由另一窗口锚定，只是本窗口占比 0 时不写）；
  ② 改 `TestApplyClinePassEstimatesWeeklyResetFallsBackToMonthly` 的**周窗口期望值 `784314` → `0`**
     （月窗口 `1568628` 不变）；③ 连带把 `config.yaml.example` 与 `20260925-clinepass-quota-total-estimate.md`
     / `20261003-clinepass-quota-estimate-derivation.md` 里「占比 0 无消耗可反推」的旧句升格为严格承诺。
     走这条路等于推翻 ① 用例里被钉住的「fresh week 不留白」，需主代理重新裁定。

## 补记：左半口径（R2，2026-10-08）

- 卡片 token 对的**左半是「已用量」，但不是 JSON 的 `measured_tokens`**：它是
  `int64(总额 × 显示占比 / 100)`（`internal/planusage/report.go:765` 的 `clineMetricField`），显示占比走
  `displayPercent`（`report.go:1010`）——`UsedFraction` 存在时 `ceil`，否则用上游给的整数 `Percent`（地板值）。
  故**占比 0 时左半就是 0**：池能从别的窗口锚定时卡片读 `0/池`（`config.yaml.example` 的 quota 估算段
  已按此改口径）；`measured_tokens` 只在**耗尽窗口**（复用实测当总额）出现，与左半不是同一个数。
- 这句口径是**文档补记**：渲染与估算逻辑本轮（含 C4 修法）未动；两份数字在卡片/JSON 上对不上的
  全部原因就是「左半 = 显示占比 × 总额」这一乘。C4 修法前的真实缺陷不是这个乘，而是单位太大把
  左半渲染成了 `0千`/`0亿`（见 `.agents/notes/20261008-clinepass-c4-unit-ladder.md`）。
- 本轮（C4）的**遗留 / 未闭环清单**见 `.agents/notes/20261008-clinepass-c4-unit-ladder.md` 的同名节。
