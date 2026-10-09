---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: magpieusage, planusage, cmd/prism
---

# ClinePass 掩码归属收口后的遗留清单（D1 行级未归属指标已落地 / D5 的 13 行不改口径 + reviewer・oracle・主代理观察项 + 第二轮对抗审计三条「不改」L7–L9）

## 一句话结论
- 本轮（分支 `feature/magpie-usage-source`：magpie 用量源换入 + 双轨归属）**不阻断放行**，但下列各项必须留痕：
  一条口径决定已定案（D5：13 行 `provider` 空、`status` 404、0 词元的畸形 `model` **不归属、不改口径**），
  两条 reviewer 项现接受（日志无轮转 ⇒ 全量重扫；deprecated 死代码保留），一组 oracle 观察项（掩码尾匹配的
  可靠性边界）写明靠什么成立，一条主代理观察项（921 行 keyless 中 2 行 `status=499` 仍计入归属）**不动数字**。
- 已实现的对照项是 **D1**：行级未归属指标 `clinepass_usage_unattributed_rows_total` 已在 `/metrics` 上，
  并已登记进 README 的 `/admin/quota` 行（本次随本笔记一起补）。
- 本笔记**不改任何代码、测试、用量口径与数字**，只把「下一轮要接的事」写成可检索、带触发条件的清单。
- 第二轮对抗审计（任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]`）另判三条**不改**（L7 末 4 位匹配、L8 `INDEX.md` 被 `.gitignore` 忽略、L9 尾号碰撞误归属），同批追加为 L7–L9（只登记口径、不改行为）。

## 背景
- 触发：magpie 换源与双轨归属（Track 1 按 magpie 的 `providerKeyId`、Track 2 按 keyless 行的掩码尾唯一匹配）
  收口后，本轮评审（reviewer / oracle / 主代理）判为**可以放行**，但若干「不阻断、必须留痕」的项散落在
  对话里，需要落盘成清单，避免下一轮凭印象重启讨论。
- 已实现的 D1 让「Track 2 静默丢弃的比例」第一次成为一个数（按行计）；本清单其余各项是本轮**明确选择不做**
  或**推迟**的部分，每条都注明触发条件，便于将来按证据接续。
- 相关既有笔记（均 active、本文不取代）：`20261007-clinepass-usage-source-magpie.md`（换源与双轨归属的
  实现与实测）、`20261007-clinepass-value-drift-signals.md`（账号级零命中 + 窗口下界的补观测）。

## 遗留清单

### L1 D1：行级未归属指标已实现（本轮闭环）
- **来源**：用户任务书 D1（归属口径决定，用户已批准；与 `[MARK-IMPL-CLINE-DUALTRACK]` 同批）。
- **现象**：Track 2 的可靠性来自「`cline-pass/` 模型前缀 + 掩码尾唯一命中」。掩码形态变化、key 轮转、
  或两把 key 尾号碰撞时，keyless `cline` 行会落回空桶，账号分子只是**变小**；此前只有账号级的
  `clinepass_usage_account_unmatched_total`，**单位不同**（账号 × 每次求和），表达不了「哪些行落空」。
- **当前处理**：已实现 `clinepass_usage_unattributed_rows_total`（`internal/magpieusage/source.go` 的 expvar；
  `internal/magpieusage/usage.go` 的 `scanUsageLog` 在 Track 2 归属失败时 `+1`）；口径为**按行、按扫描**
  （每次重扫日志把落空行逐行累加），与按账号的 `clinepass_usage_account_unmatched_total` 是两个单位，
  不得混用；README 的 `/admin/quota` 行已补登并写明区别（本笔记同轮落盘）。实测该计数今天为 0
  （921 行 keyless 全部唯一命中）。
- **后续动作**：无（本轮闭环）。运维侧把它当作「Track 2 静默丢弃的比例」，**非零即表示**归属表与
  magpie 的掩码/roster 出现漂移。

### L2 D5：13 行畸形 `model` 不改归属口径
- **来源**：用户任务书 D5（同一轮归属口径决定，用户已批准「不改」）。
- **现象**：实测 13 行 `model = cline/cline-pass/deepseek-v4.1-flash`、`provider` 为空串、`status = 404`、
  0 词元。它们被 `cline-pass/` **前缀门槛**正确排除（`cline/…` 不命中前缀），因此不进任何账号的分子；
  这既不是归属失败的 bug，也不是需要「找回」的漏算。
- **当前处理**：**不改归属口径**（不加「`model` 含 `cline-pass/` 子串」这类放宽，也不把 `provider` 空串
  纳入任何门槛例外）；这 13 行继续留在空桶。13 行合计 0 词元，计与不计都不影响当前任何数字。
- **后续动作**：若将来同类畸形 `model` 开始**带词元**，它们会被**静默漏算**（前缀门槛只看前缀，行本身又
  落不进任何账号，且 provider 空 ⇒ 也不触发 Track 2 的计数）。届时需要**另立**一条错误观测口径
  （例如按 model/status 形状计行），而**不是**改归属口径。触发条件：上游 magpie 的 model 命名或代理
  换名让 `cline/…` 形态开始带量。

### L3 reviewer：usage.jsonl 无限追加、无轮转 ⇒ 每轮全量重扫，成本随日志线性增长
- **来源**：reviewer 本轮复核意见（不阻断放行）。
- **现象**：`/root/.config/magpie/usage.jsonl` 由 magpie **无限追加、永不轮转、永不回填**；`Source` 在文件
  版本（`SameFile + size + mtime(Nanosecond)`）变化时**全量重扫**，扫描成本随日志行数线性增长，且每个新
  版本都要重扫一整遍。
- **当前处理**：**现接受**。版本缓存已保证「一轮内每账号 3 个窗口共享同一次扫描」，且增长相对配额刷新
  周期（`quota.refresh_interval`，默认 120s）尚在可接受范围；**不**引入轮转/截断（轮转会丢历史窗口，
  与「日志起点即分子下界」的自愈语义冲突）。
- **后续动作**：可后续加**增量偏移**（记住上次扫描的 offset + 文件身份，只消费追加部分；文件替换/截断时
  回落全量重扫）。触发条件：日志体量或单次扫描耗时进入可观测区间（把配额轮询周期拖长）。

### L4 reviewer：deprecated 的订阅级 `SumClinePassTokens` 无调用方，保留为死代码
- **来源**：reviewer 本轮复核意见（不阻断放行）。
- **现象**：订阅级 `SumClinePassTokens`（整包/全订阅求和，账号级求和之外的旧入口）已 deprecated，
  **无调用方**，仅作为死代码保留；其注释已说明「系刻意保留」。
- **当前处理（后修订：已删）**：本轮任务书明确要求删除，故已连同 `Source.sumAll` 与
  `usageIndex.sumAllTokens` 及其测试引用一起移除（`[MARK-PRISM-REWORK3-ATTRIB]`，2026-10-07），
  代码里已无引用（只剩本笔记与历史笔记的记述）；per-account 路径
  `SumClinePassTokensByAccount` → `sumByAccount` 未动。删后「唯一求和入口是按账号的」恢复成立。
- **后续动作**：无。

### L5 oracle：掩码尾匹配的可靠性边界（一组观察项）
- **来源**：oracle 本轮复核意见（条件放行；各项均不阻断）。
- **现象（逐条）**：
  1. 尾匹配用 `HasSuffix`、**不校验掩码形态**（不要求省略号 `…`、不要求 `API key …` 前缀）——可靠性
     **不来自**形状校验，而来自「`cline-pass/` 模型前缀 + 掩码尾唯一命中」两点同时成立。
  2. 尾号碰撞会**废掉整张 Track 2**（`newClineAttribution` 判定碰撞后整张归属表丢弃），**不是**只废掉
     相撞的那个尾号 ⇒ 一次碰撞让所有 keyless 行同时落空（L1 的计数会整体抬头）。
  3. **单行坏 JSON 会让整份文件总额都不出**（`scanUsageLog` 遇不可解析行返回 error ⇒ 本轮退化为无总额），
     这是有意的**大声失败**：宁可不出，也不出一半。
  4. `providers.json` 里**有的 key 就进 roster**，`status=active`／其它筛选**没有替代品**（magpie 的
     provider 文件没有等价的状态字段）⇒「文件里出现」就等于「会被轮询、也参与归属」。
  5. ~~全订阅求和 `SumClinePassTokens`（deprecated、无调用方）**仍可供误用**~~：**已删除**（见 L4），
     这一误用面随之消失。
- **当前处理**：**全部现接受**。可靠性来源已写在 README 的 `/admin/quota` 行与
  `20261007-clinepass-usage-source-magpie.md`；碰撞有「每状态转换一次 WARN」、单行坏 JSON 有「本轮无
  总额」的既有降级契约；roster 规则（无 status 过滤）是本轮的有意取舍；死代码见 L4。
- **后续动作**：**不**做掩码形状校验（避免与 magpie 的掩码文案产生新耦合）；碰撞的整表失效保持现状
  （WARN 已点名相撞的两个账号的**单向 id**——`providerKeyId` 同形，尾号与密钥本身不进日志）；单行坏 JSON
  的降级保持现状。若将来要求「坏行不拖累整份」，需另立口径
  （跳行 + 单独计错），属改口径，不在本轮。

### L6 主代理：921 行 keyless 中 2 行 `status=499` 仍被计入归属
- **来源**：主代理本轮观察。
- **现象**：921 行 keyless `cline`（掩码尾 `326d`）中有 **2 行 `status = 499`**（客户端断开/取消），它们
  仍被计入账号归属与分子。
- **当前处理**：**不动数字**。归属只看「模型前缀 + 掩码尾唯一命中」，**不看 `status`**；本轮不引入
  「成功才计」的口径。
- **后续动作**：若口径定为「成功才计」，这 2 行（以及任何非 2xx 的 keyless 行）应排除；届时须同时明确
  keyed 行（Track 1）是否同口径，避免两条路径一个算一个不算。触发条件：用户确认按 `status` 过滤。

### L7 第二轮对抗审计 R1：末尾匹配用 `strings.HasSuffix`（判为「现行语义即末 4 位」，正确）
- **来源**：任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]` 第三段第 1 条（第二轮对抗审计结论，判「不改」）。
- **现象**：掩码尾匹配用 `strings.HasSuffix(providerAccount, tail)`，故标签 `…aaaa326d`（尾 4 位同为 `326d`）会归给尾号 `326d` 的账号，尽管它不等于 roster 的 `API key …326d` 形状。
- **当前处理**：**不改**。归属的现行语义就是「末 4 位相同即同一把 key 的掩码」：magpie 只承诺掩码尾 4 位（省略号后的位数不承诺），改成「边界匹配」（要求 `…` 前一位不是字母数字）会**新引入**形状耦合——magpie 改掩码文案（去掉 `API key ` 前缀、换省略号字符、改尾号位数）就会让归属整体失效，而 `HasSuffix` 只依赖「尾 4 位」这一条已被 `providerKeyId` 独立印证的事实。碰撞的代价与观测已由 L5.2（整表失效 + WARN）与「唯一命中才归属」（0 命中/≥2 命中都不归属）承担。
- **后续动作**：无。若 magpie 未来给出显式账号标识（非掩码），优先改用它，而不是收紧字符串匹配。

