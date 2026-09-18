---
status: active
superseded_by: ""
supersedes: ""
模块: "proxy, usage"
---

# prism 与 metapi 打通及 NewAPI 协议兼容与 reasoning token 计费

## 一句话结论
- 补齐 metapi / NewAPI 协议兼容（响应头透传与 normalize 转换、`GET /api/user/self`、`GET /api/log/self` 端点及断流 token 捕获与有界 drain），并修复 `ComputeCost` 在子集语义下将 reasoning token 纳入 Output 单价且不重复计费。

## 背景
- 下游网关（metapi / NewAPI 生态）聚合 prism 时存在三项能力断层：
  1. 缺少上游限流/配额响应头透传（`x-ratelimit-*`, `x-quota-*`, `anthropic-ratelimit-*`），导致下游无法感知上游余量并及时降级。
  2. 缺少 NewAPI 鉴权与日志账单端点（`/api/user/self`, `/api/log/self`），导致下游无法同步余额与请求日志，且 1 USD = 500,000 quota units 换算缺失。
  3. 流式传输客户端断流时上游 body 直接被丢弃，导致断流请求末尾 token 统计丢失（记 0 token）；若无界 drain 又可能阻塞耗尽连接池。
- 计费层面：`ComputeCost` 早期未单独拆分 reasoning token，而后曾误将 reasoning 与 completion 直接相加导致重复计费；需基于 OpenAI wire 规范（reasoning 属于 completion 的子集）进行去重计费。

## 决策
1. **响应头透传与规范化（internal/proxy & internal/config）**：
   - 增加 `proxy.header_passthrough` 配置，支持默认开启与热重载。
   - 两级白名单判定：保留基础头（`Content-Type`, `Content-Disposition`, `Content-Language`, `Retry-After`）+ 前缀放行（`x-ratelimit-`, `x-quota-`, `anthropic-ratelimit-`）。
   - 敏感黑名单过滤：剥离租户/账号及基础设施敏感头（`x-ratelimit-user*`, `x-ratelimit-account*`, `x-ratelimit-org*`, `x-ratelimit-project*`, `x-quota-account*`, `x-quota-user*`, `x-request-id`, `x-amzn-trace-id`, `x-cloud-trace-context`, `x-upstream-*`, `server`, `via`, `x-powered-by`, `x-envoy-*` 等）。
   - 支持 `passthrough`（默认保真透传）与 `normalize` 模式：
     - `normalize` 模式将 Anthropic 限流头映射为 OpenAI 规范头，并将 ISO8601/RFC3339 重置时间转换为秒级 unix 时间戳。
     - 该规范化仅对 OpenAI wire 路径（`/v1/chat/completions`、`/v1/responses`）生效，`/v1/messages` 路径保持原生透传，不做盲目重写。
2. **NewAPI 兼容端点（internal/newapi & cmd/prism）**：
   - 在 `cmd/prism` 挂载 `GET /api/user/self` 和 `GET /api/log/self`，由 `internal/newapi` 独立封装。
   - 鉴权主体直接复用 `api_keys` 配置，多租户按 `key_id` 强制隔离，查询严格带 `WHERE key_id = ?`。
   - 汇率对齐 NewAPI 标准：1 USD = 500,000 quota。
   - `UserSelf`：返回 quota 余额与 used_quota 已消耗额度。未配置 `quota_usd` 时 quota 值为 5,000,000,000（等价 $10,000），语义为未设预算限制、防止下游网关误熔断；`used_quota` 始终按实际 `cost_usd` 真实累积。账单统计纳入 `success=0` 但捕获到 token 或 cost 的断流记录。
   - `LogSelf`：支持分页、时间戳范围过滤（自动兼容毫秒与秒级时间戳输入，`> 1e11` 时除以 1000）、排序与 model 过滤；输出兼容 `data.items` 与 `data.total`，other 字段序列化 cache token 比例。
3. **断流 token 捕获与有界 drain（internal/stream）**：
   - 客户端断流后将上游未读 body 读入 `usageEventCapture`，并施加 16MB `io.LimitReader` 上限与 30s 独立超时控制，超限/超时中止并记录，防止挂满连接池。
   - 在 EOF 或 drain 完成后执行 `capture.Finish()` 提取末尾 usage chunk 并注入 `middleware.RequestAudit`。
4. **reasoning token 计费（internal/usage）**：
   - 确认上游解析事实：OpenAI / xAI wire 格式中 `reasoning_tokens` 为 `completion_tokens` 的子集（位于 `completion_tokens_details.reasoning_tokens` 中，`completion_tokens` 已含思维链 token）。
   - `ComputeCost` 采用子集去重公式，避免重复计费：
     - OpenAI: `(Prompt - Cached)*Input + Cached*CacheRead + CacheWrite*CacheWrite + max(0, Completion - Reasoning)*Output + Reasoning*ReasoningPrice`（`ReasoningPrice` 默认等于 `Output`）。
     - Anthropic: `Prompt*Input + Cached*CacheRead + CacheWrite*CacheWrite + max(0, Completion - Reasoning)*Output + Reasoning*ReasoningPrice`。
   - 当 `Reasoning <= Completion` 时，公式自然化简为 `Completion * Output`，不重复计费；若遇 `Reasoning > Completion` 异常或并列数据，打 stderr 警告并将非 reasoning 输出部分钳位为 0，reasoning 正常计费。
   - 仅对新增记录计算，不篡改历史存量数据。

## 被放弃的方案
- **强制在代理层统一将 Anthropic 响应头重写为 OpenAI x-ratelimit 格式**：放弃默认重写。不同上游重置时间单位（秒 vs ISO8601 vs 毫秒）及配额维度存在语义差异，强制重写会导致部分依赖上游原生 SDK 的客户端解析失败；采用 `passthrough` 保真作为缺省策略。
- **单独引入 NewAPI 用户表与鉴权数据库**：放弃。prism 定位为轻量透明网关，直接基于现有 `config.yaml` 的 `api_keys` 鉴权与 SQLite `usage_events` 实时聚合，避免引入双重状态源与数据同步一致性负担。
- **历史已记录账单数据全量回溯重算 reasoning token 费用**：放弃。已入库的 `cost_usd` 属于既定审计财务事实，回溯重算会导致审计对账不一致，仅对新产生的请求应用新计费逻辑。

## 来源
- 架构设计方案：`/tmp/planner-metapi-compat-design.md` [MARK-PLANNER-METAPI-COMPAT-01]
- 双审修复要点：`metapi-compat-review-round-02` [MARK-WORKER-METAPI-FIX-02]
