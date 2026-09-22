const about = {
  title: "关于 Feigram",
  publisherName: "g-star1024",
  supportEmail: "",
  releaseUrl: "https://github.com/g-star1024/Feigram-Public",
  privacyPolicyUrl: "https://github.com/g-star1024/Feigram-Public/blob/main/docs/privacy-policy.md",
  termsUrl: "https://github.com/g-star1024/Feigram-Public/blob/main/docs/terms-of-service.md",
  body: [
    "Feigram Public 是第三方开发的非官方 Telegram 客户端，仅适用于飞牛 OS / fnOS。",
    "Feigram 不隶属于 Telegram、Telegram Messenger Inc. 或飞牛官方。",
    "公开部署前请配置 HTTPS，并在发布仓库中同步隐私政策、服务条款和支持邮箱。"
  ].join("\n")
};

const announcements = [
  {
    id: "release-2.3.0",
    title: "Feigram 2.3.0 更新",
    version: "2.3.0",
    level: "success",
    createdAt: "2026-09-22T13:37:51.000Z",
    body: [
      "健康检查自动化：登录成功后自动执行健康检查；无缓存文件时自动抽样低体积媒体验证文件池；每 30 分钟定时巡检，账号状态变化实时推送到界面，无需手动刷新。",
      "媒体源自动升级：有账号通过健康检查后，自动把媒体源从 HTTP 桥接切换到 Go 原生 MTProto，兑现原生下载速度；每进程只自动升级一次，仍保留手动回退。",
      "E2E 回归入库：端到端脚本整理到 scripts/e2e（含零预置数据的首次登录路径），支持 npm run e2e 一键全量回归，便于后续持续核证。",
      "发版工具化：新增 scripts/bump-version.sh 一键同步版本五处真源、scripts/verify-fpk.sh 解包核证，避免版本号漏改。"
    ].join("\n")
  },
  {
    id: "release-2.2.0",
    title: "Feigram 2.2.0 更新",
    version: "2.2.0",
    level: "success",
    createdAt: "2026-09-22T20:50:00.000Z",
    body: [
      "重构管理后台「运行诊断」与「缓存信息」页：卡片、状态徽标对齐设计规范；历史日志折叠展示不再刷屏；去掉冗余指标与裸 JSON 输出。",
      "诊断页新增「残留记录」分组：没有对应应用内账号的 Go 会话记录（多来自中途放弃的登录）可一键清理；退出登录现在会同步删除该账号的 Go 会话记录。",
      "聊天页默认选择「已就绪」的账号，不再因为列表里的旧失败账号而整页报「尚未就绪」。",
      "健康检查改进：没有缓存视频时改为基础检查通过（session 已授权），不再误报为失败；文件池抽样会在有缓存视频后自动补做。",
      "补齐登录弹窗的 Telegram 服务条款与账号观察合规提示（此前版本该提示静默缺失）。"
    ].join("\n")
  },
  {
    id: "release-2.1.3",
    title: "Feigram 2.1.3 更新",
    version: "2.1.3",
    level: "success",
    createdAt: "2026-09-22T09:40:00.000Z",
    body: [
      "修复首次登录报「native account is not prepared」：全新账号登录时服务器会自动用登录请求里的手机号与 API 凭据建档，不再要求账号记录预先存在。2.1.2 及之前版本的所有首次登录都会命中此问题。",
      "扫码登录入口同步修复；API 凭据缺失时改为明确提示「apiId/apiHash is required」。"
    ].join("\n")
  },
  {
    id: "release-2.1.2",
    title: "Feigram 2.1.2 更新",
    version: "2.1.2",
    level: "success",
    createdAt: "2026-09-22T08:50:00.000Z",
    body: [
      "新增网络代理支持：此前原生模式完全无法走代理，在不能直连 Telegram 的网络里会表现为「登录转圈、一直失败」。现在可在「设置 → 网络代理」填写 SOCKS5（含 socks5h）或 HTTP/HTTPS 代理。",
      "代理改完立即生效，无需重启；也可用环境变量兜底（FEIGRAM_PROXY_URL / ALL_PROXY / SOCKS5 / HTTPS_PROXY / HTTP_PROXY）。",
      "MTProto 登录与媒体下载都会走代理；指向本机的地址（回环）自动直连，不会因为配了代理而打断本机网关调用。",
      "代理地址写错会明确报出原因，不再静默直连；代理连不上时，登录与诊断里会给出「检查代理进程 / 核对地址端口」的可操作提示。",
      "加固登录流程：同一账号重复点登录会取消上一个流程；发起登录超时或代理变更时会取消在途登录，避免旧代理被持续拨号、并把新登录的状态覆盖成失败。"
    ].join("\n")
  },
  {
    id: "release-2.1.1",
    title: "Feigram 2.1.1 更新",
    version: "2.1.1",
    level: "success",
    createdAt: "2026-09-22T07:30:00.000Z",
    body: [
      "修复公告弹窗交互问题：此前点击顶栏铃铛可能毫无反应（浏览器通知权限申请在部分环境下长时间不返回，阻塞了弹窗打开）。",
      "公告弹窗新增背板点击关闭；关闭按钮改为醒目的圆形按钮；Esc 键关闭保持可用。",
      "修复弹窗与管理后台叠加时可能互相遮挡、无法关闭的问题：现在弹窗之间互斥打开。"
    ].join("\n")
  },
  {
    id: "release-2.1.0",
    title: "Feigram 2.1.0 更新",
    version: "2.1.0",
    level: "success",
    createdAt: "2026-09-22T06:20:00.000Z",
    body: [
      "界面全面焕新为 fnOS 设计风格：左侧 Dock 导航（首页 / 会话 / 下载 / 资源库 / 管理 / 设置）+ 毛玻璃顶栏 + 应用窗口内容区。",
      "亮色与暗色双主题等权：默认亮色，可在 Dock 底部手动切换并记住选择；同时支持跟随飞牛系统的主题参数自动切换。",
      "新增首页 Dashboard（账号 / 下载 / 缓存统计、近期活动、快捷操作）、独立下载中心页面与资源库页面（按视频 / 图片 / 文件筛选，点击直接播放或打开）。",
      "会话与聊天重设计：文件夹改为顶部胶囊行、气泡与媒体卡片圆角化、输入区毛玻璃质感；管理后台、扫码登录、播放器统一套用新组件规范。",
      "响应式与无障碍：1024px 以下 Dock 折叠为图标栏、768px 以下转为底部导航；可点区域不小于 44px；弹窗支持 Esc 关闭并补充读屏标签；统一焦点环，并尊重系统「减弱动态效果」设置。",
      "重要：本版起 Telegram 连接只走 Go 原生通道，旧版（gramjs 模式）账号会提示「需重新登录以升级到原生模式」，删除该账号重新登录一次即可，不会影响你的 Telegram 账号数据。",
      "修复安装包版本号上报滞后的问题（此前升级后应用可能被判定为同版本而不重启）。"
    ].join("\n")
  },
  {
    id: "release-2.0.45",
    title: "Feigram 2.0.45 更新",
    version: "2.0.45",
    level: "success",
    createdAt: "2026-06-20T17:45:00.000Z",
    body: [
      "修复二维码状态轮询重叠启动多个 gotd client，导致 rpcDoRequest: engine was closed 的问题。",
      "Go 服务对同一二维码采用单飞轮询；连接切换、超时和短暂断线会自动重试，不再直接终止扫码流程。",
      "native 账号未通过健康检查时，已有下载任务会自动切换 HTTP 桥接继续续传；升级时也会修复旧的 session 未就绪失败任务。",
      "修复永久 error 任务被调度器每秒重复启动的问题；只有 queued 任务会自动执行，可恢复错误继续使用退避重试。",
      "二维码改为独立居中弹窗：桌面显示 320px 二维码，移动端自适应，无需在管理员后台滚动查找。"
    ].join("\n")
  },
  {
    id: "release-2.0.44",
    title: "Feigram 2.0.44 更新",
    version: "2.0.44",
    level: "success",
    createdAt: "2026-06-20T13:30:00.000Z",
    body: [
      "修复 Telegram App 扫码已经授权，但随后健康检查提示 gotd session 未授权的问题。",
      "根因是授权成功后旧账号快照覆盖了 gotd 刚写入的新 session；现在所有扫码和验证码成功分支都会读取最新持久化 session。",
      "重新扫码会清除旧的无效 session，强制 Go 生成新的 MTProto auth key，避免误判旧授权。",
      "健康检查通过次数、读取字节、Telegram DC 和耗时现在会完整持久化，不再反复停留在 0/2。",
      "扫码成功后账号进入待检查状态；连续通过 2 次真实 Telegram 文件检查后，才启用 native 大文件下载资格。"
    ].join("\n")
  },
  {
    id: "release-2.0.43",
    title: "Feigram 2.0.43 更新",
    version: "2.0.43",
    level: "success",
    createdAt: "2026-06-20T12:00:00.000Z",
    body: [
      "Go 原生健康检查改为先由 Go 重取 Telegram 消息刷新 fileReference，再从 document 所在 media DC 真实读取 64KB 分片。",
      "新增连续健康闸门：新账号连续通过 2 次真实文件检查后，才具备 native-mtproto 大文件下载资格。",
      "补齐私聊和普通群 peer 回查；messages.getMessages 未命中时，会在指定 peer 内精确查询目标消息附近记录。",
      "连续通过检查的账号，新建大于 100MB 的任务会自动灰度到 Go 原生传输；小文件、旧任务和未通过账号继续使用 HTTP 回退。",
      "管理后台新增健康通过次数、真实读取字节、Telegram DC 和耗时信息，便于判断 native 通道是否真正可用。"
    ].join("\n")
  },
  {
    id: "release-2.0.42",
    title: "Feigram 2.0.42 更新",
    version: "2.0.42",
    level: "success",
    createdAt: "2026-06-15T12:20:00.000Z",
    body: [
      "公告排序继续按版本号置顶，避免客户端停留在旧的 2.0.39 公告。",
      "Go 原生下载新增 media-only DC 读取：文件分片优先走 document dcId，对 _MIGRATE_ 会自动切换目标 DC 后续传。",
      "Go 原生 FILE_REFERENCE_EXPIRED 优先通过 gotd 重取 Telegram 消息刷新 fileReference，旧任务才回退 Node 元数据接口。",
      "Go 扫码登录补齐 Telegram DC 迁移导入流程，遇到 auth.loginTokenMigrateTo 时会自动 MigrateTo 后导入 token。",
      "HTTP 媒体桥接继续保留一个版本作为回退；native-mtproto 长时间稳定后再默认切换并删除 Node 媒体桥接。"
    ].join("\n")
  },
  {
    id: "release-2.0.41",
    title: "Feigram 2.0.41 更新",
    version: "2.0.41",
    level: "success",
    createdAt: "2026-06-15T11:30:00.000Z",
    body: [
      "公告排序改为按版本号优先，避免发布时间填写差异导致旧版本置顶。",
      "Go 下载任务补齐 native peer 元数据，后续原生模式可直接在 Go 侧重取消息。",
      "Go 原生下载遇到 FILE_REFERENCE_EXPIRED 时，优先使用 gotd 重新读取 Telegram 消息并刷新 document fileReference。",
      "旧任务缺少 native peer 元数据时仍保留内部 media-meta 回退，避免升级后任务直接失效。",
      "HTTP 桥接仍保留一个版本作为回退；native-mtproto 稳定后再移除 Node 媒体桥接。"
    ].join("\n")
  },
  {
    id: "release-2.0.40",
    title: "Feigram 2.0.40 更新",
    version: "2.0.40",
    level: "success",
    createdAt: "2026-06-14T18:30:00.000Z",
    body: [
      "Go 原生账号登录入口移入账号管理，新增 Telegram App 扫码登录，验证码登录保留为兜底。",
      "Go 下载服务新增 QR login token 生成与轮询确认，成功后由 Go 侧生成独立 MTProto auth key。",
      "Go 原生下载遇到 FILE_REFERENCE_EXPIRED 时，会通过内部元数据接口刷新 fileReference 并继续续传。",
      "Go 原生账号健康检查会优先真实抽样读取 Telegram 文件分片，没有样本时才退回 session 授权检查。",
      "缓存信息页去掉过期开发提示，只保留运行、并发、传输层和任务数等有效状态。"
    ].join("\n")
  },
  {
    id: "release-2.0.39",
    title: "Feigram 2.0.39 更新",
    version: "2.0.39",
    level: "success",
    createdAt: "2026-06-14T23:30:00.000Z",
    body: [
      "Go 下载服务新增 Telegram 重新登录流程，可在管理后台为 Go 侧生成独立 MTProto auth key。",
      "新增 gotd session 加密存储与真实授权健康检查，健康后才允许切换 native-mtproto。",
      "Go 原生传输层新增 upload.getFile 分片读取实验通道，支持断点续传、限速、连接重试和基础错误分类。",
      "原生读取会识别 FILE_REFERENCE_EXPIRED、FLOOD_WAIT 和 DC_MIGRATE，并给出明确诊断，后续继续补齐无感刷新。",
      "默认仍保留 HTTP 桥接；native 通过长时间测试后，再让大文件完全走 Go 原生并删除 Node 媒体桥接。"
    ].join("\n")
  },
  {
    id: "release-2.0.38",
    title: "Feigram 2.0.38 更新",
    version: "2.0.38",
    level: "success",
    createdAt: "2026-06-14T20:30:00.000Z",
    body: [
      "新增 Go 原生 Telegram 账号元数据同步，管理后台运行诊断可以看到每个账号是否具备 Go 原生 session。",
      "Go 下载服务新增加密 native session 存储，密钥跟随 FPK 数据目录生成，不在接口和日志中返回 session 内容。",
      "下载任务补齐 Go 原生 file location 预备字段：peer、messageId、fileReference、dcId、size、mimeType 和文件名。",
      "新增 native-mtproto 健康检查闸门：账号未完成 Go 重新登录前，媒体源仍会自动保持稳定 HTTP 桥接，避免再次拖崩聊天和下载。",
      "本版本完成 gotd/tdl 风格原生下载前置迁移；下一版会增加 Go 重新登录流程和 upload.getFile 原生分片读取。"
    ].join("\n")
  },
  {
    id: "release-2.0.37",
    title: "Feigram 2.0.37 更新",
    version: "2.0.37",
    level: "success",
    createdAt: "2026-06-14T17:30:00.000Z",
    body: [
      "新增媒体源传输层配置，Go 下载服务现在能区分 HTTP 桥接和 Go 原生 MTProto 实验边界。",
      "运行诊断新增媒体源状态，便于识别当前是否仍依赖 Node/GramJS 本机媒体桥接。",
      "Go 下载任务模型新增 transport 字段，为下一版 gotd/tdl 风格原生文件读取做兼容准备。",
      "文档补充 Go 原生 MTProto 迁移路线：账号 session、file location、DC、fileReference 刷新和灰度启用策略。",
      "默认继续使用稳定 HTTP 桥接，避免在 Go session 迁移完成前影响现有下载。"
    ].join("\n")
  },
  {
    id: "release-2.0.36",
    title: "Feigram 2.0.36 更新",
    version: "2.0.36",
    level: "success",
    createdAt: "2026-06-14T23:30:00.000Z",
    body: [
      "修复 Go 下载服务在 FPK 启动时早于 Node 媒体桥接启动，导致 connect refused 后反复重试的问题。",
      "Go 下载队列新增可恢复错误退避：连接拒绝、unexpected EOF、timeout、连接重置和 5xx 会保留 .part 文件，等待冷却后自动续传。",
      "运行诊断的速度只统计真实 downloading/running 任务，不再把排队任务残留速度算进总速率。",
      "管理后台清理旧 Node 下载管线选择和过期提示，统一展示当前 Go 下载服务状态。",
      "本版本继续保留本机 Telegram 媒体桥接，后续会按 tdl/gotd 思路迁移原生 Go 传输层以提升大文件吞吐。"
    ].join("\n")
  },
  {
    id: "release-2.0.35",
    title: "Feigram 2.0.35 更新",
    version: "2.0.35",
    level: "success",
    createdAt: "2026-06-14T21:30:00.000Z",
    body: [
      "修复启动阶段 Telegram 账号恢复或旧下载任务迁移失败时，Node 后端直接退出导致飞牛客户端显示“拒绝连接”的问题。",
      "Feigram 现在会先启动 HTTP 服务，再在后台恢复 Telegram 账号和下载任务；恢复失败只写入日志，不再拖垮整个 App。",
      "Go 下载服务开始接管大文件下载队列：任务排序、并发、限速、断点 .part 文件和完成校验都由 Go sidecar 维护。",
      "Node 下载管线不再直接执行大文件下载，只保留本机受保护的 Telegram 媒体流桥接，避免双状态管线互相打架。",
      "手动缓存和群后台自动缓存统一进入管理后台的缓存信息列表，聊天窗口不再显示独立下载状态。",
      "升级时会尝试把旧 Node 未完成下载任务迁移到 Go 队列，继续从原来的文件位置续传。",
      "运行诊断的缓存速度现在显示 Go 队列聚合速度，不再额外抢占 Telegram 连接做测速。"
    ].join("\n")
  },
  {
    id: "release-2.0.33",
    title: "Feigram 2.0.33 更新",
    version: "2.0.33",
    level: "success",
    createdAt: "2026-06-14T17:30:00.000Z",
    body: [
      "新增内嵌 Go 下载 sidecar：FPK 启动时会同时拉起独立 Go 服务，单独保存下载队列、配置和运行状态。",
      "管理后台服务端设置新增下载引擎选择，可查看并配置 Go sidecar 的启用状态、并发、限速和保守/高速模式。",
      "运行诊断新增 Go 下载服务健康状态、日志路径和 sidecar 日志，方便定位后续 Telegram 大文件下载桥接问题。",
      "本版本先完成 sidecar 基础设施接入，默认仍使用 Node 内置下载管线；后续将把 Telegram 大文件传输迁移到 Go 服务，避免旧缓存状态机继续叠补丁。"
    ].join("\n")
  },
  {
    id: "release-2.0.32",
    title: "Feigram 2.0.32 更新",
    version: "2.0.32",
    level: "success",
    createdAt: "2026-06-14T09:30:00.000Z",
    body: [
      "修复群视频统一缓存队列在同一 Telegram 账号下并发多个大文件时导致 GramJS 反复重连、TIMEOUT 和 hanging states 的问题。",
      "后台缓存并发改为跨 Telegram 账号并行：同一 Telegram 账号始终只运行 1 个大文件任务，多个账号会按设置并发执行。",
      "新增统一下载队列停滞监控：任务长期没有真实文件增长时会自动打断并回到队列，继续从 .part 断点续传。",
      "FILE_REFERENCE_EXPIRED、TIMEOUT、Not connected 等可恢复错误会按冷却时间自动重新排队，避免任务长期停在失败状态。",
      "缓存信息和诊断页补充说明，明确并发数量、有效上限和跨账号高速模式的关系。"
    ].join("\n")
  },
  {
    id: "release-2.0.31",
    title: "Feigram 2.0.31 更新",
    version: "2.0.31",
    level: "success",
    createdAt: "2026-06-13T23:45:00.000Z",
    body: [
      "重构群视频后台缓存：废弃独立 silent-cache 下载状态机，改为统一进入下载任务管线。",
      "群组信息页勾选后台自动缓存后，仍会扫描并加入大于 100MB 的视频，但任务由统一下载队列执行和恢复。",
      "管理后台的缓存信息合并展示手动缓存和群后台自动缓存任务，支持统一排序、取消、并发和模式设置。",
      "聊天窗口不再显示独立下载入口，避免下载状态和聊天阅读互相干扰。",
      "升级时会把旧后台缓存记录迁移到统一下载任务，旧 silent-cache 文件仅保留设置兼容。"
    ].join("\n")
  },
  {
    id: "release-2.0.30",
    title: "Feigram 2.0.30 更新",
    version: "2.0.30",
    level: "success",
    createdAt: "2026-06-13T22:05:00.000Z",
    body: [
      "修复后台缓存任务在 Telegram 连接持续抖动时仍长期显示 running 的问题。",
      "后台缓存增加持续低速 watchdog：不限速时低于 128KB/s 持续 3 分钟会自动释放运行槽、重置连接并等待断点续传。",
      "任务重新排队和重新开始时会清理旧的低速状态，避免前一次卡顿影响下一次续传。",
      "缓存速度诊断不再把聚合速度误认为真实测速，会显示低速观察状态和是否疑似卡死。",
      "诊断窗口在已有后台任务运行时不再额外抢占 Telegram 连接，减少前台聊天和后台缓存互相拖慢。"
    ].join("\n")
  },
  {
    id: "release-2.0.29",
    title: "Feigram 2.0.29 更新",
    version: "2.0.29",
    level: "success",
    createdAt: "2026-06-13T20:05:00.000Z",
    body: [
      "修复下载列表显示已完成但本地文件不完整，导致视频无法加载或提示格式不支持的问题。",
      "用户主动缓存下载改为复用 Telegram 分片续传链路，不再每次开始都删除 part 断点文件。",
      "目标文件、后台缓存文件和重启恢复记录都会严格校验实际文件大小，未达到 Telegram 原始大小时自动转回队列续传。",
      "高速模式下前台会话、消息、搜索等操作也会短暂暂停同账号后台缓存，避免后台任务把 Telegram 连接拖到 TIMEOUT 后前端列表空白。",
      "高速模式多任务全 0 速停滞时会降回保守模式，并给重排任务加入短冷却，避免立即重启后再次卡住。"
    ].join("\n")
  },
  {
    id: "release-2.0.28",
    title: "Feigram 2.0.28 更新",
    version: "2.0.28",
    level: "success",
    createdAt: "2026-06-13T15:05:00.000Z",
    body: [
      "缓存速度诊断拆分显示实际运行数、有效运行上限和管理员设置并发数，避免保守模式下误判并发异常。",
      "后台缓存状态输出前会按真实运行任务自校准运行槽，避免异常路径造成运行数量显示不准或调度被假运行数卡住。",
      "限速分片计算改为按有效并发估算，保守模式同账号单任务时不再被设置并发数误压低。",
      "缓存信息说明补充保守模式规则：同一 Telegram 账号只运行 1 个后台缓存任务，多个账号或高速模式才会按并发数量并行。"
    ].join("\n")
  },
  {
    id: "release-2.0.27",
    title: "Feigram 2.0.27 更新",
    version: "2.0.27",
    level: "success",
    createdAt: "2026-06-13T14:25:00.000Z",
    body: [
      "修复后台缓存 running 任务 0 B/s 后长期占用运行槽的问题。",
      "停滞判定改为基于真实写盘时间 lastObservedAt，不再被普通状态刷新或测速刷新误重置。",
      "高速模式新增自保护：同账号多任务连续 0 速时会自动降级为保守模式，并释放多余任务重新排队。",
      "缓存速度诊断只统计真实运行中的后台任务，并输出 lastProgressAt / lastObservedAt 便于判断是否真实写盘。"
    ].join("\n")
  },
  {
    id: "release-2.0.26",
    title: "Feigram 2.0.26 更新",
    version: "2.0.26",
    level: "success",
    createdAt: "2026-06-13T11:30:00.000Z",
    body: [
      "根据最新运行日志修复保守模式下同一 Telegram 账号仍会并发缓存多个视频的问题。",
      "保守模式现在强制同账号最多 1 个后台缓存任务运行，减少 Telegram 连接反复断开重连。",
      "高速模式保留多任务并发能力，由管理员手动开启。",
      "后台缓存监控新增完成收口：part 文件达到视频大小后会直接完成并释放运行槽，避免临近完成时长时间显示 running。"
    ].join("\n")
  },
  {
    id: "release-2.0.25",
    title: "Feigram 2.0.25 更新",
    version: "2.0.25",
    level: "success",
    createdAt: "2026-06-13T09:30:00.000Z",
    body: [
      "修复后台缓存停滞后占用运行槽，导致前台聊天记录一直加载中的问题。",
      "保守模式下前台会话、消息、搜索和发送会优先执行，后台群视频缓存会让出连接并重新排队续传。",
      "新增后台缓存模式：默认保守模式；管理员可手动切换高速模式，尽量保持后台缓存运行。",
      "后台缓存真实写盘停滞时会释放运行槽并重新排队，避免卡死后不再启动。",
      "缓存信息列表新增多选和批量取消，便于一次性移除异常或不需要的后台缓存任务。",
      "聊天列表、聊天记录和媒体读取增加 Telegram 连接超时提示，不再无限停留在加载状态。"
    ].join("\n")
  },
  {
    id: "release-2.0.24",
    title: "Feigram 2.0.24 更新",
    version: "2.0.24",
    level: "success",
    createdAt: "2026-06-12T18:15:00.000Z",
    body: [
      "修复升级安装时旧 Node 后端进程未重启，导致新前端连接旧后端的问题。",
      "启动脚本新增版本检测，安装新版本时会自动停止旧进程并启动新服务。",
      "启动脚本会按 Feigram app 路径清理残留 Node 进程，避免 PID 文件丢失时旧服务继续占用 Telegram session。",
      "Telegram 账号连接增加账号级连接锁，启动恢复、后台任务和前端请求不会再同时为同一个账号创建多个 GramJS client。",
      "遇到 AUTH_KEY_DUPLICATED 时会先断开旧连接并重试；如果 Telegram 已判定 session 失效，会提示重新登录账号。"
    ].join("\n")
  },
  {
    id: "release-2.0.23",
    title: "Feigram 2.0.23 更新",
    version: "2.0.23",
    level: "success",
    createdAt: "2026-06-12T17:55:00.000Z",
    body: [
      "修复后台缓存 406 AUTH_KEY_DUPLICATED 导致 Telegram 连接被判定重复的问题。",
      "后台缓存不再创建独立 Telegram 连接，改为复用账号唯一连接，避免同一 session/auth key 被并发登录。",
      "会话列表加载失败时会在前端直接展示错误和重新加载按钮，不再只显示空白或暂无会话。"
    ].join("\n")
  },
  {
    id: "release-2.0.22",
    title: "Feigram 2.0.22 更新",
    version: "2.0.22",
    level: "success",
    createdAt: "2026-06-12T17:20:00.000Z",
    body: [
      "修复后台缓存 Cannot cast Document to any kind of InputFileLocation 的问题。",
      "后台缓存和缓存速度诊断会把 Telegram Document 转换为 InputDocumentFileLocation 后再读取。",
      "视频在线播放路径也统一使用明确的 Telegram 文件定位对象，避免同类类型转换错误。"
    ].join("\n")
  },
  {
    id: "release-2.0.21",
    title: "Feigram 2.0.21 更新",
    version: "2.0.21",
    level: "success",
    createdAt: "2026-06-12T16:05:00.000Z",
    body: [
      "修复后台缓存 400 CONNECTION_NOT_INITED、401 AUTH_KEY_UNREGISTERED 后无法继续续传的问题。",
      "后台缓存下载链路从手工 getSender/invokeWithSender 改为 GramJS iterDownload，使用库内置的下载重试和 sender 管理。",
      "400/401、Not connected、TIMEOUT 等连接类错误会重置缓存专用连接并重新排队续传。",
      "任务重新进入运行状态时会清空旧错误，避免正在缓存仍显示历史 400/401。",
      "运行诊断页优化缓存速度诊断和飞牛日志区域高度，飞牛日志不会再被测速窗口挤压。"
    ].join("\n")
  },
  {
    id: "release-2.0.20",
    title: "Feigram 2.0.20 更新",
    version: "2.0.20",
    level: "success",
    createdAt: "2026-06-12T15:45:00.000Z",
    body: [
      "复盘飞牛日志后修复后台缓存 Telegram sender 重连风暴问题。",
      "撤销 2.0.19 的外层 Promise 超时，避免底层 GramJS 请求残留为 hanging states。",
      "群视频后台缓存改用独立 Telegram client，不再和前台聊天、消息更新共用连接。",
      "真实写盘停滞时只重置缓存专用连接，然后从现有 .silent.part 文件续传，不影响前台会话。",
      "确认服务端缓存清理为每天一次且保留至少 1 天，不会 3-5 分钟删除正在缓存的文件。"
    ].join("\n")
  },
  {
    id: "release-2.0.19",
    title: "Feigram 2.0.19 更新",
    version: "2.0.19",
    level: "success",
    createdAt: "2026-06-12T15:05:00.000Z",
    body: [
      "修复后台缓存停滞重排时被误显示为“已取消”的问题。",
      "后台缓存不再强行并发重启同一个文件，避免旧任务和新任务同时写同一个 part 文件。",
      "Telegram upload.GetFile 单次分片请求新增 45 秒本地超时，超时后 3 秒重新排队并从现有 part 文件续传。",
      "真实写盘停滞时会显示“等待当前请求超时后续传”，不再误导为用户取消。",
      "这版将后台缓存状态机调整为更接近 Telegram 客户端的顺序续传模型。"
    ].join("\n")
  },
  {
    id: "release-2.0.18",
    title: "Feigram 2.0.18 更新",
    version: "2.0.18",
    level: "success",
    createdAt: "2026-06-12T14:20:00.000Z",
    body: [
      "修复后台缓存显示有速度但真实文件不继续增长的问题。",
      "缓存列表会定期读取 .silent.part 真实文件大小，界面进度以实际写盘大小为准。",
      "后台缓存巡检从 10 分钟缩短到 30 秒；真实写盘超过 90 秒无增长会自动重新排队。",
      "速度超过 20 秒没有真实进度会自动清零，避免旧速度一直显示。",
      "优化后台缓存运行标记，停滞重排时旧任务和新任务不会互相影响并发计数。"
    ].join("\n")
  },
  {
    id: "release-2.0.17",
    title: "Feigram 2.0.17 更新",
    version: "2.0.17",
    level: "success",
    createdAt: "2026-06-12T13:30:00.000Z",
    body: [
      "缓存速度诊断默认改为聚合当前运行中的后台缓存任务速度，不再额外读取 Telegram 文件。",
      "新增“抽样测速”入口，仅在需要深挖单文件或 Telegram DC 速度时读取 1MB 样本。",
      "避免诊断读取和 5 个后台缓存任务抢同一 Telegram 账号/DC 连接，导致测速结果显著低于真实后台缓存速度。",
      "诊断结果会标明“运行聚合”或“抽样读取”，方便判断是整体缓存速度还是单文件探针速度。"
    ].join("\n")
  },
  {
    id: "release-2.0.16",
    title: "Feigram 2.0.16 更新",
    version: "2.0.16",
    level: "success",
    createdAt: "2026-06-12T12:45:00.000Z",
    body: [
      "修复后台缓存直接调用 Telegram upload.getFile 时偶发 LIMIT_INVALID 的问题。",
      "Telegram 分片请求会固定使用合法对齐分片，并在 LIMIT_INVALID 时自动降级分片继续请求。",
      "缓存速度诊断新增请求分片、实际分片、降级次数和 LIMIT_INVALID 次数。",
      "如果单个文件仍然低速，可通过诊断结果判断是分片降级、Telegram DC 慢，还是全局调度问题。"
    ].join("\n")
  },
  {
    id: "release-2.0.15",
    title: "Feigram 2.0.15 更新",
    version: "2.0.15",
    level: "success",
    createdAt: "2026-06-12T12:20:00.000Z",
    body: [
      "移除 FPK 中的 ffmpeg/ffprobe 运行时残留，进一步缩小安装包体积。",
      "构建脚本不再下载或打包 ffmpeg 静态运行时。",
      "服务端不再使用 ffmpeg 为已缓存视频兜底截帧。",
      "视频封面改为只使用 Telegram 自带缩略图；Telegram 未提供时会显示无封面状态。"
    ].join("\n")
  },
  {
    id: "release-2.0.14",
    title: "Feigram 2.0.14 更新",
    version: "2.0.14",
    level: "success",
    createdAt: "2026-06-12T11:30:00.000Z",
    body: [
      "后台群视频缓存改为直接调用 Telegram upload.getFile 顺序分片，避开大 offset 续传时的慢路径。",
      "续传 offset 会按 4096 字节对齐，减少旧分片尾部导致的隐性失败。",
      "管理员后台“诊断”更名为“运行诊断”。",
      "运行诊断新增缓存速度诊断，可输出实测速度、样本大小、耗时、Telegram DC、限速、并发、运行任务和队列数量。"
    ].join("\n")
  },
  {
    id: "release-2.0.13",
    title: "Feigram 2.0.13 更新",
    version: "2.0.13",
    level: "success",
    createdAt: "2026-06-12T10:30:00.000Z",
    body: [
      "移除客户端内置 HLS/hls.js 播放器引用，视频播放模式仅保留原始在线播放和本地播放器。",
      "移除服务端 HLS 播放路由，旧配置中的 HLS 模式会自动回退到原始视频在线播放。",
      "后台缓存继续使用 Feigram 内置 Telegram 下载链路，不引入 Gopeed 作为 Telegram MTProto 下载后端。",
      "新增下载后端评估文档，说明 Gopeed、tdl/TG Downloader 与 Feigram 后台缓存的边界。"
    ].join("\n")
  },
  {
    id: "release-2.0.12",
    title: "Feigram 2.0.12 更新",
    version: "2.0.12",
    level: "success",
    createdAt: "2026-06-12T08:40:00.000Z",
    body: [
      "修复后台缓存大视频时 FILE_REFERENCE_EXPIRED 反复失败的问题。",
      "后台缓存 document 视频改为可续传下载，失败重试时保留 .silent.part 分片，不再每次从 0 开始。",
      "遇到 Telegram 文件引用过期时会重新拉取消息刷新 fileReference，并在短时间内重新排队继续缓存。",
      "缓存进度、速度和限速计算会从已存在分片大小继续，减少隔夜缓存白跑。"
    ].join("\n")
  },
  {
    id: "release-2.0.11",
    title: "Feigram 2.0.11 更新",
    version: "2.0.11",
    level: "success",
    createdAt: "2026-06-12T05:00:00.000Z",
    body: [
      "缓存信息的最大缓存速率确认支持不限速，选择不限速时后台缓存不再执行全局节流。",
      "后台缓存并发数量新增 10，服务端会强制限制在 1-10。",
      "限速模式下并发任务继续共享总速率限制；不限速模式下使用 Telegram 默认下载策略。",
      "新增缓存与下载逻辑文档，便于后续继续调整限速、并发、分片和重试策略。"
    ].join("\n")
  },
  {
    id: "release-2.0.10",
    title: "Feigram 2.0.10 更新",
    version: "2.0.10",
    level: "success",
    createdAt: "2026-06-12T04:00:00.000Z",
    body: [
      "修复后台缓存暂停后重新开启，前几个运行任务仍显示已暂停且不继续缓存的问题。",
      "重新开启后台缓存、调整限速或调整并发时，会重置限速窗口，避免旧下载进度导致恢复后长时间补等待。",
      "正在退出的旧暂停任务会标记为重新排队，退出后自动回到队列继续缓存。",
      "优化动态下载分片策略，2MB/s、5 并发这类场景会使用更大的分片提高吞吐。"
    ].join("\n")
  },
  {
    id: "release-2.0.9",
    title: "Feigram 2.0.9 更新",
    version: "2.0.9",
    level: "success",
    createdAt: "2026-06-12T03:00:00.000Z",
    body: [
      "修复后台缓存高并发高限速时实际速度只有几十 KB/s 的问题。",
      "限速模式下 Telegram 下载分片不再固定为 32KB，会根据最大缓存速率和并发数量动态调整。",
      "低速限速仍使用较小分片控制瞬时峰值；5MB/s、5 并发这类场景会使用更大的分片提高吞吐。",
      "总速率限制仍然全局共享，不会因为并发数量提高而放大上限。"
    ].join("\n")
  },
  {
    id: "release-2.0.8",
    title: "Feigram 2.0.8 更新",
    version: "2.0.8",
    level: "success",
    createdAt: "2026-06-12T02:00:00.000Z",
    body: [
      "修复后台缓存恢复时偶发提示找不到会话的问题。",
      "后台缓存任务现在会直接通过 Telegram peer id 解析群组/频道，不再只依赖当前会话列表缓存。",
      "会话列表和 Telegram 文件夹同步拉取上限提高到 500，减少大号会话展示不全的情况。",
      "会话确实不可访问时，会提示可能已退出群组、会话已删除或 Telegram 暂时无法解析。"
    ].join("\n")
  },
  {
    id: "release-2.0.7",
    title: "Feigram 2.0.7 更新",
    version: "2.0.7",
    level: "success",
    createdAt: "2026-06-12T01:00:00.000Z",
    body: [
      "缓存信息新增后台缓存并发数量设置。",
      "管理员可在 1-5 个并发任务之间选择，最少 1 个，最多 5 个。",
      "并发任务继续共享最大缓存速率限制，避免把限速按任务数放大。",
      "调整并发数量后会立即重排当前后台缓存任务。"
    ].join("\n")
  },
  {
    id: "release-2.0.6",
    title: "Feigram 2.0.6 更新",
    version: "2.0.6",
    level: "success",
    createdAt: "2026-06-12T00:00:00.000Z",
    body: [
      "进一步增强群缓存限速策略。",
      "限速开启时，后台缓存强制单任务运行，避免多个任务并发叠加突破上限。",
      "限速开启时改用 Telegram 底层下载接口，并指定 32KB 小分片，降低单个大分片造成的瞬时冲高。",
      "切换为限速模式时，会主动暂停多余运行任务并重新排队。"
    ].join("\n")
  },
  {
    id: "release-2.0.5",
    title: "Feigram 2.0.5 更新",
    version: "2.0.5",
    level: "success",
    createdAt: "2026-06-11T23:30:00.000Z",
    body: [
      "修复群缓存最大速率限制不准确的问题。",
      "限速策略改为全局总速率限制，所有后台缓存任务共享同一个速率上限。",
      "移除单次节流最多 5 秒的限制，避免大分片下载时突破设置的限速。",
      "切换限速值时会重置限速窗口，新设置会立即作用于正在运行的后台缓存任务。"
    ].join("\n")
  },
  {
    id: "release-2.0.4",
    title: "Feigram 2.0.4 更新",
    version: "2.0.4",
    level: "success",
    createdAt: "2026-06-11T23:00:00.000Z",
    body: [
      "清理旧版本遗留的已取消群缓存记录，已取消任务不会再出现在缓存信息列表。",
      "优化首次打开加载顺序，账号和会话优先加载，公告、下载、缓存信息和文件夹延后加载。",
      "管理员后台重构为账号管理、服务端设置、缓存信息、隐私设置和诊断。",
      "账号管理合并飞牛账号管理和 Telegram 账号管理。",
      "服务端设置合并 API、缓存下载和播放器设置。",
      "隐私设置合并通知、隐私和分组设置。"
    ].join("\n")
  },
  {
    id: "release-2.0.3",
    title: "Feigram 2.0.3 更新",
    version: "2.0.3",
    level: "success",
    createdAt: "2026-06-11T22:00:00.000Z",
    body: [
      "群缓存取消后会直接移出缓存列表，不再保留已取消记录。",
      "群缓存新增按视频标题和大小排重，避免同一视频反复进入队列。",
      "群缓存列表支持拖动排序，排序结果会保存到服务端并影响后续缓存顺序。",
      "后台每 10 分钟巡检群缓存状态，失败、停滞或排队任务会自动重新拉起，直到缓存完成。",
      "下载列表和群缓存都增加网络波动自动重试，缓解 Request was unsuccessful 5 time(s) 这类 Telegram 网络请求失败。"
    ].join("\n")
  },
  {
    id: "release-2.0.2",
    title: "Feigram 2.0.2 更新",
    version: "2.0.2",
    level: "success",
    createdAt: "2026-06-11T21:00:00.000Z",
    body: [
      "优化群视频后台缓存状态恢复，刷新页面或重新打开窗口后会继续显示任务状态。",
      "群缓存新增一键开启/暂停，暂停时不再继续拉起队列任务，重新开启后会恢复未完成任务。",
      "群缓存新增最大缓存速率限制，可在管理员后台按需选择。",
      "群缓存任务标题后新增 X，可单独取消某个后台缓存任务。",
      "改进 Telegram 消息链接定位策略，目标消息未返回时会提示内容可能已被删除。",
      "支持已同步会话中的 t.me/c 私有群组/频道链接在客户端内定位。"
    ].join("\n")
  },
  {
    id: "release-2.0.1",
    title: "Feigram 2.0.1 更新",
    version: "2.0.1",
    level: "success",
    createdAt: "2026-06-11T20:00:00.000Z",
    body: [
      "修复聊天消息里的 Telegram 链接跳转后无法定位到目标消息的问题。",
      "修复从跳转群聊返回时无法恢复原聊天阅读位置的问题。",
      "管理员后台新增“群缓存”列表，可查看群组后台自动缓存视频的标题、进度、速度和状态。",
      "群视频后台缓存进度会落盘并通过实时事件更新，重启或升级后继续展示和恢复。",
      "修复诊断页读取日志时 handle.close is not a function 的问题。"
    ].join("\n")
  },
  {
    id: "release-2.0.0",
    title: "Feigram 2.0 fnOS Client Edition",
    version: "2.0.0",
    level: "success",
    createdAt: "2026-06-11T18:00:00.000Z",
    body: [
      "Feigram 2.0 开始按 fnOS Client Edition 方向整合，保留现有稳定功能。",
      "新增系统诊断页，可查看版本、缓存大小、任务数量、数据目录和日志尾部。",
      "新增应用内更新检测，可跳转到 GitHub 发布页。",
      "用户主动下载任务和群组后台静默缓存任务支持落盘，重启或升级后自动恢复。",
      "群组后台缓存限制最多 5 个并发，避免大群一次性拉起过多任务。",
      "群组信息资源列表支持滚动加载更多。"
    ].join("\n")
  },
  {
    id: "release-1.5.9",
    title: "Feigram 1.5.9 更新",
    version: "1.5.9",
    level: "success",
    createdAt: "2026-06-11T15:30:00.000Z",
    body: [
      "下载中心改用任务创建时间排序，下载进度刷新不会再让列表上下跳动。",
      "群组信息新增图片、视频、文件资源入口，可按类型浏览并直接打开资源。",
      "群组信息新增后台自动缓存开关，可静默缓存本群大于 100MB 的视频，不进入用户下载列表。",
      "视频消息新增封面预览和时长展示，优先使用 Telegram 自带缩略图，缓存后可用 ffmpeg 生成封面。",
      "去掉视频窗口上的播放器模式提示，减少聊天界面干扰。"
    ].join("\n")
  },
  {
    id: "release-1.5.8",
    title: "Feigram 1.5.8 更新",
    version: "1.5.8",
    level: "success",
    createdAt: "2026-06-11T12:00:00.000Z",
    body: [
      "修复管理员后台和群组信息侧栏被内容撑开的布局问题。",
      "下载中心移除右键菜单，所有操作改为任务卡片内的小按钮。",
      "下载标题、进度、速度和时间改为稳定展示，减少标题抖动。",
      "同一视频下载记录统一去重，成功记录不会重复堆叠。",
      "默认播放策略改为原始视频在线播放优先。",
      "优化图片和视频消息尺寸规则，媒体组展示更接近 Telegram 官方客户端。"
    ].join("\n")
  },
  {
    id: "release-1.5.7",
    title: "Feigram 1.5.7 更新",
    version: "1.5.7",
    level: "success",
    createdAt: "2026-06-10T11:00:00.000Z",
    body: [
      "修复下载失败后被播放器重试反复拉起的问题。",
      "下载缓存改用 GramJS 官方 downloadMedia 流程，降低 InputFileLocation 兼容问题。",
      "下载完成的视频可在下载列表中直接点击播放。",
      "管理员后台新增播放器模式设置：浏览器原始播放和本地播放器。",
      "公告和管理员后台弹窗统一限制高度，内容区支持滚动。",
      "群组信息中的图片、视频和文件支持快捷访问。"
    ].join("\n")
  },
  {
    id: "release-1.5.6",
    title: "Feigram 1.5.6 更新",
    version: "1.5.6",
    level: "success",
    createdAt: "2026-06-10T10:00:00.000Z",
    body: [
      "修复视频缓存下载 Cannot cast Message to any kind of InputFileLocation 的问题。",
      "下载中心区分清除列表记录和删除缓存文件。",
      "聊天消息和视频预览按窗口宽度展开，未播放和播放后的窗口尺寸保持一致。",
      "新增群组信息侧栏，点击聊天头部可查看基础信息、媒体统计和最近文件。",
      "FPK 内置 ffmpeg，用于视频封面兜底截帧和媒体辅助处理。"
    ].join("\n")
  },
  {
    id: "release-1.5.5",
    title: "Feigram 1.5.5 更新",
    version: "1.5.5",
    level: "success",
    createdAt: "2026-06-10T08:00:00.000Z",
    body: [
      "新增下载功能区，缓存视频会进入统一下载列表。",
      "下载任务支持进度、速度展示，并可开始、取消、删除。",
      "视频缓存按钮移动到播放器右上角，状态与下载任务联动。",
      "缓存任务使用临时分片文件，取消后再次开始会从已写入位置继续。",
      "继续保留 Range 边加载边播放能力；浏览器无法解码的视频可缓存后使用本地播放器。"
    ].join("\n")
  },
  {
    id: "release-1.5.4",
    title: "Feigram 1.5.4 更新",
    version: "1.5.4",
    level: "success",
    createdAt: "2026-06-10T00:00:00.000Z",
    body: [
      "文件夹改为类似官方客户端的左侧竖向分组栏。",
      "聊天搜索支持回车搜索和一键清除。",
      "群聊消息可展示发言人头像与 ID，并可在后台关闭。",
      "同一组图片、视频消息会合并为媒体组网格展示。",
      "视频下载改为客户端内缓存，缓存完成后优先使用本地缓存播放。"
    ].join("\n")
  }
];

function readAbout() {
  return about;
}

function compareVersion(a, b) {
  const left = String(a || "").split(".").map((part) => Number(part) || 0);
  const right = String(b || "").split(".").map((part) => Number(part) || 0);
  const length = Math.max(left.length, right.length);
  for (let index = 0; index < length; index += 1) {
    const diff = (right[index] || 0) - (left[index] || 0);
    if (diff) return diff;
  }
  return 0;
}

function readAnnouncements() {
  return announcements.slice().sort((a, b) => compareVersion(a.version, b.version) || String(b.createdAt).localeCompare(String(a.createdAt)));
}

module.exports = {
  readAbout,
  readAnnouncements
};