### L8 第二轮对抗审计 R2：`.agents/notes/INDEX.md` 被 `.gitignore` 忽略
- **来源**：同 `[MARK-PRISM-ATTRIB-FIX-20261008]` 第三段第 2 条。
- **现象**：`.agents/notes/INDEX.md` 被 `.gitignore:17` 忽略，`scripts/notes-index.sh` 生成的索引**不入版本库**：提交后仓库里没有笔记索引，clone 出来的工作区需要自己跑一次 `scripts/notes-index.sh`。
- **当前处理**：**不动 `.gitignore`**。笔记**正文**（`*.md`）入库、索引按需重生成（本轮的索引已在本地重生成，但不进提交）。放开忽略会让每加一条笔记都在提交里带出一份生成物的 diff（`updated: <date>` 还会每天漂）。
- **后续动作**：无（有意取舍）。需要索引时本地执行 `scripts/notes-index.sh`。

### L9 第二轮对抗审计 R3：旧表仍新鲜时，新 key 的尾号撞上 roster 内另一把 key 的尾号会误归属
- **来源**：同 `[MARK-PRISM-ATTRIB-FIX-20261008]` 第三段第 3 条（实测 `sib` 被多算 11 词元）。
- **现象**：发现未成功（表仍新鲜）期间 magpie 若已换用**新 key**，而该新 key 的掩码尾 4 位恰等于 roster 内**另一把** key 的尾号，则新 key 的 keyless 行会被归到**被撞账号**；不同 key 的末 4 位相同 ⇒ 掩码相同 ⇒ 光看标签分不出是本账号的旧 key 还是一把尾号相同的新 key。
- **当前处理**：**不改**。在固定 4 位掩码下这一歧义在**数据源层面不可判**：prism 手上只有「掩码尾 4 位 → 账号」一张表与逐行标签，新 key 与旧 key 尾号一旦相同，任何字符串判据都分不开。要收紧就得改**归属可用性契约**（例如「只在发现新鲜期间才归属」），而那是改口径、并且与 L1/L5 已定的「providers.json 里出现即轮询与参归」冲突。
- **后续动作**：触发条件是「发现落空 + 期间换 key + 尾号碰撞」三者同时成立；若届时要求收口，改的应是**发现路径的可用性契约**（或用 magpie 的显式 key 标识替代掩码尾匹配），而不是收紧尾匹配本身。

