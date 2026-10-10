---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: pool
---

# 漏唤醒修复收窄：`Release`/`transfer` 去掉 provider 过滤，`MarkHealthy` 保留「优先 + 兜底」两遍语义

## 一句话结论
- `internal/pool` 的漏唤醒修复**不做三个调用点一刀切**：`Release` 与
  `removeWaiterAndTransfer` 的唤醒扫描**不再按事件源 provider 过滤**（从队首唤醒第一个
  `capacityAvailableFor(w.provider, w.key, w.max)` 为真的等待者），这是消除漏唤醒所必需；
  但 `MarkHealthy` 改为**两遍扫描**——**优先遍**只找「刚恢复的那个账号本身能服务
  （`w.provider == "" || w.provider == a.Provider()`，且 `accountCanServe(a, w)`）」的等待者，
  **仅当优先遍一个都没找到时**才跑 **兜底遍**（与 `Release` 同语义的无过滤扫描）。
- 这样既**保留旧文档语义**（恢复事件优先服务刚恢复账号能服务的等待者），又**消除新引入的
  跨 provider 饿死**（恢复事件绝不因「事件源 provider 不匹配」而让一个自身已有可用容量的
  等待者继续停摆）。
- 实现形态（**方案 B 收窄后**）：`wakeNextUsable(prefer *Account)`；`prefer == nil` 表示不做优先遍
  （`Release`/`transfer` 传 `nil`，`MarkHealthy` 传**刚恢复的那个账号对象** `a`）。优先遍判据为
  `accountCanServe(prefer, w)` —— **刚恢复的那个账号本身**能服务该等待者，而不是「该 provider 下存在
  某个账号能服务它」。

## 背景
- `internal/pool` 的等待队列在混合 provider / 混合 model-key 场景下存在**漏唤醒**：唤醒信号
  被绑定到「事件源账号的 provider」而非「等待者自身能否选中」，于是一个**自己已有可用容量**的
  等待者可能一直停摆到 60s 的 `2*AccountSelectTimeout` 兜底（`config.AccountSelectTimeout =
  30s`，见 `internal/config/constants.go`）。
- 修复第一轮把 `Release`、`MarkHealthy`、`removeWaiterAndTransfer` 三个调用点的 provider
  过滤**全部删除**（`wakeNextUsable` 签名改为无参）。这修掉了漏唤醒，但 `MarkHealthy` 因此出现
  **未授权的行为变化**：它原有注释明写「the freshly-healthy account may only serve waiters
  whose provider/key/max fit」，一刀切删除后该语义消失且无测试覆盖，独立审计判为阻断项。
- 本次为**收窄返工**：只把 `MarkHealthy` 拉回「优先 + 兜底」两遍语义，其余两个调用点保持第一轮
  的无过滤形态。

## 决策
- **`Release` / `removeWaiterAndTransfer`：无过滤扫描**。二者是「容量被释放」事件，唤醒本质是
  队列重检信号、不是该槽位的定向交接；被唤醒者会重跑自己的
  `trySelectLocked(provider, key, max)`，可能落到**别的账号**（同 provider 的另一账号，或全池
  游标）。因此按释放账号的 provider 过滤会饿死一个自身 provider 一直有空闲容量的等待者——这就是
  本次要消除的**混合 provider 漏唤醒**。
- **`MarkHealthy`：两遍扫描**。
  - 优先遍：从队首找第一个**同时**满足 (a) 刚恢复账号能服务它（`w.provider == "" ||
    w.provider == a.Provider()`）与 (b) **刚恢复的那个账号本身**能服务它
    （`accountCanServe(a, w)` = `a.IsHealthy() && !a.IsInCooldown() && a.canAcquire(w.key, w.max)`）的
    等待者并唤醒。这一遍**等价于旧实现的定向意图**（恢复事件优先服务刚恢复账号能服务的等待者），但对
    `w.provider == ""` 的**全局等待者不跳过**，实现与旧过滤一致。注意旧实现判据用的是 provider 级
    存在性 `capacityAvailableFor(w.provider, ...)`（「该 provider 下**某个**账号能服务它」），本次收窄为
    「**这一个**账号能服务它」；`accountCanServe` 额外补上 `trySelectLocked` 会做的冷却/不健康跳过
    （`canAcquire` 只镜像 `TryAcquire` 的两道容量闸，不含健康/冷却闸）。
  - 兜底遍：**仅当优先遍没找到任何人**时，从队首唤醒第一个 `capacityAvailableFor(...)` 为真的
    等待者（与 `Release` 同语义，无过滤，**本次未改动**）。
  - 语义意图：恢复事件**优先**服务刚恢复账号能服务的等待者，但**绝不允许**因事件源 provider
    不匹配而让自身已有可用容量的等待者继续停摆。
  - **残留局限（存量，本次未修）**：优先遍只决定「唤醒谁」，**不把刚恢复账号的容量交接给被唤醒者**。
    被唤醒者随后跑自己的 `trySelectLocked(provider, key, max)`，仍从 `providerNextIdx` / `nextIdx`
    **游标**起转，**可能落到同 provider 的另一个账号**。因此本次把优先遍定向从「provider 级存在性」收紧到
    「该账号能服务它」，但**没有**做到「恢复的那一份容量必然被本次唤醒接住」；旧实现同样如此。
