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
    id: "release-2.4.4",
    title: "Feigram 2.4.4 更新",
    version: "2.4.4",
    level: "success",
    createdAt: "2026-09-22T23:11:10.000Z",
    body: [
"重复账号根治：同一手机号重新登录后自动清理旧记录，不再出现同号 healthy/failed 双卡片；当前账号未就绪时会自动切换到可用账号，会话页不再整页报错。",
      "下载健康度可观测：账号卡新增「连续失败 N 次 / 最近成功时间」，运行诊断与 /health 增加「隐性退化」预警（degraded）。",
      "排队透明化：因账号未就绪而等待的下载任务，现在会显示原因（「将在账号恢复后自动继续」），不再静默卡住。",
      "日志增强：运行诊断的日志支持一键导出 .txt、新增「跟随最新」自动滚动开关。",
      "代理说明：设置页补充「本机已全局科学上网时直接填本机代理端口」的用法说明。"
    ].join("\n")
  },
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
