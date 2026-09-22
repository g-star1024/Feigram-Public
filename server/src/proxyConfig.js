// 网络代理配置的规范化与脱敏（纯函数，便于单测）。
//
// 单一真源说明：真正决定「代理是否生效」的是 Go 侧（MTProto dialer 与媒体下载都在那里）。
// 本模块只做两件事：
//   1. 保存设置时做一次轻量校验，让用户在表单上立刻看到错误，而不是保存后才发现；
//   2. 打印日志时脱敏，避免把代理账号密码写进日志。
// 权威状态一律以 Go 侧 /api/state 的 proxy 字段为准（source / enabled / address / error）。
//
// 规则与 Go 侧 proxy.go 的 normalizeProxyURL 保持一致，改动时两边必须同步。

const SUPPORTED_PROTOCOLS = new Set(["socks5:", "socks5h:", "http:", "https:"]);

// 主机名必须是纯 ASCII 主机字符。
// 需要这道检查的原因：WHATWG URL 对非特殊协议不做主机校验，
// new URL("socks5://这不是地址") 会把中文百分号编码成 "%E8%BF%99..." 并当成合法主机，
// 于是「非法输入」会伪装成一个看起来正常的代理地址流进日志与界面。
// 带 % 或非 ASCII 一律判为非法；punycode（xn--）、IPv4、[::1] 都能通过。
const PROXY_HOST_PATTERN = /^[A-Za-z0-9.\-:[\]]+$/;

function isValidProxyHost(hostname) {
  const value = String(hostname ?? "");
  if (!value || value.includes("%")) {
    return false;
  }
  return PROXY_HOST_PATTERN.test(value);
}

/**
 * 规范化代理地址。
 * @param {unknown} raw 用户输入，例如 "socks5://127.0.0.1:7890" 或 "127.0.0.1:7890"
 * @returns {{ value: string, error: string }} error 非空表示校验未通过
 */
function normalizeProxyUrl(raw) {
  const trimmed = String(raw ?? "").trim();
  if (!trimmed) {
    return { value: "", error: "" };
  }
  const withScheme = trimmed.includes("://") ? trimmed : `socks5://${trimmed}`;

  let parsed;
  try {
    parsed = new URL(withScheme);
  } catch {
    return { value: trimmed, error: "代理地址无法解析，示例：socks5://127.0.0.1:7890" };
  }
  if (!SUPPORTED_PROTOCOLS.has(parsed.protocol)) {
    return {
      value: trimmed,
      error: `不支持的代理协议 ${parsed.protocol.replace(":", "")}（仅支持 socks5 / socks5h / http / https）`
    };
  }
  if (!parsed.hostname) {
    return { value: trimmed, error: "代理地址缺少主机名" };
  }
  if (!isValidProxyHost(parsed.hostname)) {
    return { value: trimmed, error: "代理地址的主机名无效，请填写 IPv4/IPv6/域名" };
  }
  if (!parsed.port) {
    return { value: trimmed, error: "代理地址缺少端口，请写完整（如 socks5://127.0.0.1:7890）" };
  }
  if (parsed.pathname && parsed.pathname !== "/") {
    return { value: trimmed, error: "代理地址不应包含路径" };
  }
  const credentials = parsed.username
    ? `${parsed.username}${parsed.password ? `:${parsed.password}` : ""}@`
    : "";
  return { value: `${parsed.protocol}//${credentials}${parsed.host}`, error: "" };
}

/**
 * 脱敏：只保留 协议://主机:端口，去掉账号密码。
 * 先过一遍校验，保证不会把「非法输入」脱敏成看似正常的地址写进日志或界面。
 * @param {unknown} raw
 * @returns {string} 非法或无法解析时返回空串
 */
function maskProxyUrl(raw) {
  const { value, error } = normalizeProxyUrl(raw);
  if (error || !value) {
    return "";
  }
  const parsed = new URL(value);
  return `${parsed.protocol}//${parsed.host}`;
}

/**
 * 把 Go 侧上报的 source 翻译成界面可读的来源说明。
 * @param {unknown} source "settings" | "env:ALL_PROXY" | "none" | "invalid" | 其它
 * @returns {string}
 */
function describeProxySource(source) {
  const value = String(source ?? "").trim();
  if (value === "settings") {
    return "应用内设置";
  }
  if (value === "none" || value === "") {
    return "未启用（直连）";
  }
  if (value === "invalid") {
    return "配置无效";
  }
  if (value.startsWith("env:")) {
    return `环境变量 ${value.slice(4)}`;
  }
  return value;
}

module.exports = {
  describeProxySource,
  maskProxyUrl,
  normalizeProxyUrl
};
