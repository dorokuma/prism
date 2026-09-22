---
status: active
superseded_by: ""
supersedes: ""
模块: usage, planusage
---

# usage/quota 面板文案中文化 + π 面板去掉重复大标题

## 一句话结论
- `prism usage` 卡片与 `prism quota` 卡片里仅剩的几处英文标签改为中文：usage 标题头 `Usage`→`用量`、空态 `(no data)`→`（暂无数据）`、`group_by` 键与其列头统一走一张 `groupKeyLabels` 映射表（model→模型、provider→供应商、account→账号、hour→小时、day→日期、stream→流式、success→状态，未知键原样透传）、`stream`/`success` 值 `yes/no`→`是/否`、`ok/fail`→`正常/失败`；quota 窗口标签 `5h window`/`weekly window`/`monthly window`→`5小时限额`/`周限额`/`月限额`。
- π 扩展 `extensions/prism.ts` 的 `StaticReport.render` 删掉了面板首行大标题（`truncateToWidth("  " + bold(data.title))`），渲染直接从 prism 输出的正文行开始；`ReportData.title` 与 `runAndShow` 的 `title` 入参**保留**（status 栏与错误通知仍在用）。
- **布局零变化**：列宽、胶囊格数、颜色、对齐、`填充 dash` 数、JSON 输出、CLI flag、帮助文本全部未动；标题行的 dash 填充由既有公式按新的 `DisplayWidth` 自动重算，每行仍恒 60 列（`assertCardWidth`/`DisplayWidth` 断言未放松）。

## 背景
- 任务书 [MARK-WORKER-PRISM-CN-TEXT] 给出精确改动清单，要求除清单外一律不动（布局、胶囊、颜色、对齐逻辑、JSON 输出、CLI flag、帮助文本保持原样），且不执行 git commit。
- 上一轮 [MARK-WORKER-PRISM-USAGE-CAPSULE]（见 `20260922-usage-capsule-report.md`）把 usage 报表改成胶囊卡片后，卡片里残留的英文只剩这几处标签；quota 卡片同理。中文标签散落在 3 个函数（`groupDesc`/`reportColumns`/`formatGroupValue`）里，若各写一份必然漂移。

## 决策
- **一张映射表两处复用**：新增 `internal/usage` 包级 `groupKeyLabels`（未导出映射）+ `groupKeyLabel(g)` 助手，同时喂给卡片标题 `groupDesc`（多键逐个映射、`, ` 分隔）与列头 `reportColumns`（`title := groupKeyLabel(g)`），键值天然一致。未知键 `return g` 原样透传——这是**标签表而非白名单**，SQL 侧新增分组键不会被 UI 藏掉。
- **success 列名取「状态」而非「成功率」**：该列的值是 `正常/失败`（布尔语义），叫「成功率」会与值矛盾；`hour` 取「小时」、`day` 取「日期」（存的是日期桶，不是天粒度计数）。
- **时间桶格式不动**：`hour`/`day` 的 `"01-02 15:00"` / `"01-02"` 是数据格式而非英文文案，`formatGroupValue` 里原样保留。
- **model 列头逻辑简化**：`reportColumns` 里 `g == "model"` 分支只保留 `MaxWidth` 截断（模型 20 列），标题直接取 `groupKeyLabel("model") == "模型"`，等价但去掉了重复的字面量。
- **π 面板标题行的删除方式**：只删 `render()` 里拼首行的那一句，其余逐行 `truncateToWidth` 不动；`withUsageIndent` 的行为因此不变（正文本来就带 prism 卡片的 `│` 边框，缩进逻辑对首行同样适用）。
- **测试连带改动的定性**：断言字符串同步（`report_test.go`/`handler_test.go`/`cmd/prism/usage_test.go` 的整卡精确串、`planusage/report_test.go` 的窗口标签）、`formatGroupValue` 四组值、以及 `handler_test.go` 的 `tableDataRows` 助手——该助手原先靠 `Contains(l, "模型")` 定位表头，而标题里现在也含「模型」，故改为靠 `命中率`（标题行永不含该词）定位表头。这是助手定位方式修正，不是放宽断言。
- **门禁**：`go build -o prism ./cmd/prism`、`go vet ./...`、`go clean -testcache && go test ./...`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全过。prism.ts 无既有类型检查入口（`/root/.pi/agent` 下无 package.json/tsconfig，node_modules 为空），改用外部 `tsc --strict --noEmit` + `node --experimental-strip-types --check` 双重验证，均零错误。

## 被放弃的方案（必填）
- **把 provider/success 等键名也一并中文化到 JSON 输出**：清单明确 JSON 输出保持原样，且会破坏 API 契约，否决。
- **success 列名用「成功率」**：与列值 `正常/失败` 语义冲突，且会让用户误以为该列是百分比，否决（改取「状态」）。
- **在 π 面板保留大标题但改成中文**：清单要求删除该行（prism 卡片自带标题，两行标题重复），否决。
- **改 `titleLine` 的宽度公式给新表头腾地方**：实测「用量」(4 列) 比 "Usage"(5 列) 窄 1 列，`fill = 60-3-1-headW-3-descW-1` 与 `descMax` 都由 `DisplayWidth(head)` 推导，无需任何改动；若写成硬编码 dash 数反而会在下次改文案时漂移，否决。
- **顺带改 `internal/render/table.go` 的 `(no data)`**：那是 legacy 通用表格渲染器的空态（`models` 等命令仍在用），不在清单内，保留。

