#!/bin/bash
# E2E 01 · 代理五阶段：核证应用内/环境变量代理是否真的作用到 MTProto 建连。
# 判据不是「接口 200」，而是「代理桩收到了指向 Telegram DC 的 CONNECT」。
#
# 本用例为聚焦代理，trigger_dial 会预建账号；零预置路径由 02-first-login 覆盖。
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
source "${HERE}/lib.sh"
init_work

SIDE_PORT="$(pick_port)"; SIDE="http://127.0.0.1:${SIDE_PORT}"
SOCKS_PORT="$(pick_port)"; HTTP_PORT="$(pick_port)"; DEAD_PORT="$(pick_port)"
SOCKS_LOG="${E2E_WORK}/socks.log"; HTTP_LOG="${E2E_WORK}/http.log"
DATA_DIR="${E2E_WORK}/dl"; BINARY="${E2E_WORK}/sidecar"

build_sidecar "$BINARY" || { echo "构建失败"; exit 1; }

proxy_status() {
  curl -s -m 8 --noproxy '*' "${SIDE}/health" | jget proxy
}
# 预建账号并发起登录，触发真实 MTProto 建连。
trigger_dial() {
  curl -s -m 8 --noproxy '*' -X POST "${SIDE}/api/native/accounts" \
    -H 'Content-Type: application/json' \
    -d '{"userId":"u1","accountId":"a1","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}' >/dev/null
  curl -s -m 25 --noproxy '*' -X POST "${SIDE}/api/auth/start" \
    -H 'Content-Type: application/json' \
    -d '{"userID":"u1","accountID":"a1","phone":"+8613800000000","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}' >/dev/null
}

echo "阶段 1：仅环境变量 FEIGRAM_PROXY_URL 指向 SOCKS5 桩"
start_proxy_stub socks5 "$SOCKS_PORT" "$SOCKS_LOG"
FEIGRAM_DOWNLOADER_PORT="$SIDE_PORT" FEIGRAM_DOWNLOADER_DATA="$DATA_DIR" \
  FEIGRAM_PROXY_URL="socks5://127.0.0.1:${SOCKS_PORT}" \
  "$BINARY" > "${E2E_WORK}/sidecar-1.log" 2>&1 &
SIDECAR_PID=$!
wait_health "$SIDE"
STATUS=$(proxy_status); echo "  /health.proxy = $STATUS"
echo "$STATUS" | grep -q '"enabled": true' && check "代理已启用" 0 || check "代理已启用（${STATUS}）" 1
echo "$STATUS" | grep -q "env:FEIGRAM_PROXY_URL" && check "来源识别为 env:FEIGRAM_PROXY_URL" 0 || check "来源识别为环境变量" 1
echo "$STATUS" | grep -q "socks5://127.0.0.1:${SOCKS_PORT}" && check "地址正确" 0 || check "地址正确" 1
trigger_dial; sleep 8
HITS=$(sort -u "$SOCKS_LOG" | wc -l | tr -d ' ')
[ "$HITS" -ge 1 ] && check "MTProto 建连确实经过 SOCKS5 代理（${HITS} 个目标）" 0 || check "MTProto 建连经过 SOCKS5（桩未收到）" 1

echo "阶段 2：应用内设置改用 HTTP CONNECT，应覆盖环境变量"
curl -s -m 8 --noproxy '*' -X PUT "${SIDE}/api/config" -H 'Content-Type: application/json' \
  -d "{\"proxyUrl\":\"http://127.0.0.1:${HTTP_PORT}\"}" >/dev/null
