---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: cache
---

# cache 并发合并用例偶发失败：轮询 inflight 表证明不了 follower 已 join，改用真 join 栅栏加固

## 一句话结论
- `internal/cache` 的两个「同 provider 并发合并成一次上游请求」用例（`TestFetchWithContext_SharedResultAcrossCallers`、`TestFetch_ConcurrentSameProviderSingleUpstream`）的偶发失败是**测试同步缺陷**，不是产品 bug：它们用轮询 `fetches` 表 / 启动计数来等 follower 挂上 leader，而这两者都证明不了 join 时刻。
- 已用**真 join 栅栏**加固：`ModelCache` 新增测试用 hook `fetchJoinHook func(provider string)`，follower 在 park 到 leader 的 `done` 之前调用它（nil 时完全 no-op，生产零行为变化），测试改为挂在真 join 事件上同步。**未改任何产品语义**，`hits == 1` 断言**未放宽**。
- 加固后压测 **0 失败**：12 进程 × `-test.count=400`（9600 次执行）与 CPU 饱和单进程 `-count=20000`（40000 次执行）全绿；`-race` 同配方 0 失败且无 DATA RACE。变异自证：把测试同步退回原轮询启发式后，同压测复现失败 113/9600 ≈ 1.2%（饱和配方 293/40000 ≈ 0.73%）。

## 背景
- **现象与失败率**：`go test` 偶发失败，仅命中并发合并用例，断言是 `upstream hits = N, want 1`。原调查报告：12 进程并发压测 **93/4800 ≈ 1.9%**；CPU 打满单跑 2 万轮 **259 次**；`-race` 无数据竞态。`models_test.go` 的 `TestFetch_ConcurrentSameProviderSingleUpstream`（原 `:3429`，轮询 `started` 计数于 `:3481`）有**同款窗口**（19200 轮失败 35 次）。
- **根因（测试同步缺陷）**：`fetchctx_test.go:164-179` 那段用 `mc.fetchMu` 轮询 `mc.fetches["p"]`，等到条目存在就 `close(release)`，注释声称这「证明 follower 已挂上 leader」。但**那个条目是 leader 自己**在 `fetchWait` 里注册的（follower 从不写 `fetches`），所以轮询到它**只能证明 leader 已注册**，证明不了 follower 已 join。
- **失败的因果链**：轮询一命中（= leader 刚注册）就 `close(release)`；此时 follower 的 goroutine 可能还没被调度到 `fetchMu` 那次 lookup。leader 读上游返回、更新内存缓存、`delete(fetches, provider)`、`close(f.done)` 全部完成后，follower 才终于跑到 lookup —— 表已空，于是按设计**自己开新一轮上游拉取**，`hits` 变 2。follower 只要晚 ~150µs 被调度即触发（上游是本地 httptest，leader 一轮极快）。
- **兄弟用例**：`TestFetch_ConcurrentSameProviderSingleUpstream` 用「`started` 计数 == 20」当作「都已被启动 ⇒ 都只能 lead 或 join」，同病——`started` 在 `mc.Fetch` **之前**自增，goroutine 可以在自增后被抢占、尚未 lookup 就遇到 leader 完成清理，于是成为第二个 leader。两处都是「用与 join 无关的信号近似 join」。
- 事件密度与并发压力相关，故单跑不复现、并发进程数/CPU 打满时放大。

## 决策
- **加真 join 栅栏（生产文件 +9 行，零行为）**：
  - `internal/cache/models.go` 的 `ModelCache` 新增字段 `fetchJoinHook func(provider string)`，命名与注释风格对齐既有先例 `fetchLeaderHook`（同为测试专用、nil = 生产）。
  - 调用点在 `fetchWait` 的 **follower 分支**：`mc.fetchMu.Unlock()` 之后、`select { case <-f.done: ... }` **之前**调用 `if h := mc.fetchJoinHook; h != nil { h(provider) }`。此处 follower **已捕获 leader 的 `*inflightFetch` 指针**，无论 leader 之后是否完成并 `delete` 条目，它都只会 park 到 `f.done` 并返回 leader 的结果 —— 所以 hook 一投递就**确证**了「这是一个 joiner，不会另开一轮」。
  - **nil 时零行为**：`if h := ...; h != nil` 在字段为 nil（生产构造路径 `New` 与所有未设置该字段的测试字面量）时是直接的 no-op 分支，不读表、不写状态、不改 leader 注册/清理顺序、不加宽限窗口、不改 `f.err`/`close(done)` 的既有语义。生产 `New` 不设置该字段，等价于不存在这段。
- **测试同步重写为确定性等待（不改断言）**：
  - `fetchctx_test.go`：`fetchCtxMC` 建好 cache 后设 `joined := make(chan string, 1)` 与 `mc.fetchJoinHook = func(p string){ joined <- p }`；起 follower goroutine 后 `select` 等到 `joined`（真 join 事件）再 `close(release)`。**删除**轮询 `mc.fetches` 的循环，**无 sleep**，`hits == 1` 断言原样保留。
  - `models_test.go`：同一 hook 把「等 20 个并发调用者都 join 完」变成**确定性等待** —— `joined := make(chan string, n)`（n=20），hook 投递 provider，测试收满 **19** 个（恰好 1 个 leader + 19 个 follower）再 `close(release)`；**删除** `started` 计数与轮询，`hits == 1` 断言原样保留。leader 在此期间 blocked 在上游，条目不可能被清理。