## 来源
- 任务书 [MARK-WORKER-PRISM-CN-TEXT]（A–E 清单）。
- 上游既有实现：`internal/usage/report.go`（`titleLine`/`groupDesc`/`reportColumns`/`formatGroupValue`）、`internal/planusage/report.go`（`windowTitle`）、`/root/.pi/agent/extensions/prism.ts`（`StaticReport.render`）。

---

## 追加（2026-09-22 晚，任务书 [MARK-WORKER-PRISM-GROUPDESC-NOSPACE]）

### 问题
上一轮中文化只把键名换成中文，标题描述仍沿用英文模板的半角空格：单键输出「按 模型 分组」、多键「按 模型, 供应商 分组」，中英混排且逗号是 ASCII `,`，中文语句里读起来像未排版完的英文。

### 改动
- `groupDesc` 从 `"按 " + join(labels, ", ") + " 分组"` 改为 `"按" + join(labels, "、") + "分组"`：单键「按模型分组」，多键「按流式、状态分组」「按模型、供应商分组」。未知键透传、空 `groupBy` 的「未分组」、`groupKeyLabels` 映射表与 `groupKeyLabel` 逻辑均未动。
- 连带测试：`report_test.go`（190 行前缀断言、234 行整卡精确串、563 行多键串）与 `handler_test.go`（405 行整卡精确串）的标题串同步；标题行的 `fill` dash 数由 `titleLine` 既有公式按新的 `DisplayWidth` 自动重算（单键 desc 由 12 列变 10 列，dash 多 2 条），公式本身零改动。
- 文档注释同步：`report.go` 顶部卡片示例的标题行。

### 涉及
- 模块：usage
- 实际输出样例（`./prism usage` / `./prism usage --by stream,success --since 7d`）：
  - `╭─ 用量 · 按模型分组 ──────────────────────────────────────╮`
  - `╭─ 用量 · 按流式、状态分组 ────────────────────────────────╮`
  - 每行仍恒 60 显示列（`assertCardWidth` / `DisplayWidth` 未放松）。

### 决策
- **只改文案，不动表格对齐**：`reportIndent`(2)、`colGap`(1)、`PadRight`/`PadLeft` 的列宽填充仍是列间间距，用于表格对齐而非文案，明确排除在本次改动外（清单第 2 条）。
- **沿用 `titleLine` 的宽度公式，不硬编码 dash 数**：`descMax`/`fill` 均由 `DisplayWidth(desc)` 推导，文案写死 dash 数会在下次改文案时再次漂移（与上一轮同一结论）。
- **不把顿号/空格决定扩散到 JSON / 列头**：`group_by` 入参与 JSON 键名仍是 ASCII（API 契约），列头 `title` 仍取 `groupKeyLabel` 的中文标签（本就无空格）。
- **门禁**：`go build -o prism ./cmd/prism`、`go vet ./...`、`go clean -testcache && go test ./...`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全过；未执行任何 git commit。

---

## 追加二（2026-09-22 晚，用户指令：多键分隔符由「、」改「/」）

### 问题
上一条追加把多键标题定成中文顿号（「按流式、状态分组」）。用户随后给出新指令「按流式/状态分组」，要求多键分隔符改用半角斜杠 `/`。用户指令优先于任务说明的顿号写法，故再次修正。

### 改动
- `groupDesc`：`strings.Join(labels, "、")` → `strings.Join(labels, "/")`。单键仍为「按模型分组」（无空格、无分隔符），多键现为「按流式/状态分组」「按模型/供应商/流式分组」。
- 连带测试：`internal/usage/report_test.go:563` 多键串由「按模型、供应商分组」改为「按模型/供应商分组」。单键相关的 190/234 行与 `handler_test.go:406` 整卡精确串不受影响（单键形态无分隔符，字节未变）。
- 文档注释：`groupDesc` 的 doc comment 改为说明 `/` 分隔，并引用本笔记。

### 涉及
- 模块：usage
- 实际输出样例：
  - `╭─ 用量 · 按模型分组 ──────────────────────────────────────╮`（单键，未变）
  - `╭─ 用量 · 按流式/状态分组 ─────────────────────────────────╮`（多键）
  - `╭─ 用量 · 按模型/供应商/流式分组 ──────────────────────────╮`（三键）
  - 每行仍恒 60 显示列（Python 按 CJK=2 复核全行 60，`assertCardWidth` 未放松）。

### 决策
- **只动多键分隔符，单键与表格对齐不动**：`reportIndent`、`colGap`、`PadRight/PadLeft`、`titleLine` 宽度公式全部未动；`/` 宽 1 列，`fill` dash 数由既有公式自动重算（两键 desc 17 列 → 31 条 dash）。
- **仍不扩散到其它输出**：`group_by` 入参、JSON 键名、列头 `title` 均保持 ASCII/中文标签原样，本次只影响卡片标题描述字符串。
- **斜杠是 ASCII 标点，与「中文排版」目标有张力**：这里按用户指令取斜杠（用户意图优先），不自行回退成顿号。
- **门禁**：`go build -o prism ./cmd/prism`、`go vet ./...`、`go clean -testcache && go test ./...`、`for s in scripts/test_*.sh; do bash "$s"; done`、`python3 scripts/test_generate_mcp_tools.py` 全过；未执行任何 git commit。
