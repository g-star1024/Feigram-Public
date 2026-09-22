const test = require("node:test");
const assert = require("node:assert/strict");

const {
  accountKey,
  diffNativeAccounts,
  createNativeHealthMonitor
} = require("../src/nativeHealthMonitor");

function account(userId, accountId, patch = {}) {
  return { userId, accountId, status: "healthy", ready: true, error: "", healthPasses: 2, ...patch };
}

test("accountKey 以 userId|accountId 为唯一键", () => {
  assert.equal(accountKey(account("u", "a")), "u|a");
});

test("diffNativeAccounts: 首次快照全部算 changed", () => {
  const { changed, removed } = diffNativeAccounts([], [account("u", "a")]);
  assert.equal(changed.length, 1);
  assert.deepEqual(removed, []);
});

test("diffNativeAccounts: 无变化时 changed/removed 均为空", () => {
  const list = [account("u", "a", { updatedAt: "2026-09-22T20:00:00Z" })];
  const next = [account("u", "a", { updatedAt: "2026-09-22T20:30:00Z" })];
  const { changed, removed } = diffNativeAccounts(list, next);
  assert.deepEqual(changed, []);
  assert.deepEqual(removed, []);
});

test("diffNativeAccounts: 状态变化进入 changed", () => {
  const prev = [account("u", "a", { status: "checking", ready: false })];
  const current = [account("u", "a", { status: "healthy", ready: true })];
  const { changed } = diffNativeAccounts(prev, current);
  assert.equal(changed.length, 1);
  assert.equal(changed[0].status, "healthy");
});

test("diffNativeAccounts: 消失的账号进入 removed", () => {
  const prev = [account("u", "a"), account("u", "b")];
  const current = [account("u", "a")];
  const { changed, removed } = diffNativeAccounts(prev, current);
  assert.deepEqual(changed, []);
  assert.deepEqual(removed, ["u|b"]);
});

test("createNativeHealthMonitor: 有变化才调用 push，无变化不推送", async () => {
  const pushed = [];
  const lists = [
    [account("u", "a")],
    [account("u", "a")],
    [account("u", "a", { status: "failed", ready: false })]
  ];
  let call = 0;
  const monitor = createNativeHealthMonitor({
    getAccounts: async () => lists[call++],
    push: async (payload) => pushed.push(payload)
  });
  await monitor.poll(); // 首次
  await monitor.poll(); // 无变化
  await monitor.poll(); // 变 failed
  assert.equal(pushed.length, 2);
  assert.equal(pushed[1].changed[0].status, "failed");
});
