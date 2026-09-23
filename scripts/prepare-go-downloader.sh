#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_VERSION="${GO_VERSION:-1.22.12}"
# R4.25：把包版本注入二进制（-X main.version），使 Go 日志首行与 FPK 安装包版本一致。
# 调用方（build-native-fpk.sh）以环境变量传入；单独调用时回落到 dev。
APP_VERSION="${VERSION:-dev}"
RUNTIME_DIR="${ROOT_DIR}/release/go-runtime"
GO_ROOT="${RUNTIME_DIR}/go"
GO_BIN="${GO_ROOT}/bin/go"
TARGET="${ROOT_DIR}/fnos-native-package/app/bin/feigram-downloader"

host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch="$(uname -m)"
case "${host_arch}" in
  x86_64|amd64) host_arch="amd64" ;;
  arm64|aarch64) host_arch="arm64" ;;
  *) echo "Unsupported build host architecture: ${host_arch}" >&2; exit 1 ;;
esac
case "${host_os}" in
  darwin|linux) ;;
  *) echo "Unsupported build host OS: ${host_os}" >&2; exit 1 ;;
esac

archive="go${GO_VERSION}.${host_os}-${host_arch}.tar.gz"
url="https://go.dev/dl/${archive}"

mkdir -p "${RUNTIME_DIR}" "$(dirname "${TARGET}")"
if [ ! -x "${GO_BIN}" ]; then
  rm -rf "${GO_ROOT}" "${RUNTIME_DIR:?}/${archive}"
  echo "Downloading Go ${GO_VERSION} toolchain for ${host_os}-${host_arch}..."
  curl -fsSL "${url}" -o "${RUNTIME_DIR}/${archive}"
  tar -xzf "${RUNTIME_DIR}/${archive}" -C "${RUNTIME_DIR}"
fi

echo "Building Feigram Go downloader sidecar (version=${APP_VERSION})..."
(
  cd "${ROOT_DIR}/downloader"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "${GO_BIN}" build -trimpath \
    -ldflags "-s -w -X main.version=${APP_VERSION}" \
    -o "${TARGET}" ./cmd/feigram-downloader
)
chmod +x "${TARGET}"
ls -lh "${TARGET}"
