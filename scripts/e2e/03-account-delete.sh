#!/bin/bash
# E2E 03 · Go 账号记录 DELETE 链路（零预置数据）。
# 流程：建档 → 确认存在 → DELETE → 再查 404 → native-sessions.json 无残留。
# 回归「登出/清理残留记录」依赖的删除能力。
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
source "${HERE}/lib.sh"
init_work

SIDE_PORT="$(pick_port)"; SIDE="http://127.0.0.1:${SIDE_PORT}"
DATA="${E2E_WORK}/dl"; BINARY="${E2E_WORK}/sidecar"
build_sidecar "$BINARY" || { echo "构建失败"; exit 1; }
start_sidecar "$BINARY" "$SIDE_PORT" "$DATA"

ACCOUNT_FILE="${DATA}/native-sessions.json"

echo "1) 创建一个账号记录"
CREATE=$(curl -s -m 8 --noproxy '*' -X POST "$SIDE/api/native/accounts" \
  -H 'Content-Type: application/json' \
  -d '{"userId":"u1","accountId":"del-1","phone":"+8613800000000","apiId":123456,"apiHash":"0123456789abcdef0123456789abcdef"}')
echo "    $CREATE" | head -c 200; echo
curl -s -m 5 --noproxy '*' "$SIDE/api/native/accounts/u1/del-1" | grep -q 'del-1' \
  && check "记录已创建" 0 || check "记录已创建" 1
grep -q 'del-1' "$ACCOUNT_FILE" && check "记录已落盘" 0 || check "记录已落盘" 1

echo "2) DELETE 删除该记录"
DEL=$(curl -s -m 8 --noproxy '*' -X DELETE "$SIDE/api/native/accounts/u1/del-1" -w '|%{http_code}')
CODE="${DEL##*|}"; BODY="${DEL%|*}"
echo "    HTTP $CODE body: $(echo "$BODY" | head -c 160)"
[ "$CODE" = "200" ] && check "DELETE 返回 200" 0 || check "DELETE 返回 200（实际 $CODE）" 1

echo "3) 再查应为 404，且文件中无残留"
GET=$(curl -s -m 5 --noproxy '*' -o /dev/null -w '%{http_code}' "$SIDE/api/native/accounts/u1/del-1")
[ "$GET" = "404" ] && check "删除后查询返回 404" 0 || check "删除后 404（实际 $GET）" 1
grep -q 'del-1' "$ACCOUNT_FILE" && check "磁盘文件无残留" 1 || check "磁盘文件无残留" 0

echo "4) 删除不存在的账号应返回 404"
DEL=$(curl -s -m 5 --noproxy '*' -o /dev/null -w '%{http_code}' -X DELETE "$SIDE/api/native/accounts/u/missing")
[ "$DEL" = "404" ] && check "删除缺失账号返回 404" 0 || check "删除缺失账号 404（实际 $DEL）" 1

stop_sidecar
e2e_finish
