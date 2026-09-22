---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: render, planusage, usage
---

# 宽字符截断补列 + 标题超限改「先截可变段再算 fill」

## 一句话结论
- `render.PadRight`/`PadLeft` 对「超宽字符串截断后差 1 列」补空格回到恰好 `w` 列（不动 `Truncate` 语义：超宽仍截 + `…`）；两张卡片标题行的「整行 Truncate」安全网删除，改为按优先级截断可变段（账号 → 窗口标签 → 服务名 / 分组描述）后用实测 `DisplayWidth` 重算 fill dash 数，标题行恒 60 列且右端 `╮` 完好。

## 背景
- reviewer 应修项：60 列卡片依赖「每个槽位宽恒定」这一契约，而 `Truncate` 只保证 **至多** `maxWidth`：省略号占 1 列，当截断预算停在奇数列时，下一个双宽字（中文/全角/emoji）放不进去，结果落在 `w-1` 列（`…` 已计入）。`PadRight`/`PadLeft` 原来把截断结果原样返回，超宽中文值就让槽位窄 1 列、右边框被顶歪（即「59 列坑」）。
- `cardTitleLine`（planusage）与 `titleLine`（usage）原来的安全网是 `DisplayWidth(line) > cardWidth` 时对**整行** `Truncate(line, cardWidth)`：这会裁掉右端 `╮`，且宽字符场景可能停在 59 列。可达路径：未知上游窗口名 ≥ 54 列时，账号先被清零、服务名预算为负（旧逻辑原样保留、不截也不删），`used > bodyMax` 使 fill 被钳到 1，整行顶到 73 列后被整行截断。

## 决策
- **补列不改截断**：`PadRight`/`PadLeft` 在 `DisplayWidth(s) > w` 分支先 `Truncate(s, w)`，再用 `topUpRight`/`topUpLeft` 的空格补到恰好 `w`。`PadLeft` 的补列放**左侧**，省略号仍紧贴右对齐边。`Truncate` 本身逐字未动（`TestTruncate` 的期望值全部不变），`PadRight`/`PadLeft` 非截断路径（含 ANSI、恰好等宽、`w<=0`）也逐字不变——ASCII 截断本来就落在 `w` 上，`topUp*` 直接返回原串。
- **标题先截可变段**：`cardTitleLine` 用 `shrink` 循环按优先级缩段，每轮用 `DisplayWidth` 重新测量：①账号（长账号是常见情形，给它留服务和窗口标签的量，没量就清零）；②窗口标签（账号没了之后，只有未知上游名才会超长，已知标签短而固定，给它服务名+账号剩下的量）；③服务名品牌（前两者都借不出量时，例如超长未知 provider key）。整行不再 `Truncate`。usage 侧 `titleLine` 只有分组描述一个可变段，保持「先 `Truncate(desc, descMax)` 再按实测 `DisplayWidth(desc)` 算 fill」，同样删掉整行安全网。
- 每轮重测的原值：某段截断少 1 列时下一轮会自然吸收（fill 由实测宽度导出：3 + body + 1 + fill + 1 = 60），不会因「以为用满了预算」而把右边框顶出 61 列。

## 测试锁死
- `internal/render/width_test.go`：`TestPadWideTruncationFillsToWidth`（中文/全角/emoji/混合 6 组输入 × w=1..60，断言 `PadRight`/`PadLeft` 都恰 `w` 列、省略号仍在、UTF-8 未被劈开）；`TestPadRightCJKTruncatePadsToWidth`（60 列预算塞 40 个中：`中×29 + "… "` 恰 60，`PadLeft` 镜像为 `" " + 中×29 + "…"`；恰好 60 列的串原样返回）；表驱动用例补 `cjk truncate odd budget` / `emoji truncate odd budget` 两条。既有断言（含 `TestTruncate`、`TestPadLeftPadRight` 的通用宽度断言）一条未放松。
- `internal/planusage/report_test.go`：`TestRenderCardsOverLongWindowNameTitle`（70 列 ASCII/CJK/混合/emoji 窗口名：标题恰 60、`╮` 在位、省略号在、fill 只余 dash、其余卡片行不受影响）；`TestRenderCardsCJKAccountTitleFill`（80 列中文账号：截断停在预算少 1 列，fill 按实测宽度重算，卡片仍 60 且 `· 5小时限额 ─` 在位）。
- `internal/usage/report_test.go`：`TestRenderUsageReportOverLongTitle`（超长 ASCII 键、CJK 键、9 个 group key、CJK 多键：标题恰 60、`╮` 在位、描述被截且 fill 只余 dash）。
- 反向验证：把 `cardTitleLine` 临时改回旧逻辑（两段缩 + 整行安全网），`TestRenderCardsOverLongWindowNameTitle` 立即失败于 `"╭─ Opus · www…"`（`╮` 被裁），确认新用例真的锁住修复而不是空断言。

## 被放弃的方案（必填）
- **改 `Truncate` 语义**（例如超宽时截到 `w-2` 再加 `…`，或省略号改成占 0 列）：会影响 `TestTruncate` 的全部既有期望与所有卡片的截断口径，属于改动截断契约而非修调用方，且 reviewer 明令「不改截断语义本身」。
- **保留整行安全网但改成截到 `w` 后手工补 `╮`**：补出来的 `╮` 会紧贴一个可能停在 `w-1` 的宽字符行，仍是补丁摞补丁；正确做法是让可变段先让位。
- **标题超限时直接丢掉服务名/窗口标签**：旧逻辑正是这么做的（服务名预算为负时保留全宽、任由整行被截），观感与信息量都更差；改为给窗口标签留量、服务名品牌最后才动。
- **给 `PadLeft` 的补列放右侧**：会破坏右对齐语义（省略号离右边框多 1 列），放左侧才与 `PadRight` 的非截断路径对称。

## 顺带订正的两处过时文档（上轮范围外观察，用户已确认 format=table 统一走卡片）
- `README.md` 端点表 `/admin/usage/summary` 行：删掉「single-line compact table suited to the Pi panel … one row per model/group, short headers (model / reqs / in tokens / cache / hit% / out tokens / cost …)」整套表述，改为「v0.31.0 起与 CLI `prism usage` 同源输出 60 列胶囊卡片（标题/汇总行/`├─` 规则线/明细行/`╰─` 底边框、命中率 10 格 ▰▱ 胶囊、超长值截断不溢出、每行恰 60 列）」，并明确 `format=json` 仍是默认且不受影响。
- `internal/usage/handler.go` `serveTable` 的 godoc：「The compact single-line table is the only layout」改为 v0.31.0 起 60 列胶囊卡片布局的描述（仅注释，无代码行为变化）。

## 明确未动
- `Truncate` 语义、`RenderTable`/`RenderTableAt`/`formatRemain`、JSON tag、耗尽整条纯红逻辑、usage 命中率 66.7%→7 格既有舍入、行宽恒 60 的既有断言、四轮其余改动。

## 来源
- 本轮 reviewer 应修项（宽字符截断宽度不足 + 两处过时文档）；AGENTS.md 决策留痕要求。
- 相关笔记：`.agents/notes/20260922-quota-capsule-bar.md`、`20260922-usage-capsule-report.md`、`20260922-card-symmetric-gutter.md`、`20260923-quota-tui-card-format.md`。