- 两遍共用一个唤醒原语 `wakeWaiter(elem, w)`（出队 + `active=false` + `close(ch)`），保证两遍
  在唤醒机制上不会分叉。
- 注释同步：`MarkHealthy` 内联注释与 `wakeNextUsable` 文档注释都按最终实现改写；
  `wakeNextUsable` 里把混合 provider 饿死补上前提——**全局等待者已被唤醒出队但尚未取到槽**这一
  窗口（一旦它取到槽就没有等待者被落下）。

## 被放弃的方案（必填）
- **三个调用点一刀切删除 provider 过滤（第一轮做法，已否决）**：修掉了漏唤醒，但顺手删掉了
  `MarkHealthy` 的既有语义（恢复事件只服务刚恢复账号能服务的等待者），属**未授权的行为变化**，
  且无测试覆盖。看似更简单统一，实为把「恢复事件的定向语义」和「容量释放的队列重检语义」混为一谈。
- **`Release` 也加优先遍（否决）**：`Release` 的语义是纯容量释放，没有「刚恢复账号」这一事件源
  概念；给它加优先遍会重新引入本次要消除的漏唤醒（优先遍挑不到时虽有兜底遍，但优先遍会先唤醒
  一个与该释放无关的等待者，改变既有 FIFO 语义并可能复现 flake）。
- **把 `MarkHealthy` 恢复为纯过滤扫描（否决）**：保留旧语义但重新引入跨 provider 饿死——即
  T2 测试所钉的缺口。
- **保留 provider 级 `capacityAvailableFor` 作优先遍判据（本次否决）**：该判据问的是「该 provider 下
  **存在某个**账号能服务该等待者」，于是一个「刚恢复账号服务不了、但同 provider 的另一个账号能服务」
  的队首等待者会吞掉恢复事件，把刚恢复账号**真正能服务**的等待者留在队里直到 60s 兜底。收窄为
  `accountCanServe(刚恢复账号, w)` 才对齐「恢复事件服务刚恢复账号」的语义。
- **按 provider 字符串反查而不传账号对象（本次否决）**：`MarkHealthy` 传 `a.Provider()` 时优先遍只能
  拿 provider 字符串去 `p.accounts` 里再找一个账号，既多一次反查、也无法表达「就是这一个刚恢复的账号」
  （同 provider 多账号时语义立即退化为 provider 级）。直接传 `*Account` 让判据无歧义，也不引入新加锁。
- **让优先遍与取槽共游标以强保证落到恢复账号（本次否决）**：若要让「恢复的那份容量必然被本次唤醒接住」，
  需要把唤醒与取槽绑定（例如把被唤醒者钉到刚恢复账号、或让优先遍直接交接槽位），这会改变
  `trySelectLocked` 的取槽/轮转语义、扩大改动面，并可能复现别的饿死；本次只在**不改取槽语义**的前提下
  收窄判据，残留局限单列（见「遗留清单」）。

