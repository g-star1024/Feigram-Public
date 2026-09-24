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
    id: "release-2.6.18",
    title: "Feigram 2.6.18 更新",
    version: "2.6.18",
    level: "success",
    createdAt: "2026-09-24T07:24:42.000Z",
    body: [
"- 【下载内核更换】改用 gotd 官方 telegram/downloader：并发分片（4 路并行拉取）+ 分片级重试（FLOOD_WAIT 按 Telegram 秒数等待、TIMEOUT 立即重试），取代此前手写的单线程顺序取片循环。官方实现同时保证分片 4KB 对齐、默认 512KB 分片。",
      "- 【退避分级：断流不再罚站】「服务端未确认（retryUntilAck retry limit reached）」这类链路断流改用短退避 5 秒 → 60 秒封顶并立刻重建媒体连接（2.6.16 实测同样是断流却要等 2 分 40 秒，1.5GB 文件要跑十几小时）；FLOOD_WAIT 限流仍按 Telegram 给的秒数精确等待。",
      "- 【同一 DC 反复断流会降级】某媒体 DC 连续断流 2 次后，在候选序列中降级（只降级不排除，兜底路径仍在）；该 DC 重新跑出流量即恢复首选。",
      "- 【分片自适应】一次尝试内连续断流时，分片自动从 1MB 缩到 512KB、再缩到 256KB —— 大分片在丢包链路上「断一次就整片重来」。",
      "- 【断点续传对官方下载器透明】请求分片偏移与写入偏移同时平移，已下载部分不会重下；并发乱序写入用「已完成区间合并」求出连续前缀作为续传点，不会在文件里留下空洞。",
      "- 【数据安全】下载中文件改为读写模式打开（并发分片写与追加模式互斥）、失败时裁到连续前缀、完成判定改用连续前缀而非文件大小 —— 避免「有空洞的文件被当成完整文件」交付。",
      "- 【看门狗适配】已开始传输后的无进度窗口从 2 分钟放宽到 5 分钟：官方下载器会在内部静默等待 FLOOD_WAIT，原窗口会把「正在按规矩等限流」误判成挂死。首字节窗口仍保持 30 秒快速失败。"
    ].join("\n")
  },
  {
    id: "release-2.6.17",
    title: "Feigram 2.6.17 更新",
    version: "2.6.17",
    level: "success",
    createdAt: "2026-09-24T06:24:15.000Z",
    body: [
"修复会话列表翻页游标：messages.getDialogs 的 offset_date 在 TL 里是无条件字段（官方迭代器每页都会带上它），此前只传 offset_peer + offset_id、offset_date 恒为 0，服务端因此定位不到「上一页末尾」，每页都从列表开头重发——实测 4 页共取回 400 条，按会话去重后只剩 102 条，这才是「群组列表还是不全」的真因。",
      "翻页终止判定改用服务端上报的总数（messages.dialogsSlice.Count）：取满即收工，不再用「本页不满一页」去猜末尾，避免漏页与空翻。",
      "翻页途中遇限流不再直接截断：FLOOD_WAIT 在 30 秒内会等待后继续翻页（此前第 5 页 FLOOD_WAIT(24) 直接收工，列表被永久截断）；页与页之间加 200ms 间隔，避免连发自造限流。",
      "新增逐页诊断日志：每页记录「请求条数 / 返回条数 / 新增条数 / 累计条数 / 服务端共计 / 下一页游标」，翻页是否真的在前进一眼可判。",
      "列表不完整时显式标注【不完整列表】并说明已返回多少条、服务端共计多少条，不再静默返回一个短列表。",
      "peer 索引深翻（backfillNativePeerIndex）同步修正同一处游标缺陷——它此前同样漏传 offset_date，这也是 peer 索引长期涨不上去的原因。"
    ].join("\n")
  },
  {
    id: "release-2.6.16",
    title: "Feigram 2.6.16 更新",
    version: "2.6.16",
    level: "success",
    createdAt: "2026-09-24T05:58:28.000Z",
    body: [
"修复 2.6.15 安装包依赖树不完整导致的「本地应用启动失败」：构建时 npm ci 静默丢了 11 个文件，其中 engine.io/build/parser-v3/index.js 是 socket.io 的加载期依赖，node 一启动就 MODULE_NOT_FOUND 秒退，飞牛便报启动失败。",
      "本版重建完整依赖树；下载器与前端功能与 2.6.15 完全一致（DC 迁移修复、AUTH_BYTES_INVALID 转瞬态、会话分页韧性等全部保留）。",
      "新增依赖树守卫：逐个 require 生产依赖、校验 node_modules 文件数下限、真实启动服务并断言 /api/health 返回 200。",
      "守卫已接入构建流程与 FPK 核证，判据从「版本号对得上、文件存在」升级为「依赖图能加载、服务真能起来」；用 2.6.15 坏包跑守卫可稳定复现失败，正负例均已验证。",
      "启动失败原因落盘：启动脚本失败时把原因与日志尾部写入 feigram.log，不再因飞牛吞掉标准输出而无从定位。"
    ].join("\n")
  },
  {
    id: "release-2.6.15",
    title: "Feigram 2.6.15 更新",
    version: "2.6.15",
    level: "success",
    createdAt: "2026-09-24T05:10:00.000Z",
    body: [
"### R4.37 DC 迁移修复 + 分页韧性（2.6.14 实测反馈）",
      "- **FILE_MIGRATE 两种形态均识别**。gotd 会打出 `FILE_MIGRATE (1)`（空格",
      "括号形态），此前只认 `FILE_MIGRATE_1` 下划线形态——迁移指令被当普通瞬态，",
      "任务在错误 DC 与文件所在 DC 之间打转（2.6.14 实测 6f32 复现）。现在命中即",
      "切目标 DC 续传（带 3 次迁移预算防循环）。",
      "- **AUTH_BYTES_INVALID 转瞬态**。export/import 授权字节被目标 DC 拒收",
      "（多为代理连接损坏），重试常能自愈；现可读文案 + 自动重试，不再一票终态。",
      "- **会话分页韧性**。真分页后中间页失败会重试一次，仍失败则返回已拉取的",
      "部分结果——不再「一页失败、整单报废」导致前端列表全空。",
    ].join("\n")
  },
  {
    id: "release-2.6.14",
    title: "Feigram 2.6.14 更新",
    version: "2.6.14",
    level: "success",
    createdAt: "2026-09-24T04:00:00.000Z",
    body: [
"### R4.36 群组列表显示不全的根因修复（2.6.13 实测反馈）",
      "- **会话列表/群组显示不全的根因找到并修复**。`messages.getDialogs` 的单页",
      "硬上限是 100 条，而代码把它当成可一次拉 500 条——于是每次都只拿到前 100 个",
      "会话，第 100 条之后的群组在列表里**永不出现**（你的账号正是这种情况）。",
      "现在改为按 offset 真分页累加，直到取满上限或列表末尾。",
      "- **顺带终结「找不到会话」**。同一个误判让深翻分页在第一轮就被误认为",
      "「已到列表末尾」，分页从未真正推进过——这就是 Channel:2052039292 一直",
      "解析不出来的真正原因（此前几轮一直以为是限流）。修复后 peer 索引能覆盖",
      "更深的会话列表。",
      "- **peer 解析冷却分级**。一个不可达频道不再冻结整个账号的解析：限流类失败",
      "才走账号级退避，结构性「找不到会话」只退避该 peer，其他频道照常解析。",
      "- **连续 3 次结构性失败转可操作终态**：直接告诉你「该频道已不在会话列表，",
      "请重新加入后点重试」，不再无限重试；手动重试会重置计数，重新加入即恢复。",
      "- **调度前感知解析冷却**：冷却期内不再白白建连、选 DC、探测后才失败。",
      "- **下载进度心跳**：长任务期间每 30 秒一条进度（字节/百分比/DC/速率），",
      "不再出现「日志静默两分钟，最后只等来一句 context canceled」。",
      "- **选 DC 前做真实 RPC 探针**：握手成功不等于能跑数据流（实测 DC 1/4 握手",
      "全绿却反复失败）。现在按探测结论优选候选 DC，并逐个做一次真实加密往返",
      "验证，不通就换下一个。",
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
