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
    id: "release-2.6.48",
    title: "Feigram 2.6.48 更新",
    version: "2.6.48",
    level: "success",
    createdAt: "2026-10-04T11:46:08.000Z",
    body: [
"修复后台缓存扫描「永远只扫 1 页 100 条消息、重复点击零新增」：messages.getHistory 单次上限为 100，此前按 200 请求导致「本页不满一页」被误判为「已到历史尽头」",
      "扫描翻页改以「空页」为唯一尽头判据，页大小锁在服务端硬上限 100 之内，单次页数预算 20 页（约 2000 条消息），并新增逐页进度日志便于排障",
      "Go /messages 改为按 OffsetID 真分页，把调用方请求量如实兑现（此前请求 200 条只回 100 条）；分页中途失败改为显式报错，不再静默返回部分结果",
      "新增分页守卫单测（Go 6 例 + Node 5 例），跨语言锁定「扫描页大小 ≤ 服务端硬上限」，防止同类根因第三次复发"
    ].join("\n")
  },
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