## 测试
- 新增三条确定性测试（追加于既有 `internal/pool/waiter_provider_test.go`，未新建文件、未改动既有
  测试任何一行、无 `t.Skip`）：
  - `TestMarkHealthyPrefersRecoveredProviderWaiter`（T1，优先遍）：队列为 [Y 等待者, X 等待者]，
    出带外释放让队首 Y 等待者「自身可服务但仍停在队里」（模拟真实 `Release` 留下的
    wake→acquire 窗口），`MarkHealthy(x2)` 必须唤醒 X 等待者而非队首 Y 等待者。
  - `TestMarkHealthyFallbackWakesServableOtherProviderWaiter`（T2，兜底遍）：队列为两个 X 等待者，
    x1 容量被带外释放（自身可服务、仍在停），`MarkHealthy(y1)`（Y provider，服务不了任何 X
    等待者）必须经兜底遍唤醒队首 X 等待者。
  - `TestMarkHealthyWakesWaiterRecoveredAccountCanServe`（T3，优先遍的**账号级**判据）：队列为
    [等待者 A（X，key `m`）, 等待者 B（X，key `n`）]，x2 是恢复账号、其 `m` 已达 per-key 上限，x1 的
    `m` 被带外释放（A 因此**当前可服务——但只能由 x1**）；B 只能由 x2 服务（x1 的 `n` 已达 per-key
    上限）。provider 级判据会跳过 B 去唤醒 A（把 x2 的恢复浪费在 x2 服务不了的 A 上），账号级判据
    `accountCanServe(x2, B)` 才够到 B；断言**B 被唤醒且落到 x2**。
- **判别力实测（四态对照）**：把改动后的测试文件分别对上四种 `pool.go` 实现态各跑 T1/T2/T3：
  - 原始过滤实现（`git show 59ea8b9e…:internal/pool/pool.go`，sha256 前 8 位 `e1eb9b55`）：
    T1 **PASS**、T2 **FAIL**（2.00s 超时，Y 恢复事件无人可唤醒，X 等待者饿死）、T3 **FAIL**
    （2.00s 超时）。
  - 第一轮一刀切实现（sha256 前 8 位 `95bcdb29`）：T1 **FAIL**（2.00s 超时，队首 Y 等待者吞掉恢复
    事件）、T2 **PASS**、T3 **FAIL**（2.00s 超时）。
  - 改动前两遍 `capacityAvailableFor` 优先遍（sha256 前 8 位 `30b2d42d`）：T1 **PASS**、T2 **PASS**、
    T3 **FAIL**（2.00s 超时）。
  - 本次改动后（`accountCanServe` 优先遍）：T1 **PASS**、T2 **PASS**、T3 **PASS**。
  - 即 T1 钉「跳过不可服务的队首」、T2 钉「兜底遍不丢」、T3 钉「优先遍判据是账号级而非 provider 级」；
    T3 在改动前三种实现态**必然 FAIL**、改动后必然 PASS。复跑实测分两次、执行者不同：**worker 自测**
    ——pre 态 `-count=10` 10/10 FAIL、post 态 `-count=50` 0 FAIL；**verifier 独立复现**——pre 态
    `-count=10` 10/10 FAIL、post 态 `-count=10` 10/10 PASS。两次都是真实执行、方向一致，各自独立，
    不可读成同一次执行。T2 对第一轮实现也 PASS，因此其定位是把该行为固化为回归保护
    （防未来把兜底遍删掉），而**不是**复现第一轮的 bug。

## 遗留清单
本轮独立审计给出的 5 条遗留 + 本次收窄新增 1 条，逐条标注状态：

1. **新回归测试的构造方式与真实取槽窗口有差距**（仍挂着，**事实已改正**）。
   `TestReleaseMismatchedProviderWakesServableWaiter`（`waiter_provider_test.go:978+`）用「手工
   `PushFront` 一个 `waiter` 结构 + 带外 `Account.Release` 造出空闲容量」来模拟「已唤醒出队但尚未取槽」
   窗口，与真实 `Select` 取槽窗口有差距：测试**证明机制存在**，但**不证明真实场景稳定复现**（真实窗口
   的时序由调度决定，无法用真实等待者钉死）。需要的话后续可用真实等待者 + 注入点补强。
   **更正**：T1 `TestMarkHealthyPrefersRecoveredProviderWaiter` 与 T2
   `TestMarkHealthyFallbackWakesServableOtherProviderWaiter`（以及 T3
   `TestMarkHealthyWakesWaiterRecoveredAccountCanServe`）**并不**用手工 `PushFront`；它们用**真实
   `SelectByProvider` goroutine**（靠 `waitUntil(WaitingCount)` 钉住入队顺序）+ **账号级
   `TryAcquire`/`Release`** 把容量状态钉死。原文把三者混为一谈、并称手工 `PushFront`「只出现在
   `TestReleaseMismatchedProviderWakesServableWaiter`」，与事实不符。实测
   `internal/pool/waiter_provider_test.go` 中 `PushFront` 共 **6 处**，逐处行号与归属用例为：
   `:422` → `TestRemoveWaiterTransfersWokenWakeupOnBail`、`:467` →
   `TestRemoveWaiterNoTransferWhenCapacityGone`、`:521` →
   `TestRemoveWaiterNoTransferToNonMatchingProvider`、`:586` → `TestRemoveWaiterTransferMixedKey`、
   `:665` → `TestRemoveWaiterTransferSkipsUnusableMixedKey`、`:1020` →
   `TestReleaseMismatchedProviderWakesServableWaiter`。即手工 `PushFront` **并非只出现在**那一条用例，
   而是分布于上列 6 条；T1/T2/T3 确实一条都不用（它们走真实等待者路径，如上段所述）。