## 被放弃的方案（必填）
- **为 D5 的 13 行改归属口径（放宽到 `model` 含 `cline-pass/` 子串）**：否决。这些行 provider 为空、
  `status` 404、合计 0 词元；放宽只会把非 ClinePass 的 `cline/…` 模型拉进订阅分子（串账方向），当前
  收益为 0。
- **本轮就为 D5 另立错误观测口径**：否决（推迟）。触发前提（这类畸形 `model` 开始带词元）尚未出现，
  先留触发条件，避免为不存在的形状增加契约面。
- **把 reviewer 两项（日志增量偏移、删 `SumClinePassTokens`）在文档收口轮一并做掉**：否决。本任务书
  明令「不改任何代码、测试、口径与数字」，该两项都是代码改动。
- **把尾匹配从 `HasSuffix` 改成「边界匹配」（L7，第二轮对抗审计 R1）**：否决。归属的可靠性来自
  「模型前缀 + 尾 4 位唯一命中」，而不是掩码**形状**；加形状约束会把 magpie 的文案（`API key …` 前缀、
  省略号字符、尾号位数）变成归属的前提，风险大于收益。
- **放开 `.gitignore` 让 `INDEX.md` 入库（L8，第二轮对抗审计 R2）**：否决。索引是生成物，入库会把每日漂移的
  `updated:` 日期带进提交；正文入库、索引按需重生成。
