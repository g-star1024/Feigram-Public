// R4.60：后台缓存扫描异步作业——提交即受理、结果经状态接口轮询、失败进 job.error。
// 用不存在的账号（nativeAccountRecord 为空 → reloginError）驱动完整的
// running → error 状态流转，保证前端轮询一定能拿到可区分的终态。
process.env.DATA_DIR = process.env.DATA_DIR || "/tmp/feigram-autocache-test";
process.env.DOWNLOAD_DIR = process.env.DOWNLOAD_DIR || "/tmp/feigram-autocache-test/downloads";
process.env.TELEGRAM_API_ID = process.env.TELEGRAM_API_ID || "123456";
process.env.TELEGRAM_API_HASH = process.env.TELEGRAM_API_HASH || "0123456789abcdef0123456789abcdef";
process.env.FEIGRAM_DOWNLOADER_URL = process.env.FEIGRAM_DOWNLOADER_URL || "http://127.0.0.1:3091";

const test = require("node:test");
const assert = require("node:assert/strict");

const tg = require("../src/telegramService");

test("cacheLargeVideosStatus 初始返回 none", () => {
  assert.deepEqual(tg.cacheLargeVideosStatus("u1", "a1", "p1"), { status: "none" });
});

test("startAutoCacheScan 受理后状态流转到 error（账号需重新登录，不静默）", async () => {
  const first = tg.cacheLargeVideosInChat("u1", "a-missing", "p1", null);
  assert.deepEqual(first, { started: true });
  // 首个 job 刚受理仍在 running（同 tick 内不可能已完成），防重入应生效。
  const second = tg.cacheLargeVideosInChat("u1", "a-missing", "p1", null);
  assert.deepEqual(second, { started: false, alreadyRunning: true });

  // 轮询直到终态（异步 job 立即开始，等待最多 2s）。
  let snapshot = null;
  for (let i = 0; i < 40; i += 1) {
    snapshot = tg.cacheLargeVideosStatus("u1", "a-missing", "p1");
    if (snapshot.status === "error") break;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert.equal(snapshot.status, "error", `期望 error，实际 ${snapshot.status}`);
  assert.match(snapshot.error, /重新登录/);
  assert.ok(snapshot.startedAt, "应带 startedAt 时间戳");
});
