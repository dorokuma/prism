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
if [ -n "${FAKE_CURL_ARGV:-}" ]; then
  printf '%s\n' "$@" > "$FAKE_CURL_ARGV"
fi
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

# 5a. validate_health_curl_max_time：非法值全部拒绝
for bad in "" abc -1 0 1.5 "12 " " 12"; do
  if (
    HEALTH_CURL_MAX_TIME="$bad"
    source "$SRC"
    validate_health_curl_max_time
  ) 2>/dev/null; then
    fail "validate accepted invalid HEALTH_CURL_MAX_TIME='$bad'"
  else
    pass "validate rejects HEALTH_CURL_MAX_TIME='$bad'"
  fi
done

# 5b. validate_health_curl_max_time：合法值接受
for good in 1 5 35; do
  if (
    HEALTH_CURL_MAX_TIME="$good"
    source "$SRC"
    validate_health_curl_max_time
  ) 2>/dev/null; then
    pass "validate accepts HEALTH_CURL_MAX_TIME=$good"
  else
    fail "validate rejected valid HEALTH_CURL_MAX_TIME=$good"
  fi
done

# 5c. wait_healthy 调用 curl 时带 --max-time，且取 min(CURL_MAX, TIMEOUT)
if (
  HEALTH_TIMEOUT=10
  HEALTH_CURL_MAX_TIME=5
  HEALTH_URL=http://x/health
  export FAKE_CURL_ARGV="$TMP/curl.argv"
  SYSTEMCTL_BIN="$TMP/bin/systemctl"
  CURL_BIN="$TMP/bin/curl"
  source "$SRC"
  wait_healthy
); then
  if grep -qx -- '--max-time' "$TMP/curl.argv" && grep -qx -- '5' "$TMP/curl.argv"; then
    pass "wait_healthy curl uses --max-time 5 (min of 5 and 10)"
  else
    fail "wait_healthy curl argv missing --max-time 5: $(tr '\n' ' ' < "$TMP/curl.argv")"
  fi
else
  fail "wait_healthy failed while recording curl argv"
fi

if (
  HEALTH_TIMEOUT=2
  HEALTH_CURL_MAX_TIME=30
  HEALTH_URL=http://x/health
  export FAKE_CURL_ARGV="$TMP/curl.argv2"
  SYSTEMCTL_BIN="$TMP/bin/systemctl"
  CURL_BIN="$TMP/bin/curl"
  source "$SRC"
  wait_healthy
); then
  if grep -qx -- '--max-time' "$TMP/curl.argv2" && grep -qx -- '2' "$TMP/curl.argv2"; then
    pass "wait_healthy curl --max-time capped by HEALTH_TIMEOUT (min of 30 and 2)"
  else
    fail "wait_healthy curl argv missing --max-time 2: $(tr '\n' ' ' < "$TMP/curl.argv2")"
  fi
else
  fail "wait_healthy failed while recording capped curl argv"
fi

# 5d. /ready → /health URL helpers
if (
  source "$SRC"
  is_ready_health_url "http://127.0.0.1:18790/ready"
); then
  pass "is_ready_health_url accepts .../ready"
else
  fail "is_ready_health_url rejected default /ready URL"
fi
if (
  source "$SRC"
  is_ready_health_url "http://127.0.0.1:18790/health"
); then
  fail "is_ready_health_url accepted /health"
else
  pass "is_ready_health_url rejects /health"
fi
if (
  source "$SRC"
  is_ready_health_url "http://127.0.0.1:18790/custom"
); then
  fail "is_ready_health_url accepted custom path"
else
  pass "is_ready_health_url rejects custom path"
fi
got="$(
  source "$SRC"
  liveness_url_from_ready "http://127.0.0.1:18790/ready"
)"
if [ "$got" = "http://127.0.0.1:18790/health" ]; then
  pass "liveness_url_from_ready maps /ready to /health"
else
  fail "liveness_url_from_ready = '$got', want http://127.0.0.1:18790/health"
fi
got="$(
  source "$SRC"
  liveness_url_from_ready "https://example:443/ready"
)"
if [ "$got" = "https://example:443/health" ]; then
  pass "liveness_url_from_ready keeps host and scheme"
else
  fail "liveness_url_from_ready https = '$got'"
fi
got="$(
  source "$SRC"
  liveness_url_from_ready "http://127.0.0.1:18790/health"
)"
if [ "$got" = "http://127.0.0.1:18790/health" ]; then
  pass "liveness_url_from_ready leaves /health unchanged"
else
  fail "liveness_url_from_ready /health = '$got'"
fi

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
