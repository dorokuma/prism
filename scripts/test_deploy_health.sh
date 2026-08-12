#!/bin/bash
# test_deploy_health.sh — deploy_health.sh 的最小行为测试（无副作用：不接触
# 真实 systemctl/curl/服务，全部走注入的假命令与临时目录）。
# 运行：bash scripts/test_deploy_health.sh   （退出 0 = 全部通过）
set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/deploy_health.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# 假 systemctl / curl，行为由环境变量控制：
#   FAKE_ACTIVE: is-active 的退出码（默认 0 = active）
#   FAKE_CURL:   curl 的退出码（默认 0 = 200 ok）
#   FAKE_FLAG:   设置后 is-active 以该文件是否存在为准（延迟恢复模拟）
mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<'EOF'
#!/bin/bash
if [ "${1:-}" = "is-active" ]; then
  if [ -n "${FAKE_FLAG:-}" ]; then
    [ -f "$FAKE_FLAG" ] && exit 0 || exit 1
  fi
  exit "${FAKE_ACTIVE:-0}"
fi
exit 0
EOF
cat > "$TMP/bin/curl" <<'EOF'
#!/bin/bash
exit "${FAKE_CURL:-0}"
EOF
chmod +x "$TMP/bin"/*

# 1. validate_health_timeout：非法值全部拒绝
for bad in "" abc -1 0 1.5 "12 " " 12"; do
  if (
    HEALTH_TIMEOUT="$bad"
    source "$SRC"
    validate_health_timeout
  ) 2>/dev/null; then
    fail "validate accepted invalid HEALTH_TIMEOUT='$bad'"
  else
    pass "validate rejects HEALTH_TIMEOUT='$bad'"
  fi
done

# 2. validate_health_timeout：合法值接受
for good in 1 35 3600; do
  if (
    HEALTH_TIMEOUT="$good"
    source "$SRC"
    validate_health_timeout
  ) 2>/dev/null; then
    pass "validate accepts HEALTH_TIMEOUT=$good"
  else
    fail "validate rejected valid HEALTH_TIMEOUT=$good"
  fi
done

# 3. 立即成功：不等待整个窗口（HEALTH_TIMEOUT=10 时须远快于 10s 返回 0）
start=$(date +%s)
if (
  HEALTH_TIMEOUT=10
  HEALTH_URL=http://x/health
  SYSTEMCTL_BIN="$TMP/bin/systemctl"
  CURL_BIN="$TMP/bin/curl"
  source "$SRC"
  wait_healthy
); then
  elapsed=$(( $(date +%s) - start ))
  if [ "$elapsed" -le 2 ]; then
    pass "wait_healthy returns immediately on success (${elapsed}s)"
  else
    fail "wait_healthy took ${elapsed}s on immediate success — should return at once"
  fi
else
  fail "wait_healthy failed on immediately-healthy service"
fi

# 4. 延迟成功：1 秒后转健康，须在窗口内尽快返回 0（不空等整个窗口）
# 注：FAKE_FLAG 必须 export——假 systemctl 是独立进程，只读环境变量。
( sleep 1; : > "$TMP/up" ) &
start=$(date +%s)
if (
  HEALTH_TIMEOUT=8
  HEALTH_URL=http://x/health
  export FAKE_FLAG="$TMP/up"
  SYSTEMCTL_BIN="$TMP/bin/systemctl"
  CURL_BIN="$TMP/bin/curl"
  source "$SRC"
  wait_healthy
); then
  elapsed=$(( $(date +%s) - start ))
  if [ "$elapsed" -le 4 ]; then
    pass "wait_healthy succeeds after service recovers (~${elapsed}s, window 8s)"
  else
    fail "wait_healthy took ${elapsed}s after recovery — should return as soon as healthy"
  fi
else
  fail "wait_healthy failed although service recovered within the window"
fi
wait 2>/dev/null || true

# 5. 超时：一直不健康 → 窗口耗尽返回 1（HEALTH_TIMEOUT=2，约 2-3s）
start=$(date +%s)
if (
  HEALTH_TIMEOUT=2
  HEALTH_URL=http://x/health
  export FAKE_FLAG="$TMP/never"
  SYSTEMCTL_BIN="$TMP/bin/systemctl"
  CURL_BIN="$TMP/bin/curl"
  source "$SRC"
  wait_healthy
) 2>/dev/null; then
  fail "wait_healthy succeeded though service never became healthy"
else
  elapsed=$(( $(date +%s) - start ))
  if [ "$elapsed" -ge 1 ] && [ "$elapsed" -le 4 ]; then
    pass "wait_healthy times out after ~HEALTH_TIMEOUT (${elapsed}s for 2s window)"
  else
    fail "wait_healthy timeout took ${elapsed}s, expected ~2s"
  fi
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS"
  exit 0
fi
echo "$fails test(s) FAILED"
exit 1
