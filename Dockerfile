# Feigram Docker 镜像（单容器：Go 原生下载器 + Node 网关 + 前端静态资源）
# 多架构：linux/amd64 + linux/arm64（gotd 纯 Go、CGO_ENABLED=0，天然可交叉）。
# 数据持久化：单卷 /data（账号、session、任务、下载文件全部在其中）。

# ---------- Stage 1：编译 Go 原生下载器 ----------
FROM golang:1.22-bookworm AS go-build
ARG APP_VERSION=dev
WORKDIR /src
COPY downloader/go.mod downloader/go.sum ./
RUN go mod download
COPY downloader ./
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${APP_VERSION}" \
    -o /out/feigram-downloader ./cmd/feigram-downloader

# ---------- Stage 2：构建前端 ----------
FROM node:22-bookworm-slim AS client-build
WORKDIR /app/client
COPY client/package*.json ./
RUN npm ci
COPY client ./
RUN npm run build

# ---------- Stage 3：运行层 ----------
FROM node:22-bookworm-slim
WORKDIR /app
# Node 网关与 Go 下载器的全部持久化数据默认收敛到 /data（可用环境变量覆盖）
ENV NODE_ENV=production \
    DATA_DIR=/data \
    DOWNLOAD_DIR=/data/downloads \
    FEIGRAM_DOWNLOADER_PORT=3090
COPY server/package*.json ./server/
RUN cd server && npm ci --omit=dev \
    && npm cache clean --force
COPY server ./server
COPY --from=client-build /app/client/dist ./server/public
COPY --from=go-build /out/feigram-downloader ./bin/feigram-downloader
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/bin/feigram-downloader /app/docker-entrypoint.sh \
    && mkdir -p /data
VOLUME ["/data"]
EXPOSE 3088
ENTRYPOINT ["/app/docker-entrypoint.sh"]
