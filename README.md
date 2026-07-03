# reasonix-lb

Lightweight HTTP reverse proxy + load balancer for OpenAI-compatible LLM backends.
Multi-account round-robin selection, automatic exhaustion marking, quota recovery probing,
429 cooldown, and Chat↔Responses API format translation.

## Quick Start

```bash
git clone <repo>
cd reasonix-lb
go build -o reasonix-lb .
cp config.yaml.example config.yaml
./reasonix-lb
```

Listens on `:18790` by default. Use `wire_api` to control protocol surface.

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen` | string | `:18790` | Listen address |
| `probe_interval` | duration | `10m` | Exhausted account probe interval |
| `wire_api` | string | `both` | `legacy` / `responses` / `both` |
| `accounts` | array | required | Upstream account list |
| `model_tiers` | map | — | Tier name → upstream model identifier |
| `model_remap` | map | — | Virtual model name → tier name |
| `default_tier` | string | — | Fallback tier for unmapped models |

### `wire_api`

| Value | Path | Typical client |
|-------|------|----------------|
| `legacy` | `POST /v1/chat/completions` only | Reasonix, legacy clients |
| `responses` | `POST /v1/responses` only | Codex CLI (`wire_api = "responses"`) |
| `both` | Both paths | Mixed usage on same port |

### Model tiers

Tiers decouple virtual model names from upstream identifiers.
Change the upstream model in one place without touching any client config.

```yaml
model_tiers:
  frontier: deepseek-v4-pro
  standard: deepseek-v4-flash
default_tier: standard
```

### Model remap

Maps virtual model names (shown to clients) to tiers.

```yaml
model_remap:
  gpt-5.5: frontier
  gpt-5.5-pro: frontier
  gpt-5.4: standard
  gpt-5.4-mini: standard
```

### Accounts

| Field | Description |
|-------|-------------|
| `name` | Account label for logs |
| `key` | API key (or read from env `LB_KEY_{NAME}`) |
| `base_url` | Upstream API base URL |

## Core mechanism

```
Request → Select (round-robin) → Forward
  ├─ 402/401/403 → MarkExhausted → retry next account
  ├─ 429 → SetCooldown → retry next account
  ├─ 5xx (×5 consecutive) → MarkExhausted
  └─ 2xx → Return response

Background probe (periodic):
  └─ GET /models → 200 → MarkHealthy (rejoin pool)
```

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/v1/chat/completions` | POST | Proxy to upstream `/chat/completions` (legacy/both mode) |
| `/v1/responses` | POST | Responses↔Chat conversion then proxy (responses/both mode) |
| `/v1/models` | GET | Return virtual model list from `model_remap` keys |
| `/health` | GET | Health check, returns `ok` (200) |

### Codex config

`~/.codex/config.toml`:

```toml
[model_providers.reasonix-lb]
name = "reasonix-lb"
base_url = "http://127.0.0.1:18790/v1"
requires_openai_auth = false
api_key = "lb-local-placeholder"
wire_api = "responses"

model_provider = "reasonix-lb"
model = "gpt-5.5"
```

### MCP tools

MCP tool definitions are auto-generated from `~/.codex/config.toml` by
`scripts/generate_mcp_tools.py`. Install new MCP servers normally in Codex,
then run the script before restarting LB:

```bash
python3 scripts/generate_mcp_tools.py mcp_tools.json
systemctl restart reasonix-lb
```

Or use `scripts/codex-wrapper.sh` to chain generation before each Codex launch.

## License

MIT
