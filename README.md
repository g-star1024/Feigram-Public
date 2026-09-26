# Feigram

Feigram 是第三方开发的非官方 Telegram 客户端，支持飞牛 OS / fnOS 原生应用（FPK）与 Docker 两种部署方式，可用于飞牛 NAS 及其他支持 Docker 的 NAS / 服务器。Feigram 不隶属于 Telegram、Telegram Messenger Inc. 或飞牛官方。

- 官网：<http://feigram.dpdns.org>
- Telegram 群：<https://t.me/feigram>
- 官方仓库：<https://github.com/g-star1024/Feigram-Public>
- Docker 镜像：`ghcr.io/g-star1024/feigram`（amd64 / arm64）

## 功能特性

- 多用户隔离：每个用户账户独立 Telegram session、缓存目录和登录状态。
- Telegram 多账号：支持添加、切换、退出 Telegram 账号，原生 MTProto 单客户端架构。
- 聊天体验：支持私聊、群组、频道、会话搜索、文本发送、站内跳转和返回上层位置。
- Telegram 文件夹：同步 Telegram 聊天文件夹，并在左侧功能栏展示分组。
- 媒体预览：支持图片预览、视频在线播放、视频封面和时长、媒体组网格展示和群组信息侧栏；头像与缩略图多级缓存，大群加载流畅。
- 群组信息面板：成员数 / 媒体统计 / 最近资源本地缓存秒显；按图片、视频、文件浏览资源；已提交缓存的视频带绿色对勾标记。
- 后台自动缓存：群组信息中开启后，自动扫描并缓存本群大于 100MB 的视频（深度翻页覆盖更早历史，单次最多提交 30 个，重复勾选逐轮补齐）。
- 下载中心：展示缓存任务、进度、速度、状态；支持开始（断点续传）、取消、清除列表、删除缓存和完成后播放；失败任务按错误类型自动重试或给出可读原因。
- 资源库：已缓存媒体统一浏览，自动同步外部删除。
- 运行诊断：健康检查、媒体 DC 连通性探测、缓存速度诊断、日志尾部查看、应用内更新检测与更新公告。
- 安全与合规：Telegram session 加密存储，登录限流，验证码请求限流，内置隐私政策和服务条款入口。

## 播放与缓存策略

Feigram 运行在 Web 容器中，和 Telegram 官方桌面客户端不同，无法直接复用官方本地播放器或系统解码栈。当前策略是：

- 默认使用原始视频在线播放，尽量减少等待。
- 图片默认缓存。
- 视频可手动缓存到下载中心，下载完成后优先从本地缓存播放。
- 用户主动下载任务和后台缓存任务由 Go 原生 MTProto 下载引擎处理：断点续传、分片并发、限速、限流精确等待、服务重启或版本升级后自动恢复。
- 浏览器无法解码的视频，可以切换本地播放器模式，或先缓存后用系统播放器打开。

## 飞牛 OS 部署（FPK）

1. 在飞牛 OS 应用中心安装 Feigram Public FPK（可在 [Releases](https://github.com/g-star1024/Feigram-Public/releases) 下载 `feigrampub-<版本>.fpk`）。
2. 打开 Feigram，创建第一个管理员用户账户。
3. 进入管理员后台添加 Telegram 账号。
4. 如需使用自己的 Telegram API 配置，可在「覆盖 API 设置」中填写。
5. 公开部署前请配置 HTTPS，并确认隐私政策、服务条款、支持邮箱和发布说明符合你的发布场景。

## Docker 部署

适用于飞牛 OS 以外的 NAS（群晖、威联通、极空间等）与任意支持 Docker 的环境。镜像为单容器架构（Go 下载器 + Node 网关 + 前端），支持 amd64 / arm64。

### 方式一：docker compose（推荐）

```yaml
# docker-compose.yml
services:
  feigram:
    image: ghcr.io/g-star1024/feigram:latest
    container_name: feigram
    restart: unless-stopped
    ports:
      - "3088:3088"
    environment:
      - TZ=Asia/Shanghai
    volumes:
      - ./data:/data
```

```bash
docker compose up -d
```

### 方式二：docker run

```bash
docker run -d \
  --name feigram \
  --restart unless-stopped \
  -p 3088:3088 \
  -e TZ=Asia/Shanghai \
  -v /volume1/feigram/data:/data \
  ghcr.io/g-star1024/feigram:latest
```

启动后访问 `http://NAS_IP:3088`，首次使用创建管理员账户并添加 Telegram 账号，步骤与 FPK 相同。

**数据持久化**：所有数据（账户、Telegram session、任务记录、下载文件）都在容器的 `/data` 卷内，升级镜像只需保留该卷。常用环境变量（与 FPK 同名约定）：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DATA_DIR` | `/data` | 数据根目录 |
| `DOWNLOAD_DIR` | `/data/downloads` | 下载落盘目录 |
| `FEIGRAM_DOWNLOADER_PORT` | `3090` | Go 下载器端口（容器内部，一般无需改动） |

如需从源码构建镜像：`docker build -t feigram .`（构建参数 `APP_VERSION` 可注入版本号）。

## 开源依赖与致谢

Feigram 使用和打包了以下主要开源项目：

- React、React DOM、Vite：前端界面和构建。
- Go `gotd/td`：Telegram MTProto 客户端能力（原生单客户端下载引擎）。
- Express、Socket.IO、cors、dotenv、fs-extra、big-integer：后端服务和实时通信。
- lucide-react：界面图标。
- Node.js、Go：运行时与编译工具链。

Telegram 名称、协议和相关商标归其各自权利人所有。Feigram 只是第三方客户端项目，不代表 Telegram 或飞牛官方。

## 文档

- [发布说明](docs/release-notes.md)
- [缓存与下载逻辑](docs/cache-download-logic.md)
- [隐私政策](docs/privacy-policy.md)
- [服务条款](docs/terms-of-service.md)

## 重要声明

Feigram 使用 Telegram 公开协议能力连接 Telegram 服务。使用者需要遵守 Telegram 服务条款、本项目发布平台规则，以及所在地法律法规。请不要将 Feigram 用于任何违法、侵权、骚扰、垃圾信息或规避平台规则的用途。
