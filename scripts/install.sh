#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${PATH}:/usr/local/go/bin"
go test ./...
# 构建 ./cmd/prism（main 包在 cmd/prism，不是仓库根）。临时产物放 /tmp，
# 安装后清理，不弄脏仓库工作树。
tmpbin="$(mktemp /tmp/prism-install.XXXXXX)"
trap 'rm -f "$tmpbin"' EXIT
go build -o "$tmpbin" ./cmd/prism
install -m 755 "$tmpbin" /usr/local/bin/prism
systemctl restart prism
echo "installed -> /usr/local/bin/prism (restart prism)"