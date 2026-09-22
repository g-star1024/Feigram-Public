const test = require("node:test");
const assert = require("node:assert/strict");

const { shouldAutoUpgrade, createTransportAutoUpgrade } = require("../src/transportAutoUpgrade");

function stateWith(transport) {
  return { config: { transport }, transport };
}
const healthyAccount = { accountId: "a", ready: true, status: "healthy" };
const failedAccount = { accountId: "b", ready: false, status: "failed" };

test("http-bridge 且有 healthy 账号时应升级", () => {
  assert.equal(shouldAutoUpgrade(stateWith("http-bridge"), [healthyAccount], false), true);
});

test("已是 native 时不升级", () => {
  assert.equal(shouldAutoUpgrade(stateWith("native-mtproto"), [healthyAccount], false), false);
});

test("http-bridge 但无 healthy 账号时不升级", () => {
  assert.equal(shouldAutoUpgrade(stateWith("http-bridge"), [failedAccount], false), false);
});

test("已自动升级过后不再升级（避免覆盖用户手动退回）", () => {
  assert.equal(shouldAutoUpgrade(stateWith("http-bridge"), [healthyAccount], true), false);
});

test("顶层 transport 缺省时回退 state.transport 判定", () => {
  assert.equal(shouldAutoUpgrade({ transport: "http-bridge" }, [healthyAccount], false), true);
});

test("createTransportAutoUpgrade: 满足条件只升级一次，之后不再触发", async () => {
  let applied = 0;
  const upgrade = createTransportAutoUpgrade({
    getState: async () => stateWith("http-bridge"),
    getAccounts: async () => [healthyAccount],
    applyTransport: async () => { applied += 1; }
  });
  assert.equal(await upgrade.check(), true);
  assert.equal(await upgrade.check(), false); // done=true 不再触发
  assert.equal(applied, 1);
});

test("createTransportAutoUpgrade: 条件不满足时不调用 applyTransport", async () => {
  let applied = 0;
  const upgrade = createTransportAutoUpgrade({
    getState: async () => stateWith("http-bridge"),
    getAccounts: async () => [failedAccount],
    applyTransport: async () => { applied += 1; }
  });
  assert.equal(await upgrade.check(), false);
  assert.equal(applied, 0);
});
