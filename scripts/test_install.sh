#!/bin/bash
# test_install.sh — install.sh 的最小行为测试（无副作用：不接触
# 真实 systemctl/curl/go/服务，全部走注入的假命令与临时目录）。
# 运行：bash scripts/test_install.sh   （退出 0 = 全部通过）
set -euo pipefail

INSTALL="$(cd "$(dirname "$0")" && pwd)/install.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

mkdir -p "$TMP/bin"
printf 'new\n' > "$TMP/newbin"
chmod 755 "$TMP/newbin"

# 假 systemctl：restart 记一笔；RUNS_BEFORE_FAIL 控制前 N 次 restart 成功后注入
# 失败（场景 5：新版本 restart 成功、回退 restart 失败）；FAIL_RESTART=1 恒失败。
# is-active 由 RESTARTS 文件行数和 MIN_RESTARTS 决定。
cat > "$TMP/bin/systemctl" <<'EOF'
#!/bin/bash
if [ "${1:-}" = "restart" ]; then
  if [ "${FAIL_RESTART:-0}" = "1" ]; then
    echo "systemctl: restart failed (injected)" >&2
    exit 1
  fi
  if [ -n "${RUNS_BEFORE_FAIL:-}" ]; then
    n=0
    if [ -f "${RESTARTS_FILE:-}" ]; then
      n=$(wc -l < "$RESTARTS_FILE")
    fi
    if [ "$n" -ge "$RUNS_BEFORE_FAIL" ]; then
      echo "systemctl: restart failed (injected)" >&2
      exit 1
    fi
  fi
  printf 'r\n' >> "${RESTARTS_FILE:?}"
  exit 0
fi
if [ "${1:-}" = "is-active" ]; then
  n=0
  if [ -f "${RESTARTS_FILE:-}" ]; then
    n=$(wc -l < "$RESTARTS_FILE")
  fi
  min="${MIN_RESTARTS:-1}"
  [ "$n" -ge "$min" ] && exit 0
  exit 1
fi
exit 0
EOF

# 假 curl：与 is-active 同一门槛，避免「进程 active 但探测失败」的交叉。
cat > "$TMP/bin/curl" <<'EOF'
#!/bin/bash
n=0
if [ -f "${RESTARTS_FILE:-}" ]; then
  n=$(wc -l < "$RESTARTS_FILE")
fi
min="${MIN_RESTARTS:-1}"
[ "$n" -ge "$min" ] && exit 0
exit 1
EOF
chmod +x "$TMP/bin"/*

run_install() {
  BINARY="$TMP/prism" \
  BACKUP="$TMP/prism.bak" \
  HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:18790/ready}" \
  HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-8}" \
  HEALTH_CURL_MAX_TIME="${HEALTH_CURL_MAX_TIME:-1}" \
  SYSTEMCTL_BIN="$TMP/bin/systemctl" \
  CURL_BIN="$TMP/bin/curl" \
  SKIP_GO_TEST=1 \
  SKIP_BUILD=1 \
  INSTALL_SRC="$TMP/newbin" \
  RESTARTS_FILE="$TMP/restarts" \
  MIN_RESTARTS="${MIN_RESTARTS:-1}" \
  bash "$INSTALL"
}

# 1. 健康成功：旧文件被换成新文件，备份删除，exit 0
: > "$TMP/restarts"
printf 'old\n' > "$TMP/prism"
chmod 755 "$TMP/prism"
HEALTH_TIMEOUT=8
MIN_RESTARTS=1
if ( run_install ); then
  if [ "$(cat "$TMP/prism")" = "new" ] && [ ! -e "$TMP/prism.bak" ]; then
    pass "healthy install replaces binary and removes backup"
  else
    fail "healthy install left prism='$(cat "$TMP/prism" 2>/dev/null || echo missing)' bak=$( [ -e "$TMP/prism.bak" ] && echo yes || echo no )"
  fi
else
  fail "healthy install exited $? (want 0)"
fi

# 2. 健康失败且有旧二进制：回滚到旧内容，exit 1
: > "$TMP/restarts"
printf 'old\n' > "$TMP/prism"
chmod 755 "$TMP/prism"
rm -f "$TMP/prism.bak"
# 第一次 wait_healthy（restart 次数=1）失败；回滚后再 restart（次数=2）成功。
HEALTH_TIMEOUT=1
MIN_RESTARTS=2
set +e
( MIN_RESTARTS=2 HEALTH_TIMEOUT=1 run_install )
rc=$?
set -e
if [ "$rc" -eq 1 ] && [ "$(cat "$TMP/prism")" = "old" ]; then
  pass "unhealthy install rolls back to old binary (exit 1)"
else
  fail "unhealthy install rc=$rc body='$(cat "$TMP/prism" 2>/dev/null || echo missing)' (want rc=1, body=old)"
fi

# 3. 非法 HEALTH_TIMEOUT：exit 3，目标文件未被改
: > "$TMP/restarts"
printf 'old\n' > "$TMP/prism"
chmod 755 "$TMP/prism"
set +e
(
  BINARY="$TMP/prism" \
  BACKUP="$TMP/prism.bak" \
  HEALTH_TIMEOUT=abc \
  HEALTH_CURL_MAX_TIME=1 \
  SYSTEMCTL_BIN="$TMP/bin/systemctl" \
  CURL_BIN="$TMP/bin/curl" \
  SKIP_GO_TEST=1 \
  SKIP_BUILD=1 \
  INSTALL_SRC="$TMP/newbin" \
  bash "$INSTALL"
)
rc=$?
set -e
if [ "$rc" -eq 3 ] && [ "$(cat "$TMP/prism")" = "old" ]; then
  pass "invalid HEALTH_TIMEOUT exits 3 without changing the binary"
else
  fail "invalid timeout rc=$rc body='$(cat "$TMP/prism" 2>/dev/null || echo missing)' (want rc=3, body=old)"
fi

# 4. systemctl restart 失败：明确 exit 3，不再吞失败继续健康检查，备份保留
: > "$TMP/restarts"
printf 'old\n' > "$TMP/prism"
chmod 755 "$TMP/prism"
rm -f "$TMP/prism.bak"
set +e
( FAIL_RESTART=1 run_install )
rc=$?
set -e
if [ "$rc" -eq 3 ] && [ -e "$TMP/prism.bak" ]; then
  pass "systemctl restart failure exits 3 and keeps the backup (not swallowed)"
else
  fail "restart failure rc=$rc bak=$( [ -e "$TMP/prism.bak" ] && echo yes || echo no ) (want rc=3, backup kept)"
fi

# 5. 回退 restart 失败：exit 2（服务可能已停机，需人工），不再吞失败。
# 注入：RUNS_BEFORE_FAIL=1 → 第一次 restart（新版本）成功但健康验证失败
# （MIN_RESTARTS=99），进入回退；回退后的第二次 restart 失败。
: > "$TMP/restarts"
printf 'old\n' > "$TMP/prism"
chmod 755 "$TMP/prism"
rm -f "$TMP/prism.bak"
set +e
( MIN_RESTARTS=99 RUNS_BEFORE_FAIL=1 run_install )
rc=$?
set -e
if [ "$rc" -eq 2 ] && [ "$(cat "$TMP/prism")" = "old" ]; then
  pass "rollback restart failure exits 2 with the old binary restored"
else
  fail "rollback restart failure rc=$rc body='$(cat "$TMP/prism" 2>/dev/null || echo missing)' (want rc=2, body=old)"
fi

if [ "$fails" -ne 0 ]; then
  echo "FAILED: $fails"
  exit 1
fi
echo "OK"
exit 0
