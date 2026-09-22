#!/bin/bash
# E2E 02 · 首次登录自动建档（零预置数据）。
# 回归 2026-09-22「native account is not prepared」事故：
# 用真实二进制 + 全新数据目录，不预建任何账号记录，走真实用户首次登录路径。
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
source "${HERE}/lib.sh"
init_work

SIDE_PORT="$(pick_port)"; SIDE="http://127.0.0.1:${SIDE_PORT}"
DATA="${E2E_WORK}/dl"; BINARY="${E2E_WORK}/sidecar"; DEAD_PORT="$(pick_port)"
build_sidecar "$BINARY" || { echo "构建失败"; exit 1; }

echo "启动全新 sidecar（空数据目录）"
start_sidecar "$BINARY" "$SIDE_PORT" "$DATA"
HEALTH=$(curl -s -m 3 --noproxy '*' "$SIDE/health")
echo "$HEALTH" | grep -q '"ok":true' && check "sidecar 启动" 0 || check "sidecar 启动" 1

echo "1) 全新账号 auth/start（未预建档），凭据缺失应报明确错误"
BODY=$(curl -s -m 8 --noproxy '*' -X POST "$SIDE/api/auth/start" \
  -H 'Content-Type: application/json' \
  -d '{"userID":"u1","accountID":"fresh-no-cred","phone":"+8613800000000"}')
echo "    resp: $(echo "$BODY" | head -c 160)"
echo "$BODY" | grep -q 'apiId/apiHash is required' && check "缺凭据报 apiId/apiHash is required" 0 || check "缺凭据报 apiId/apiHash is required" 1
echo "$BODY" | grep -q 'not prepared' && check "不再出现 not prepared" 1 || check "不再出现 not prepared" 0

echo "2) 不可达代理下全新账号 auth/start 应走到网络层（自动建档生效）"
curl -s -m 8 --noproxy '*' -X PUT "$SIDE/api/config" -H 'Content-Type: application/json' \
  -d "{\"proxyUrl\":\"socks5://127.0.0.1:${DEAD_PORT}\"}" >/dev/null
sleep 1
START=$(date +%s)
BODY=$(curl -s -m 60 --noproxy '*' -X POST "$SIDE/api/auth/start" \
  -H 'Content-Type: application/json' \
  -d '{"userID":"u1","accountID":"fresh-1","phone":"+8613800000000","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}')
ELAPSED=$(( $(date +%s) - START ))
echo "    耗时 ${ELAPSED}s resp: $(echo "$BODY" | head -c 220)"
echo "$BODY" | grep -q 'not prepared' && check "fresh 账号不再报 not prepared" 1 || check "fresh 账号不再报 not prepared" 0
echo "$BODY" | grep -q '已配置代理但连接 Telegram DC 仍失败' && check "超时响应带代理诊断提示" 0 || check "超时响应带代理诊断提示" 1

echo "3) fresh-1 记录已自动落盘，且含请求里的手机号与 apiId"
ACC=$(curl -s -m 8 --noproxy '*' "$SIDE/api/native/accounts/u1/fresh-1")
echo "    $(echo "$ACC" | head -c 200)"
echo "$ACC" | "$PY" -c "import json,sys
d=json.load(sys.stdin)
sys.exit(0 if d.get('phone')=='+8613800000000' and d.get('apiId')==123456 else 1)" \
  && check "记录含请求里的手机号与 apiId" 0 || check "记录含请求里的手机号与 apiId" 1

stop_sidecar
e2e_finish
