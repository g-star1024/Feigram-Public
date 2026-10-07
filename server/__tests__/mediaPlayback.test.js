// R4.73：资源库已缓存视频「点播一直转圈」回归测试。
// 根因：播放一律走 Go blob 在线拉流（proxyBlob），无超时、失败返回空响应。
// 修复：① 已完成下载任务的本地文件优先直出（支持 Range）；② proxyBlob 拉流超时；
// ③ 在线失败/超时显式报错（502/504），不再空响应导致永久转圈。
process.env.DATA_DIR = process.env.DATA_DIR || "/tmp/feigram-mediaplayback-test";
process.env.DOWNLOAD_DIR = process.env.DOWNLOAD_DIR || "/tmp/feigram-mediaplayback-test/downloads";
process.env.TELEGRAM_API_ID = process.env.TELEGRAM_API_ID || "123456";
process.env.TELEGRAM_API_HASH = process.env.TELEGRAM_API_HASH || "0123456789abcdef0123456789abcdef";
process.env.FEIGRAM_DOWNLOADER_URL = process.env.FEIGRAM_DOWNLOADER_URL || "http://127.0.0.1:3091";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { EventEmitter } = require("node:events");

const tg = require("../src/telegramService");

// ---- 测试用假 res（捕获 writeHead / write / end，模拟 Express 响应）----
// 必须是 EventEmitter 且具备 write/end，才能被 fs.createReadStream(...).pipe(res) 使用。
function fakeRes() {
  const chunks = [];
  const res = new EventEmitter();
  res.headers = {};
  res.headersSent = false;
  res.statusCode = 0;
  res.destroyed = false;
  res.writeHead = function (status, headers) {
    res.statusCode = status;
    res.headersSent = true;
    Object.assign(res.headers, headers);
    return res;
  };
  res.status = function (code) {
    res.statusCode = code;
    return res;
  };
  res.write = function (chunk) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    return true;
  };
  res.end = function (chunk) {
    if (chunk) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    res.destroyed = true;
    res.headersSent = true; // status().end() 也会真正发出响应
    res.emit("finish");
    return res;
  };
  Object.defineProperty(res, "body", { get() { return Buffer.concat(chunks); } });
  return res;
}

function stubFetch(handler) {
  const original = globalThis.fetch;
  globalThis.fetch = handler;
  return () => {
    globalThis.fetch = original;
  };
}

function upstreamStub(status, body, extraHeaders = {}) {
  const buf = Buffer.isBuffer(body) ? body : Buffer.from(body || "");
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: {
      get: (k) => {
        const lk = String(k).toLowerCase();
        if (lk === "content-type") return extraHeaders["content-type"] || "video/mp4";
        if (lk === "content-length") return String(buf.length);
        if (lk === "content-range") return extraHeaders["content-range"] || null;
        return null;
      }
    },
    body: buf.length ? [buf] : null
  };
}

test("serveLocalFile 全量直出：200 + 完整内容", async () => {
  const tmp = path.join(os.tmpdir(), `feigram-mp-test-${Date.now()}.mp4`);
  const content = Buffer.from("0123456789abcdefghij".repeat(8)); // 160 bytes
  fs.writeFileSync(tmp, content);
  try {
    const res = fakeRes();
    const handled = tg.serveLocalFile(res, tmp, content.length, null);
    assert.equal(handled, true);
    await new Promise((resolve) => res.once("finish", resolve)); // createReadStream 异步写入
    assert.equal(res.statusCode, 200);
    assert.equal(res.headers["Accept-Ranges"], "bytes");
    assert.equal(Number(res.headers["Content-Length"]), content.length);
    assert.deepEqual(res.body, content);
  } finally {
    fs.unlinkSync(tmp);
  }
});