start_proxy_stub http "$HTTP_PORT" "$HTTP_LOG"
sleep 6
STATUS=$(proxy_status); echo "  /health.proxy = $STATUS"
echo "$STATUS" | grep -q '"source": "settings"' && check "应用内设置覆盖环境变量" 0 || check "应用内设置覆盖环境变量" 1
echo "$STATUS" | grep -q "http://127.0.0.1:${HTTP_PORT}" && check "地址切换为 HTTP 代理" 0 || check "地址切换为 HTTP 代理" 1
SOCKS_BEFORE=$(wc -l < "$SOCKS_LOG" | tr -d ' ')
trigger_dial; sleep 8
HTTP_HITS=$(sort -u "$HTTP_LOG" | wc -l | tr -d ' ')
SOCKS_AFTER=$(wc -l < "$SOCKS_LOG" | tr -d ' ')
[ "$HTTP_HITS" -ge 1 ] && check "MTProto 建连经过 HTTP CONNECT（${HTTP_HITS} 个目标）" 0 || check "MTProto 建连经过 HTTP CONNECT（桩未收到）" 1
[ "${SOCKS_BEFORE}" = "${SOCKS_AFTER}" ] && check "切换后 SOCKS5 桩不再收到新请求" 0 || check "切换后旧代理停用（${SOCKS_BEFORE}→${SOCKS_AFTER}）" 1

echo "阶段 3：非法代理地址必须显式报错"
curl -s -m 8 --noproxy '*' -X PUT "${SIDE}/api/config" -H 'Content-Type: application/json' \
  -d '{"proxyUrl":"socks4://127.0.0.1:1080"}' >/dev/null
sleep 1; STATUS=$(proxy_status); echo "  /health.proxy = $STATUS"
echo "$STATUS" | grep -q '"source": "invalid"' && check "非法地址标记为 invalid" 0 || check "非法地址标记为 invalid" 1
echo "$STATUS" | grep -q '"enabled": false' && check "非法地址不生成 dialer" 0 || check "非法地址不生成 dialer" 1
echo "$STATUS" | grep -q "不支持的代理协议" && check "错误信息含可读原因" 0 || check "错误信息含可读原因" 1

echo "阶段 4：代理不可达时的诊断提示（断言 auth/start 响应）"
curl -s -m 8 --noproxy '*' -X PUT "${SIDE}/api/config" -H 'Content-Type: application/json' \
  -d "{\"proxyUrl\":\"socks5://127.0.0.1:${DEAD_PORT}\"}" >/dev/null
sleep 2
START_BODY=$(curl -s -m 70 --noproxy '*' -X POST "${SIDE}/api/auth/start" \
  -H 'Content-Type: application/json' \
  -d '{"userID":"u1","accountID":"a1","phone":"+8613800000000","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}')
echo "    auth/start → $(echo "$START_BODY" | head -c 300)"
echo "$START_BODY" | grep -q '已配置代理但连接 Telegram DC 仍失败' && check "提示点明代理已配置但连不上 DC" 0 || check "提示点明代理问题" 1
echo "$START_BODY" | grep -q '网络代理' && check "提示给出「设置→网络代理」去处" 0 || check "提示给出代理设置去处" 1

echo "阶段 5：代理变更必须取消在途登录"
curl -s -m 8 --noproxy '*' -X PUT "${SIDE}/api/config" -H 'Content-Type: application/json' \
  -d "{\"proxyUrl\":\"socks5://127.0.0.1:${SOCKS_PORT}\"}" >/dev/null
sleep 2; : > "$SOCKS_LOG"
curl -s -m 5 --noproxy '*' -X POST "${SIDE}/api/auth/start" -H 'Content-Type: application/json' \
  -d '{"userID":"u1","accountID":"a1","phone":"+8613800000000","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}' >/dev/null
sleep 6
INFLIGHT=$(wc -l < "$SOCKS_LOG" | tr -d ' ')
[ "$INFLIGHT" -ge 1 ] && check "在途登录确实在使用当前代理" 0 || check "在途登录确实在拨号" 1
curl -s -m 8 --noproxy '*' -X PUT "${SIDE}/api/config" -H 'Content-Type: application/json' \
  -d "{\"proxyUrl\":\"http://127.0.0.1:${HTTP_PORT}\"}" >/dev/null
BEFORE=$(wc -l < "$SOCKS_LOG" | tr -d ' '); sleep 25
AFTER=$(wc -l < "$SOCKS_LOG" | tr -d ' ')
[ "$BEFORE" = "$AFTER" ] && check "代理变更后旧代理立即停止被拨号" 0 || check "代理变更取消在途（${BEFORE}→${AFTER}）" 1

stop_sidecar
e2e_finish
