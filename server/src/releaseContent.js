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
