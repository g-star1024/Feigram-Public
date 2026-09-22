// M5.3：回归守卫——遗留 GramJS 账号路径必须统一返回 409 needsRelogin，
// 而不是悄悄走进已无 session 的 GramJS 客户端（M4.0 后 Node 不再持有 session）。
process.env.DATA_DIR = process.env.DATA_DIR || "/tmp/feigram-relogin-test";
process.env.DOWNLOAD_DIR = process.env.DOWNLOAD_DIR || "/tmp/feigram-relogin-test/downloads";
process.env.TELEGRAM_API_ID = process.env.TELEGRAM_API_ID || "123456";
process.env.TELEGRAM_API_HASH = process.env.TELEGRAM_API_HASH || "0123456789abcdef0123456789abcdef";
process.env.FEIGRAM_DOWNLOADER_URL = process.env.FEIGRAM_DOWNLOADER_URL || "http://127.0.0.1:3091";

const test = require("node:test");
const assert = require("node:assert/strict");

const tg = require("../src/telegramService");

const USER = "user-guard-test";
const ACCOUNT = "acc-guard-test";

async function expectRelogin(name, call) {
  await test(name, async () => {
    await assert.rejects(
      () => call(),
      (error) => {
        assert.equal(error.status, 409, `${name} 期望 409，实际 ${error.status}: ${error.message}`);
        assert.equal(error.needsRelogin, true, `${name} 期望 needsRelogin=true`);
        return true;
      }
    );
  });
}

expectRelogin("listChats 对遗留账号返回 409", () => tg.listChats(USER, ACCOUNT));
expectRelogin("listFolders 对遗留账号返回 409", () => tg.listFolders(USER, ACCOUNT));
expectRelogin("listMessages 对遗留账号返回 409", () => tg.listMessages(USER, ACCOUNT, "peer-1"));
expectRelogin("chatDetails 对遗留账号返回 409", () => tg.chatDetails(USER, ACCOUNT, "peer-1"));
expectRelogin("chatMedia 对遗留账号返回 409", () => tg.chatMedia(USER, ACCOUNT, "peer-1"));
expectRelogin("sendText 对遗留账号返回 409", () => tg.sendText(USER, ACCOUNT, "peer-1", "hi"));
expectRelogin("clickMessageButton 对遗留账号返回 409", () => tg.clickMessageButton(USER, ACCOUNT, "peer-1", 1, Buffer.from("{}").toString("base64")));
expectRelogin("resolveTelegramLink 对遗留账号返回 409", () => tg.resolveTelegramLink(USER, ACCOUNT, "https://t.me/durov"));
expectRelogin("search 对遗留账号返回 409", () => tg.search(USER, ACCOUNT, "hello"));
expectRelogin("downloadMedia 对遗留账号返回 409", () => tg.downloadMedia(USER, ACCOUNT, "peer-1", 1));
expectRelogin("mediaThumbnail 对遗留账号返回 409", () => tg.mediaThumbnail(USER, ACCOUNT, "peer-1", 1));
expectRelogin("profilePhoto 对遗留账号返回 409", () => tg.profilePhoto(USER, ACCOUNT, "peer-1"));

test("relogin 错误不触发任何网络调用（同步拒绝，不挂起）", async () => {
  const started = Date.now();
  await assert.rejects(() => tg.listChats(USER, ACCOUNT), (error) => error.status === 409);
  assert.ok(Date.now() - started < 3000, "守卫应立即失败，不应等待网络超时");
});
