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
    id: "release-2.6.31",
    title: "Feigram 2.6.31 更新",
    version: "2.6.31",
    level: "success",
    createdAt: "2026-09-25T02:10:00.000Z",
    body: [
"新增聊天/群组内容本地缓存：再次打开会话时直接秒开上次内容（不用再等网络），后台自动刷新最新消息",
      "「加载更早消息」与刷新的结果自动留存到本地（每会话最多 600 条），断网或超时也能翻看已缓存的历史",
      "会话列表同步本地缓存：网络异常时自动显示上次缓存的列表并轻提示，不再整页报错"
    ].join("\n")
  },
  {
    id: "release-2.6.30",
    title: "Feigram 2.6.30 更新",
    version: "2.6.30",
    level: "success",
    createdAt: "2026-09-25T00:30:42.000Z",
    body: [
"修复群组列表加载不全：会话列表扫描上限从 500 放开到 2000（此前服务端有 545+ 会话只显示 500 条，分组里缺的会话就是这个截断导致）",
      "会话列表前端请求同步加 80 秒超时兜底，列表变大后也不会出现永久加载"
    ].join("\n")
  },
  {
    id: "release-2.6.29",
    title: "Feigram 2.6.29 更新",
    version: "2.6.29",
    level: "success",
    createdAt: "2026-09-24T23:17:54.000Z",
    body: [
"修复「加载更早消息」一直显示加载中的问题：前端请求增加超时兜底（80 秒），超时或失败立即复位按钮并弹中文提示，不再永久卡住",
      "服务端到 Go 下载服务的消息/媒体/文件夹/Peer 查询超时从 45 秒对齐到 65 秒（与 Go 侧 60 秒查询上限匹配，避免先被掐断只看到英文报错）",
      "加载到没有更早消息时按钮自动收起并提示「没有更早的消息了」"
    ].join("\n")
  },
  {
    id: "release-2.6.28",
    title: "Feigram 2.6.28 更新",
    version: "2.6.28",
    level: "success",
    createdAt: "2026-09-24T22:43:49.000Z",
    body: [
"修复桌面图标四周的黑色描边：裁除原图自带的 9px 黑边后重新应用飞牛官方风格大圆角"
    ].join("\n")
  },
  {
    id: "release-2.6.27",
    title: "Feigram 2.6.27 更新",
    version: "2.6.27",
    level: "success",
    createdAt: "2026-09-24T22:29:56.000Z",
    body: [
"后台缓存列表：已完成的任务自动移出列表（在「资源库」查看），列表与角标只显示进行中的任务",
      "后台缓存面板：调度设置（开关/限速/并发/模式）与运行诊断页重复，统一收敛到「运行诊断 → Go 下载服务」，面板只保留状态展示与任务列表",
      "模式功能确认有效（保守=单任务串行、高速=多任务并行），文案按真实行为优化为「稳妥模式（单任务串行）/高速模式（多任务并行）」",
      "桌面应用图标更新为飞牛官方风格大圆角"
    ].join("\n")
  },
  {
    id: "release-2.6.26",
    title: "Feigram 2.6.26 更新",
    version: "2.6.26",
    level: "success",
    createdAt: "2026-09-24T22:08:26.000Z",
    body: [
"资源库「已缓存」徽标改为绿色，与「缓存中」（黄色）状态明确区分"
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
