---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage, cmd/prism
---

# 配额卡片统一成「极简单行式」：三家 provider 一套模板，去掉 `~` 与「· 估算池」

## 一句话结论
- `prism quota` / `GET /admin/quota` 的卡片**只剩一种版式**：**一张卡 = 一个 `(Provider, Window)` 分组**，由「标题行 + N 个账号数据行 + 底边行」组成（N = 该分组账号数）。Gemini、ClinePass、SuperGrok **一律**走这一套，不再有「ClinePass 合卡 + 其余 provider 两行式」的分叉。
- 标题行**只**有 `<Provider> · <Window>`，**不含账号名**、**不含「估算池」**（周/月就是 `ClinePass · 周限额` / `ClinePass · 月限额`）。账号名下移到数据行的第一个元素。
- 数据行 = `·` 色点 + 账号**显示名**（`stripNumericSuffix`：`Cline2`/`Cline1` 都显示 `Cline`）+ 胶囊条(23) + 百分比(4，右对齐) + **指标段(13，右对齐)**。
- 指标段是**智能单指标**（见 D3 的回落矩阵）：5 小时窗口 → 重置倒计时；周/月窗口 → `已用量/总量`（**不带 `~`**）；拿不到数据 → 倒计时兜底，再不行 → `-`。
- 卡宽不再是固定 56 列，而是 `max(行宽, 标题所需宽)`，其中 `行宽 = 4 + 45 + nameMax`、`nameMax` = 卡内最长**显示名**单元格宽度。
- 被**删除**的行为：`~` 标记、「· 估算池」标题段、非 ClinePass 的**两行式**（标题带账号 + 47 格胶囊行 + `已用 x% / 总额 …  倒计时` 明细行）、`已达限额` 页脚行、以及只为它们存在的 `renderWindowCard` / `cardDetailRow` / `windowDetail` / `col2Label` / `totalPart` / `cardFooter` / `cardBarRow` / `renderInfoCard` / `capsuleBar` 等函数与 `cardWidth=56`/`barCells=47` 常量。
- **口径零改动**：估算算法（周锚定单向派生、月 = 2×周、5h 不参与估算）、分子剔 cache、`account_id <= 0` 的 WARN + expvar、`SumClinePassTokens` 的 Deprecated、JSON 字段与路径（`limit_tokens_estimate` / `measured_tokens` …）**全部不变**；行身份（`clineRowID` = 全名 + 指纹）与调色盘配色不变。

## 背景
- 历史版式线：`20260922-quota-capsule-bar.md`（56 列两行胶囊卡）→ `20260923-quota-tui-card-format.md` / `20260923-card-title-plain-text.md`（标题去色、标题带账号）→ `20260923-card-symmetric-gutter.md` → `20260924-card-width-60-to-56.md` → `20260924-quota-card-cn-text.md`（`已达限额` / `后重置` 中文化）→ `20260929-clinepass-multi-account-metapi.md`（**ClinePass 合卡**：一张卡一个 (profile, window)、一行一个账号）→ `20261003-clinepass-quota-estimate-derivation.md`（`~` 标记 + 「估算池」标题 + 5h 倒计时）。
- 用户当次明示的口径（逐字）：**「配额卡片统一成 ClinePass 极简单行式，三家 provider 一套模板」**，并要求**去掉 `~` 与「· 估算池」**。
- 于是本笔把「ClinePass 合卡」这一支提升为**唯一**版式，其余 provider 的两行式整体退役。

## 决策

