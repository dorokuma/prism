#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${PATH}:/usr/local/go/bin"
go test ./...
go build -o reasonix-lb .
install -m 755 reasonix-lb /usr/local/bin/reasonix-lb
systemctl restart reasonix-lb
echo "installed $(/usr/local/bin/reasonix-lb 2>&1 | head -0 || true) -> /usr/local/bin/reasonix-lb (restart reasonix-lb)"