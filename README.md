# prism

> Version: v0.14.1  Date: 2026-08-12  Status: living document

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
HEALTH_URL=http://127.0.0.1:18790/health ./deploy.sh   # HEALTH_URL 可覆盖，默认即此值
```

`deploy.sh` 编译 → 原子替换二进制 → `systemctl restart prism` → 健康验证 → 失败自动回退。
健康检查地址默认 `http://127.0.0.1:18790/health`，可用环境变量 `HEALTH_URL` 覆盖（如非默认端口或远程探测）。

手动部署：

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
| `default_provider` | string | — | Fallback provider for requests missing the X-Prism-Provider header; unset = reject them with HTTP 400 |
| `max_concurrent_per_account` | map | — | Model → max concurrent requests per account (exact match; silences the unknown-model default warning) |
| `max_upstream_response_bytes` | int | `33554432` (32 MiB) | Cap for non-streaming upstream response bodies (both the legacy chat path and the responses translation path) AND for model-cache success reads (`/v1/models` and ollama `/api/show`); larger bodies are rejected with HTTP 502 `response_too_large` (chat) or the model fetch fails (`model_fetch_failed`). `0`/absent = default, negative or above `268435456` (256 MiB, hard upper bound) = startup error. Hot-reloadable |
| `usage` | map | disabled | Token usage recording to SQLite: `enabled` (default false, opt-in), `db_path` (default `/var/lib/prism/usage.db`; NOT hot-reloadable), `retention_days` (absent → default 30; explicit `0` → keep forever, cleanup disabled), `channel_size` (default 4096), `batch_size` (default 50), `batch_flush_ms` (default 200), `default_key_id` (default `anonymous`; key_id recorded for requests without an authenticated api key — explicit empty falls back to `anonymous`). Cost is priced from `model_metadata[].cost` (USD per 1M tokens); the pricing 口径 follows the upstream wire format (OpenAI `prompt_tokens` includes cached tokens, Anthropic `input_tokens` excludes the cache counters) and is persisted per row as `usage_source`. Any usage failure degrades to logs + counters and never affects `/v1` forwarding |

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
| `model_metadata` | map | — | Per-model metadata returned by /v1/models: context_window, max_tokens, reasoning, input, cost, thinking_level_map, extra. `cost` unit is USD per 1M tokens (e.g. `input: 0.6` = $0.6 per 1M input tokens) |
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
| `/v1/chat/completions` | POST | Chat proxy (non-POST → 405 `method_not_allowed`) |
| `/v1/responses` | POST | Responses path (non-POST → 405 `method_not_allowed`) |
| `/v1/models` | GET | Model list. No `X-Prism-Provider` header → empty 200 list. Cache hit → cached models. Cache miss → fetch from upstream; fetch failure is never a 200 empty list: no healthy account → 503 `no_healthy`, account saturated → 503 `model_fetch_saturated`, other upstream/parse/size errors → 502 `model_fetch_failed`. Request body cap 10 MiB (413 `request_too_large` / 400 `invalid_request`) on the POST surfaces |
| `/health` | GET | `ok` |
| `/metrics` | GET | expvar metrics. Auth (fail-closed): when `METRICS_TOKEN` is configured every request — loopback included — must present it as a Bearer token; only when it is unset is a direct loopback request (no `X-Forwarded-For`/`X-Real-IP`) allowed without a token, while loopback with forwarding headers (same-machine reverse proxy) and all remote requests are denied |
| `/admin/usage/summary` | GET | Aggregated token usage/cost summary (when `usage.enabled`). Auth (fail-closed): when `PRISM_ADMIN_TOKEN` is configured every request — loopback included — must present it as a Bearer token; only when it is unset is a direct loopback request (no `X-Forwarded-For`/`X-Real-IP`) allowed; mounted before the global api_keys gate (like `/metrics`). Query params: `from`/`to` (unix seconds), `group_by` (`model`,`provider`,`account`,`key_id`,`stream`,`success`,`hour`,`day`), filters `model`/`provider`/`account`/`key_id`/`stream`/`success`, `limit` (default 100, max 1000). Returns 503 `store_unavailable` when usage is disabled or the store failed |

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

