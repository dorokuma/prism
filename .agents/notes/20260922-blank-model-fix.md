# Blank-Model Audit & Display Fix (Rework Round 2)

**日期**: 2026-09-22  
**触发**: 任务 `[MARK-WORKER-PRISM-BLANKMODEL-FIX-R2]`  
**涉及模块**: `cmd/prism`, `internal/proxy`, `internal/usage`, `internal/middleware`

## 问题

`prism usage` 模型表格出现空模型名行（62 条，数字与 agy-usage.db 中 model=NULL/空串的记录吻合）。原因是 agy sidecar 解析时 `parseModel` 返回空串，`parseGeneration` 不丢弃空 model 记录；进入汇总表后空串作为 group key 被 append 成独立行。

此外，代理侧在多种早退路径（413/400 body 读取失败、missing_model、missing_provider、no_accounts 等）向 audit 写入空 model 字符串，导致将来即使修复了解析侧，仍然可能产生空 model 记录。

## 决策

### A. 模型表过滤空 model 行

**位置**: `internal/usage` 包下沉了 `FilterBlankModelRows` helper。`cmd/prism/usage.go` 的 render 函数与 `internal/usage/handler.go` 的 `serveTable`/JSON 路径均在 MergeSummaryRows 之后调用。

**理由**:
- 不在 `internal/agyusage` 的 SQL Query 中过滤：会导致 `--by day`/`--by hour` 等视图丢失这些请求的日期时间维度计数。
- 不在 `internal/usage/agy.go` 的 `MergeSummaryRows` 中过滤：该函数自身不做展示过滤；若在其中无条件过滤，会伤到 `--by day`/`--by hour` 等非 model 视图，因此过滤由调用方按 GroupBy 条件决定。
- 在 `internal/usage` 包下沉 helper，由 CLI 与 HTTP handler 根据 GroupBy 条件决定是否调用：只在 GroupBy 为且仅为 `["model"]` 时执行过滤，其他分组视图保留 agy 行的完整计数。
- 过滤基于拷贝后的切片（`make([]SummaryRow, 0, len(rows))`），不再使用 `rows[:0]` 原地压缩，避免未来复用踩脏底层数组。
- **关键行为**：总览（`AddOverview`）始终使用未过滤的 agy extra。过滤只作用于喂给表格的那份数据。因此 `--by model` 视图顶栏总览仍包含空 model 请求计数，表内行数少于总览计数，差额 = usage_events 历史空 model + agy 空 model 两部分之和（运维按「表合计 vs 总览」对账时须知）。`--by day` 等视图不过滤，顶栏与表一致。

**对视图的影响**:

| 分组视图 | 影响 |
|---|---|
| `--by model`（默认） | 空 model 行被过滤；请求数、token 数合计与顶栏总览存在差异（差额 = usage_events 历史空 model + agy 空 model 两部分之和） |
| `--by provider` | 无影响；agy 行的 provider 恒为 `"gemini"`，不为空 |
| `--by day` / `--by hour` | 无影响；时间维度非空 |
| `--by account` | 无影响；agy 行的 account 恒为 `"agy-local"` |
| `--by key_id` | 无影响（且 agy 查询在 key_id 过滤下被 skipAgy 短路，不返回数据） |

### B. 记账侧占位值

**占位值**: `<unknown>`  
**选择理由**:
- `<` 和 `>` 在 OpenAI 模型名（如 `gpt-4`）和 Anthropic 模型名（如 `claude-3-5-sonnet`）中均不使用。
- `<unknown>` 不是任何已注册的真实模型名，不可能与真实 model 冲突。
- 保持可读性，运维排查 audit 时一眼可见。

