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
  {
    id: "release-2.6.13",
    title: "Feigram 2.6.13 更新",
    version: "2.6.13",
    level: "success",
    createdAt: "2026-09-24T03:00:00.000Z",
    body: [
"### R4.35 限流自噬修复（2.6.12 实测反馈）",
      "- 2.6.12 实测好消息：网络恢复后健康检查自动转绿、任务自动启动、媒体连接",
      "510ms 建立、探测全绿自动复活、调度紧循环冷却全部按预期工作。",
      "- **RPC 传输层失败转瞬态**。网络抖动时 `retryUntilAck: retry limit reached",
      "after 5 attempts` 被一票终态，任务躺死等手动点。现按退避自动续传；升级时",
      "落盘的此类终态任务自动复活。",
      "- **peer 解析加账号级闸门**。三个任务同时解析同一个缺失 accessHash 的频道，",
      "各自全量深翻会话列表 → Telegram 限流 FLOOD_WAIT(9)/(6) → 5~10s 后再翻，",
      "限流被自己越喂越大。现并发解析只跑一轮（结果共享），失败后账号级冷却",
      "（FLOOD_WAIT 按 Telegram 秒数，封顶 15 分钟）——不再自噬。",
      "另：本轮节点转发质量仍不稳（探测在「全绿」与「全黑洞」间摆动），建议更换节点。"
    ].join("\n")
  },
  {
    id: "release-2.6.12",
    title: "Feigram 2.6.12 更新",
    version: "2.6.12",
    level: "success",
    createdAt: "2026-09-24T02:30:00.000Z",
    body: [
"### R4.34 网络自愈补漏（2.6.11 实测反馈）",
      "- **「媒体连接建立超时」转瞬态**。2.6.11 实测：网络抖动时任务第一次 45s",
      "连接建立超时就被打成终态，一次自动重试机会都没拿到。现按退避自动续传，",
      "重试上限兜底；升级时落盘的此类终态任务自动复活。",
      "- **自动复活按「断网窗口」重臂**。此前每个任务只自动复活一次，复活后一旦",
      "撞上网络恶化就永久躺死。现网络转差时重置复活标记，每个断网窗口结束后仍能",
      "自动拉起一次；手动重试也会清标记。",
      "- **连接建立窗口自适应**：45s 基础档，按最近握手耗时放宽（8×握手ms，",
      "封顶 120s），高延迟节点不再被固定窗口误判；错误文案附最新媒体探测结论。",
      "- 启动窗口「文件夹列表加载超时」由裸 AbortError 转可读提示。",
      "另：本轮失败的直接原因仍是代理节点转发质量（丢包/限速），建议更换节点。"
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
