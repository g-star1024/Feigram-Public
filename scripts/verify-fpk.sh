#!/usr/bin/env bash
# FPK 包内容级核证：不能只看「构建脚本 exit 0 / 文件名」，
# 必须解包核对版本号是否贯穿到包内各处（2.0.45 事故：包名新、包内旧）。
#
# 核对：
#   - manifest version 与目标版本一致；
#   - cmd/main APP_VERSION 与目标版本一致；
#   - app.tgz 内 releaseContent.js 置顶公告为目标版本；
#   - gzip 完整性；
#   - Go 二进制/前端 bundle 存在。
#
# 用法：bash scripts/verify-fpk.sh <version> [fpk_path]
#   fpk_path 缺省取 release/feigrampub-<version>.fpk
set -euo pipefail

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "用法：bash scripts/verify-fpk.sh <version> [fpk_path]" >&2
  exit 1
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FPK="${2:-${ROOT}/release/feigrampub-${VERSION}.fpk}"

if [ ! -f "$FPK" ]; then
  echo "找不到 FPK：$FPK" >&2
  exit 1
fi

PY="${PYTHON:-$(command -v python3 || echo /Users/huluobo/.workbuddy/binaries/python/versions/3.13.12/bin/python3)}"
ok=1
say() { if [ "$1" = "0" ]; then echo "  PASS  $2"; else echo "  FAIL  $2"; ok=0; fi; }

echo "核证 $(basename "$FPK")"

# 完整性
gzip -t "$FPK" && say 0 "gzip 完整性" || say 1 "gzip 完整性"

# manifest version
# 注意：本脚本开了 pipefail，`cmd | grep -q` 会因 grep 提前退出、上游 SIGPIPE(141)
# 而被误判为失败（概率性竞态，2026-09-22 踩中：包内确实有前端 bundle 却报"缺少"）。
# 故一律用 grep -m1 早停取首行，条件判定用 here-string（无管道）规避 SIGPIPE。
MAN_VERSION="$(tar -xzOf "$FPK" manifest | grep -E -m1 '^version=' | cut -d= -f2)"
[ "$MAN_VERSION" = "$VERSION" ] && say 0 "manifest version=${VERSION}" || say 1 "manifest version（实际 ${MAN_VERSION}）"

# cmd/main APP_VERSION
APPVER="$(tar -xzOf "$FPK" cmd/main | grep -E -m1 '^export APP_VERSION=' | sed -E 's/.*="([^"]*)".*/\1/')"
[ "$APPVER" = "$VERSION" ] && say 0 "cmd/main APP_VERSION=${VERSION}" || say 1 "cmd/main APP_VERSION（实际 ${APPVER}）"

# app.tgz 内置顶公告版本
TOP_ID="$(tar -xzOf "$FPK" app.tgz | tar -xzO ./server/src/releaseContent.js 2>/dev/null \
  | grep -E -m1 'id: "release-' | sed -E 's/.*release-([^"]*)".*/\1/')"
[ "$TOP_ID" = "$VERSION" ] && say 0 "应用内置顶公告 release-${VERSION}" || say 1 "置顶公告（实际 ${TOP_ID}）"

# 包内 Go 二进制与前端 bundle 存在（均在 app.tgz 内，归档路径无 app/ 前缀）
APP_LISTING="$(tar -xzOf "$FPK" app.tgz | tar -tz 2>/dev/null)"
if grep -Eq 'bin/feigram-downloader$' <<<"$APP_LISTING"; then
  say 0 "包含 Go 下载器二进制"
else
  say 1 "缺少 Go 下载器二进制"
fi
if grep -Eq 'server/public/assets/index-.*\.(js|css)$' <<<"$APP_LISTING"; then
  say 0 "包含前端 bundle"
else
  say 1 "缺少前端 bundle"
fi

# R4.38：判据补「依赖图可加载 + 能真起来」。
# 2.6.15 事故：verify 6/6 PASS 却起不来 —— 包内 node_modules 缺 engine.io/build/parser-v3/index.js，
# require('socket.io') 在加载期即抛 MODULE_NOT_FOUND。只查版本号/文件存在永远抓不到这一类，
# 必须把「真实加载依赖 + 真实启动监听」纳入核证。
GUARD_TMP="$(mktemp -d)"
if tar -xzOf "$FPK" app.tgz | tar -xz -C "$GUARD_TMP" server 2>/dev/null; then
  if bash "${ROOT}/scripts/check-server-deps.sh" "${GUARD_TMP}/server" --boot >"${GUARD_TMP}/guard.log" 2>&1; then
    say 0 "包内 server 依赖真实加载 + 启动冒烟（/api/health 200）"
  else
    say 1 "包内 server 依赖真实加载 + 启动冒烟"
    sed 's/^/        /' "${GUARD_TMP}/guard.log" >&2 || true
  fi
else
  say 1 "解包 app.tgz 内 server 目录"
fi
rm -rf "$GUARD_TMP" >/dev/null 2>&1 || true

echo
if [ "$ok" = "1" ]; then
  echo "FPK 核证通过：${VERSION}"
else
  echo "FPK 核证失败：${VERSION}"
  exit 1
fi
