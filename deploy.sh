#!/bin/bash
# prism 部署：编译 → 原子替换二进制 → systemctl restart → 健康验证 → 失败自动回退
#
# 本脚本只做机械部署，不做 git。
# 原因：commit message / Changelog 内容 / tag 号每次都不同，需调用方（主代理）判断，
# 写死在脚本里无意义。调用方负责：部署前改 README+commit+tag（本地），部署成功后再 push。
#
# 部署前提：代码改动已 commit + tag（本地）。本脚本编译当前工作区代码。
# 退出码：0 成功；1 新版失败已回退旧版；2 回退后仍不健康或恢复失败（需人工）；3 前置失败（编译/备份/安装，prism 未受影响）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="/usr/local/bin/prism"
BACKUP="${BINARY}.bak"
HEALTH_URL="http://127.0.0.1:18790/health"

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

echo "=== systemctl restart 加载新二进制（停机窗口仅 restart 瞬间，不单独 stop）==="
systemctl restart prism || true
sleep 2

echo "=== 健康验证 ==="
if systemctl is-active --quiet prism && curl -sf "$HEALTH_URL" >/dev/null; then
  echo "DEPLOY OK: prism active + health ok"
  rm -f "$BACKUP" || echo "WARN: 删除备份 $BACKUP 失败（保留备份不影响运行）"
  exit 0
fi

echo "=== 健康验证失败，自动回退到旧二进制 ==="
# 回退安装同样不能静默失败：失败意味着路径上二进制可能缺失/损坏，需人工。
if ! install -m 755 "$BACKUP" "$BINARY"; then
  echo "CRITICAL: 回退安装失败，$BINARY 可能缺失或损坏，需人工介入"
  exit 2
fi
systemctl restart prism || true
sleep 2
if systemctl is-active --quiet prism && curl -sf "$HEALTH_URL" >/dev/null; then
  echo "ROLLBACK OK: 已回退旧版本（新版本启动失败）。代码未 push，可 git reset 撤销"
  echo "查失败原因：journalctl -u prism -n 50 --no-pager"
  exit 1
fi

echo "CRITICAL: 回退后仍不健康，prism 可能已停机，需人工介入"
echo "journalctl -u prism -n 50 --no-pager"
exit 2