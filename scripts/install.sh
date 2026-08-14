#!/usr/bin/env bash
# prism 本地安装：测编 → 备份旧二进制 → 安装 → restart → 健康验证 → 失败回滚。
# 复用 scripts/deploy_health.sh，不复制 deploy.sh。
# 测试注入：BINARY / BACKUP / SYSTEMCTL_BIN / CURL_BIN / SKIP_GO_TEST / SKIP_BUILD / INSTALL_SRC。
# 退出码：0 成功；1 新版失败已回退（或首次安装无备份可回）；2 回退后仍不健康或恢复失败；3 前置失败。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
export PATH="${PATH}:/usr/local/go/bin"

# shellcheck disable=SC1091
source "$ROOT/scripts/deploy_health.sh"

BINARY="${BINARY:-/usr/local/bin/prism}"
BACKUP="${BACKUP:-${BINARY}.bak}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:18790/ready}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-35}"
HEALTH_CURL_MAX_TIME="${HEALTH_CURL_MAX_TIME:-5}"
SYSTEMCTL_BIN="${SYSTEMCTL_BIN:-systemctl}"

if ! validate_health_timeout; then
  echo "HEALTH_TIMEOUT must be a positive integer, got '${HEALTH_TIMEOUT:-<empty>}' — aborting before any change" >&2
  exit 3
fi
if ! validate_health_curl_max_time; then
  echo "HEALTH_CURL_MAX_TIME must be a positive integer, got '${HEALTH_CURL_MAX_TIME:-<empty>}' — aborting before any change" >&2
  exit 3
fi

if [ "${SKIP_GO_TEST:-}" != "1" ]; then
  go test ./...
fi

# 构建 ./cmd/prism（main 包在 cmd/prism，不是仓库根）。临时产物放 /tmp，
# 安装后清理，不弄脏仓库工作树。
tmpbin="$(mktemp /tmp/prism-install.XXXXXX)"
trap 'rm -f "$tmpbin"' EXIT

if [ "${SKIP_BUILD:-}" = "1" ]; then
  if [ -z "${INSTALL_SRC:-}" ] || [ ! -f "${INSTALL_SRC}" ]; then
    echo "SKIP_BUILD=1 requires INSTALL_SRC to point at an existing file" >&2
    exit 3
  fi
  cp -a "$INSTALL_SRC" "$tmpbin"
  chmod 755 "$tmpbin"
else
  go build -o "$tmpbin" ./cmd/prism
fi

had_backup=0
if [ -e "$BINARY" ]; then
  if ! cp -a "$BINARY" "$BACKUP"; then
    echo "备份失败，中止（prism 未受影响）" >&2
    exit 3
  fi
  had_backup=1
fi

if ! install -m 755 "$tmpbin" "$BINARY"; then
  echo "INSTALL FAILED — 新二进制未装上"
  if [ "$had_backup" = "1" ]; then
    if ! install -m 755 "$BACKUP" "$BINARY"; then
      echo "CRITICAL: 从备份恢复也失败，$BINARY 可能缺失或损坏，需人工介入" >&2
      exit 2
    fi
    echo "已从备份恢复旧二进制，中止安装"
  fi
  exit 3
fi

# 安装前 /ready 基线：新二进制已 install，进程仍是旧 inode。
# 若默认 /ready 此刻已经失败，本轮改打 /health，避免把「上游本来就挂」当成新版本失败。
if is_ready_health_url "$HEALTH_URL"; then
  if ! curl_health "$HEALTH_URL"; then
    echo "WARN: pre-restart $HEALTH_URL failed (old process not ready or down); this install will probe /health so an already-unready pool is not treated as a new-binary failure"
    HEALTH_URL="$(liveness_url_from_ready "$HEALTH_URL")"
  fi
fi

echo "=== systemctl restart 加载新二进制 ==="
# restart 失败绝不静默继续：新二进制已 install，服务状态未知，立即停止安装
# 并保留备份供人工回退/诊断（旧版 `|| true` 会带着可能已停止的服务跑健康检查）。
if ! "$SYSTEMCTL_BIN" restart prism; then
  echo "RESTART FAILED — systemctl restart prism 失败；新二进制已 install，备份保留在 $BACKUP。停止安装，请人工诊断：systemctl status prism / journalctl -u prism -n 50" >&2
  exit 3
fi

echo "=== 健康验证（每 1 秒轮询 systemctl active + $HEALTH_URL，最长 ${HEALTH_TIMEOUT}s）==="
if wait_healthy; then
  echo "installed -> $BINARY (restart prism)"
  if [ "$had_backup" = "1" ]; then
    rm -f "$BACKUP" || echo "WARN: 删除备份 $BACKUP 失败（保留备份不影响运行）"
  fi
  exit 0
fi

echo "=== 健康验证失败 ==="
if [ "$had_backup" != "1" ]; then
  echo "无备份可回滚（首次安装），$BINARY 保留新文件" >&2
  exit 1
fi

if ! install -m 755 "$BACKUP" "$BINARY"; then
  echo "CRITICAL: 回退安装失败，$BINARY 可能缺失或损坏，需人工介入" >&2
  exit 2
fi
# 回退后的 restart 同样不能吞失败：失败意味着服务可能已停机，需人工。
if ! "$SYSTEMCTL_BIN" restart prism; then
  echo "CRITICAL: 回退后 systemctl restart prism 失败，prism 可能已停机，需人工介入" >&2
  exit 2
fi
if wait_healthy; then
  echo "ROLLBACK OK: 已回退旧版本（新版本启动失败）"
  exit 1
fi

echo "CRITICAL: 回退后仍不健康，prism 可能已停机，需人工介入" >&2
exit 2
