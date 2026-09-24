#!/usr/bin/env bash
# server 依赖树完整性守卫（R4.38 新增，2.6.15 事故驱动）
#
# 事故回放（2.6.15）：
#   `npm ci --omit=dev` 在沙箱内**静默丢了 11 个文件**（engine.io/build/parser-v3/index.js、
#   engine.io-parser/build/cjs/contrib/base64-arraybuffer.js、socket.io-parser/build/esm-debug/binary.js、
#   bufferutil darwin-arm64 prebuild、若干 .github/FUNDING.yml）。
#   其中 engine.io/build/parser-v3/index.js 被 engine.io/build/transport.js 在**加载期** require，
#   而 server/src/index.js 第 8 行 `require("socket.io")` 是顶层加载 →
#   node 秒退 MODULE_NOT_FOUND → cmd/main 的 `sleep 2; is_running` 判定失败 → exit 1
#   → 飞牛弹出「本地应用启动失败」，日志里只有一句 MODULE_NOT_FOUND 栈。
#   而 verify-fpk.sh 当时只查「版本号贯穿 / 文件存在 / gzip 完整性」，**从不加载依赖图**，
#   于是 6/6 PASS 却起不来 —— 典型的「判据没覆盖真实故障面」。
#
# 本守卫做三件事（依次增强）：
#   1. 依赖**真实加载**：对 package.json dependencies 逐个 require()，任何 MODULE_NOT_FOUND 立刻失败；
#   2. 文件数下限：抓「整体截断」类事故（当前实测 1081，下限取 1000 的粗网）；
#   3. 真实启动冒烟（--boot）：用临时 DATA_DIR + 随机端口真跑 src/index.js，
#      等 "listening"，并 curl /api/health 断言 200 —— 只有「包能起来」才算通过。
#
# 用法：
#   bash scripts/check-server-deps.sh <server_dir> [--boot]
# 环境变量：
#   CHECK_NODE_BIN  指定 node 可执行文件（默认取 PATH 里的 node；核证 Linux 包时可指向 app/bin/node）
set -euo pipefail

SERVER_DIR="${1:-}"
BOOT="${2:-}"

if [ -z "$SERVER_DIR" ] || [ ! -d "$SERVER_DIR" ]; then
  echo "用法：bash scripts/check-server-deps.sh <server_dir> [--boot]" >&2
  exit 1
fi
if [ ! -f "${SERVER_DIR}/package.json" ]; then
  echo "找不到 ${SERVER_DIR}/package.json" >&2
  exit 1
fi

NODE_BIN="${CHECK_NODE_BIN:-}"
if [ -z "$NODE_BIN" ]; then
  if command -v node >/dev/null 2>&1; then
    NODE_BIN="$(command -v node)"
  elif [ -x "${SERVER_DIR}/../bin/node" ]; then
    NODE_BIN="${SERVER_DIR}/../bin/node"
  else
    echo "找不到 node 可执行文件（可用 CHECK_NODE_BIN 指定）" >&2
    exit 1
  fi
fi

echo "依赖树守卫：$(basename "$SERVER_DIR")   node=$( "$NODE_BIN" -v 2>/dev/null || echo '?' )"

# ---- 1. 依赖真实加载 ----
"$NODE_BIN" -e '
const { createRequire } = require("module");
const fs = require("fs");
const path = require("path");
const dir = path.resolve(process.argv[1]);
const req = createRequire(path.join(dir, "package.json"));
const pkg = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
const names = Object.keys(pkg.dependencies || {});
const failed = [];
for (const name of names) {
  try {
    req(name);
  } catch (error) {
    failed.push(`${name}: ${String(error.message).split("\n")[0]}`);
  }
}
if (failed.length) {
  console.error("依赖真实加载失败（" + failed.length + "/" + names.length + "）：");
  for (const line of failed) console.error("  - " + line);
  process.exit(1);
}
console.log(`  PASS  依赖真实加载 ${names.length}/${names.length}（含 socket.io 加载期依赖链）`);
' "$SERVER_DIR"

# ---- 2. 文件数下限（粗网，抓整体截断）----
SERVER_DEPS_MIN_FILES="${SERVER_DEPS_MIN_FILES:-1000}"
if [ -d "${SERVER_DIR}/node_modules" ]; then
  ACTUAL="$(find "${SERVER_DIR}/node_modules" -type f 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$ACTUAL" -lt "$SERVER_DEPS_MIN_FILES" ]; then
    echo "  FAIL  node_modules 文件数 ${ACTUAL} < 下限 ${SERVER_DEPS_MIN_FILES}（疑似安装被整体截断）" >&2
    exit 1
  fi
  echo "  PASS  node_modules 文件数 ${ACTUAL}（下限 ${SERVER_DEPS_MIN_FILES}）"
else
  echo "  FAIL  缺少 node_modules" >&2
  exit 1
fi

# ---- 3. 真实启动冒烟 ----
if [ "$BOOT" = "--boot" ]; then
  SMOKE_DIR="$(mktemp -d)"
  PORT="$(( (RANDOM % 2000) + 33000 ))"
  SMOKE_LOG="${SMOKE_DIR}/boot.log"
  cleanup() {
    if [ -n "${SMOKE_PID:-}" ] && kill -0 "$SMOKE_PID" >/dev/null 2>&1; then
      kill "$SMOKE_PID" >/dev/null 2>&1 || true
      sleep 1
      kill -9 "$SMOKE_PID" >/dev/null 2>&1 || true
    fi
    rm -rf "$SMOKE_DIR" >/dev/null 2>&1 || true
  }
  trap cleanup EXIT

  (
    cd "$SERVER_DIR" || exit 1
    DATA_DIR="${SMOKE_DIR}/data" \
    DOWNLOAD_DIR="${SMOKE_DIR}/data/downloads" \
    APP_PORT="$PORT" \
    PUBLIC_BASE_URL="http://127.0.0.1:${PORT}" \
    NODE_ENV="production" \
    TELEGRAM_API_ID="1" TELEGRAM_API_HASH="smoke" \
    FEIGRAM_DOWNLOADER_URL="http://127.0.0.1:1" \
    "$NODE_BIN" src/index.js >"$SMOKE_LOG" 2>&1
  ) &
  SMOKE_PID=$!
  # 断开作业表，避免收尾 kill 时 bash 把 "Terminated" 通知打到核证输出里（污染判据输出）。
  disown "$SMOKE_PID" 2>/dev/null || true

  LISTENED=0
  for _ in $(seq 1 40); do
    if grep -q "listening on" "$SMOKE_LOG" 2>/dev/null; then
      LISTENED=1
      break
    fi
    if ! kill -0 "$SMOKE_PID" >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  if [ "$LISTENED" != "1" ]; then
    echo "  FAIL  启动冒烟：进程未在 20s 内监听（退出码 $?）" >&2
    echo "  ---- boot.log 尾部 ----" >&2
    tail -25 "$SMOKE_LOG" >&2 || true
    exit 1
  fi
  echo "  PASS  启动冒烟：已监听 0.0.0.0:${PORT}"

  if command -v curl >/dev/null 2>&1; then
    CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PORT}/api/health" || echo 000)"
    if [ "$CODE" = "200" ]; then
      echo "  PASS  启动冒烟：/api/health 200"
    else
      echo "  FAIL  启动冒烟：/api/health 返回 ${CODE}" >&2
      tail -25 "$SMOKE_LOG" >&2 || true
      exit 1
    fi
  fi
  cleanup
  trap - EXIT
fi

echo "依赖树守卫通过：$(basename "$SERVER_DIR")"