### D1. 分组与卡结构（三家统一）
- 分组键 `clineKey{provider, window}`：provider 取 `strings.ToLower(strings.TrimSpace(s.Provider))`（同名不同大小写归一组），window 取**原始窗口名**（`5h`/`rolling`/`weekly`/`monthly`）。`clinePassGroups` 泛化为 `cardGroups(sorted []Snapshot)`：**不再**按 `isClinePass` 过滤（该函数随之删除）。
- 卡片在「第一个贡献该分组的快照」的位置输出（原 `emitted` 机制不变），所以“先按 provider 排位、再按快照内窗口顺序”的既有卡片顺序与 `accountSortKey` 排序保持不变。
- 行数 = 该分组账号数；行序 = 快照排序（`accountSortKey`：provider 排位 → 首个账号名）里的出现顺序，即账号名字典序。
- 窗口名缺省：`windowless`（401/403 清窗后的失败快照）分组**不带窗口段**（标题就是 `╭─ Opus ─…╮`），**不再**渲染占位的 `--`；窗口名映射仍走 `windowTitle`（`rolling`/`5h` → `5小时限额`）。
- 一个快照内**同名窗口**只取第一个（分组按名字键）。当前四个 fetcher（gemini/xai/go/clinepass）都不可能产出同名窗口（各自 `map[string]Window` 去重或固定追加 distinct 名字），因此这是不可达分支而非数据丢失，已写入 `cardGroups` 的文档注释。

### D2. 字段与几何（逐列定义）
- 行 = `"│ " + dot(1) + " " + name(n) + " " + capsule(23) + " " + pct(4) + " " + metric(13) + " │"`，即 `clineRowFixed + n + 4` 列，`clineRowFixed = 1 + 4 + 23 + 4 + 13 = 45`。
- 卡宽 = `max(行宽, titleWidth(profile, window) + 6)`；`nameMax` = 卡内最长**账号名单元格**（显示名 + 旧标记）宽度。卡宽由内容决定，故**永远不截断**账号名/窗口名，也永远不会把右边框顶出去（`cardLines` 测试对每张卡逐行断言同一宽度）。
- 账号**显示名**沿用既有契约 `stripNumericSuffix`（尾部纯数字后缀剥离；`Cline-1` → `Cline-`，全数字名保留原名），**行身份不变**（`clineRowID` = 全名 + 指纹；无指纹时按位置）。
- 色点与账号名**同色**（`accountColor(fp)` 调色盘，按指纹取）。无有效指纹（空或 < 8 hex）或 `--no-color`：回落为普通 `·` 与默认色。
- 胶囊条 23 格：`render.CapsuleBar` + `render.CapsuleUsedCells`（逐格 green→yellow→red 渐变）；**耗尽**（≥100% / `used up` / `rate-limited`）时整条纯红（`pal.red`），这是行内唯一的耗尽信号。
- 指标段 13 列右对齐，用 `render.PadLeft`；超出 13 列由 `PadLeft` 省略号裁剪，不撑行宽。

### D3. 指标段的单一规则（智能单指标）+ 回落矩阵
判定谓词 `clineTokenPairWindow(name)`：`weekly` / `monthly` 是「带池窗口」，其余（`5h`、`rolling` 及未知名）不是。

| 窗口 | 有总量/实测 | 无总量无实测 | 无 `ResetsAt` |
| --- | --- | --- | --- |
| 周 / 月（带池） | `已用量/总量`（无 `~`）；耗尽强制 `X/X` | 有 `ResetsAt` → 倒计时；都没有 → `-` | 同左（→ `-`） |
| 5h / rolling / 其它 | 仍走倒计时（该窗口无池，不看 token） | 有 `ResetsAt` → 倒计时 | → `-` |

- 已用量算法沿用既有口径：`总 = LimitTokensEstimate > 0 ? LimitTokensEstimate : MeasuredTokens`；`已用 = 总 × displayPercent%`（`displayPercent` 走 `UsedFraction` 的 ceil 修正）；`PadLeft` 到 13 列。
- 倒计时文案沿用既有 `resetText`（`cardCountdown` + ` 后重置`，过期为 `已重置`）——即 `2h 21m 后重置` / `42m 后重置` 形态。**规范文字里的「沿用 `formatRemain` 文案」是笔误**：`formatRemain` 是 legacy 表的列格式（`2h21m` 无空格、`已到`/`<1m`），`prism usage`/卡片行从来不用它；本笔记按规范给出的**示例**（`42m 后重置` / `2h 21m 后重置`）落地，即继续用卡片的 `cardCountdown` 口径。
- 生产实拍（`go run ./cmd/prism quota --config /var/lib/prism/config.yaml --no-color`，只读）验证了三条规则同时成立：
  ```
  ╭─ ClinePass · 5小时限额 ────────────────────────────╮
  │ · Cline ▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱  19% 1h 58m 后重置 │
  │ · Cline ▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱   0%             - │
  ╰────────────────────────────────────────────────────╯
  ```
  第二行是「5h 且上游未给 `ResetsAt`」的真实案例（`-`），第一行是正常倒计时。

