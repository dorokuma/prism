#!/usr/bin/env bash
# codex-wrapper — auto-sync MCP tools before launching Codex.
# Place in PATH before the real codex binary, e.g. /usr/local/bin/codex-wrapper.
# Usage: symlink as "codex" or set CODEX_BIN=/path/to/real/codex.
#
# Self-recursion guard: when this wrapper is symlinked as "codex" in PATH,
# `command -v codex` would find the wrapper ITSELF; the resolver below skips
# any candidate that resolves (readlink -f) to this script. The same guard
# also applies to an EXPLICIT CODEX_BIN: a path form is compared after
# readlink -f, and a BARE command name (no slash, e.g. CODEX_BIN=codex) is
# first resolved through PATH — skipping this wrapper — before the
# comparison, so a bare name can never resolve to the wrapper itself. If no
# real codex exists anywhere in PATH, the wrapper exits 127 with a clear
# message instead of exec-ing itself into an infinite loop.

set -euo pipefail

CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
CONFIG="${CODEX_HOME}/config.toml"
TOOLS_JSON="${PRISM_TOOLS_JSON:-/var/lib/prism/mcp_tools.json}"
GENERATOR="${PRISM_GENERATOR:-/root/prism/scripts/generate_mcp_tools.py}"

# script_path resolves THIS script (following symlinks) once for every
# self-recursion comparison below.
script_path="$(readlink -f -- "$0" 2>/dev/null || echo "$0")"

# resolve_real_codex scans PATH for a binary of the given name (default
# "codex") that is NOT this script (resolved via readlink -f, so symlinks
# like /usr/local/bin/codex → codex-wrapper are detected). Prints the first
# real candidate; exits 1 when none exists (the only match, if any, is this
# wrapper itself).
resolve_real_codex() {
    local bin_name="${1:-codex}"
    local dir cand resolved
    local IFS=:
    for dir in ${PATH:-}; do
        [ -n "$dir" ] || dir=.
        cand="$dir/$bin_name"
        [ -x "$cand" ] || continue
        resolved="$(readlink -f -- "$cand" 2>/dev/null || echo "$cand")"
        if [ "$resolved" != "$script_path" ]; then
            echo "$cand"
            return 0
        fi
    done
    return 1
}

# bin_name is the command name we will exec: the explicit CODEX_BIN when
# set (a path or a bare name), "codex" otherwise.
bin_name="${CODEX_BIN:-codex}"

if [ -z "${CODEX_BIN:-}" ]; then
    if ! CODEX_BIN="$(resolve_real_codex)"; then
        echo "[codex-wrapper] ERROR: real $bin_name binary not found in PATH (the only $bin_name found is this wrapper itself)" >&2
        echo "[codex-wrapper] set CODEX_BIN=/path/to/real/codex or install the real codex binary" >&2
        exit 127
    fi
else
    # An explicit CODEX_BIN: a bare command name (no slash) must first be
    # resolved through PATH exactly like a plain `codex` invocation would —
    # exec-ing a bare name would re-hit the wrapper itself when it is first
    # in PATH (infinite recursion), and readlink -f on a bare name only
    # resolves relative to the current directory, which is wrong. The
    # resolver skips this wrapper, so a PATH whose first (or only) match is
    # the wrapper leads to a clean 127 here, never recursion.
    case "$CODEX_BIN" in
        */*) ;;
        *)
            if ! CODEX_BIN="$(resolve_real_codex "$bin_name")"; then
                echo "[codex-wrapper] ERROR: real $bin_name binary not found in PATH (the only $bin_name found is this wrapper itself)" >&2
                echo "[codex-wrapper] set CODEX_BIN=/path/to/real/$bin_name or install the real $bin_name binary" >&2
                exit 127
            fi
            ;;
    esac
fi

# The explicit CODEX_BIN path form must still never resolve to THIS wrapper:
# a caller that sets CODEX_BIN to the wrapper (directly or via a symlink)
# would otherwise exec the wrapper from inside the wrapper — infinite
# recursion, exactly like the PATH case. Refuse with the same clear 127.
bin_path="$(readlink -f -- "$CODEX_BIN" 2>/dev/null || echo "$CODEX_BIN")"
if [ "$bin_path" = "$script_path" ]; then
    echo "[codex-wrapper] ERROR: CODEX_BIN=$CODEX_BIN resolves to this wrapper itself; refusing to exec (infinite recursion)" >&2
    echo "[codex-wrapper] set CODEX_BIN=/path/to/real/codex" >&2
    exit 127
fi

# Only re-gen if config.toml is newer than mcp_tools.json
if [ -f "$CONFIG" ] && [ -f "$GENERATOR" ]; then
    if [ ! -f "$TOOLS_JSON" ] || [ "$CONFIG" -nt "$TOOLS_JSON" ]; then
        echo "[codex-wrapper] config.toml changed, regenerating mcp_tools.json..." >&2
        /usr/bin/python3 "$GENERATOR" "$TOOLS_JSON"
        echo "[codex-wrapper] reloading prism..." >&2
        systemctl reload prism 2>/dev/null || systemctl kill -s HUP prism 2>/dev/null || true
    fi
fi

exec "$CODEX_BIN" "$@"
