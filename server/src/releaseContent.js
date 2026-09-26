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
    id: "release-2.6.47",
    title: "Feigram 2.6.47 更新",
    version: "2.6.47",
    level: "success",
    createdAt: "2026-09-26T17:18:17.000Z",
    body: [
"Docker 运行层注入 APP_VERSION：修复容器内应用自报版本显示为 0.1.0/dev 的问题",
      "修复诊断页「版本」显示 dev 与更新检查恒报「有新版本」的误报（此前版本参照恒为 dev）"
    ].join("\n")
  },
  {
    id: "release-2.6.46",
    title: "Feigram 2.6.46 更新",
    version: "2.6.46",
    level: "success",
    createdAt: "2026-09-26T03:40:00.000Z",
    body: [
"新增 Docker 部署：官方多架构镜像（amd64 / arm64）发布到 ghcr.io/g-star1024/feigram",
"其他 NAS（群晖/威联通/极空间等）与服务器 docker run 或 docker compose 即可部署，无需飞牛 OS",
"单容器架构（Go 下载器 + Node 网关 + 前端），全部数据持久化在 /data 单卷，升级镜像只需保留数据卷",
"README 重写：同步最新功能说明与 Docker 部署指南，修正过时的架构描述"
    ].join("\n")
  },
  {
    id: "release-2.6.45",
    title: "Feigram 2.6.45 更新",
    version: "2.6.45",
    level: "success",
    createdAt: "2026-09-26T03:15:00.000Z",
    body: [
"修复群信息面板绿勾只对首屏内容有效（2026-09-26 实测）：后台缓存扫描此前只覆盖最近 200 条消息，下拉加载出的更早视频从未入队",
"扫描改深度翻页：按游标向更早历史翻页（最多约 3000 条消息），单次仍最多入队 30 个；已入队的按重复跳过、不占名额",
"重复勾选可逐轮向更早的历史补齐，几轮即可覆盖全群大视频；扫描结果文案会显示翻页深度与是否已到历史尽头"
    ].join("\n")
  },
  {
    id: "release-2.6.44",
    title: "Feigram 2.6.44 更新",
    version: "2.6.44",
    level: "success",
    createdAt: "2026-09-26T00:30:00.000Z",
    body: [
"修复群信息勾选缓存仍报「请求超时」（2026-09-26 实测）：受理改走 WebSocket 长连接，不再受浏览器 6 连接被媒体缩略图占满的限制，断连时自动回退 HTTP",
"✓ 缓存徽标修正：此前被视频时长胶囊样式覆盖成白勾并与之重叠——改为绿色圆标并移到左上角，不再遮挡时长",
"群组信息面板本地缓存：打开秒显成员数/媒体统计/最近资源，网络刷新后台更新；「加载更多媒体」结果同步入缓存"
    ].join("\n")
  },
  {
    id: "release-2.6.43",
    title: "Feigram 2.6.43 更新",
    version: "2.6.43",
    level: "success",
    createdAt: "2026-09-25T23:50:00.000Z",
    body: [
"群信息视频列表标 ✓：已提交/已缓存（下载中、排队、已完成）的视频右上角绿色对勾，重复勾选自动跳过",
"单次后台缓存提交上限 30 条：大群不再一次性全量入队；重新勾选会跳过已提交的继续补下一批",
"缓存提交受理超时放宽至 30s 兜底（配合 2.6.42 头像缓存减少连接占用，提交排队场景大幅减少）"
    ].join("\n")
  },
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