2. **基线压力对照的统计力不足**（仍挂着，**已补记独立进程复核**）。
   基线对照 `-count=200` 首轮即 panic 中止，第 2–200 轮未运行，**对照只有 1 个有效样本**；修复后
   200 轮零失败按 rule of three 仅给 ≈1.5% 失败率上界。即「修复有效」的统计证据是弱的。
   **补记（独立进程复核）**：对基线实现用**独立进程**重复复核，结果是 **30 个样本 0 次失败
   （0/30）**。这与先前「`-count=200` 首轮即 panic」**并不矛盾**——一次命中 vs 三十次未命中，只说明
   该失败**罕见**；但「基线一跑就炸」的说法**不成立**。据此写明：**`Release` 修复的主要证据是确定性
   用例，不是压力统计**；压力测试失败是罕见的，不能拿它当复现手段。
3. **`SetCooldown` 到期不主动唤醒**（仍挂着，非本次可修）。
   与本次同一设计思路的相邻缺口：信号绑定「事件源」而非绑定「等待者自身能否选中」。cooldown 是
   timer 路径（`selectKeyed` 里只为最近的 matching cooldown 起 timer，且不 `close(w.ch)`），
   timer 到期后等待者靠自身循环重试，但**没有**跨等待者的主动唤醒——一个等待者的 cooldown 到期
   不会唤醒另一个自身已有容量的等待者。属 timer 路径，本次范围外。
4. **`max <= 0` 时 `canAcquire` 与 `TryAcquire` 分叉**（存量，仍挂着）。
   `Account.TryAcquire` 对 `max <= 0` **直接返回 nil**（`internal/pool/account.go` 开头的
   `if max <= 0 { return nil }`），而 `Account.canAcquire` **没有这道门**（它只比较
   `InFlightForKey(key) >= max`，`max <= 0` 时该比较恒真而返回 false，但语义来源不同）。正常调用方
   不传 `max <= 0`，故未触发；属存量分叉，本次不改。
5. **`wakeNextUsable` 的容量检查与取槽之间存在 TOCTOU**（存量，仍挂着）。
   容量检查（`capacityAvailableFor` / `accountCanServe`）与等待者真正 `TryAcquire` 取槽之间存在窗口：
   并发的 `Select` 可能抢先取走该容量，被唤醒者于是 re-park 回队尾。属存量、低概率，靠队列重检自愈，
   本次不改。
