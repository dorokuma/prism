#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${PATH}:/usr/local/go/bin"
go test ./...
go build -o prism .
install -m 755 prism /usr/local/bin/prism
systemctl restart prism
echo "installed $(/usr/local/bin/prism 2>&1 | head -0 || true) -> /usr/local/bin/prism (restart prism)"