- **2026-08-12** — v0.14.1 — audit round 2: 10 reliability/security fixes. Model cache: `Fetch` releases its concurrency slot through the pool (`mc.pool.Release`), so a completed fetch wakes a parked provider waiter (previously waiters could strand until an unrelated request freed the account); `/v1/models` and ollama `/api/show` SUCCESS responses are now read with the same bounded helper as chat responses (`max_upstream_response_bytes`, default 32 MiB, invalid caps never degrade to an unbounded read — helper moved to `internal/util` so cache and proxy share it without an import cycle); SIGHUP no longer blocks the signal loop on `RefreshAll`/`SyncTools` — a controlled background refresh (reentry-coalesced, internal cancellable context, `Stop()` aborts an in-flight refresh and waits for it, no cache file written after cancellation, config snapshotted to avoid races with `UpdateConfig`) runs the refresh + tools sync; `/v1/models` cache-miss fetch failures are classified instead of returning 200 empty lists — no healthy account → 503 `no_healthy`, saturated → 503 `model_fetch_saturated`, other upstream/parse/size errors → 502 `model_fetch_failed`. Upstream error handling: every 401/402/429/4xx/5xx log and audit error body (runtime, startup probe, probe loop) is redacted with `RedactBodyBytesWithKeys(..., acc.Key())`, closing the leak for account keys that are not `sk-`/Bearer shaped (custom `auth_header`). Request bodies: `/v1/chat/completions`, `/v1/responses`, `/v1/messages` bodies over 10 MiB return HTTP 413 `request_too_large` (JSON envelope), other read errors return HTTP 400 `invalid_request` — one shared helper instead of three divergent paths. Auth: the Bearer scheme is matched case-insensitively (`bearer`/`BEARER`/`BeArEr`) across `Authenticate`, `CheckAuth` and the usage admin auth, while the token bytes stay strictly compared (bare tokens and wrong tokens still rejected). Anthropic SSE wire-format detection parses the `data:` JSON `type` field (whitespace- and field-order-insensitive) instead of matching the exact byte string `"type":"message_start"`. `deploy.sh` health-check URL is now overridable via the `HEALTH_URL` environment variable (default unchanged). All round-1 guarantees kept: empty-token keys, fail-closed admin auth, cross-provider waiters, XFF multi-hop, 256 MiB cap, select error codes, cost clamping, bucket cap, 405.

- **2026-08-12** — v0.14.0 — audit round: 14 reliability/security fixes. Account selection: select timeout now bound to `r.Context()` (client disconnect cancels the wait immediately) and select failures are classified with `errors.Is` into four response codes — `no_healthy` / `select_timeout` / `client_canceled` / `select_failed` — replacing the old single `no_accounts` code on the select path (HTTP stays 503; **compat note**: clients matching on `code` must update). Pool waiters are provider-aware: `Release`/`MarkHealthy` wake the first queued waiter that can use the freed slot (provider match, or `provider=""` waiters that can use any slot), FIFO within the matching set, no lost wakeups. Auth hardening: `api_keys` with empty/whitespace tokens are rejected at load and skipped defensively by `Authenticate`; `/metrics` and `/admin/usage/summary` auth is fail-closed — when `METRICS_TOKEN`/`PRISM_ADMIN_TOKEN` is configured EVERY request (loopback included) must present it as a Bearer token, and token-free direct loopback (no `X-Forwarded-For`/`X-Real-IP`) is allowed only when the token is unset. Upstream response handling: non-streaming success reads capped at `max_upstream_response_bytes` (default 32 MiB, hard upper bound 256 MiB, read max+1, over-limit → 502 `response_too_large`); `IsQuotaError` now accepts only the structured permanent quota envelope (`insufficient_quota`/`gousagelimiterror`) — plain-text quota messages on 429 go to cooldown; bare 403 is never permanent, while a 403 carrying a recognized structured credential/quota body exhausts the account via the shared `ClassifyUpstreamError` used by both runtime and startup — the original 403 body/status still passes through to the client (body read once, redacted). Trusted-proxy client IP: `GetClientIP` walks XFF right-to-left skipping all trusted hops (multi-hop chains resolve to the real client). Model cache: non-200 error bodies are redacted before entering errors/logs; `Fetch` now `TryAcquire`s a concurrency slot and fails fast when saturated, using `config.ResolveFetchConcurrency` (configured `*` wildcard, else the smallest positive per-model limit, else the built-in default) because a fetch is not tied to a single business model. `/v1/chat/completions` and `/v1/responses` return 405 `method_not_allowed` for non-POST. Rate limiter buckets have a deterministic cap (default 100000; oldest `lastCheck` evicted; test-injectable). Cost accounting: all four token counters (prompt/completion/cached/cache-write) are clamped non-negative at the cost entry and at every parse entry — OpenAI keeps cached ≤ prompt, Anthropic only drops negatives and keeps its formula semantics — so no broken upstream report can ever produce a negative cost.