test("serveLocalFile 范围直出：206 + 切片内容 + Content-Range", async () => {
  const tmp = path.join(os.tmpdir(), `feigram-mp-test-${Date.now()}-r.mp4`);
  const content = Buffer.from("0123456789abcdefghij".repeat(8)); // 160 bytes
  fs.writeFileSync(tmp, content);
  try {
    const res = fakeRes();
    const handled = tg.serveLocalFile(res, tmp, content.length, "bytes=10-19");
    assert.equal(handled, true);
    await new Promise((resolve) => res.once("finish", resolve));
    assert.equal(res.statusCode, 206);
    assert.equal(res.headers["Content-Range"], `bytes 10-19/${content.length}`);
    assert.equal(Number(res.headers["Content-Length"]), 10);
    assert.deepEqual(res.body, content.subarray(10, 20));
  } finally {
    fs.unlinkSync(tmp);
  }
});

test("serveLocalFile 非法范围回退全量：200", async () => {
  const tmp = path.join(os.tmpdir(), `feigram-mp-test-${Date.now()}-bad.mp4`);
  const content = Buffer.from("abcdefghij".repeat(10));
  fs.writeFileSync(tmp, content);
  try {
    const res = fakeRes();
    const handled = tg.serveLocalFile(res, tmp, content.length, "bytes=999999-");
    assert.equal(handled, true);
    await new Promise((resolve) => res.once("finish", resolve));
    assert.equal(res.statusCode, 200);
    assert.deepEqual(res.body, content);
  } finally {
    fs.unlinkSync(tmp);
  }
});

test("proxyBlob 在线成功：200 且流式透传", async () => {
  const restore = stubFetch(async () => upstreamStub(200, "hello-proxy-body"));
  try {
    const res = fakeRes();
    const ok = await tg.proxyBlob(res, "http://fake/blob", { inline: true, fileName: "x.mp4" });
    assert.equal(ok, true);
    assert.equal(res.statusCode, 200);
    assert.equal(res.body.toString(), "hello-proxy-body");
  } finally {
    restore();
  }
});

test("proxyBlob 在线 Range 成功：206 + Content-Range", async () => {
  const restore = stubFetch(async () => upstreamStub(206, "partial", { "content-range": "bytes 0-5/100" }));
  try {
    const res = fakeRes();
    const ok = await tg.proxyBlob(res, "http://fake/blob", { inline: true, fileName: "x.mp4", range: "bytes=0-5" });
    assert.equal(ok, true);
    assert.equal(res.statusCode, 206);
    assert.equal(res.headers["Content-Range"], "bytes 0-5/100");
  } finally {
    restore();
  }
});

test("proxyBlob 上游非成功（③）：显式错误码，不返回空响应", async () => {
  const restore = stubFetch(async () => upstreamStub(500, ""));
  try {
    const res = fakeRes();
    const ok = await tg.proxyBlob(res, "http://fake/blob", { inline: true, fileName: "x.mp4" });
    assert.equal(ok, false);
    assert.equal(res.headersSent, true); // 关键：已写状态码，前端 onError 可触发
    assert.equal(res.statusCode, 500);
    assert.equal(res.body.length, 0);
  } finally {
    restore();
  }
});

test("proxyBlob 拉流失败/超时（②）：显式 504，不永久挂起", async () => {
  const restore = stubFetch(async () => {
    throw new Error("upstream hung / network unreachable");
  });
  try {
    const res = fakeRes();
    const ok = await tg.proxyBlob(res, "http://fake/blob", { inline: true, fileName: "x.mp4" });
    assert.equal(ok, false);
    assert.equal(res.headersSent, true);
    assert.equal(res.statusCode, 504); // 超时被 AbortController 中止后走同一 catch 分支
  } finally {
    restore();
  }
});

test("nativeCachedFile 任务缺失时安全返回 null（不抛错，回退在线）", async () => {
  // 测试环境无 Go sidecar，getTask 会抛错；必须被 catch 吞掉返回 null，绝不能让播放崩溃。
  const result = await tg.nativeCachedFile("u-missing", "a-missing", "p1", "m1");
  assert.equal(result, null);
});
