"use strict";

// R4.1 · 账号状态变化的 socket 推送
//
// Go 不持有 socket.io，状态变化只落在它自己的账号文件里。这里定时（每 30s）
// 拉一次 Go 账号列表，与上一次快照做 diff，把变化的账号推给对应用户的房间，
// 前端无需手动刷新即可看到「检查中 → healthy / failed」的跳转。
//
// 纯 diff 逻辑（diffNativeAccounts）抽出来以便单测。

const downloaderSidecar = require("./downloaderSidecar");

const POLL_INTERVAL_MS = 30 * 1000;

// 账号在 diff 时的唯一键。
function accountKey(account) {
  return `${account.userId}|${account.accountId}`;
}

// 只取可能变化、前端关心的字段，避免 updatedAt 抖动造成的误报。
function fingerprint(account) {
  return JSON.stringify({
    s: account.status,
    r: Boolean(account.ready),
    e: account.error || "",
    p: account.healthPasses || 0
  });
}

// diffNativeAccounts 对比上一轮与当前账号，返回 { changed, removed }。
// changed: 当前快照中指纹发生变化（或新出现）的账号；
// removed: 上一轮存在、当前已消失的账号键（如被清理）。
function diffNativeAccounts(previous, current) {
  const prevMap = new Map((previous || []).map((item) => [accountKey(item), item]));
  const changed = [];
  (current || []).forEach((account) => {
    const key = accountKey(account);
    const old = prevMap.get(key);
    if (!old || fingerprint(old) !== fingerprint(account)) {
      changed.push(account);
    }
    prevMap.delete(key);
  });
  const removed = Array.from(prevMap.keys());
  return { changed, removed };
}

// 巡检器持有上一轮快照与目标房间解析函数。
function createNativeHealthMonitor({ getAccounts, push }) {
  let previous = [];
  async function poll() {
    const current = await getAccounts();
    const { changed, removed } = diffNativeAccounts(previous, current);
    if (changed.length || removed.length) {
      await push({ changed, removed, accounts: current });
    }
    previous = current;
  }
  return {
    poll,
    start() {
      const timer = setInterval(() => {
        poll().catch((error) => console.warn("Native health monitor failed:", error.message));
      }, POLL_INTERVAL_MS);
      timer.unref?.();
      return timer;
    }
  };
}

// 默认装配：账号来自 Go，推送给每个变更账号所属用户的房间。
function startDefaultMonitor(io) {
  const monitor = createNativeHealthMonitor({
    async getAccounts() {
      const list = await downloaderSidecar.nativeAccounts();
      return Array.isArray(list) ? list : [];
    },
    async push({ changed, removed }) {
      changed.forEach((account) => {
        io.to(`user:${account.userId}`).emit("native:account-changed", account);
      });
      if (removed.length) {
        // 账号被删除时不知道原属用户（当前列表里已没有），广播到所有已认证房间，
        // 前端按 key 自行判断是否关心。
        io.emit("native:account-removed", { keys: removed });
      }
    }
  });
  return monitor;
}

module.exports = {
  POLL_INTERVAL_MS,
  accountKey,
  fingerprint,
  diffNativeAccounts,
  createNativeHealthMonitor,
  startDefaultMonitor
};
