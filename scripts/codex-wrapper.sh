#!/usr/bin/env bash
# codex-wrapper — auto-sync MCP tools before launching Codex.
# Place in PATH before the real codex binary, e.g. /usr/local/bin/codex-wrapper.
# Usage: symlink as "codex" or set CODEX_BIN=/path/to/real/codex.

set -euo pipefail

CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
CONFIG="${CODEX_HOME}/config.toml"
TOOLS_JSON="${REASONIX_LB_TOOLS_JSON:-/root/reasonix-lb/mcp_tools.json}"
GENERATOR="${REASONIX_LB_GENERATOR:-/root/reasonix-lb/scripts/generate_mcp_tools.py}"
CODEX_BIN="${CODEX_BIN:-$(which codex 2>/dev/null || echo /usr/local/bin/codex)}"

# Only re-gen if config.toml is newer than mcp_tools.json
if [ -f "$CONFIG" ] && [ -f "$GENERATOR" ]; then
    if [ ! -f "$TOOLS_JSON" ] || [ "$CONFIG" -nt "$TOOLS_JSON" ]; then
        echo "[codex-wrapper] config.toml changed, regenerating mcp_tools.json..." >&2
        /usr/bin/python3 "$GENERATOR" "$TOOLS_JSON"
        echo "[codex-wrapper] reloading reasonix-lb..." >&2
        systemctl reload reasonix-lb 2>/dev/null || systemctl kill -s HUP reasonix-lb 2>/dev/null || true
    fi
fi

exec "$CODEX_BIN" "$@"
