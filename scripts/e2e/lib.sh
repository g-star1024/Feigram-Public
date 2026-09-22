#!/bin/bash
# E2E 公共库：为各用例提供真实二进制构建、临时工作区、端口分配、
# 健康等待、断言与统一清理。只被 source，不直接执行。
#
# 约定：
#   - 所有用例都必须用真实 Go 二进制（不 mock HTTP 层）；
#   - 至少一条「零预置数据」路径，避免预建档掩盖首次使用问题；
#   - 临时产物全部放在 E2E_WORK 下，退出时一律 trap 清理。

set -u

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${E2E_DIR}/../.." && pwd)"
PY="${PYTHON:-$(command -v python3 || echo /Users/huluobo/.workbuddy/binaries/python/versions/3.13.12/bin/python3)}"

# 全局计数（用例脚本 source 后各自归零亦可）。
E2E_PASS=0
E2E_FAIL=0

# check <描述> <0|非0>：按结果打印并计数。
check() {
  if [ "$2" = "0" ]; then
    echo "  PASS  $1"
    E2E_PASS=$((E2E_PASS + 1))
  else
    echo "  FAIL  $1"
    E2E_FAIL=$((E2E_FAIL + 1))
  fi
}

# jget：从 stdin 的 JSON 读取一个键的原始值（依赖 python3）。
jget() {
  "$PY" -c "import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
for part in sys.argv[1].split('.'):
    if isinstance(d, dict):
        d=d.get(part)
    else:
        sys.exit(1)
print(json.dumps(d, ensure_ascii=False) if not isinstance(d, str) else d)" "$1"
}

# wait_health <base_url> [次数]：轮询 /health 直到可达。
wait_health() {
  local url="$1" tries="${2:-30}" i
  for i in $(seq 1 "$tries"); do
    if curl -s -m 2 --noproxy '*' "${url}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# pick_port：取一个当前空闲的高端口。
pick_port() {
  "$PY" -c "import socket
s=socket.socket()
s.bind(('127.0.0.1',0))
print(s.getsockname()[1])
s.close()"
}

# build_sidecar <output_path>：构建真实 Go 二进制。
build_sidecar() {
  (cd "${REPO_ROOT}/downloader" && go build -o "$1" ./cmd/feigram-downloader)
}

# start_sidecar <binary> <port> <data_dir>：后台启动并把 PID 写入全局 SIDECAR_PID。
start_sidecar() {
  local binary="$1" port="$2" data="$3"
  mkdir -p "$data"
  FEIGRAM_DOWNLOADER_PORT="$port" FEIGRAM_DOWNLOADER_DATA="$data" \
    "$binary" > "${E2E_WORK}/sidecar-${port}.log" 2>&1 &
  SIDECAR_PID=$!
  wait_health "http://127.0.0.1:${port}"
}

stop_sidecar() {
  if [ -n "${SIDECAR_PID:-}" ]; then
    kill "$SIDECAR_PID" >/dev/null 2>&1 || true
    wait "$SIDECAR_PID" 2>/dev/null || true
    SIDECAR_PID=""
  fi
}

# init_work：建立一次性工作区并注册退出清理。
init_work() {
  E2E_WORK="$(mktemp -d "${TMPDIR:-/tmp}/feigram-e2e.XXXXXX")"
  trap 'e2e_cleanup' EXIT INT TERM
}

e2e_cleanup() {
  stop_sidecar
  # 停掉用例启动的代理桩。
  if [ -n "${STUB_PIDS:-}" ]; then
    # shellcheck disable=SC2086
    kill $STUB_PIDS >/dev/null 2>&1 || true
  fi
  rm -rf "$E2E_WORK"
}

# start_proxy_stub <mode socks5|http> <port> <logfile>：启动代理桩并登记 PID。
start_proxy_stub() {
  "$PY" "${E2E_DIR}/proxy-stub.py" --port "$2" --mode "$1" --log "$3" \
    >/dev/null 2>&1 &
  STUB_PIDS="${STUB_PIDS:-} $!"
  sleep 0.6
}

# finish：打印汇总并以失败数作为退出码。
e2e_finish() {
  echo
  echo "RESULT: PASS=${E2E_PASS} FAIL=${E2E_FAIL}"
  [ "$E2E_FAIL" = "0" ]
}
