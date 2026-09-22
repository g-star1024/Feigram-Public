"use strict";

// R4.2 · 传输层自动升级
//
// Go 的媒体源传输层是全局配置（非按账号）。历史数据目录可能停在 http-bridge，
// 老用户升级后若不知道要手动切换，就会一直静默走桥接、拿不到原生下载速度。
//
// 规则：当当前传输层为 http-bridge 且至少一个账号已 healthy/ready 时，
// 自动升级到 native-mtproto。为避免「用户手动退回桥接又被切回」，
// 每个进程只自动升级一次（内存标记，重启后可再次触发一次——属可接受）。
//
// 判定逻辑（shouldAutoUpgrade）为纯函数，便于单测。

// state: Go /api/state 返回（含 transport 与 config.transport）；
// accounts: Go 账号列表（publicNativeAccount 形态）。
function shouldAutoUpgrade(state, accounts, alreadyAutoUpgraded) {
  if (alreadyAutoUpgraded) return false;
  const transport = (state && (state.config?.transport || state.transport)) || "";
  if (transport !== "http-bridge") return false;
  return (accounts || []).some((account) => account.ready || account.status === "healthy");
}

function createTransportAutoUpgrade({ getState, getAccounts, applyTransport }) {
  let done = false;
  async function check() {
    if (done) return false;
    const [state, accounts] = await Promise.all([getState(), getAccounts()]);
    if (!shouldAutoUpgrade(state, accounts, done)) return false;
    await applyTransport("native-mtproto");
    done = true;
    return true;
  }
  return { check, reset: () => { done = false; } };
}

// 默认装配：状态/账号来自 Go，切换走 Go config，并经 socket 通知前端。
function startDefaultAutoUpgrade({ io, state, nativeAccounts, updateConfig }) {
  const upgrade = createTransportAutoUpgrade({
    getState: state,
    getAccounts: nativeAccounts,
    async applyTransport(transport) {
      await updateConfig({ transport });
      io?.emit("native:transport-changed", { transport });
    }
  });
  return upgrade;
}

module.exports = {
  shouldAutoUpgrade,
  createTransportAutoUpgrade,
  startDefaultAutoUpgrade
};
