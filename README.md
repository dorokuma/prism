# prism

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
