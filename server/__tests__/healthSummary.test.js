const test = require("node:test");
const assert = require("node:assert/strict");

const { summarizeNativeAccounts, pickAccountsSummary, sidecarTaskCount } = require("../src/healthSummary");

test("summarizeNativeAccounts: 空列表返回零值汇总", () => {
  assert.deepEqual(summarizeNativeAccounts([]), { total: 0, healthy: 0, byStatus: {} });
});

test("summarizeNativeAccounts: 兼容 { accounts: [...] } 形态", () => {
  const summary = summarizeNativeAccounts({
    accounts: [{ status: "healthy", ready: true }, { status: "needs-relogin", ready: false }]
  });
  assert.equal(summary.total, 2);
  assert.equal(summary.healthy, 1);
  assert.deepEqual(summary.byStatus, { healthy: 1, "needs-relogin": 1 });
});

test("summarizeNativeAccounts: 无 status 的账号按 ready 判定", () => {
  const summary = summarizeNativeAccounts([{ ready: true }, { ready: false }]);
  assert.equal(summary.healthy, 1);
  assert.deepEqual(summary.byStatus, { healthy: 1, "needs-login": 1 });
});

test("summarizeNativeAccounts: sidecar 不可达时带出 error 且不抛错", () => {
  const summary = summarizeNativeAccounts({ ok: false, error: "Go 下载服务无响应" });
  assert.equal(summary.total, 0);
  assert.equal(summary.error, "Go 下载服务无响应");
});

test("summarizeNativeAccounts: 非法入参（null/undefined/字符串）退化为零值", () => {
  assert.equal(summarizeNativeAccounts(null).total, 0);
  assert.equal(summarizeNativeAccounts(undefined).total, 0);
  assert.equal(summarizeNativeAccounts("garbage").total, 0);
});

test("pickAccountsSummary: Go 可用时优先采用 Go 侧汇总", () => {
  const state = { ok: true, accounts: { total: 3, healthy: 2, byStatus: { healthy: 2 } } };
  assert.deepEqual(pickAccountsSummary(state, { accounts: [] }), state.accounts);
});

test("pickAccountsSummary: Go 不可用或没给汇总时回退本地统计", () => {
  const fallback = pickAccountsSummary({ ok: false }, { accounts: [{ status: "healthy", ready: true }] });
  assert.deepEqual(fallback, { total: 1, healthy: 1, byStatus: { healthy: 1 } });
});

test("sidecarTaskCount: 兼容 taskCount / tasks / counts 三种形态", () => {
  assert.equal(sidecarTaskCount({ ok: true, taskCount: 7 }), 7);
  assert.equal(sidecarTaskCount({ ok: true, tasks: [{}, {}] }), 2);
  assert.equal(sidecarTaskCount({ ok: true, counts: { done: 2, running: 1 } }), 3);
  assert.equal(sidecarTaskCount({ ok: false, tasks: [{}] }), undefined);
});
