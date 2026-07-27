# prism

> Version: v0.8.0  Date: 2026-07-27  Status: living document

LLM API Load Balancer  
Multi-account round-robin, exhaustion / cooldown, Chat↔Responses translation.

## Quick start (local)

```bash
git clone <repo>
cd prism
go build -o prism .
cp config.yaml.example config.yaml
# set keys (pick one):
#   export LB_KEY_ACCOUNT_1=... LB_KEY_ACCOUNT_2=...
#   or put key: in config.yaml (local only, never commit)
./prism
```

Default listen: `:18790` if unset. Prefer `127.0.0.1:18790` in production.

## Production layout (eqi / systemd)

| Piece | Path |
|-------|------|
| Binary | `/usr/local/bin/prism` |
| Runtime config | `/var/lib/prism/config.yaml` |
| Tool cache (optional) | `/var/lib/prism/mcp_tools.json` |
| Account secrets | `/etc/credstore/prism/LB_KEY_*` via `LoadCredential` |
| Unit | `/etc/systemd/system/prism.service` (see `scripts/prism.service.example`) |

Source tree (`/root/prism` or the git clone) is for build only — **no production `config.yaml` / `.env`**.

### Account keys

For each account `name` (e.g. `go-plan-1`), if `key` is empty in YAML the process loads:

1. **systemd LoadCredential** file named `LB_KEY_GO_PLAN_1` under `$CREDENTIALS_DIRECTORY`
2. else environment **`LB_KEY_GO_PLAN_1`**

Hyphens in the account name become underscores in the credential/env name.

### Deploy / update binary

```bash
cd /path/to/prism
go build -o prism .
install -m 755 prism /usr/local/bin/prism
# config edits: edit /var/lib/prism/config.yaml then:
systemctl restart prism   # only when you intend downtime / reload
# or: systemctl kill -s HUP prism   # if process supports SIGHUP for partial reload
```

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen` | string | `:18790` | Listen address |
| `probe_interval` | duration | `10m` | Exhausted-account probe interval |
| `wire_api` | string | `both` | `legacy` / `responses` / `both` |
| `accounts` | array | required | Upstream accounts |
| `model_tiers` | map | — | Tier → upstream model |
| `model_remap` | map | — | Virtual model → tier |
| `default_tier` | string | — | Fallback tier |

### Model remapping behavior

`model_remap` maps virtual model names to tiers. `model_tiers` maps each tier to an
upstream model name. The resolution logic is:

1. If the requested model is **in `model_remap`**:
   - Look up its tier, then look up that tier in `model_tiers`.
   - If both mappings exist → use the upstream model.
   - If the tier has no upstream mapping → fall back to `default_tier` (if set).
2. If the requested model is **not in `model_remap`**: pass through unchanged.
   (Real upstream model names are sent directly to the provider.)

> **Note:** Previously, all unknown models went through `default_tier`. The current
> behavior only applies `default_tier` to models that exist in `model_remap` but
> whose target tier has no upstream mapping.
| `mcp_tools_json` | string | — | Optional path to tool-definitions JSON |
| `model_metadata` | map | — | Per-model metadata returned by /v1/models: context_window, max_tokens, reasoning, input, cost, thinking_level_map, extra |
| `probe_model` | string | `deepseek-chat` | Startup/probe model id |

### `wire_api`

| Value | Path | Typical client |
|-------|------|----------------|
| `legacy` | `POST /v1/chat/completions` only | Legacy clients |
| `responses` | `POST /v1/responses` only | Codex CLI |
| `both` | Both | Mixed |

### Accounts

| Field | Description |
|-------|-------------|
| `name` | Label; also builds `LB_KEY_*` name |
| `key` | Optional inline key (avoid in production) |
| `base_url` | Upstream API base |

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/v1/chat/completions` | POST | Chat proxy |
| `/v1/responses` | POST | Responses path |
| `/v1/models` | GET | Virtual models |
| `/health` | GET | `ok` |

### Codex

`~/.codex/config.toml` example:

```toml
[model_providers.prism]
name = "prism"
base_url = "http://127.0.0.1:18790/v1"
requires_openai_auth = false
api_key = "lb-local-placeholder"
wire_api = "responses"

model_provider = "prism"
model = "gpt-5.5"
```

### Tool definitions cache

Optional JSON used when injecting tool schemas for some clients. Generate from Codex MCP config:

```bash
python3 scripts/generate_mcp_tools.py /var/lib/prism/mcp_tools.json
systemctl kill -s HUP prism   # or restart
```

## License

MIT

## Changelog

- **2026-07-27** — v0.8.0 — feat: model metadata single source of truth — fetch from upstream (ollama /api/show for context_length) + config model_metadata field-level override, syncPIModelsJSON rewrites existing models too (fixes glm-5.2 128k→1M), /v1/models returns upstream+config merged metadata, cache ModelMeta + providerCache.Meta
- **2026-07-27** — v0.7.0 — feat: provider-dimension effort mapping (X-Prism-Provider header → opencode/ollama schema), same model no crosstalk across providers (glm-5.2/deepseek differ opencode vs ollama), ollama profiles match ollama.com real levels (xhigh→max, off→none, no thinking.type), drop outer ModelRemapEnabled gate (Apply always runs, remap still internally gated), config base_url host detection for provider schema
- **2026-07-27** — v0.6.2 — feat: generic reasoning effort mapping (internal/reasoning package), downward-proximity clamp, per-model effort remap for non-DeepSeek models (hy3/glm/kimi/qwen/mimo/minimax), DeepSeek behavior unchanged
- **2026-07-25** — v0.6.1 — fix: gofmt alignment, Non-Prim→Non-Prism typo, syncPIModelsJSON unmarshal-fail pc reset, atomic write (tmp+rename)
- **2026-07-23** — v0.6.0 — /v1/models returns model_metadata (context_window/max_tokens/cost/think etc.), syncPIModelsJSON changed to merge (no overwrite), config adds model_metadata section
- **2026-07-23** — v0.5.2 — provider routing (X-Prism-Provider), disk model cache, prism setup command, probe changed to GET /v1/models, model_remap_enabled toggle, credential three-tier fallback, cleanup of ProbeModel dead code
