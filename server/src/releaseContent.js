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
    id: "release-2.6.0",
    title: "Feigram 2.6.0 更新",
    version: "2.6.0",
    level: "success",
    createdAt: "2026-09-23T06:14:32.000Z",
    body: [
"### R4.22 架构简化：健康检查只做一件事，下载错误直出真实原因",
      "- **健康检查收敛为「授权成功 = 健康」**",
      "- 删除整层「媒体抽样」（自动找缩略图、读 64KB、验证媒体池）。该层自 R4.17 起已不参与",
      "健康判定，却是 DC_ID_INVALID 等假故障的主要来源，属纯冗余。",
      "- 一次授权 RPC 通过即就绪，消除「界面显示健康、下载却报账号未就绪」的中间态。",
      "- 健康检查的开始/结束与分级探测结论都写入服务端日志（`health check start/end`、",
      "`health probe`），不再有零输出路径。",
      "- **下载错误直接透出真实原因**",
      "- 移除「账号不可用 → 自动回退 HTTP 桥」链路。M4.1 之后该「回退源」指向 Go 自身的",
      "blob 端点，回退只会绕回同一个坏账号，把真实原因包装成",
      "`source returned 404: {\"error\":\"Go 原生账号 … 尚未就绪…\"}` 这类三层转述。",
      "- 账号不可用时任务留在队列并显示可读原因（直接带上健康检查诊断结论），",
      "账号恢复后自动续传，无需手动重试。",
      "- **分级探测补盲区**：拿不到账号主 DC 时不再静默跳过，错误信息里必带「分级探测：」结论，",
      "便于区分「探测被跳过」与「诊断没生效」。",
      "- **保留**：R4.18 媒体主 DC 直连（根治 DC_ID_INVALID）、R4.20 出口链路/MTProto 分级探测。"
    ].join("\n")
  },
  {
    id: "release-2.5.6",
    title: "Feigram 2.5.6 更新",
    version: "2.5.6",
    level: "success",
    createdAt: "2026-09-23T05:57:22.000Z",
    body: [
"- 启动即对「有会话但未就绪」的账号自动补跑健康检查（原先要等 30 分钟巡检或 3 分钟访问冷却）",
      "- 升级/重启后新版诊断（分级探测结论）秒级可见，不再先看到旧版落盘的错误信息"
    ].join("\n")
  },
  {
    id: "release-2.5.5",
    title: "Feigram 2.5.5 更新",
    version: "2.5.5",
    level: "success",
    createdAt: "2026-09-23T05:08:38.000Z",
    body: [
"- 健康检查新增分级探测：先用与 MTProto 相同的代理出口对账号主 DC 做裸 TCP 拨号（10s），把「代理/出口链路不通」与「MTProto 握手/授权卡死」拆开",
      "- 探测失败 → 明确提示「问题在代理出口链路（未放行该 DC 直通/节点故障/端口配错）」；探测成功但超时 → 明确提示「TCP 层已通，多为节点转发质量差，建议更换节点」",
      "- 探测结果同时写入服务端日志（health probe 行）与账号错误详情，替代笼统的 context deadline exceeded",
      "- 新增 primaryDCAddr（gotd 生产 DC 表，静态 IPv4 优先）与 6 组单测；Go 全量测试通过"
    ].join("\n")
  },
  {
    id: "release-2.5.4",
    title: "Feigram 2.5.4 更新",
    version: "2.5.4",
    level: "success",
    createdAt: "2026-09-23T04:26:27.000Z",
    body: [
"修复会话页「visible is not defined」加载失败错误卡（历史重构遗留的未定义变量引用，开启「文件夹自动选第一个会话」时必现），该设置恢复生效",
      "下载目录权限不足时给出可操作提示（给目录开放写权限或改用应用可写目录），不再裸报 permission denied"
    ].join("\n")
  },
  {
    id: "release-2.5.3",
    title: "Feigram 2.5.3 更新",
    version: "2.5.3",
    level: "success",
    createdAt: "2026-09-23T03:51:02.000Z",
    body: [
"根治媒体抽样/下载的 DC_ID_INVALID：目标媒体 DC 与账号主 DC 相同时改用主连接直读，不再经 gotd 媒体池「导出授权给自己」",
      "健康检查自动抽样、缓存任务抽样、下载 worker 三条路径统一接入主 DC 短路，避免无意义的跨 DC 建池与授权往返",
      "新增主 DC 解析（从加密落库 session 读取），解析失败自动回落原有行为，不影响老账号"
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
