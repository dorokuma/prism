---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: planusage, cmd/prism
---

# quota 倒计时文案去掉「后重置」后缀，provider 展示序改为 Gemini < SuperGrok < ClinePass

## 一句话结论
- quota 卡片的倒计时文案去掉「后重置」后缀（`3h 45m 后重置` → `3h 45m`），provider 展示序由 `gemini < clinepass < xai` 改为 `gemini < xai < clinepass`（Gemini < SuperGrok < ClinePass）。
- 两处都只动**渲染层**：`internal/planusage/report.go` 的 `resetText`（`cardCountdown(now, *w.ResetsAt) + " 后重置"` → `cardCountdown(now, *w.ResetsAt)`）与 `internal/planusage/order.go` 的 `providerDisplayOrder`（`{"gemini","clinepass","xai"}` → `{"gemini","xai","clinepass"}`）。
- 行宽不变量不变：指标段仍 13 列（`clineNumberWidth`）、`PadLeft` 右对齐，每张卡仍 58 列（9 列名字下限 + 不截断长账号名）。`已重置` 与 `-` 两个伴随态本来就没有后缀，逐字不变。

## 背景
- 指标段只有 **13 列**（`clineNumberWidth = 13`，见 `report.go:294`；`clineRowFixed = 1 + 4 + 23 + 4 + 13 = 45`），而 `后重置` 连同其前导空格占 **7 列**（宽字符口径：`后重置` 3 个汉字 = 6 列 + 1 空格），即指标段 **过半宽度花在一个零信息的词上**。
- 该词零信息是结构性的：指标段是「智能单指标」，**不是 token 对时按定义就是倒计时**（`clineMetricField`：`clineTokenPairWindow(w.Name)` 为真才显示 `已用量/总量`，否则回落到 `clineCountdownField`，`report.go:630-647`）；既然该列只有「token 对」与「倒计时」两种形态、且形态可由列内容与窗口名区分，「后重置」不增加任何可区分信息。
- 两个伴随态本就无后缀：过期 → `已重置`（`resetText` 的 `!w.ResetsAt.After(now)` 分支），无 `ResetsAt` / 零值 → `-`（`clineCountdownField` 返回空串、由 `clineMetricField` 回落）。带后缀的只有「未来重置」这一态，同一列因此有两种尾部形态，视觉上反而不齐。
- 展示序侧：账号名 `ClinePass` 按字典序排在 `Gemini` 之前（C < G），旧序 `gemini < clinepass < xai` 是把 ClinePass 顶到 SuperGrok 之前的办法；现按用户要求改为 SuperGrok 在前。

## 决策
- **`resetText` 去掉后缀**（`report.go:869`）：未来重置态返回裸倒计时 `cardCountdown(now, *w.ResetsAt)`，例如 `3h 45m` / `42m` / `12h`。`cardCountdown` 的时间格式（`2h 01m` / `3d 4h`）不动，`已重置` / `-` 两态不动。
- **去掉后缀而非加宽列**：13 列对裸倒计时绰绰有余（5 小时窗口上界 `4h 59m` = 6 列），`PadLeft` 的裁剪保护也随之从「理论上不可能触发」变得更宽松，行宽不变量与测试的 `cardLines` 断言均无需放宽。
- **`resetText` 是两条路径的公共文案源**：① 5 小时窗口的指标段；② 周/月「无 token 总额」的回落倒计时（`clineTokenPairWindow` 为真但 `total <= 0` 且无 `MeasuredTokens` 时，`clineMetricField` 走倒计时兜底）。因此**同时**给两处去后缀——本次不按窗口分叉，回落路径的倒计时一并变形（Gemini 周限额卡在无总额时肉眼可见）；无窗口失败快照（`clineRowLine` 的 `r.win == nil` 分支）的指标段仍为空，不受影响。这与 v0.36.0 定下的「三家 provider 一套模板、版式不按 provider/窗口分叉」一致（见 `2026-10-03-unified-quota-cards-spec.md`）。
- **`providerDisplayOrder` 改为 `{"gemini","xai","clinepass"}`**（`order.go:18`）：Gemini 保持首位，SuperGrok 升至第二，ClinePass 落到末位。**未列出的 provider 仍共享尾 rank 回退字典序**（`providerDisplayRank` 的既有语义，rank = `len(providerDisplayOrder)` = 3），同一 provider 内仍按首账号名 tie-break，模块不交错——即「未列出 provider 不得插到精选 provider 之前」这条 v0.32.0 定下的展示契约不变。
- **该顺序被两个渲染入口共用**：`accountSortKey` 是排序键的唯一实现，`RenderCards`（CLI 卡片与 `GET /admin/quota` 的卡片视图）与 legacy `?format=table`（`RenderTableAt`）都消费它。因此**表格的行序也随本次改变**；表格的**列文本与列格式逐字未动**（`formatRemain` 等 legacy 格式未触碰）。
- **程序化接口不变**：`Snapshot`/`Window` 模型、`/admin/quota` JSON 的字段与数组顺序来源、`/metrics`、CLI flag 全部未动；本次只改渲染层的拼接与排序常量。
- **测试同步**（只改断言，不新增用例）：`report_test.go` 各 `rowTail`/`resetText` 断言改裸倒计时；`TestRenderCardsLocalizedCardText` 的 `bad` 禁止词列表追加 `"后重置"`；`order_test.go` 的排序断言改 `gemini < xai < clinepass`；`cmd/prism/metapi_test.go` 的 `2h 后重置` 断言改 `2h │`（**仅测试断言**，未触碰 `cmd/prism` 任何生产代码）。
- **门禁**：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`scripts/test_*.sh` 全跑、`python3 scripts/test_generate_mcp_tools.py` 全绿；`gofmt -l` 对本次触碰文件无输出（仓库整体另有 4 个存量未格式化文件，与本笔无关）。

