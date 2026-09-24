const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const root = path.resolve(__dirname, "../..");

test("telegram service exports the GramJS download pipeline", () => {
  const source = fs.readFileSync(path.join(__dirname, "telegramService.js"), "utf8");
  assert.match(source, /cacheLargeVideosInChat,\s*\n\s*cancelDownloadTask,/);
  assert.match(source, /listDownloadTasks,\s*\n\s*listSilentCacheTasks,/);
  assert.match(source, /startDownloadTask,\s*\n\s*streamVideoMedia,/);
  assert.match(source, /restoreBackgroundTasks,/);
  assert.doesNotMatch(source, /downloaderSidecar|startGoDownloadTask|restoreGoBackgroundTasks/);
});

test("background downloads reuse the account's single GramJS auth-key owner", () => {
  const source = fs.readFileSync(path.join(__dirname, "telegramService.js"), "utf8");
  assert.match(source, /async function getCacheClient\([^)]*\) \{[\s\S]*?return getClient\(userId, accountId\);/);
  assert.doesNotMatch(source, /const cacheClients = new Map/);
  assert.match(source, /client = await getCacheClient\(task\.userId, task\.accountId\)/);
  assert.match(source, /await resetDownloadSender\(client, mediaDcId\)/);
});

test("native package does not start or build the removed Go sidecar", () => {
  const command = fs.readFileSync(path.join(root, "fnos-native-package/cmd/main"), "utf8");
  const build = fs.readFileSync(path.join(root, "scripts/build-native-fpk.sh"), "utf8");
  assert.doesNotMatch(command, /nohup .*feigram-downloader|FEIGRAM_DOWNLOADER_URL/);
  assert.doesNotMatch(build, /prepare-go-downloader/);
  assert.match(build, /rm -f .*feigram-downloader/);
});

test("admin UI no longer exposes Go downloader controls", () => {
  const source = fs.readFileSync(path.join(root, "client/src/main.jsx"), "utf8");
  assert.doesNotMatch(source, /扫码登录 Go|Go 原生账号|Go 下载服务地址|\/api\/admin\/downloader/);
  assert.match(source, /Telegram\/GramJS/);
});
