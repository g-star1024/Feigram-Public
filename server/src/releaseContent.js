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
    id: "release-2.5.2",
    title: "Feigram 2.5.2 更新",
    version: "2.5.2",
    level: "success",
    createdAt: "2026-09-23T03:21:30.000Z",
    body: [
"修复：健康检查分级——session 授权成功即账号可用，媒体抽样失败（如 DC_ID_INVALID）不再把账号打成 failed 堵死会话列表",
      "优化：DC_ID_INVALID 映射为可读提示（媒体 DC 授权导出被拒，账号本身可用）",
      "修复：基础检查通过也推进 Ready 恢复计数，此前「无媒体可抽/抽样失败」的账号永远无法恢复就绪"
    ].join("\n")
  },
  {
    id: "release-2.5.1",
    title: "Feigram 2.5.1 更新",
    version: "2.5.1",
    level: "success",
    createdAt: "2026-09-23T02:53:44.000Z",
    body: [
"修复：「尚未就绪（failed）」错误现在会附带健康检查的真实失败原因（如代理连不通/DC 超时），不再只有状态词"
    ].join("\n")
  },
  {
    id: "release-2.5.0",
    title: "Feigram 2.5.0 更新",
    version: "2.5.0",
    level: "success",
    createdAt: "2026-09-23T02:25:37.000Z",
    body: [
"下载中心整合：原管理后台「缓存信息」整块迁入下载中心，成为「下载任务 / 后台缓存」两个子标签",
      "后台缓存的设置项（开关·最大速率·缓存模式·并发）、运行统计（运行中/传输层/任务数）、任务列表、批量取消与拖拽排序全部保留",
      "首页统计卡可点击直达：进行中/已完成下载 → 下载任务分区，缓存任务 → 后台缓存分区"
    ].join("\n")
  },
  {
    id: "release-2.4.6",
    title: "Feigram 2.4.6 更新",
    version: "2.4.6",
    level: "success",
    createdAt: "2026-09-23T02:14:30.000Z",
    body: [
"修复：应用重启后 failed 账号不再干等 30 分钟——每次配置同步（含启动回推）都会立即全量重检",
      "优化：代理设置提示明确 v2rayA 端口配对（HTTP=20171、SOCKS5=20170），避免协议错位连不上"
    ].join("\n")
  },
  {
    id: "release-2.4.5",
    title: "Feigram 2.4.5 更新",
    version: "2.4.5",
    level: "success",
    createdAt: "2026-09-23T01:17:55.000Z",
    body: [
"修复：账号因网络不通被判 failed 后，代理一改立即自动重检，不再等 30 分钟",
      "修复：访问未就绪账号时自动安排健康检查（3 分钟冷却），聊天页可更快自愈",
      "修复：头像拉取失败改为显示占位图，日志不再刷 \"Go downloader 404\"",
      "修复：媒体源长时间不可用的任务最多自动重试 120 次后停止，不再无限刷日志"
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
