#!/usr/bin/env bash
# Install OpenCode Go model metadata for Codex (removes "Model metadata not found" warnings).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
SRC="$ROOT/scripts/opencode-go-codex-models.json"
DEST="$CODEX_HOME/opencode-go-models.json"
CFG="$CODEX_HOME/config.toml"

if [[ ! -f "$SRC" ]]; then
  echo "missing $SRC" >&2
  exit 1
fi
mkdir -p "$CODEX_HOME"
cp "$SRC" "$DEST"
chmod 600 "$DEST"

if [[ ! -f "$CFG" ]]; then
  echo "missing $CFG — create Codex config first" >&2
  exit 1
fi

KEY='model_catalog_json'
# Must live at file root — appending after [tui.*] tables breaks TOML parsing.
grep -v "^${KEY}" "$CFG" > "${CFG}.tmp" && mv "${CFG}.tmp" "$CFG"
{
  echo "# OpenCode Go models (reasonix-lb); see scripts/opencode-go-codex-models.json"
  echo "${KEY} = \"${DEST}\""
  echo
  cat "$CFG"
} > "${CFG}.tmp" && mv "${CFG}.tmp" "$CFG"

echo "installed catalog -> $DEST"
echo "updated $CFG (${KEY})"