6. **优先遍唤醒者仍可能按游标取到别的账号**（存量，仍挂着；本次收窄的残留局限）。
   优先遍用 `accountCanServe(刚恢复账号, w)` 判「该账号能服务它」，但被唤醒者随后跑自己的
   `trySelectLocked` 仍从 `providerNextIdx` / `nextIdx` 游标起转，可能先取走**同 provider 的另一个空闲
   账号**。即「恢复的那一份容量必然被本次唤醒接住」**不成立**；本次只把定向从 provider 级存在性收紧到
   账号级可服务性。要进一步收紧须改取槽语义（见「被放弃的方案」），本次范围外。
   **另有全局等待者一支（本轮实测补写，残留局限原先写窄了）**：优先遍**不因 provider 字符串跳过**全局
   等待者（`pool.go:295` 的过滤条件是 `w.provider != "" && w.provider != provider`；`:294` 是
   `w := elem.Value.(*waiter)`、`:296` 是 `continue`）。全局等待者与恢复 provider 的等待者**适用同一个
   `accountCanServe(prefer, w)` 谓词**，但**候选集不同**：同 provider 的等待者额外受 provider 过滤，全局
   等待者不受（两类都可被挑中，并非同权）。只要恢复账号能过它的 key/max 闸就会被优先遍挑中。但被唤醒者随后跑
   自己的 `trySelectLocked` 仍从 `nextIdx`（全池游标）起转，**可能取走游标前面另一个它也能用的空闲
   账号**，而恢复账号那一份容量没人接，队列里剩下的等待者继续 park。
   **交错（下列前提齐备时确定发生，前提不齐则不发生）**：池 `[x1, x2]`，`nextIdx` 停在 x1；占满 x1 的
   key `m`（`max=1`），`x2.MarkExhausted`；
   入队两个全局等待者 `G1(provider="", key=m)` 在前、`G2(provider="", key=n)` 在后；带外 `x1.Release(m)`
   （不走池扫描，游标仍在 x1）；`MarkHealthy(x2)` → 优先遍对 `G1` 调 `accountCanServe(x2, G1)`，x2 的 `m`
   从未 acquire、`inFlightByKey["m"]` 为 nil，`canAcquire`（`account.go:506`；`:510` 是 per-key 容量闸
   `if g != nil && int(g.Load()) >= max`，
   放行的 `return true` 在 `:516`）直接放行 → **唤醒 G1 并
   return**；G1 重跑 `trySelectLocked` 从 x1 起、`TryAcquire("m",1)` 成功 → **取走 x1**；x2 的 n 仍空闲、
   G2 继续 park。
   **定性**：这一支**不是本轮新引入**（旧 provider 级优先遍同样会唤醒该全局等待者），**本轮收窄也没
   覆盖它**；在**上述前提齐备**时它是确定发生，不是轮转边角——**前提不齐则不发生**：`G2` 排在 `G1` 前则
   唤醒 `G2` 并落到 x2（x2 的 n 被接住）；`nextIdx` 已停在 x2 则 `G1` 直接取 x2；`max > 1` 且 x1 仍有余量
   则 `G1` 在优先遍前根本不会 park；x2 的 `m` 已满则 `accountCanServe` 跳过 `G1`；另有 `totalCap > 0`
   且 x2 总量已满这一道闸（`account.go:513-515`，默认 `NewPool` 的 `totalCap=0` 下不触发）。
   生产路径里全局等待者真实存在（`Select` 传 `""`，
   `pool.go:424` → `selectKeyed(..., "")`）。**状态**：**已由用户裁定接受为已知残留**（不改代码、不改
   取槽语义，理由见「被放弃的方案」），本次只如实记录。
   该条同时保留上面「同 provider 另一个账号」的表述——两者都成立，不互相排斥。

**本轮补落盘的 consider / 观察项**（各一行；**不阻断、已记录**，本次不改代码）：

- `TestReleaseDoesNotWakeNonMatchingProvider`（`waiter_provider_test.go:175-211`）与
  `TestRemoveWaiterNoTransferToNonMatchingProvider`（`:492-537`）：名字/注释在新语义
  （`wakeNextUsable(nil)` 无 provider 过滤）下**名不副实**——新语义下「不匹配 provider 但自有容量」的
  等待者**应该**被唤醒，这两条只因**被断言的那个等待者自身无容量**才保持绿色，并未钉住旧不变量：第一条
  被断言不唤醒的是 **X waiter**（`:204`），第二条（`:492-537`）是 **Y waiter**（`:528`
  `expectNoSlotResult(t, chY, …, "Y waiter after X slot freed")`，其中 X waiter 是手工 `PushFront`
  后 bail 的那个）。属测试命名/注释债，本轮不动其断言。**不阻断、已记录**。
- 收窄的收益与代价（已确认）：减轻「同 provider 队首兄弟吞掉恢复事件」；未发现新的永久饿死；无唤醒
  风暴（两遍都是扫到第一个即 `return`，一次 `MarkHealthy` 最多关一个 channel）。**不阻断、已记录**。
- 已复核不成立的担忧（记一行即可）：`prefer == nil` 路径与旧 `""` 逐字等价、无 panic；`max <= 0` 分叉
  **未被本轮放大**；T3 是确定性判别测试且**未**把「A 不被唤醒」写成硬断言。**不阻断、已记录**。

## 来源
- 本轮 `internal/pool` 漏唤醒修复的**独立审计结论**（收窄返工轮）。
- 相关代码：`internal/pool/pool.go`（`Release` / `MarkHealthy` / `removeWaiterAndTransfer` /
  `capacityAvailableFor` / `accountCanServe` / `wakeNextUsable` / `wakeWaiter`）、`internal/pool/account.go`
  （`TryAcquire` / `canAcquire` / `Release` / `MarkHealthy` / `SetCooldown`）、
  `internal/config/constants.go`（`AccountSelectTimeout = 30s`）。
- 修复起点 commit：`59ea8b9e105b452de60e71ce6fffad05fa10cdaf`（分支 `main`）。
- 回归测试：`internal/pool/waiter_provider_test.go`。
