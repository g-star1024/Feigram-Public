// 头像获取失败时的占位图（2.4.4 实测反馈）：
// 此前 /api/avatar 在账号未就绪/头像拉取失败时直接抛错，
// 聊天列表每次渲染都会在 Node 日志里刷一行 "Error: Go downloader 404"。
// 这里统一返回一张内置 SVG 占位图：前端拿到 200 + 图片，日志不再刷屏，
// 账号恢复健康后下一次头像请求自然回到真实头像。

function avatarFallbackBuffer(reason) {
  const note = String(reason || "").slice(0, 60);
  const svg = [
    '<svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 96 96">',
    '<rect width="96" height="96" rx="48" fill="#E7EAF3"/>',
    '<circle cx="48" cy="38" r="15" fill="#B7BFD6"/>',
    '<path d="M20 82c4-16 15-24 28-24s24 8 28 24" fill="#B7BFD6"/>',
    ...(note ? [`<title>${escapeXml(note)}</title>`] : []),
    "</svg>",
  ].join("");
  return {
    buffer: Buffer.from(svg, "utf8"),
    contentType: "image/svg+xml",
  };
}

function escapeXml(text) {
  return String(text)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

module.exports = { avatarFallbackBuffer };
