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

默认监听 `:18790`，所有请求通过 `/v1/chat/completions` 转发。

## 配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `listen` | string | `:18790` | 监听地址 |
| `probe_interval` | duration | `10m` | 探测已耗尽账号的间隔 |
| `accounts` | array | 必填 | 上游账号列表 |

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
| `/v1/chat/completions` | POST | 转发到上游 `/chat/completions`，支持 SSE 流式响应 |
| `/v1/models` | GET | 从首个可用账号获取模型列表 |
| `/health` | GET | 健康检查，返回 `ok` (200) |

### 健康检查

`GET /health` 直接返回 200 + `ok`，不依赖上游。用于 load balancer / k8s probe。

## License

MIT