- **不新增用例/文件**：本笔是加固既有用例、不是新增逻辑分支，**未新增任何 `func Test`、未新增测试文件**（`^func Test` 计数：改动前后全仓 1352，`internal/cache` 各文件计数逐项不变）。
- **门禁与取证**：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`scripts/test_*.sh`（5 个脚本全 OK）、`python3 scripts/test_generate_mcp_tools.py` 全绿，`gofmt -l` 对三个触碰文件无输出。

## 被放弃的方案（必填）
- **① 给「已完成但尚未被消费」的 inflight 条目加宽限窗口**（leader 完成后保留条目一小段时间，让迟到的 follower 仍能 join 到已完成的 `f.done`）：**改产品语义**，需要配套的条目回收机制（否则宽限窗口会变成无界缓存/内存泄漏面），并与 `TestFetch_LeaderPanicCleansInflightEntry` 锁定的「leader 退出即清条目」**契约冲突**；收益仅是「迟到的 follower 少发一次 µs 级本地 `GET /v1/models`」，为极小收益动生产并发语义，否决。
- **② 纯测试侧「启动信号 + 小睡」**：例如记下「follower goroutine 已启动」再 `time.Sleep` 一小段。这**仍是启发式**：重载下调度延迟可任意长，小睡时长无法既保证正确又保证不拖慢测试；本次失败的 150µs 窗口正是「睡眠界不可能卡死」的活例。否决。
- **③ 直接放宽 `hits == 1` 断言**（改成 `hits >= 1` / `hits <= 2`）：这会**削弱用例要守护的契约**（「同 provider 并发调用合并成恰好一次上游请求」），把产品回归掩盖成测试通过。否决。
- **④ 只改 `fetchctx_test.go` 而不管兄弟用例**：`TestFetch_ConcurrentSameProviderSingleUpstream` 是同一根因（启动计数 ≠ join），漏掉即留一颗同款哑弹。一并收掉（本笔采用的方案内已含）。

## 复现配方
```bash
# 1) 编测试二进制（加固版或退回原启发式的变异版都可用）
go test -c -o /tmp/prism-cache.test ./internal/cache/
RUN='TestFetchWithContext_SharedResultAcrossCallers|TestFetch_ConcurrentSameProviderSingleUpstream'

# 2) 多进程并发：12 个并发进程 × 各 400 轮（× 2 个用例 = 9600 次执行）
for i in $(seq 1 12); do
  /tmp/prism-cache.test -test.run "$RUN" -test.count=400 >/tmp/proc-$i.log 2>&1 &
done; wait
grep -h '^--- FAIL' /tmp/proc-*.log | wc -l

# 3) CPU 饱和单进程：占满全部核后单跑 20000 轮（× 2 = 40000 次执行）
for i in $(seq 1 "$(nproc)"); do ( while :; do :; done ) & done
/tmp/prism-cache.test -test.run "$RUN" -test.count=20000
# 结束前 kill 掉上面的忙等进程

# 4) 可选 -race 同配方
go test -c -race -o /tmp/prism-cache-race.test ./internal/cache/
```
**实测对照**（同一配方，本机 12 vCPU）：
| 版本 | 多进程 12×400×2 | 饱和单跑 20000×2 | `-race` |
| --- | --- | --- | --- |
| 加固后 | **0/9600 失败** | **0/40000 失败** | 0 失败、无 DATA RACE |
| 退回原启发式（变异） | **113/9600 ≈ 1.2% 失败** | **293/40000 ≈ 0.73% 失败**（279 × `SharedResultAcrossCallers` + 14 × `ConcurrentSameProviderSingleUpstream`） | — |

## 遗留（未决 / 需后续处理）
- **残余风险 ≈ 0（本笔加固的正确性边界）**：栅栏触发点与 park 在同一代码路径上——在捕获 leader 的 `f` 指针之后、`<-f.done` 之前——因此 hook 一旦投递即**结构性地**保证该 caller 会 park 到 leader 的 `done`（它已持有该 `*inflightFetch`，不可能再另开一轮）。唯一残余形态是**极端调度饥饿**下 5s watchdog 超时，其失败签名是 `follower never joined…`（超时）而**非** `hits == 2`——两者可区分，不会把加固后的同步问题误诊为「合并契约被破坏」。
- **`internal/util` 的 `TestDebugDumpOmitsBusinessContentKeepsStructure` 用固定路径 `/tmp/prism-debug/...`**：多进程并发跑套件时会互相踩（多个测试二进制同时写同一目录），**单套运行不复现**。本笔**未修**——它不在本笔允许改动范围内（`internal/util` 属禁改），且与本次 cache 合并用例失败是两码事。后续如需并发跑套件，要么让该用例用 `t.TempDir()`/带 PID 的唯一路径（改测试），要么在 CI/压测里对 `internal/util` 串行化。待用户决策。
- **`models_test.go` 的用例在变异配置下用 `-test.count>` 复现率低于 `fetchctx_test.go`**（饱和配方 14 vs 279）：窗口更窄，属正常；加固后两者均 0 失败。

## 来源
- 派发单 `[MARK-PRISM-FLAKE-85]`：按调查结论（test 同步缺陷）修掉偶发失败，备 v0.36.4 代码 + 留痕 + README。
- 相关实现：`internal/cache/models.go`（`ModelCache.fetchJoinHook` 字段、`fetchWait` follower 分支调用点）、`internal/cache/fetchctx_test.go`（真 join 栅栏）、`internal/cache/models_test.go`（`TestFetch_ConcurrentSameProviderSingleUpstream` 真 join 栅栏，仅测试同步）。
- 相关既有先例：`internal/cache/models.go` 的 `fetchLeaderHook`（测试专用 hook、nil = 生产的先例）。