- **为尾号碰撞收紧归属（L9，第二轮对抗审计 R3）**：否决。数据源不可判（同尾 4 位即同掩码），且收紧只能靠改
  「发现新鲜期间才归属」这类可用性契约，会改变既有归属行为（与 L5.4 的 roster 规则冲突）。
- **把 2 行 `status=499` 从分子里剔除（L6）**：否决（本轮）。「成功才计」是**口径决定**，须用户确认；
  单方面改数会让 keyed/keyless 两条路径口径不一致且不可追溯。
- **把 `status` 纳入归属判据（成功才归属）**：否决。归属的可靠性契约是「模型前缀 + 掩码尾唯一命中」，
  加入 `status` 会造出第二个判据点，并与 Track 1（按 id 归属、不看 `status`）不对称。

## 与既有笔记的关系
- `20261007-clinepass-usage-source-magpie.md`（active）：**不取代**。本文是同一改动集（未提交分支
  `feature/magpie-usage-source`）的**遗留清单**，其结论（求和口径 `in + out` 剔 cache、过滤口径
  `cline-pass/` 前缀 + provider 门槛、降级契约、D1 行级指标）全部仍成立；本文只登记那些结论之外
  「不阻断放行但要留痕」的项。
- `20261007-clinepass-value-drift-signals.md`（active）：**不取代**。该类补的是**账号级**零命中与**窗口**
  下界两条信号；本文 L1 是**行级**未归属信号，两者单位不同（见 L1「当前处理」一栏）。
- `20260930-clinepass-multi-account-review-followups.md`（active）：**不取代**。其 O5 死代码清单属 metapi
  时代；本文 L4 的死代码是换源后新产生的 `SumClinePassTokens`。

## 来源
- 用户任务书 `[MARK-PRISM-FOLLOWUPS-DOC]`（本轮评审遗留清单落盘）；相关上游任务书
  `[MARK-IMPL-CLINE-DUALTRACK]`（双轨归属）、`[MARK-PRISM-FIX-DRIFT]`（账号级零命中 + 下界）。
- 第二轮对抗审计任务书 `[MARK-PRISM-ATTRIB-FIX-20261008]`（L7–L9 三条「不改」的判据）。
- reviewer / oracle / 主代理在本轮（magpie 用量源换入 + 双轨归属）的复核意见。
- 只读实测（2026-10-07）：`/root/.config/magpie/usage.jsonl` 与 `/root/.config/magpie/providers.json`
  的计数与形状，按 `20261007-clinepass-usage-source-magpie.md` 的记录引用（本文不复制日志原文、不记录
  key，仅用 key 尾 4 位 `326d`/`5f27` 与账号标识）。
