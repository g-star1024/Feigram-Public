const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

// 必须在 require store/migrations 之前设置 DATA_DIR：
// store.js 在模块加载时就依据该环境变量解析数据目录。
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "feigram-migrate-"));
process.env.DATA_DIR = tmpDir;

const { migrateStore, metaPath, planMigrations, schemaVersion, migrations } = require("../src/migrations");

const accountsPath = path.join(tmpDir, "accounts.json");

function readJson(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

test("planMigrations 按 version 升序排列，并跳过已执行过的迁移", () => {
  const list = [
    { version: 1, name: "a", up: async () => {} },
    { version: 3, name: "c", up: async () => {} },
    { version: 2, name: "b", up: async () => {} }
  ];
  assert.deepEqual(planMigrations(0, list).map((item) => item.version), [1, 2, 3]);
  assert.deepEqual(planMigrations(2, list).map((item) => item.version), [3]);
  assert.deepEqual(planMigrations(3, list).map((item) => item.version), []);
  // 非法/缺失的 from 值按 0 处理，保证首次安装能跑全量迁移。
  assert.deepEqual(planMigrations(undefined, list).map((item) => item.version), [1, 2, 3]);
});

test("schemaVersion 等于迁移表中的最大版本", () => {
  assert.equal(schemaVersion(), Math.max(...migrations.map((item) => item.version)));
});

test("migrateStore 首次执行后到达最新 schemaVersion 并落盘", async () => {
  const result = await migrateStore();
  assert.equal(result.schemaVersion, schemaVersion());
  const meta = readJson(metaPath);
  assert.equal(meta.schemaVersion, schemaVersion());
  assert.ok(fs.existsSync(path.join(tmpDir, "download-tasks.json")));
  assert.ok(fs.existsSync(path.join(tmpDir, "silent-cache-tasks.json")));
});

test("migrateStore 可重跑：重复执行不再应用任何迁移", async () => {
  const before = readJson(metaPath);
  const result = await migrateStore();
  assert.deepEqual(result.applied, [], "重复执行不应再触发迁移");
  assert.equal(result.schemaVersion, before.schemaVersion);
});

// store.js 的账户存储结构是 { accounts: [...] }，不是裸数组。
function readAccounts(file) {
  return (JSON.parse(fs.readFileSync(file, "utf8")).accounts) || [];
}

test("存量账号被补齐 authMode，且已迁移账号的标记不会被覆盖", async () => {
  fs.writeFileSync(accountsPath, JSON.stringify({
    accounts: [
      { id: "acct_legacy", userId: "u1", session: "encrypted", label: "legacy" },
      { id: "acct_native", userId: "u1", authMode: "native", label: "native" }
    ]
  }));

  // 把 schemaVersion 退回到 1，强制重跑第 2 项迁移，以此验证幂等性。
  const meta = readJson(metaPath);
  fs.writeFileSync(metaPath, JSON.stringify({ ...meta, schemaVersion: 1 }));

  await migrateStore();

  const accounts = readAccounts(accountsPath);
  const legacy = accounts.find((item) => item.id === "acct_legacy");
  const native = accounts.find((item) => item.id === "acct_native");
  assert.equal(legacy.authMode, "gramjs", "有 session 的存量账号应标记为 gramjs");
  assert.equal(native.authMode, "native", "已迁移账号的 native 标记不应被覆盖");

  // 再跑一次，确认不产生副作用。
  const snapshot = fs.readFileSync(accountsPath, "utf8");
  await migrateStore();
  assert.equal(fs.readFileSync(accountsPath, "utf8"), snapshot);
});