**兜底位置**: `internal/middleware/logging.go` 的 `EmitAudit` 入口。理由：该函数是所有 audit 落库的唯一公共入口（rejectAudit 与 proxyChatWithBody defer 均调用它），在此做统一 choke 可覆盖所有调用方，避免未来新增路径时遗漏。最终方案为 EmitAudit 单点 choke；过程中试过的 per-caller 赋值路线已废弃。创建 audit 时（proxy_chat.go 约 :257-261）记的是 remap 前的 opts.Model；remap 后的名字由约 :297 的 `opts.Model = res.normalizedModel; aud.Model = opts.Model` 写下，该行必须保留（删了它 remap 审计会退回虚拟名）。废弃的只是 proxyChatWithBody 函数内 4 处早退分支的 `aud.Model = opts.Model` 赋值路线；readRequestBody 与 proxy_responses.go 转换失败路径在调用点直接传 `blankModelPlaceholder`（与 choke 重复但无害）。

**修改路径**:

1. **`internal/middleware/logging.go` — `EmitAudit`**：在 `a.KeyID` 兜底之后、cost 计算之前增加 `a.Model` 空/空白检查：`strings.TrimSpace(a.Model) == ""` 时写 `<unknown>`。覆盖 rejectAudit 与 proxyChatWithBody defer 两条生产写入路径。

2. **`internal/proxy/proxy_chat.go` — `readRequestBody`**（413/400 分支）：body 读取失败时无法解析 model，写入 `blankModelPlaceholder` (`"<unknown>"`)。

3. **`internal/proxy/proxy_chat.go` — `proxyChatWithBody` aggregate remap 分支**：保留 `aud.Model = opts.Model`（创建 audit 时记 remap 前的 opts.Model，remap 后的名字由 `opts.Model = res.normalizedModel; aud.Model = opts.Model` 写下）。第一轮曾尝试过的 no_accounts、select 失败、client_disconnect、upstream_connection_failed 等早退分支的 4 处 `aud.Model = opts.Model` 赋值路线已废弃（choke 落地后彻底多余，最终代码里不存在）。

4. **`internal/proxy/proxy_responses.go` — conversion 失败路径**：`virtualModel` 来自 `util.RawStringField(raw, "model")`，可能为空。新增 `auditModel` 变量：非空时用原值，空时替换为 `blankModelPlaceholder`。`rejectAudit` 使用 `auditModel`。`proxyChatWithBody` 调用仍使用原始 `virtualModel`，保持现有失败语义不变。

## 用户决策

- 2026-09-22：JSON `group_by=model` 不增加不过滤的 totals 字段；需要全量口径时以顶栏总览（Overview）为准。

## 过滤口径说明（问题 3）

过滤 helper 下沉到 `internal/usage` 包后，CLI 与 HTTP handler 共用同一实现。口径为：**合并后**的 rows 执行过滤（在 helper 内对 merge 结果统一过滤），以确保无论 rows 来自 usage_events 还是 agy extra，只要最终 group key 为空白模型名即被剔除。这样即使将来 usage_events 历史路径出现空 model（问题 2 落地前存在此可能），模型表也不会出现空行。

当前输出口径差异：CLI `--json` 输出自带未过滤的 overview 顶栏；HTTP JSON 只返回 rows（无顶栏、无 totals），需要全量口径时以 CLI 总览或 `/admin` 表格顶栏为准。

## 遗留

- 空 model 记录在 `internal/agyusage` 解析层仍然被写入 `agy-usage.db`（`replaceConversation` 不丢弃空 model 行）。本次任务范围只针对展示层过滤和 audit 侧兜底；如需彻底清除，应在 `parseGeneration` 或入库时丢弃空 model 记录。
- `SumTokens`（Gemini 周限额查询）明确包含 `model = ''` 行：`WHERE model = '' OR LOWER(model) LIKE 'gemini-%'`。如果后续在解析层丢弃空 model 行，需同步调整该 SQL 条件，否则周限额会略低于实际值（差异量 = 被丢弃的空 model token 数）。
- `--by model,day` 复合分组下空 model 单元格仍会出现（本次不扩权修）；`<unknown>` 在 audit 层视作保留占位名（理论上客户端可传同名虚拟模型与之合桶，已知晓并接受）。
