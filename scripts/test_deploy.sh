#!/bin/bash
# test_deploy.sh — deploy.sh 与 scripts/install.sh 的静态行为检查。
# 不执行部署、不接触 systemctl/服务/二进制（无副作用）。
# 覆盖审计项：systemctl restart 失败不得被 `|| true` 吞掉，必须有明确退出分支。
# 运行：bash scripts/test_deploy.sh   （退出 0 = 全部通过）
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY="$ROOT/deploy.sh"
INSTALL="$ROOT/scripts/install.sh"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }

# 1. bash 语法检查（两个脚本都必须可解析）
if bash -n "$DEPLOY" && bash -n "$INSTALL"; then
  pass "bash -n syntax check (deploy.sh + install.sh)"
else
  fail "bash -n syntax check"
fi

# 2. deploy.sh 不得吞 systemctl restart 失败（不允许 `|| true` 出现在 restart 行）
if grep -n 'systemctl restart' "$DEPLOY" | grep -q '|| true'; then
  fail "deploy.sh swallows a systemctl restart failure (found '|| true')"
else
  pass "deploy.sh never swallows systemctl restart failures"
fi

# 3. deploy.sh 的每个 restart 都必须有显式失败分支（新版本 restart + 回退 restart）
n=$(grep -c 'if ! systemctl restart prism; then' "$DEPLOY")
if [ "$n" -ge 2 ]; then
  pass "deploy.sh guards systemctl restart explicitly ($n guarded restarts)"
else
  fail "deploy.sh has $n guarded restarts, want >= 2 (new-binary restart + rollback restart)"
fi

# 4. install.sh 不得吞 systemctl restart 失败
if grep -n 'restart prism' "$INSTALL" | grep -q '|| true'; then
  fail "install.sh swallows a systemctl restart failure (found '|| true')"
else
  pass "install.sh never swallows systemctl restart failures"
fi

# 5. install.sh 的每个 restart 都必须有显式失败分支
n=$(grep -Fc 'if ! "$SYSTEMCTL_BIN" restart prism; then' "$INSTALL")
if [ "$n" -ge 2 ]; then
  pass "install.sh guards systemctl restart explicitly ($n guarded restarts)"
else
  fail "install.sh has $n guarded restarts, want >= 2 (new-binary restart + rollback restart)"
fi

if [ "$fails" -ne 0 ]; then
  echo "FAILED: $fails"
  exit 1
fi
echo "OK"
exit 0
