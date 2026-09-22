---
status: superseded
superseded_by: 20260922-quota-capsule-bar.md
supersedes: ""
模块: planusage, render
---

# 配额报告改卡片式 TUI（RenderTable → RenderCards）

## 一句话结论
- 新增 `RenderCards` 卡片式渲染器替换 `prism quota` CLI 的纯文本表格输出；`RenderTable`/`RenderTableAt` 函数体逐字未动，`/admin/quota?format=table` 与 JSON 行为不变。

## 背景
- `prism quota` 原输出 ASCII 纯文本表格，无颜色、无进度条、无卡片。
- 任务要求一比一复刻卡片式 TUI 看板：固定宽度、Unicode 圆角边框、多列网格、ANSI 真彩色、进度条、耗尽警告行。

## 几何（以 DisplayWidth 去色实测为准，每行恒定 60 列）
- 标题行：`Dim("╭─ ") + Brand(服务名) + " " + Dim(账号[+旧]) + Dim(" " + ─*fill + "╮")`。每个标题元素后一空格，补线**只在标题右侧**；账号超长先截账号、服务名超长再截服务名（Truncate 带 …），行宽守恒 60。
- 数据行：`Dim("│ ") + col1(8) + 2sp + col2(7) + 2sp + col3(8) + 2sp + col4(7) + 2sp + bar(18) + Dim(" │")` = 2+56+2 = 60。bar 全长 18 格（曾经试过缩到 15+3 gutter，主代理要求不动长度，改走高度方案，见下）。
- col1 只放窗口标签（`windowLabel`），**账号只出现在标题**——长账号永不截断指标列。
- col3 右对齐；`$1010.00`（8 列）与百分比共用。中间空行/注释行统一切到 56 内宽。底边 `╰`+58`─`+`╯`。
- 边框与补线全部 `Dim`（#666666，规格 #333~#666 区间内）。

## 颜色（ANSI 24-bit，internal/render/color.go）
- 服务名 Brand 青 #00B4D8；剩余配额段 Green #52B788；耗尽 Red #E63946；警告 Yellow #F4A261（`! limit reached` 行）；辅助/边框 Dim #666。
- **规格色板没有独立橙色档**：首版曾按 ≥80% 上橙色，双审判定非规格，已删除 `render.Orange`（无其他调用方）。

## col2 标签语义（主代理实机验收时纠正）
- **prism 的 `Percent` 是占用（已用）比例，不是剩余比例**（对应表格的"占用"列）。首版照抄英文看板标 `remains`，主代理验收时指出语义错误，定稿：默认标签 **`已用`**；`used up`→`已耗尽`、`credits`→`额度`、`rate-limited`→`限流`（沿用 statusLabel 既有译法）。`! limit reached` 是规格写死的英文原文，保留。

## 进度条与耗尽（同一判定驱动）
- `windowExhausted(w)`：`Percent>=100 || Status=="used up" || Status=="rate-limited"`。
- 耗尽：整行 18 格红色点线 `┈`（#E63946），无 █/░/▄。
- 正常：**填充长度=已用%**——已用段（左，随 `已用%` 增长）= 绿色 `█` 块；剩余格（右）= **空白**（不画轨道）。填充数 `ceil(已用%*18/100)`；`UsedFraction>0` 时以其提精度（该字段本就是为防 int 地板丢小数周而设，见 types.go 注释）。
- **曾犯的三处错误（均为实机验收时主代理指出，逐一记录）**：①早版把绿块画在"剩余"侧（0% 满绿、98% 空条），与 `已用%` 反向——百分比是已用，填充必须随已用涨；②多窗口 bar 上下紧贴——缩短长度加 gutter 被否（x 轴不动）；③改用"细线轨道"方案后，剩余段仍铺满全长，0% 与 98% 两条 bar 等长，看着一样——轨道死路，最终方案就是空白未用格，长度即数值。注意：单纯加 bar 高度不会产生行间空隙，要空隙仍需行间距。
- col2 与耗尽判定对齐：used up→`已耗尽`、rate-limited→`限流`、estimated→`额度`、其余 `已用`。限流窗口绝不显示 `remains/剩余`（已无此标签）。

## 倒计时（col4）
- 有 `ResetsAt` 才渲染，分钟补零（`2h01m`/`4h51m`，规格示例格式）。
- `cardCountdown` 为卡片专用；`formatRemain` 逐字未动（ legacy 表格输出不许变）。

## 有意省略（无模型字段支撑，不臆造数据）
- 循环圆点 `○○●●●●`：`UsedFraction` 无周期语义，首版据此画点属臆造且吞掉了 `ResetsAt` 倒计时，已删。
- 菱形周期标签 `◆ 21d ◆`：`PeriodStart`/`ResetsAt` 推不出严谨账期长度。
- 套餐类型：模型无此字段，标题省略。
- 美分精度：`LimitUSDEstimate` 是 int 美元，降级渲染 `$%d.00`；要 `$1010.52` 须加 JSON 字段，属下阶段。

## JSON / 兼容红线
- `WriteJSON`、`Response`、`handler.go`、`types.go` JSON tag、`go.mod` 零改动；`format=table` 仍走 `RenderTable`。

## 返工记录（双审）
- 双 reviewer（grok-4.7×2 独立）结论一致：不能收口，6 致命 + 2 应修。致命项：行宽不稳（62/63/4/62/60）且标题按错前缀宽、标题整段 Brand 且左右补线含 `╶`、账号截进 col1、进度条形/色不符且覆盖空段、警告色 #FFC832 错值且边框未上灰、`UsedFraction` 臆造圆点吞倒计时、测试只断言子串属假绿。已全部原地修复（本文件前述几何/颜色/判定即为修复后形态），测试改为逐行宽度 60 + 分色 + 耗尽/限流/长标题截断真守护。

## 被放弃的方案（必填）
- **改用 bubbletea/rich/ink**：禁止，引入新依赖违反技术纪律。
- **与 `RenderTable` 合并**：`RenderTable` 逐字保留，`format=table` HTTP 路径零风险。
- **为 `LimitUSDEstimate` 添加 int64 美分字段**：违背不改 JSON 字段红线；降级渲染 + 本笔记记录缺口。
- **≥80% 橙色警示档**：规格色板无此档，双审判定后删除。

## 来源
- 任务书 [MARK-QUOTA-TUI-001]；审查任务书 [MARK-QUOTA-REVIEW-001]；AGENTS.md 约束文档。
- `internal/render`（`DisplayWidth`/`StripANSI`/`Truncate`）作为 ANSI 对齐基础。
