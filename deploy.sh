#!/bin/bash
# prism 部署：编译 → 原子替换二进制 → systemctl restart → 健康验证 → 失败自动回退
#
# 本脚本只做机械部署，不做 git。
# 原因：commit message / Changelog 内容 / tag 号每次都不同，需调用方（主代理）判断，
# 写死在脚本里无意义。调用方负责：部署前改 README+commit+tag（本地），部署成功后再 push。
#
# 部署前提：代码改动已 commit + tag（本地）。本脚本编译当前工作区代码。
# 退出码：0 成功；1 新版失败已回退旧版；2 回退后仍不健康或恢复失败（需人工）；3 前置失败（编译/备份/安装/参数校验，prism 未受影响）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="/usr/local/bin/prism"
BACKUP="${BINARY}.bak"
# 健康检查地址：可用环境变量覆盖（如部署在非默认端口/远程探测），默认
# http://127.0.0.1:18790/ready（readiness：至少一个账号 healthy 且不在
# cooldown 才 200；/health 只是 liveness，进程起来就 200，无法发现
# “所有账号不可用”的部署）。
#
# /ready 是 fail-closed 的部署闸门：当所有账号都在 cooldown 或 exhausted
# （例如上游密钥失效/上游故障）时，健康验证必然失败并自动回滚——这是
# 设计意图，不是 bug：新版本把账号状态搞坏时绝不该被部署上线。若确实只
# 想要 liveness 级别的部署检查，显式设置 HEALTH_URL=http://127.0.0.1:18790/health
# 覆盖即可；不要把 HEALTH_TIMEOUT 盲目延长到数分钟来“等”全账号恢复——
# 那只会把一次注定失败的上线拖到很晚才回滚。
# 部署前若 HEALTH_URL 以 /ready 结尾且旧进程这次已经失败，本轮改打同 host
# 的 /health，避免把「上游本来就挂」当成新版本失败而回滚并二次重启。
# TLS 部署必须覆盖 HEALTH_URL=https://...；本脚本不猜测证书或协议。
# 未确认证书时默认保持明文回环 http://127.0.0.1:18790/ready。
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:18790/ready}"
# 健康等待窗口（秒）：进程先 Listen，启动探活在后台跑。/health 立刻 200，
# /ready 仍 fail-closed。默认 35s 等 /ready（单账号 ProbeTimeout=30s 加余量）。
# 可用环境变量覆盖；必须为正整数，非法值在任何副作用发生前 exit 3。
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-35}"
# 单次 curl --max-time（秒）。默认 5；必须为正整数。实际取值是
# min(HEALTH_CURL_MAX_TIME, HEALTH_TIMEOUT)，避免单次探测比窗口还长。
HEALTH_CURL_MAX_TIME="${HEALTH_CURL_MAX_TIME:-5}"
# shellcheck disable=SC1091
source "$ROOT/scripts/deploy_health.sh"
if ! validate_health_timeout; then
  echo "HEALTH_TIMEOUT must be a positive integer, got '${HEALTH_TIMEOUT:-<empty>}' — aborting before any change" >&2
  exit 3
fi
if ! validate_health_curl_max_time; then
  echo "HEALTH_CURL_MAX_TIME must be a positive integer, got '${HEALTH_CURL_MAX_TIME:-<empty>}' — aborting before any change" >&2
  exit 3
fi

echo "=== 编译 ==="
cd "$ROOT" || exit 3
if ! go build -o ./bin/prism ./cmd/prism; then
  echo "BUILD FAILED — prism 未受影响，仍在跑旧版本"
  exit 3
fi
echo "BUILD OK ($(du -h ./bin/prism | cut -f1))"

echo "=== 备份旧二进制 ==="
if ! cp -a "$BINARY" "$BACKUP"; then
  echo "备份失败，中止（prism 未受影响）"
  exit 3
fi
echo "BACKUP → $BACKUP"

echo "=== 原子替换二进制（运行中进程持旧 inode，不受影响继续跑）==="
# install 覆盖已存在目标时 GNU install 会先 unlink 再复制：一旦失败，目标可能
# 缺失/损坏，必须立即从备份恢复，绝不允许静默继续（否则会 restart 旧二进制却
# 报 DEPLOY OK 并删掉备份）。恢复成功=服务未受影响（进程持旧 inode）→ exit 3。
if ! install -m 755 ./bin/prism "$BINARY"; then
  echo "INSTALL FAILED — 新二进制未装上；尝试从备份恢复旧二进制"
  if ! install -m 755 "$BACKUP" "$BINARY"; then
    echo "CRITICAL: 从备份恢复也失败，$BINARY 可能缺失或损坏，需人工介入"
    exit 2
  fi
  echo "已从备份恢复旧二进制，prism 未受影响（仍在跑旧版本），中止部署"
  exit 3
fi
# 清理失败（权限/磁盘异常）不阻塞部署：新二进制已就位，残留构建目录无害。
if ! rm -rf ./bin; then
  echo "WARN: 清理 ./bin 失败（不影响部署，残留构建目录）"
fi
echo "INSTALLED → $BINARY"

# 部署前 /ready 基线：新二进制已 install，进程仍是旧 inode。
# 若默认 /ready 此刻已经失败（进程没起来或池子本来就不 ready），本轮改打
# /health，避免把「上游本来就挂」当成新版本失败而回滚并二次重启。
if is_ready_health_url "$HEALTH_URL"; then
  if ! curl_health "$HEALTH_URL"; then
    echo "WARN: pre-restart $HEALTH_URL failed (old process not ready or down); this deploy will probe /health so an already-unready pool is not treated as a new-binary failure"
    HEALTH_URL="$(liveness_url_from_ready "$HEALTH_URL")"
  fi
fi

echo "=== systemctl restart 加载新二进制（停机窗口仅 restart 瞬间，不单独 stop）==="
systemctl restart prism || true

echo "=== 健康验证（每 1 秒轮询 systemctl active + $HEALTH_URL，最长 ${HEALTH_TIMEOUT}s）==="
if wait_healthy; then
  echo "DEPLOY OK: prism active + health ok"
  rm -f "$BACKUP" || echo "WARN: 删除备份 $BACKUP 失败（保留备份不影响运行）"
  exit 0
fi

echo "=== 健康验证超时（${HEALTH_TIMEOUT}s），自动回退到旧二进制 ==="
# 回退安装同样不能静默失败：失败意味着路径上二进制可能缺失/损坏，需人工。
if ! install -m 755 "$BACKUP" "$BINARY"; then
  echo "CRITICAL: 回退安装失败，$BINARY 可能缺失或损坏，需人工介入"
  exit 2
fi
systemctl restart prism || true
# 回退后的健康验证使用同一轮询：旧版立即健康则尽快收口，不再固定 sleep。
if wait_healthy; then
  echo "ROLLBACK OK: 已回退旧版本（新版本启动失败）。代码未 push，可 git reset 撤销"
  echo "查失败原因：journalctl -u prism -n 50 --no-pager"
  exit 1
fi

echo "CRITICAL: 回退后仍不健康，prism 可能已停机，需人工介入"
echo "journalctl -u prism -n 50 --no-pager"
exit 2