- **2026-08-11** — v0.13.2 — fix: `prism usage` summary line now splits the cache-hit figure into two independent segments by `usage_source` (OpenAI vs Anthropic), each computed with its own 口径: the OpenAI segment is `cached/prompt_tokens` (OpenAI `prompt_tokens` includes cached tokens), the Anthropic segment uses `cache_read/(input_tokens+cache_read+cache_creation)` (Anthropic `input_tokens` excludes the cache counters, `cache_read` is a sibling field) and can never exceed 100%; previously the two wire formats were mixed into one ratio with numerator and denominator from different bases, which produced hit rates over 100% whenever Anthropic `cache_read` dwarfed its tiny `input_tokens` (e.g. the claude-opus-5 batch). `usage_source` values `'openai'`, empty string and NULL all bucket into the OpenAI segment, matching the pricing partition in cost.go; a segment with no rows is omitted from the summary, and the per-row detail table keeps its original per-row cache column

- **2026-08-11** — v0.13.1 — fix: `prism usage` CLI config lookup now walks a fallback chain (`--db` > `config.yaml` in the current directory > `/var/lib/prism/config.yaml` > built-in default), so running the command outside `/var/lib/prism` no longer silently falls back to the wrong default path `/var/lib/prism/usage.db` while the real database lives at `/var/lib/prism/usage/usage.db`; the missing-database error no longer asserts "usage not enabled" but reports the actual path, where it came from, and a `--db` hint

- **2026-08-08** — v0.13.0 — feat: token usage recording (internal/usage SQLite store + batched recorder) wired into the request lifecycle and config: new `usage` config section (opt-in, default disabled; `db_path` not hot-reloadable), per-request usage/cost persisted at `EmitAudit` via an injected minimal recorder interface (middleware stays decoupled from internal/usage), cost priced per request from `model_metadata[].cost` (USD per 1M tokens, no conversion), admin endpoint `GET /admin/usage/summary` (PRISM_ADMIN_TOKEN Bearer auth or localhost, mounted before the global api_keys gate). Hard degradation guarantee: every usage failure (store open/migrate, disk full, SQLITE_BUSY, full queue, pricing) is logged + counted only, never returned, never panics, never blocks request finalization or /v1 forwarding. Also tightened `Authenticate` to require the `Bearer ` prefix (bare tokens are now rejected, matching legacy `CheckAuth`)

- **2026-08-08** — v0.12.0 — feat: new `default_provider` config option; requests missing the X-Prism-Provider header now route through the configured default provider when set, otherwise return HTTP 400 (missing X-Prism-Provider header) instead of falling back to whole-pool account selection, which could previously hit an account of a different provider (e.g. deepseek-v4-flash landing on agentrouter-ant-2, gpt-5.6-sol landing on agentrouter-ant-1); explicit `max_concurrent_per_account` example added so per-model concurrency is configured instead of falling back to the built-in default with an "unknown model" warning

