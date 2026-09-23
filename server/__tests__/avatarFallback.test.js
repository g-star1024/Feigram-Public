const test = require("node:test");
const assert = require("node:assert/strict");

const { avatarFallbackBuffer } = require("../src/avatarFallback");

test("avatarFallbackBuffer 生成非空 SVG 占位图", () => {
  const { buffer, contentType } = avatarFallbackBuffer();
  assert.ok(Buffer.isBuffer(buffer));
  assert.ok(buffer.length > 0);
  assert.equal(contentType, "image/svg+xml");
  assert.match(buffer.toString("utf8"), /<svg/);
});

test("avatarFallbackBuffer 原因文本被转义后写入 title", () => {
  const { buffer } = avatarFallbackBuffer('Go downloader 404 <script>&"');
  const text = buffer.toString("utf8");
  assert.match(text, /<title>/);
  assert.ok(!text.includes("<script>"));
  assert.ok(text.includes("&lt;script&gt;"));
});

test("avatarFallbackBuffer 无原因时不含 title", () => {
  const { buffer } = avatarFallbackBuffer();
  assert.ok(!buffer.toString("utf8").includes("<title>"));
});
