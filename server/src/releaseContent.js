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
  {
    id: "release-2.6.20",
    title: "Feigram 2.6.20 更新",
    version: "2.6.20",
    level: "success",
    createdAt: "2026-09-24T12:02:28.000Z",
    body: [
"【免费账号下载限流不再直接判失败】Telegram 对免费账号的下载带宽限流会返回 FLOOD_PREMIUM_WAIT（括号里的数字就是「请等 N 秒」），而 gotd 与我们的限流识别此前都不认识这个错误码，任务推进到几百 MB 后会直接终态失败；现在按 Telegram 给的秒数精确等待后自动续传，等待原因会显示在任务里。",
      "【等待分两层，不误撞看门狗】3 分钟以内的等待在下载器内部就地等待并重试同一个请求（4 路并发各自独立等待，代价最小）；超过 3 分钟的等待交给任务层按秒数精确等待，避免在下载器里空等被无进度看门狗误判成「挂死」而杀掉任务。",
      "【反复限流会自动降并发】同一账号累计 premium 限流 ≥3 次 → 并发减半，≥6 次 → 单线程；某一轮下载完整跑完且一次限流都没撞上即恢复原并发。免费账号的带宽配额是固定的，4 路并发只会持续撞限流、把时间耗在反复等待上，降档后总吞吐反而更高。"
    ].join("\n")
  },
  {
    id: "release-2.6.19",
    title: "Feigram 2.6.19 更新",
    version: "2.6.19",
    level: "success",
    createdAt: "2026-09-24T09:59:21.000Z",
    body: [
"【下载不再「假完成」】修掉一条「文件与自己比较」的完成判定：任务未声明大小时（照片等），此前只要落了任意字节就被判成已下载完成并改名交付；现在必须再由一次 4KB 探测确认该位置之后确实没有数据，否则拒绝交付并继续续传。",
      "【分片大小不再越界】官方下载器把「本次返回字节数 < 请求分片大小」当作文件末尾，因此分片有硬上限 512KB；此前把配置里的 1MB 直接传了进去，第一块就会被误判成「已到末尾」、官方随即以成功返回——任务看着跑完了，实际只落了一小块。现在分片被夹在 512KB 以内，断流自适应阶梯也改为 512KB → 256KB → 128KB。",
      "【官方说「到末尾」也要独立复核】官方下载器报成功只代表它遇到一次短读，而短读也可能是链路把这一块截断了。现在成功之后还会发一次 4KB 小请求独立确认：真末尾返回空块，仍能取到字节就继续续传（最多复核 5 次，避免空转）。",
      "【有洞的文件不再被交付】并发分片是乱序写入，交付前会把 .part 裁到「从 0 起的连续前缀」，杜绝「大小对得上、内容有空洞」的坏文件。",
      "【会话列表完整性不再误报】此前用「去重后条数 ≥ 扫描上限」推断是否被截断，只要翻页出现重复项就必然失效——实测扫描 500 条、去重后 494 条、服务端共计 545 条，日志却打印「完整拉取完成：494 条（服务端共计 545 条）」。现在按「已扫描条数 vs 服务端上报总数」判定，不足即明确标注【不完整列表】并说明差多少。",
      "【照片任务的大小不再缺失】Go 原生元数据里没有 message.file、照片也没有 document.size，导致照片任务的文件大小恒为 0（这正是上面那条假完成判定的入口）；现在照片会取最大一档 PhotoSize 的 size。",
      "【日志更可判】下载到达末尾时打印「连续前缀 / 声明大小 / 官方报告的末尾 / 请求次数」；交付前若裁剪过 .part 也会明确记录，便于一眼确认文件是否完整。"
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
