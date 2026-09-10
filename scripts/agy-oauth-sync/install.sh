#!/usr/bin/env bash
# Install Gemini OAuth dual-write unit fragments.
# Copies files only. Does NOT daemon-reload, enable, start, or restart.
# See README.md in this directory.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
DEST_UNIT="${DEST_UNIT:-/etc/systemd/system}"
DEST_DROPIN="${DEST_DROPIN:-/etc/systemd/system/prism.service.d}"
AGY_TOKEN_PATH="${AGY_TOKEN_PATH:-/root/.gemini/antigravity-cli/antigravity-oauth-token}"

if [[ ! -f "$AGY_TOKEN_PATH" ]]; then
  echo "error: agy token file does not exist: $AGY_TOKEN_PATH" >&2
  echo "log in with agy (or prism auth google) before installing dual-write units" >&2
  exit 1
fi

install -d -m 755 "$DEST_UNIT" "$DEST_DROPIN"
install -m 644 "$ROOT/agy-oauth-token.path" "$DEST_UNIT/agy-oauth-token.path"
install -m 644 "$ROOT/agy-oauth-token-acl.service" "$DEST_UNIT/agy-oauth-token-acl.service"
install -m 644 "$ROOT/prism-agy-oauth.conf" "$DEST_DROPIN/agy-oauth.conf"

echo "Installed unit fragments:"
echo "  $DEST_UNIT/agy-oauth-token.path"
echo "  $DEST_UNIT/agy-oauth-token-acl.service"
echo "  $DEST_DROPIN/agy-oauth.conf"
echo
echo "This script did not touch a running service. Operator next steps:"
echo "  systemctl daemon-reload"
echo "  systemctl enable --now agy-oauth-token.path"
echo "  systemctl start agy-oauth-token-acl.service"
echo "  systemctl restart prism"
