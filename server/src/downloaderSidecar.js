const DEFAULT_URL = "http://127.0.0.1:3090";

function baseUrl() {
  return String(process.env.FEIGRAM_DOWNLOADER_URL || DEFAULT_URL).replace(/\/+$/, "");
}

async function request(path, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), options.timeoutMs || 5000);
  try {
    const response = await fetch(`${baseUrl()}${path}`, {
      ...options,
      signal: controller.signal,
      headers: {
        "Content-Type": "application/json",
        ...(options.headers || {})
      }
    });
    const text = await response.text();
    const data = text ? JSON.parse(text) : {};
    if (!response.ok) {
      throw new Error(data.error || `Go downloader ${response.status}`);
    }
    return data;
  } finally {
    clearTimeout(timer);
  }
}

async function safe(call) {
  try {
    return await call();
  } catch (error) {
    return {
      ok: false,
      url: baseUrl(),
      error: error.name === "AbortError" ? "Go 下载服务无响应" : error.message
    };
  }
}

function health() {
  return safe(async () => ({ ...(await request("/health")), url: baseUrl() }));
}

function state() {
  return safe(async () => ({ ...(await request("/api/state")), url: baseUrl() }));
}

function updateConfig(patch) {
  return safe(async () => request("/api/config", {
    method: "PUT",
    body: JSON.stringify(patch || {})
  }));
}

function nativeAccounts() {
  return safe(async () => request("/api/native/accounts"));
}

function upsertNativeAccount(account) {
  return request("/api/native/accounts", {
    method: "POST",
    body: JSON.stringify(account || {})
  });
}

function checkNativeAccount(userId, accountId) {
  return safe(async () => request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/health`, {
    method: "POST"
  }));
}

function startNativeLogin(userId, accountId, payload) {
  return request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/login/start`, {
    method: "POST",
    timeoutMs: 45000,
    body: JSON.stringify(payload || {})
  });
}

function startNativeQRLogin(userId, accountId, payload) {
  return request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/login/qr-start`, {
    method: "POST",
    timeoutMs: 45000,
    body: JSON.stringify(payload || {})
  });
}

function pollNativeQRLogin(userId, accountId, payload) {
  return request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/login/qr-status`, {
    method: "POST",
    timeoutMs: 30000,
    body: JSON.stringify(payload || {})
  });
}

function submitNativeLoginCode(userId, accountId, payload) {
  return request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/login/code`, {
    method: "POST",
    timeoutMs: 65000,
    body: JSON.stringify(payload || {})
  });
}

function submitNativeLoginPassword(userId, accountId, payload) {
  return request(`/api/native/accounts/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/login/password`, {
    method: "POST",
    timeoutMs: 65000,
    body: JSON.stringify(payload || {})
  });
}

function enqueueTask(task) {
  return request("/api/tasks", {
    method: "POST",
    body: JSON.stringify(task || {})
  });
}

function listTasks() {
  return request("/api/tasks");
}

function getTask(id) {
  return request(`/api/tasks/${encodeURIComponent(id)}`);
}

function queueTask(id) {
  return request(`/api/tasks/${encodeURIComponent(id)}/queue`, {
    method: "POST"
  });
}

function cancelTask(id) {
  return request(`/api/tasks/${encodeURIComponent(id)}/cancel`, {
    method: "POST"
  });
}

function deleteTask(id) {
  return request(`/api/tasks/${encodeURIComponent(id)}`, {
    method: "DELETE"
  });
}

// --- M2.2: Go Telegram Core Auth API 客户端（/api/auth/*，M2.1 暴露） ---
// 登录鉴权统一收敛到 Go，Node 不再创建 GramJS 登录客户端。

function authStart(payload) {
  return request("/api/auth/start", {
    method: "POST",
    timeoutMs: 45000,
    body: JSON.stringify(payload || {})
  });
}

function authSubmitCode(payload) {
  return request("/api/auth/code", {
    method: "POST",
    timeoutMs: 65000,
    body: JSON.stringify(payload || {})
  });
}

function authSubmitPassword(payload) {
  return request("/api/auth/password", {
    method: "POST",
    timeoutMs: 65000,
    body: JSON.stringify(payload || {})
  });
}

function authQRStart(payload) {
  return request("/api/auth/qr/start", {
    method: "POST",
    timeoutMs: 45000,
    body: JSON.stringify(payload || {})
  });
}

function authQRStatus(payload) {
  return request("/api/auth/qr/status", {
    method: "POST",
    timeoutMs: 30000,
    body: JSON.stringify(payload || {})
  });
}

module.exports = {
  baseUrl,
  cancelTask,
  deleteTask,
  enqueueTask,
  getTask,
  health,
  listTasks,
  nativeAccounts,
  queueTask,
  state,
  updateConfig,
  upsertNativeAccount,
  checkNativeAccount,
  startNativeLogin,
  startNativeQRLogin,
  pollNativeQRLogin,
  submitNativeLoginCode,
  submitNativeLoginPassword,
  authStart,
  authSubmitCode,
  authSubmitPassword,
  authQRStart,
  authQRStatus
};
