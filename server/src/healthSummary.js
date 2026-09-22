// M5.3：健康账户汇总从 index.js 抽出为纯函数，便于单元测试（index.js 一加载即监听端口，不能直接被测试引入）。
"use strict";

// 优先采用 Go 侧 /api/state 已算好的 accountsSummary；不可达时按 /api/native/accounts 本地统计。
// 入参容错三种形态：数组、{ accounts: [...] }、{ ok: false, error }（sidecar 不可达）。
function summarizeNativeAccounts(sidecarAccounts) {
  const list = Array.isArray(sidecarAccounts)
    ? sidecarAccounts
    : Array.isArray(sidecarAccounts && sidecarAccounts.accounts) ? sidecarAccounts.accounts : [];
  const byStatus = {};
  let healthy = 0;
  for (const account of list) {
    const status = String((account && account.status) || ((account && account.ready) ? "healthy" : "needs-login"));
    byStatus[status] = (byStatus[status] || 0) + 1;
    if (status === "healthy") healthy += 1;
  }
  const summary = { total: list.length, healthy, byStatus };
  if (sidecarAccounts && sidecarAccounts.ok === false) {
    summary.error = sidecarAccounts.error || "Go 下载服务不可达";
  }
  return summary;
}

// 健康接口优先采用 Go 的账户汇总（含 healthy 判定），只有在 Go 未给出时才回退本地统计。
function pickAccountsSummary(sidecarState, sidecarAccounts) {
  if (sidecarState && sidecarState.ok && sidecarState.accounts) return sidecarState.accounts;
  return summarizeNativeAccounts(sidecarAccounts);
}

// 任务数：Go /api/state 只给 counts/tasks，这里统一折成一个标量，便于前端展示。
function sidecarTaskCount(sidecarState) {
  if (!sidecarState || !sidecarState.ok) return undefined;
  if (typeof sidecarState.taskCount === "number") return sidecarState.taskCount;
  if (Array.isArray(sidecarState.tasks)) return sidecarState.tasks.length;
  if (sidecarState.counts && typeof sidecarState.counts === "object") {
    return Object.values(sidecarState.counts).reduce((sum, value) => sum + Number(value || 0), 0);
  }
  return undefined;
}

module.exports = {
  summarizeNativeAccounts,
  pickAccountsSummary,
  sidecarTaskCount
};