- **2026-08-08** — v0.11.0 — fix: account selection is now true per-provider round-robin. Previously all providers shared a single pool-wide cursor whose start index was computed modulo the total number of accounts, and the cursor advanced by the number of accounts scanned per attempt; a provider's first account on the ring was therefore picked disproportionately often (measured 3:0 on one two-account provider and 6:2 on another), and high-traffic providers polluted the rotation of low-traffic ones. Each provider now has its own cursor that rotates strictly within its own account subset, advancing exactly one position per successful selection, and the full-pool Select path advances its cursor the same way; new tests guard rotation order, cross-provider isolation, cooldown skipping, and uniform full-pool rotation
- **2026-08-08** — v0.10.2 — docs: correct the syncPIModelsJSON write-strategy comment — direct overwrite preserves the existing owner/mode of pi models.json, which is what lets prism write it through group permissions; tmp+rename would reassign ownership
- **2026-08-08** — v0.10.1 — fix: per-account headers and auth_header now also apply to model cache fetches (/v1/models and ollama /api/show), previously only chat forwarding and probes carried them — gateways that authenticate on client identity headers rejected model fetches with 401; skip_pi_sync narrowed to its original meaning (never overwrite hand-maintained pi models.json entries) and no longer suppresses upstream model fetching
- **2026-08-08** — v0.10.0 — feat: agentrouter-class external provider accounts join the pool load balancer; per-account headers and auth_header (default Authorization Bearer, configurable e.g. x-api-key); anthropic /v1/messages pure passthrough route independent of wire_api gating; per-account probe_path with disabled support (optimistic recovery, no permanent exhausted when probe endpoint unavailable); skip_pi_sync opt-out so hand-maintained pi models.json entries are never overwritten
- **2026-07-29** — v0.9.3 — feat: add grok-4.5 reasoning profile (FormEnum, reasoning_effort low/medium/high)
- **2026-07-27** — v0.9.2 — fix: TransformRequestBodyForProvider normalizes `role:developer` → `role:system` in /v1/chat/completions, gated to ollama-schema providers only (Ollama silently drops developer-role content, causing SYSTEM.md/AGENTS.md loss for reasoning models with auto-detected supportsDeveloperRole=true; opencode-schema providers unaffected)
- **2026-07-27** — v0.9.1 — fix: TransformRequestBodyForProvider now normalizes `role:developer` → `role:system` in /v1/chat/completions messages (Ollama silently drops developer-role content, causing SYSTEM.md/AGENTS.md to be lost for reasoning models auto-detected as supportsDeveloperRole=true)
- **2026-07-27** — v0.9.0 — feat: startup auto-fetch ollama /api/show (self-heal old cache without Meta, no manual SIGHUP) + provider-scoped model_metadata (default + per-provider override layer, same model no crosstalk across providers, e.g. deepseek-v4-pro ollama upstream 512K vs opencode-go config 1M)
- **2026-07-27** — v0.8.0 — feat: model metadata single source of truth — fetch from upstream (ollama /api/show for context_length) + config model_metadata field-level override, syncPIModelsJSON rewrites existing models too (fixes glm-5.2 128k→1M), /v1/models returns upstream+config merged metadata, cache ModelMeta + providerCache.Meta
- **2026-07-27** — v0.7.0 — feat: provider-dimension effort mapping (X-Prism-Provider header → opencode/ollama schema), same model no crosstalk across providers (glm-5.2/deepseek differ opencode vs ollama), ollama profiles match ollama.com real levels (xhigh→max, off→none, no thinking.type), drop outer ModelRemapEnabled gate (Apply always runs, remap still internally gated), config base_url host detection for provider schema
- **2026-07-27** — v0.6.2 — feat: generic reasoning effort mapping (internal/reasoning package), downward-proximity clamp, per-model effort remap for non-DeepSeek models (hy3/glm/kimi/qwen/mimo/minimax), DeepSeek behavior unchanged
- **2026-07-25** — v0.6.1 — fix: gofmt alignment, Non-Prim→Non-Prism typo, syncPIModelsJSON unmarshal-fail pc reset, atomic write (tmp+rename)
- **2026-07-23** — v0.6.0 — /v1/models returns model_metadata (context_window/max_tokens/cost/think etc.), syncPIModelsJSON changed to merge (no overwrite), config adds model_metadata section
- **2026-07-23** — v0.5.2 — provider routing (X-Prism-Provider), disk model cache, prism setup command, probe changed to GET /v1/models, model_remap_enabled toggle, credential three-tier fallback, cleanup of ProbeModel dead code
