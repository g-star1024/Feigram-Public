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
const fs = require("node:fs");
const path = require("node:path");

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

// --- R4.71：扫描分页守卫 ---------------------------------------------------
//
// 根因回顾：扫描页大小曾设为 200，超过 messages.getHistory 的服务端硬上限 100，
// 服务端静默只回 100 条，而「本页不满一页 = 已到历史尽头」的判定因此在**第 1 页**
// 就成立（用户实测 2.6.47：35328 个视频的群永远只扫 1 页 100 条消息、重复点击零新增）。

test("R4.71 守卫：扫描页大小不得超过 Go /messages 的单次 RPC 硬上限", () => {
  const goSource = fs.readFileSync(
    path.join(__dirname, "..", "..", "downloader", "cmd", "feigram-downloader", "chatapi.go"),
    "utf8"
  );
  const matched = goSource.match(/chatHistoryServerLimit\s*=\s*(\d+)/);
  assert.ok(matched, "Go 侧必须存在 chatHistoryServerLimit 常量（本守卫依赖它）");
  const serverLimit = Number(matched[1]);
  assert.ok(
    tg.AUTO_CACHE_SCAN_PAGE_SIZE <= serverLimit,
    `扫描页大小 ${tg.AUTO_CACHE_SCAN_PAGE_SIZE} 不得超过服务端硬上限 ${serverLimit}`
  );
  assert.ok(tg.AUTO_CACHE_SCAN_PAGES >= 1, "扫描页数预算至少为 1");
});

test("R4.71 回归：不满一页且非空，绝不能被判为已到历史尽头", () => {
  // 旧判定是 `recent.length < AUTO_CACHE_SCAN_PAGE_SIZE → reachedEnd`，本用例必然 FAIL。
  const plan = tg.autoCachePagePlan([{ id: 300 }, { id: 250 }, { id: 210 }], 0);
  assert.equal(plan.reachedEnd, false, "不满一页不等于已到历史尽头");
  assert.equal(plan.stop, false);
  assert.equal(plan.nextBefore, 210, "下一页游标应为本页最小消息 ID");
});

test("R4.71：空页是唯一可靠的「已到历史尽头」信号", () => {
  const plan = tg.autoCachePagePlan([], 500);
  assert.equal(plan.reachedEnd, true);
  assert.equal(plan.stop, false);
});

test("R4.71：游标未前进时停止翻页，但不判已到尽头", () => {
  const plan = tg.autoCachePagePlan([{ id: 900 }, { id: 880 }], 880);
  assert.equal(plan.stop, true);
  assert.equal(plan.reachedEnd, false, "游标卡死不等于已到历史尽头");
});

test("R4.71：本页无可用消息 ID 时停止翻页，且不判已到尽头", () => {
  const plan = tg.autoCachePagePlan([{ id: 0 }, { text: "服务消息" }], 500);
  assert.equal(plan.stop, true);
  assert.equal(plan.reachedEnd, false);
});