### D4. 被删除 / 改名的符号（删前逐个 `grep` 确认无其它调用者）
**删除（旧版 `report.go` 有、现在没有）**：
| 符号 | 原来的职责 |
| --- | --- |
| `renderWindowCard` | 两行式卡（标题带账号 + 胶囊行 + 明细行） |
| `renderInfoCard` | 「无窗口」信息卡（标题带账号 + `额度 x% / …` 明细行） |
| `renderClineCard` | ClinePass 合卡的卡片包装（标题只带 profile+window），由 `renderGroupCard` 承接 |
| `cardDetailRow` / `windowDetail` / `col2Label` / `totalPart` | `已用 x% / 总额 …` / `额度 x% / $N` 明细行及其拼装 |
| `clineCardTitle` / `cardTitleLine` | 老的两套标题构造器（宽度参数不同）；现在只有 `cardTitleLineAt` |
| `cardFooter` + `pal.yellow`（`yellow` 方法） | `已达限额` 页脚（#F4A261）——耗尽改由「红胶囊 + `100%` + `X/X`」表达 |
| `cardBarRow` / `capsuleBar` / `capsuleUsedCells` / `capsuleLevel` | 47 格胶囊的旧实现；新行走 `clineBar`（23 格，直接调 `render.CapsuleBar`） |
| `cardBody` / `cardNote` | 固定宽包装的正文行与注记行；改名 `cardBodyAt` / `cardErrorNote`（带宽参数） |
| `joinCard` | 固定宽包装；改名 `joinCardAt` |
| `clinePassGroups` | 只聚合 ClinePass 的分组器；泛化为 `cardGroups` |
| `clineNumberField` | 指标段；改名 `clineMetricField`（现在服务所有 provider 并自带回落） |
| `clinePoolWindow` | 估算池判定；角色由 `clineTokenPairWindow`（带池窗口判定）承接 |
| `isClinePass` | 只剩 ClinePass 分叉时才需要；统一后无调用者 |
| `const clineEstimatePoolLabel`（`"估算池"`） | 标题里的估算池段 |
| `cardWidth` / `cardInner` / `barIndent` / `pctWidth` / `barGap` / `barCells` / `resetWidth` / `detailWidth` | 56 列几何常量；卡宽改为按内容计算 |
| `cardTitleLineAt` 的 `account` 形参 | 标题不再有账号段，形参与其去重逻辑（`Gemini Gemini`）一并删除 |
| `formatTokenPair` 的 `totalMarker` 形参 | `~` 的实现：签名收敛为 `formatTokenPair(used, total)` |
**新增（旧版没有）**：`cardGroups`、`cardProfileName`、`renderGroupCard`、`clineRowNameCell`、`clineMetricField`、`clineTokenPairWindow`。
- 删完后 `go vet`/`go test` 全绿，并逐个函数统计引用数，report.go 内**无单引用（仅声明）函数**残留。

### D5. 规范未覆盖处的取舍（两处，均在本笔落地并请复核）
1. **`旧` 标记位置**：规范只删了两行式，没提 `Stale` 的 `旧`；而它原先挂在「标题账号」上，标题已不再带账号。取舍：`旧` 骑在**数据行的账号名单元格**里（`│ · a1 旧  ▰…`），且 `nameMax` 按**带标记**的单元格计算，避免该行胶囊错列。替代方案（把 `旧` 放进标题、或整卡级标记、或直接丢弃）都更差：失败重试是**按快照**发生的，同一分组里可能一行新鲜一行过期，标题级会误标整卡，丢弃则丢信号。
2. **无窗口卡**：`401/403` 会清空窗口（`Cache.StoreFailed`），这类快照仍要可见。取舍：该分组标题**不带窗口段**（不发明 `--` 占位），行只显示账号名 + 空指标列，失败原因照旧走 `⚠ <账号>: <本地化码>` 注记。

