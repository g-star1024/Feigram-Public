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
    id: "release-2.6.25",
    title: "Feigram 2.6.25 更新",
    version: "2.6.25",
    level: "success",
    createdAt: "2026-09-24T21:59:20.000Z",
    body: [
"桌面图标显示名称「feigram」改为首字母大写的「Feigram」"
    ].join("\n")
  },
  {
    id: "release-2.6.24",
    title: "Feigram 2.6.24 更新",
    version: "2.6.24",
    level: "success",
    createdAt: "2026-09-24T15:26:27.000Z",
    body: [
"修复群组信息面板错位：点击会话标题查看群组信息时，面板现在以右侧固定抽屉展示（此前缩在窗口左下角被裁掉）"
    ].join("\n")
  },
  {
    id: "release-2.6.23",
    title: "Feigram 2.6.23 更新",
    version: "2.6.23",
    level: "success",
    createdAt: "2026-09-24T14:46:37.000Z",
    body: [
"下载列表与后台缓存列表的错误提示净化：只保留中文说明，英文技术链移入悬停提示",
      "手动下载的任务现在也会进入「资源库」",
      "后台缓存面板优化：标注下载引擎、保守模式下并发固定为 1 的说明与展示修正",
      "账号管理：重新登录按钮移到退出按钮之前",
      "桌面图标名称改为 feigram",
      "修复网络抖动时 waitSession: connection dead / engine forcibly closed 被误判为终态失败"
    ].join("\n")
  },
  {
    id: "release-2.6.22",
    title: "Feigram 2.6.22 更新",
    version: "2.6.22",
    level: "success",
    createdAt: "2026-09-24T13:55:37.000Z",
    body: [
"- 升级后不再需要手动重试：此前因 FLOOD_PREMIUM_WAIT（免费账号带宽限流）被误判终态失败的任务，升级到本版会自动复活为排队并续传（2.6.21 升级后下载页躺尸的根因：升级复活清单漏了这个新错误文案）",
      "- 手动「开始」仍可用：强制重启 + 断点续传"
    ].join("\n")
  },
  {
    id: "release-2.6.21",
    title: "Feigram 2.6.21 更新",
    version: "2.6.21",
    level: "success",
    createdAt: "2026-09-24T13:12:58.000Z",
    body: [
"修复：普通 FLOOD_WAIT（420）被误报成「免费账号带宽配额 FLOOD_PREMIUM_WAIT」，等待原因显示错误",
      "修复：普通限流不再污染 premium 计数，避免无辜触发下载并发减半",
      "说明：免费账号的 FLOOD_PREMIUM_WAIT 处置（按 Telegram 秒数精确等待、长等待交由任务层、反复限流降并发）保持不变"
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
