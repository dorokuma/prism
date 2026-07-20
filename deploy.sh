#!/bin/bash
# prism 一键部署：编译 → 停止服务 → 原子替换二进制 → 重启 → 验证
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="/usr/local/bin/prism"

echo "=== 编译 ==="
cd "$ROOT"
go build -o ./bin/prism . 2>&1
echo "BUILD OK ($(du -h ./bin/prism | cut -f1))"

echo "=== 停止旧进程 ==="
if systemctl is-active --quiet prism 2>/dev/null; then
  systemctl stop prism 2>&1 && echo "stopped prism.service"
  # systemd 停服后等待进程退出
  for i in $(seq 1 15); do
    if ! systemctl is-active --quiet prism 2>/dev/null; then
      break
    fi
    sleep 1
  done
else
  # 回退：直接 kill
  OLD_PID=$(pgrep -x prism 2>/dev/null || true)
  if [ -n "$OLD_PID" ]; then
    kill "$OLD_PID" 2>/dev/null && echo "killed pid $OLD_PID" || echo "no process to kill"
    sleep 2
  else
    echo "prism 未运行"
  fi
fi

echo "=== 替换二进制 ==="
install -m 755 ./bin/prism "$BINARY"
echo "DEPLOYED → $BINARY"

echo "=== 启动服务 ==="
if systemctl list-unit-files prism.service &>/dev/null; then
  systemctl start prism 2>&1
  sleep 2
  if systemctl is-active --quiet prism 2>/dev/null; then
    echo "prism.service started OK"
  else
    echo "WARN: prism.service 启动失败，检查 journalctl -u prism"
  fi
else
  echo "WARN: 无 prism.service，跳过 systemctl 启动"
fi

echo "=== 验证 ==="
# prism 无 version 子命令，通过运行二进制（不传 config）检查是否正常
"$BINARY" 2>&1 | head -3 || true
echo "二进制就绪: $BINARY"

echo "=== 提交推送 ==="
cd "$ROOT"
git add -A
if git diff --cached --quiet; then
  echo "无改动，跳过提交"
else
  TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
  git commit -m "deploy ${TAG}" 2>&1 || echo "commit 无改动"
  git push 2>&1 || echo "push 失败（无远程或网络问题）"
  echo "PUSHED"
fi

echo "=== 完成 ==="