### D6. 与既有笔记的关系
- `20261003-clinepass-quota-estimate-derivation.md` 的**展示契约节**（`~` 标记、「估算池」标题、5h 倒计时）**被本笔 supersede**；其**估算主体**（周锚定单向派生、月 = 2×周、剔除 cache、5h 不参与估算）**仍 active**，故不整篇标 superseded。
- `20260922-quota-capsule-bar.md` / `20260923-quota-tui-card-format.md` / `20260924-card-width-60-to-56.md` / `20260924-quota-card-cn-text.md` 里的**两行式几何、56 列卡宽、`已达限额` 页脚、明细行文案**均被本笔取代；其中「胶囊填充 = 已用占比」「避免左边距缩进」「标题文字不上色」等规则被单行式继承。
- `20260929-clinepass-multi-account-metapi.md` 的**合卡机制**（分组键、行身份、调色盘）被本笔提升为全局规则，内容不再限于 ClinePass。

## 被放弃的方案（必填）
- **只统一「外观」不统一数据流**（保留 `renderWindowCard`，仅把标题的账号挪到新行）：会让同一份数据有两条渲染路径，`~`/「估算池」也必须在两处各删一遍；否决。
- **卡宽仍固定 56 列**：ClinePass 侧早已按显示名自适应（`clineCardWidth`），统一到 56 会把长账号名截断或把列挤错；且规范明确要求 `max(行宽, 标题所需宽)`。否决（代价是卡宽因 provider/账号而异，测试改为「卡内等宽」断言）。
- **把派生池继续标 `~`**（只是从标题里去掉「估算池」）：规范逐字要求「删除 `~` 标记的全部实现」；且派生池是该窗口**唯一**的总额，标注推断反而让一列数字出现两种形态。否决。
- **`已用用量` 改由上游百分比直接呈现（不反算 token）**：与既有 ClinePass 口径冲突，且会把用户已经习惯的绝对消耗量藏起来；规范也要求沿用既有口径。否决。
- **`旧` 标记改到标题、或丢弃 `旧`**：见 D5.1。否决。
- **把同名窗口按序号再分组**（防「一个快照两个同名窗口」的假想数据丢失）：现有 fetcher 结构性不可达，且会在账号间窗口列表不一致时破坏合卡；改为文档注释说明。否决。

## 来源
- 用户当次任务说明（逐字规范：单行式结构、13 列指标段规则、卡宽公式、回落矩阵、删除清单、自证清单）；**用户当次明示免审**，故本笔自证做到「门禁六项 + 渲染实拍 + 三处变异自证」。
- 相关实现：`internal/planusage/report.go`（`RenderCards` / `cardGroups` / `renderGroupCard` / `clineRowLine` / `clineMetricField` / `clineTokenPairWindow` / `clineCardWidth` / `cardTitleLineAt`）、`internal/planusage/report_test.go`（卡片测试整节重写）、`internal/planusage/estimate_test.go` 与 `internal/planusage/order_test.go`（断言随版式更新）、`cmd/prism/metapi_test.go`（**仅测试断言**去掉 `~`，边界说明见下）。
- 边界说明：任务边界写「只改 `internal/planusage/`（含其测试）与新笔记」，但 `cmd/prism/metapi_test.go:514-515` 断言了卡片里的 `50.0K/~100.0K` / `10.0K/~200.0K`——它是**旧 `~` 契约的断言**，规范同时要求「不得留任何断言 `~` 的用法」且门禁 `go test ./...` 必须绿。本笔只在该测试文件内做**最小断言更新**（去掉 `~`，其余数字不变），未触碰 `cmd/prism` 的任何生产代码。
