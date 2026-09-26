#!/bin/sh
# Feigram 单容器入口：同时拉起 Go 原生下载器（3090）与 Node 网关（3088）。
# 环境变量约定与飞牛 FPK 启动脚本（fnos-native-package/cmd/main）保持同源：
#   DATA_DIR                  数据根目录（默认 /data）
#   DOWNLOAD_DIR              下载落盘目录（默认 $DATA_DIR/downloads）
#   FEIGRAM_DOWNLOADER_PORT   Go 下载器监听端口（默认 3090，容器内部）
#   FEIGRAM_DOWNLOADER_DATA   Go 下载器数据目录（默认 $DATA_DIR/downloader）
#   FEIGRAM_DOWNLOADER_SECRET_FILE  共享密钥文件（默认 $DATA_DIR/app-secret，
#                             与 Node 侧 store.js 生成的 app-secret 是同一个文件）

set -u

DATA_DIR="${DATA_DIR:-/data}"
DOWNLOAD_DIR="${DOWNLOAD_DIR:-$DATA_DIR/downloads}"
FEIGRAM_DOWNLOADER_PORT="${FEIGRAM_DOWNLOADER_PORT:-3090}"
export DATA_DIR DOWNLOAD_DIR
export FEIGRAM_DOWNLOADER_DATA="${FEIGRAM_DOWNLOADER_DATA:-$DATA_DIR/downloader}"
export FEIGRAM_DOWNLOADER_URL="${FEIGRAM_DOWNLOADER_URL:-http://127.0.0.1:${FEIGRAM_DOWNLOADER_PORT}}"
export FEIGRAM_DOWNLOADER_SECRET_FILE="${FEIGRAM_DOWNLOADER_SECRET_FILE:-$DATA_DIR/app-secret}"

mkdir -p "$DATA_DIR" "$DOWNLOAD_DIR" "$FEIGRAM_DOWNLOADER_DATA" || exit 1

echo "[entrypoint] Feigram starting: data=$DATA_DIR downloads=$DOWNLOAD_DIR downloader=$FEIGRAM_DOWNLOADER_URL"

# Go 下载器守护：异常退出 3 秒后自动重启（Node 网关对下载器短暂不可用有容错）。
downloader_loop() {
    while :; do
        /app/bin/feigram-downloader
        code=$?
        echo "[entrypoint] Go downloader exited (code=$code), restarting in 3s..."
        sleep 3
    done
}
downloader_loop &
DOWNLOADER_LOOP_PID=$!

cd /app/server || exit 1
node src/index.js &
NODE_PID=$!

shutdown() {
    echo "[entrypoint] shutting down..."
    kill "$NODE_PID" "$DOWNLOADER_LOOP_PID" 2>/dev/null || true
    # downloader_loop 被杀后其子进程（当前一轮 feigram-downloader）需一并终止
    pkill -f /app/bin/feigram-downloader 2>/dev/null || true
    exit 0
}
trap shutdown TERM INT

wait "$NODE_PID"
