#!/bin/bash
# deploy_health.sh — deploy.sh 的健康轮询 helper（可独立测试，无副作用）
#
# 用法：被 deploy.sh source 后调用两个函数；参数经环境变量传入：
#   HEALTH_URL            健康检查 URL（默认 http://127.0.0.1:18790/ready）
#   HEALTH_TIMEOUT        健康等待窗口秒数（默认 35；正整数）
#   HEALTH_CURL_MAX_TIME  单次 curl --max-time 秒数（默认 5；正整数）
#   SYSTEMCTL_BIN         覆盖 systemctl 命令（测试注入假命令，默认 systemctl）
#   CURL_BIN              覆盖 curl 命令（测试注入假命令，默认 curl）
#
# 本文件只做只读轮询：不编译、不安装、不重启、不写文件。
# 设计依据：prism 先 Listen 再跑启动探活。/health 进程起来就 200；deploy
# 默认打 /ready（fail-closed）。默认 35s 覆盖单账号 ProbeTimeout=30s 加余量，
# 成功后立即继续、不空等。
# 检查目标为 /ready（readiness）：仅当至少一个账号 healthy 且不在
# cooldown 时才返回 200；/health（liveness）进程起来就 200，无法区分
# “服务已启动但所有账号不可用”。旧部署可用 HEALTH_URL 覆盖回 /health。
#
# /ready 是 fail-closed 的部署闸门：所有账号 cooldown/exhausted 时健康
# 验证失败 → deploy.sh 自动回滚（设计意图：把账号状态搞坏的新版本绝不
# 该上线）。只想要 liveness 级检查时显式设 HEALTH_URL=/health 覆盖；
# 不要靠把 HEALTH_TIMEOUT 拉到数分钟来“等”全账号恢复。
# 部署前若本轮 HEALTH_URL 以 /ready 结尾且旧进程这次已经失败，deploy.sh
# 会把本轮检查改成同 host 的 /health，避免把「上游本来就挂」当成新版本
# 失败而回滚并二次重启。

# validate_health_timeout: HEALTH_TIMEOUT 必须为正整数。非法返回 1，
# 由调用方决定退出码（deploy.sh 在前置阶段校验失败时 exit 3，发生在任何
# 编译/安装/重启副作用之前，prism 不受影响）。
validate_health_timeout() {
  local t="${HEALTH_TIMEOUT:-}"
  case "$t" in
    ''|*[!0-9]*) return 1 ;;
  esac
  # 纯数字但为 0 或超出 test 整数范围 → 非法。2>/dev/null 吞掉大数报错，
  # || 接 return 1，保证不触发 set -e 退出。
  [ "$t" -ge 1 ] 2>/dev/null || return 1
  return 0
}

# validate_health_curl_max_time: HEALTH_CURL_MAX_TIME 必须为正整数。
# 非法返回 1，由调用方在副作用前 exit 3。
validate_health_curl_max_time() {
  local t="${HEALTH_CURL_MAX_TIME:-}"
  case "$t" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$t" -ge 1 ] 2>/dev/null || return 1
  return 0
}

# effective_health_curl_max_time: min(HEALTH_CURL_MAX_TIME, HEALTH_TIMEOUT)
# 避免单次 curl 比整个健康窗口还长。
effective_health_curl_max_time() {
  local curl_t="${HEALTH_CURL_MAX_TIME:-5}"
  local win_t="${HEALTH_TIMEOUT:-35}"
  if [ "$curl_t" -gt "$win_t" ] 2>/dev/null; then
    printf '%s\n' "$win_t"
  else
    printf '%s\n' "$curl_t"
  fi
}

# is_ready_health_url: URL 以 /ready 结尾（不含 query/fragment 时的默认部署地址）。
is_ready_health_url() {
  case "${1:-}" in
    */ready) return 0 ;;
    *) return 1 ;;
  esac
}

# liveness_url_from_ready: .../ready → .../health；其它 URL 原样返回。
liveness_url_from_ready() {
  local url="${1:-}"
  case "$url" in
    */ready)
      printf '%s\n' "${url%/ready}/health"
      ;;
    *)
      printf '%s\n' "$url"
      ;;
  esac
}

# curl_health: 带 --max-time 的单次探测。无超时的 curl 禁止使用。
curl_health() {
  local url="${1:-$HEALTH_URL}"
  local curl_bin="${CURL_BIN:-curl}"
  local max_time
  max_time="$(effective_health_curl_max_time)"
  "$curl_bin" -sf --max-time "$max_time" "$url" >/dev/null
}

# wait_healthy: 每 1 秒轮询一次 `systemctl is-active --quiet prism` 与
# `curl -sf --max-time $HEALTH_URL`；两者同时成功立即返回 0；HEALTH_TIMEOUT
# 秒内未就绪返回 1（调用方决定回滚）。语义与既有单次检查一致
# （is-active --quiet + curl -sf，curl 4xx/5xx 视为失败）。
wait_healthy() {
  local deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
  local systemctl_bin="${SYSTEMCTL_BIN:-systemctl}"
  while :; do
    if "$systemctl_bin" is-active --quiet prism && curl_health "$HEALTH_URL"; then
      return 0
    fi
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 1
  done
}
