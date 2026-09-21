const fs = require("fs-extra");
const path = require("path");
const { dataDir, downloadTasksPath, silentCachePath, readAccounts, upsertAccount } = require("./store");

const metaPath = path.join(dataDir, "app-meta.json");

// M5.1：按 schemaVersion 有序执行的幂等数据迁移。
//
// 约定：
//  1. 每项迁移只负责把数据从 version-1 推到 version，必须可重复执行且无副作用
//     （重复执行时不产生变更、不报错）。
//  2. 数组顺序即执行顺序；新增迁移只能追加到数组末尾并使用递增 version。
//  3. migrateStore 依据 meta 中已记录的 schemaVersion 决定从哪一项开始执行，
//     已完成的历史迁移不会被重跑。
const migrations = [
  {
    version: 1,
    name: "初始化下载任务与静默缓存存储",
    async up() {
      await fs.ensureDir(dataDir);
      if (!(await fs.pathExists(downloadTasksPath))) {
        await fs.writeJson(downloadTasksPath, { tasks: [] }, { spaces: 2 });
      }
      if (!(await fs.pathExists(silentCachePath))) {
        await fs.writeJson(silentCachePath, { tasks: [] }, { spaces: 2 });
      }
    }
  },
  {
    version: 2,
    name: "为账号补充 authMode 字段（M2.4/M4.0 的 Go 迁移标记）",
    async up() {
      const accounts = await readAccounts().catch(() => []);
      for (const account of accounts) {
        if (!account || !account.id || account.authMode) continue;
        // 有 GramJS session 的存量账号标记为 gramjs（待迁移），否则标记为 unknown。
        await upsertAccount({ id: account.id, authMode: account.session ? "gramjs" : "unknown" });
      }
    }
  }
];

// planMigrations 返回需要执行的迁移（按 version 升序）。
// 抽成纯函数是为了让「有序」「已执行不再重跑」这两个约束可被单元测试直接覆盖。
function planMigrations(fromVersion, list = migrations) {
  const from = Number.isInteger(fromVersion) && fromVersion > 0 ? fromVersion : 0;
  return [...list]
    .sort((left, right) => left.version - right.version)
    .filter((migration) => migration.version > from);
}

function schemaVersion(list = migrations) {
  return planMigrations(0, list).reduce((max, item) => Math.max(max, item.version), 0);
}

async function migrateStore() {
  await fs.ensureDir(dataDir);
  const meta = await fs.readJson(metaPath).catch(() => ({}));
  const from = Number.isInteger(meta.schemaVersion) ? meta.schemaVersion : 0;
  const pending = planMigrations(from);
  const applied = [];
  let version = from;
  for (const migration of pending) {
    await migration.up();
    version = migration.version;
    applied.push({ version: migration.version, name: migration.name });
  }
  const next = {
    ...meta,
    edition: process.env.APP_EDITION || "Feigram 2.0 fnOS Client Edition",
    version: process.env.APP_VERSION || "dev",
    schemaVersion: version,
    migrations: applied.map((item) => item.name),
    migratedAt: new Date().toISOString()
  };
  await fs.writeJson(metaPath, next, { spaces: 2 });
  return { ...next, applied };
}

module.exports = {
  migrateStore,
  metaPath,
  migrations,
  planMigrations,
  schemaVersion
};
