# reasonix-lb

一个轻量级 HTTP 反向代理 + 负载均衡器，专门为兼容 OpenAI API 格式的 LLM 后端（如 opencode.ai）设计。在多 API Key 之间做选号、自动耗尽标记、配额探测恢复和 429 限流冷却。

## 快速开始

```bash
git clone <repo>
cd reasonix-lb
go build -o reasonix-lb .
cp config.yaml.example config.yaml   # 填写真实 key
./reasonix-lb
```

默认监听 `:18790`。通过 `wire_api` 切换对外协议（上游始终是各账号的 OpenAI **Chat Completions**）。

## 配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `listen` | string | `:18790` | 监听地址 |
| `probe_interval` | duration | `10m` | 探测已耗尽账号的间隔 |
| `wire_api` | string | `both` | `legacy` / `responses` / `both`，见下文 |
| `accounts` | array | 必填 | 上游账号列表 |

### `wire_api`（新/旧 OpenAI 兼容）

| 值 | 对外路径 | 典型客户端 |
|----|----------|------------|
| `legacy` | 仅 `POST /v1/chat/completions` | Reasonix、DeepSeek 官方 API 直连配置 |
| `responses` | 仅 `POST /v1/responses` | Codex CLI（`wire_api = "responses"`） |
| `both` | 两条路径都开 | 同一端口同时给 Reasonix 和 Codex 用 |

`responses` 模式下，lb 会把 Responses 请求体转成 Chat Completions 再转发到 `base_url/chat/completions`，并把响应（含 SSE）转回 Responses 事件格式。

每个 account：

| 字段 | 说明 |
|------|------|
| `name` | 标识名，用于日志 |
| `key` | API Key（Bearer token） |
| `base_url` | 上游 API 基础地址 |

示例：

```yaml
listen: ":18790"
probe_interval: 10m
accounts:
  - name: acc-1
    key: sk-xxx
    base_url: https://opencode.ai/zen/go/v1
```

## 核心机制

```
请求进入 → Select(round-robin) → 转发
  ├─ 402 → MarkExhausted(移出池) → 重试下一个账号
  ├─ 429 → SetCooldown(2min 冷却) → 重试下一个账号
  ├─ 5xx → 重试下一个账号(不标记耗尽)
  └─ 2xx → 返回响应

后台探针(定时):
  └─ GET /models → 200 回复 → MarkHealthy(重新加入池)
```

- **选号**: round-robin 轮询，跳过冷却中和已耗尽的账号，成功借出（`TryBorrow`）即用，防止并发重复选到同一个账号。
- **耗尽标记**: 上游返回 402 Payment Required 时标记为 `StatusExhausted`，移出选号池。
- **探针恢复**: 定时（`probe_interval`，启动即执行一次）对已耗尽账号发 `GET /models`，连续重试 3 次（间隔 2s），任意一次 200 即恢复。
- **429 冷却**: 上游返回 429 Too Many Requests 时设置 2 分钟冷却，冷却期内绕过该账号。

## 端点

| 路径 | 方法 | 说明 |
|------|------|------|
| `/v1/chat/completions` | POST | 转发到上游 `/chat/completions`（`wire_api` 含 `legacy` 时） |
| `/v1/responses` | POST | Responses↔Chat 转换后转发（`wire_api` 含 `responses` 时） |
| `/v1/models` | GET | 从首个可用账号获取模型列表 |
| `/health` | GET | 健康检查，返回 `ok` (200) |

### 健康检查

`GET /health` 直接返回 200 + `ok`，不依赖上游。用于 load balancer / k8s probe。

### Codex 示例

`~/.codex/config.toml`（或 profile 覆盖）：

```toml
[model_providers.reasonix-lb]
name = "reasonix-lb"
base_url = "http://127.0.0.1:18790/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"   # 任意占位；真实 key 由 lb 账号配置注入

model_provider = "reasonix-lb"
model = "deepseek-v4-pro"    # 或 opencode-go 文档中的 model id
```

Reasonix / 旧客户端仍指向 `http://127.0.0.1:18790/v1`，`wire_api: both` 时无需改路径。

## License

MIT
