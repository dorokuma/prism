---
status: active
superseded_by: ""
supersedes: ""
模块: "cache"
---

# 聚合入口：无头请求按模型名自动解析 provider（同名模型显式裁决，歧义 fail-closed）

## 一句话结论
- 新增 opt-in 的 `provider_routing: auto`：缺少 `X-Prism-Provider` 头时，`/v1/*`（chat/responses/messages/models）按模型名解析 provider；同名模型靠 `model_provider_overrides` > 唯一候选 > `provider_priority` 裁决，裁决不了的从聚合目录剔除并 400 `ambiguous_provider`；显式 pin 永远绕过聚合。

## 背景
- 下游（metapi 等第三方聚合网关）无法按请求动态携带 `X-Prism-Provider`（其一站一 header、下游透传白名单不含 x-prism-*），却需要一个"标准 OpenAI 兼容"上游形态：一个 base URL、一个 key，`/v1/models` 返回全量目录。
- 本机生产事故史证明"让 prism 猜 provider"是错的：v0.12 之前无头请求回退全池选择，`deepseek-v4-flash` 落到 `agentrouter-ant-2`、`gpt-5.6-sol` 落到 `agentrouter-ant-1`。故聚合解析必须**规则化、可审计、争议即拒**，而不是启发式。
- prism 是自有项目，metapi 是上游项目（fork 需长期 cherry-pick），结论是"prism 适配下游"，此改动全部落在 prism 内部。

## 决策
- 配置面（全部可选，缺省行为与现状 byte-for-byte 一致）：
  - `provider_routing: auto`（仅接受空/`auto`，其他值加载报错；缺省空 = off）。
  - `provider_priority: [xai, gemini]`：同名模型消歧优先级（顺序即优先级）。引用不存在的 provider → 加载报错。
  - `model_provider_overrides: {model: provider}`：单模型最高裁决，允许"强制路由"（即使目标 provider 目录暂无该模型）。value 引用不存在的 provider → 加载报错。
  - `providers.<name>.models: [...]`：静态目录，供 `skip_model_cache`/配额型 provider（如 Gemini Cloud Code）参与聚合；与缓存目录并集去重。
- 裁决链（请求时，`X-Prism-Provider` 缺席且 `provider_routing=auto`）：
  1. `model_provider_overrides[model]` → 该 provider
  2. 注册表唯一候选 → 该 provider
  3. `provider_priority` 在候选集中取最高 → 该 provider（裁决时 WARN 一行：候选集 + 赢家）
  4. `default_provider`（仅"无人认领"模型兜底，绝不盖过 1/2/3）
  5. 前三无果且无 default_provider → 400 `unknown_model`；同名且无 1/3 可裁决 → 400 `ambiguous_provider`（错误体附候选列表）
- 注册表（internal/cache 新增 routing.go）：`model → ordered candidates`，来源 = 各 provider 模型缓存 ∪ `providers.<name>.models` 静态目录；ProviderNames 顺序 = `providers:` 块 YAML 声明顺序（UnmarshalYAML 记录，修复原 map 迭代随机序）。
- `/v1/models`（无 pin + auto）：并集，每模型一条；元数据（context_window/max_tokens/cost/reasoning/input/thinking_level_map）取**赢家 provider** 的 provider 分层元数据（与请求实际落点一致）；未裁决同名模型剔除出目录。
- `model_remap` 与聚合正交：解析发生在 remap 之后（注册表查真实上游名）；`model_remap_enabled` 的 AllModels 分支保持第一优先。
- 显式 `X-Prism-Provider`（含未来路径 pin）永远绕过注册表：pi 等老客户端零影响。
- 可观测性：歧义计数 expvar `aggregate_ambiguous_models` + 冲突 WARN；赢家切换 WARN。
- 未新增对外协议：不入站/出站响应头，可选在聚合 `/v1/models` 条目附 `x_prism_provider`（JSON 附加字段，可选，默认不做）。

## 被放弃的方案
- **metapi fork 加 prism 平台**：需长期 cherry-pick，否决。
- **prism 加 `/p/{provider}` 路径前缀（每 provider 一个 metapi 站点）**：可行且无歧义，但 metapi 站点多、provider 决策散在下游；用户选择"聚合好，metapi 视角好管理"，本方案为最终形态。路径前缀保留为未来可选（不实现）。
- **聚合目录直接按"缓存最新者/目录最大者"启发式选同名赢家**：非规则化、不可审计，与 v0.12 教训冲突，否决。
- **同名模型在目录中并列多条（不同 provider 各一条）**：OpenAI 目录语义是一模型一 id，并列会让下游（metapi）路由歧义，否决。
- **聚合模式默认开启**：会改变 pi 等无感客户端行为，opt-in 缺省 off，否决默认开。

## 来源
- 2026-09-17 对话：metapi 接入评估；prism v0.6 a243692（provider 路由起源）、v0.12 1d394bf（default_provider 与全池回退事故）。
