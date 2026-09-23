#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-2.6.2}"
PKG_NAME="feigrampub-${VERSION}"
RELEASE_DIR="${ROOT_DIR}/release"
# 使用时间戳 staging 目录：staging 永远不会预先存在，从而无需对其执行批量删除。
# 背景：沙箱 safe-delete 守卫会拦截 >5000 文件的 rm -rf，曾使构建以 FPK_EXIT=2 失败。
STAMP="$(date +%s)"
WORK_DIR="${RELEASE_DIR}/${PKG_NAME}-${STAMP}"
PACKAGE_SRC="${ROOT_DIR}/fnos-native-package"

"${ROOT_DIR}/scripts/prepare-native-runtime.sh"
"${ROOT_DIR}/scripts/prepare-go-downloader.sh"
npm --prefix "${ROOT_DIR}/client" run build
# 打包直接取 client/dist（见下方 app/server/public 拷贝），不再同步到 server/public：
# 对 server/public 的批量 rm 会触发沙箱 safe-delete 守卫（5000 文件阈值，FPK_EXIT=2）。

rm -rf "${RELEASE_DIR}/${PKG_NAME}.fpk"
mkdir -p "${WORK_DIR}"
cp -R "${PACKAGE_SRC}/." "${WORK_DIR}/"
rm -rf "${WORK_DIR}/app/server"
mkdir -p "${WORK_DIR}/app/server"
cp "${ROOT_DIR}/server/package.json" "${ROOT_DIR}/server/package-lock.json" "${WORK_DIR}/app/server/"
cp -R "${ROOT_DIR}/server/src" "${WORK_DIR}/app/server/"
mkdir -p "${WORK_DIR}/app/server/public"
cp -R "${ROOT_DIR}/client/dist/." "${WORK_DIR}/app/server/public/"
npm --prefix "${WORK_DIR}/app/server" ci --omit=dev

find "${WORK_DIR}" -name ".DS_Store" -delete
find "${WORK_DIR}/cmd" -type f -exec chmod +x {} \;
chmod +x "${WORK_DIR}/app/ui/proxy.cgi" "${WORK_DIR}/app/bin/node" "${WORK_DIR}/app/bin/feigram-downloader"

(
  cd "${WORK_DIR}"
  mkdir -p app_payload
  cp -R app/server app_payload/server
  cp -R app/bin app_payload/bin
  cp -R app/ui app_payload/ui
  cp -R config app_payload/config
  tar -czf app.tgz -C app_payload server bin ui config
  # 不删除 app / app_payload（批量 rm 会触发沙箱 safe-delete 守卫），
  # 改为在打包时按顶层条目排除，产出的 FPK 内容与删除后再打包完全等价。
  entries=()
  for entry in *; do
    case "${entry}" in
      app|app_payload) continue ;;
    esac
    entries+=("${entry}")
  done
  tar -czf "${RELEASE_DIR}/${PKG_NAME}.fpk" "${entries[@]}"
)

ls -lh "${RELEASE_DIR}/${PKG_NAME}.fpk"