## 与既有笔记的取代范围（部分取代）
本笔按仓库既有「部分取代」先例处理（先例见 `20261003-quota-card-uniform-width.md` 的「遗留清单」节：旧笔记保持 `status: active`、`superseded_by: ""`，由新笔记正文说明取代了哪一部分、哪一部分仍成立；本篇 frontmatter 的 `supersedes` 因此保持 `""`，**不整篇标 superseded**）：
- `20260924-quota-card-cn-text.md`：其**「后重置」文案定案**（`3h 12m 后重置` / `已重置`）中的**后缀部分被本笔取代**，倒计时形态自本笔起为裸 `3h 12m`；该笔记的**错误码中文化**（`cardErrorNote`：`未授权`/`无订阅`/`上游状态异常`/`拉取失败`）与 `已重置` 文案**仍 active 且未改动**。
- `20260924-clinepass-quota-display-order.md`：其**展示序结论**（Gemini < ClinePass < SuperGrok、`providerDisplayOrder` 为卡片/表格顺序的唯一实现、未列出 provider 回退字典序、`accountSortKey` 的 rank 前缀 + 首账号名）中的**排序值部分被本笔取代**（ClinePass 与 SuperGrok 互换），其余**机制层面结论全部仍成立**（唯一实现、两个入口共用、尾 rank 回退、同 provider 不交错）。
- `2026-10-03-unified-quota-cards-spec.md`：其 **D3「倒计时文案沿用既有 `resetText`（`cardCountdown` + ` 后重置`）」与同节实拍里的 `19% 1h 58m 后重置`** 被本笔取代；该节的**回落矩阵、13 列指标段、卡宽公式、分组与几何、删除清单**全部仍成立。该笔记同时是「三家统一渲染、版式不按 provider/窗口分叉」这条规范（本笔拒绝方案一的依据）的出处。

## 被放弃的方案（必填）
- **① 按窗口名加条件分支、只让 5 小时行去后缀**（周/月回落路径保留 `后重置`）：与 v0.36.0 定下的**「三家 provider 统一渲染、版式不按 provider/窗口分叉」**规范相悖（见 `2026-10-03-unified-quota-cards-spec.md`）；且回落路径的倒计时与 5 小时行的倒计时是同一列同一种语义，分叉只会让同一列出现两种尾部形态，还得为分支加测试与注释。否决。
- **② 保留「后重置」后缀**：该词占掉 13 列中的 7 列，而指标段「不是 token 对即为倒计时」这一语义已由列的形态本身体现（`已重置`/`-` 两态也从无后缀），后缀是纯冗余；保留等于长期让指标段过半宽度不可用于数字。否决。
- **③ 把指标列加宽以容纳后缀**（放宽 `clineNumberWidth` 13 或给带后缀的行单独留宽）：会破坏「同一 render 的所有卡等宽」不变量（`cardLines` 逐行断言同一宽度）或让卡宽因窗口/状态而异；且解决的是「后缀看不下」而不是「后缀无信息」这个真问题。否决。

## 来源
- 审查裁定：reviewer 应修项（按 `AGENTS.md` 在 `.agents/notes/` 留痕并跑 `scripts/notes-index.sh`）与 oracle 第二意见（判定可提交、须补笔记留痕 + README/Changelog + 门禁，版本级 = `v0.36.2` patch）；建议项（`TestRenderCardsLocalizedCardText` 的 `bad` 列表补 `"后重置"`）一并落地。
- 用户要求：倒计时文案去「后重置」后缀、provider 展示序改为 Gemini < SuperGrok < ClinePass，同车发 `v0.36.2`。
- 相关实现：`internal/planusage/report.go`（`resetText` / `clineCountdownField` / `clineMetricField` / `clineNumberWidth` / `accountSortKey` 注释）、`internal/planusage/order.go`（`providerDisplayOrder` / `providerDisplayRank`）、`internal/planusage/report_test.go`、`internal/planusage/order_test.go`、`cmd/prism/metapi_test.go`（仅断言）。
- 相关笔记：`2026-10-03-unified-quota-cards-spec.md`（单行式规范 / 回落矩阵 / 统一渲染约束）、`20260924-quota-card-cn-text.md`（被部分取代）、`20260924-clinepass-quota-display-order.md`（被部分取代）、`20261003-quota-card-uniform-width.md`（58 列宽与「部分取代」先例）。
