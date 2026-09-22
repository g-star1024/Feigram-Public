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
    id: "release-2.4.3",
    title: "Feigram 2.4.3 更新",
    version: "2.4.3",
    level: "success",
    createdAt: "2026-09-22T22:38:22.000Z",
    body: [
"首页新增「最新版本」卡片：Dashboard 直接展示最新版本与更新亮点；快捷操作移除与顶栏重复的公告入口，完整公告仍从顶栏铃铛查看。",
      "修复：管理后台「隐私设置」页打开白屏的问题（上一版引入的显示缺陷）。"
    ].join("\n")
  },
  {
    id: "release-2.4.2",
    title: "Feigram 2.4.2 更新",
    version: "2.4.2",
    level: "success",
    createdAt: "2026-09-22T18:12:32.000Z",
    body: [
"公告瘦身：应用内公告历史只保留最近 5 条版本，避免列表越积越长；完整更新说明仍可在 docs/release-notes.md 查看。",
      "通知权限入口优化：桌面通知的权限申请从公告铃铛移到「设置 → 隐私 → 通知设置」，点铃铛只管看公告，不再弹浏览器授权窗；被拒绝时页面给出恢复指引。",
      "工程清理：删除已废弃的旧版打包脚本，发版只走 build-native-fpk.sh 一条路径。"
    ].join("\n")
  },
  {
    id: "release-2.4.1",
    title: "Feigram 2.4.1 更新",
    version: "2.4.1",
    level: "success",
    createdAt: "2026-09-22T15:38:03.000Z",
    body: [
"诊断日志分级着色/级别与关键词过滤/清空按钮 + 管理后台服务端/隐私设置 tab 视觉对齐诊断页 + 双端窄屏布局核查"
    ].join("\n")
  },
  {
    id: "release-2.4.0",
    title: "Feigram 2.4.0 更新",
    version: "2.4.0",
    level: "success",
    createdAt: "2026-09-22T15:16:03.000Z",
    body: [
"Feigram 2.4.0 常规更新"
    ].join("\n")
  },
  {
    id: "release-2.3.1",
    title: "Feigram 2.3.1 更新",
    version: "2.3.1",
    level: "success",
    createdAt: "2026-09-22T13:58:01.000Z",
    body: [
"会话列表加载更稳：慢网络下放宽等待，让下载服务的具体报错（而不是一句无指向的超时）能正常返回。",
      "默认只拉当前会话、不再每次重复拉取归档文件夹，列表加载更省更快；开启「显示归档会话」时才一并拉取。",
      "加载超时时给出可读说明，并引导到「设置 → 网络代理」核对网络，而非显示无法理解的英文错误。"
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
