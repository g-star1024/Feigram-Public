# Feigram 2.0 fnOS Client Edition

Feigram 2.0 fnOS Client Edition 是第三方开发的非官方 Telegram 客户端，仅面向飞牛 OS / fnOS 设备。Feigram 不隶属于 Telegram、Telegram Messenger Inc. 或飞牛官方。

- 官网：<http://feigram.dpdns.org>
- Telegram 群：<https://t.me/feigram>
- 官方仓库：<https://github.com/g-star1024/Feigram-Public>

## 功能特性

- 多飞牛账户隔离：每个飞牛账户独立 Telegram session、缓存目录和登录状态。
- Telegram 多账号：支持添加、切换、退出 Telegram 账号。
- 聊天体验：支持私聊、群组、频道、会话搜索、文本发送、站内跳转和返回上层位置。
- Telegram 文件夹：同步 Telegram 聊天文件夹，并在左侧功能栏展示分组。
- 媒体预览：支持图片预览、视频在线播放、视频封面和时长、媒体组网格展示和群组信息侧栏。
- 下载中心：展示用户主动缓存任务、进度、速度、状态；支持开始、取消、清除列表、删除缓存和完成后播放。
- 群组资源：群组信息中可按图片、视频、文件浏览资源，并可静默缓存本群大于 100MB 的视频。
- 管理员后台：管理飞牛账户、服务端设置、缓存目录、通知、隐私和分组设置。
- Go 下载服务：FPK 内嵌独立 Go sidecar，可在管理后台查看健康状态、配置并发/限速，并为后续大文件下载迁移做准备。
- fnOS 客户端能力：运行诊断、缓存速度诊断、日志尾部查看、应用内更新检测、启动迁移记录、后台任务恢复。
- 安全与合规：Telegram session 加密存储，登录限流，验证码请求限流，内置隐私政策和服务条款入口。

## 播放与缓存策略

Feigram 运行在 Web 容器中，和 Telegram 官方桌面客户端不同，无法直接复用官方本地播放器或系统解码栈。当前策略是：

- 默认使用原始视频在线播放，尽量减少等待。
- 图片默认缓存。
- 视频可手动缓存到下载中心，下载完成后优先从本地缓存播放。
- 群组信息中的后台缓存开关会静默缓存大于 100MB 的视频，不写入用户下载列表，最多同时运行 5 个后台缓存任务。
- 用户主动下载任务和后台缓存任务会落盘保存，服务重启或版本升级后自动恢复。
- 浏览器无法解码的视频，可以切换本地播放器模式，或先缓存后用系统播放器打开。
- 2.0.34 起 Go 下载 sidecar 接管大文件下载队列、断点文件、并发和限速；Node 仅保留本机受保护的 Telegram 媒体流桥接，避免旧 Node 下载状态机继续参与任务调度。

## 首次使用

1. 在飞牛 OS 安装 Feigram Public FPK。
2. 打开 Feigram，创建第一个管理员飞牛账户。
3. 进入管理员后台添加 Telegram 账号。
4. 如需使用自己的 Telegram API 配置，可在“覆盖 API 设置”中填写。
5. 公开部署前请配置 HTTPS，并确认隐私政策、服务条款、支持邮箱和发布说明符合你的发布场景。

## 本地开发

```bash
npm run install:all
npm run dev
```

前端默认由 Vite 启动，后端由 Node.js/Express 启动。生产构建：

```bash
npm run build
npm start
```

## 打包 FPK

```bash
bash scripts/build-native-fpk.sh
```

构建产物会输出到 `release/`。脚本会打包前端静态资源、后端服务、Node.js 运行时和 Go 下载 sidecar。

## 官网部署

`website/` 是 Feigram 官网静态页面，可直接部署到 Cloudflare Pages：

- Build command 留空。
- Build output directory 填 `website`。
- 自定义域名绑定 `feigram.dpdns.org`。

## 目录结构

- `client/`：React 前端。
- `server/`：Express 后端、MTProto 接入、下载缓存、设置和公告。
- `downloader/`：Go 下载 sidecar，负责独立下载服务 API 和后续 Telegram 大文件下载迁移。
- `fnos-native-package/`：飞牛 OS FPK 包结构。
- `scripts/`：构建和运行时准备脚本。
- `docs/`：发布说明、隐私政策、服务条款。
- `website/`：官网静态页面。

## 开源依赖与致谢

Feigram 使用和打包了以下主要开源项目：

- React、React DOM、Vite：前端界面和构建。
- Go `gotd/td`：Telegram MTProto 客户端能力（原生单客户端架构，自 2.0.46 起不再使用 Node 侧 GramJS）。
- Express、Socket.IO、cors、dotenv、fs-extra、mime-types、big-integer：后端服务和实时通信。
- lucide-react：界面图标。
- Node.js：FPK 内置运行时。
- Go：构建 FPK 时用于编译内嵌下载 sidecar。

Feigram 参考 `iyear/tdl` 的 DC-aware 下载、分片并发和断点续传思路，但不复制 tdl 源码；tdl 采用 AGPL-3.0 许可证，后续若引入相关代码或二进制组件，需要按许可证要求同步开源和署名。

Telegram 名称、协议和相关商标归其各自权利人所有。Feigram 只是第三方客户端项目，不代表 Telegram 或飞牛官方。

## 文档

- [发布说明](docs/release-notes.md)
- [缓存与下载逻辑](docs/cache-download-logic.md)
- [Go 下载 Sidecar](docs/go-downloader-sidecar.md)
- [下载后端评估](docs/downloader-backend-evaluation.md)
- [隐私政策](docs/privacy-policy.md)
- [服务条款](docs/terms-of-service.md)

## 重要声明

Feigram 使用 Telegram 公开协议能力连接 Telegram 服务。使用者需要遵守 Telegram 服务条款、本项目发布平台规则，以及所在地法律法规。请不要将 Feigram 用于任何违法、侵权、骚扰、垃圾信息或规避平台规则的用途。
