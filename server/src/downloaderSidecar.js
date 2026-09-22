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

// --- M2.4: GramJS → gotd session 一键迁移（路径 A） ---
// 迁移失败（含 Go 侧健康检查不通过）不抛异常，而是返回结构化结果，
// 由调用方降级到「路径 B：重新登录」，保证前端总能拿到可展示的错误。
async function migrateAccount(payload) {
  try {
    const data = await request("/api/accounts/migrate", {
      method: "POST",
      timeoutMs: 65000,
      body: JSON.stringify(payload || {})
    });
    return { ok: true, ...data };
  } catch (error) {
    return {
      ok: false,
      migrated: false,
      error: error.name === "AbortError" ? "迁移超时，请改用重新登录" : (error.message || "迁移失败")
    };
  }
}

// --- M4.2: Go Telegram Core 聊天 API（/api/accounts/{accountID}/*） ---
// 已迁移到 Go 的账号（authMode=native）不再有 GramJS 客户端，
// 会话/文件夹/消息/头像/peer 解析统一由 Go 原生 MTProto 提供。

function accountChatPath(accountId, action, params = {}) {
  const search = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value === undefined || value === null || value === "") return;
    search.set(key, String(value));
  });
  const query = search.toString();
  return `/api/accounts/${encodeURIComponent(accountId)}/${action}${query ? `?${query}` : ""}`;
}

function accountDialogs({ userId, accountId, limit = 0, query = "" }) {
  return request(accountChatPath(accountId, "dialogs", { userId, limit, query }), { timeoutMs: 45000 });
}

function accountFolders({ userId, accountId }) {
  return request(accountChatPath(accountId, "folders", { userId }), { timeoutMs: 45000 });
}

function accountMessages({ userId, accountId, peer, limit = 0, before = 0, around = 0 }) {
  return request(accountChatPath(accountId, "messages", { userId, peer, limit, before, around }), { timeoutMs: 45000 });
}

function accountMedia({ userId, accountId, peer, limit = 0, before = 0 }) {
  return request(accountChatPath(accountId, "media", { userId, peer, limit, before }), { timeoutMs: 45000 });
}

function accountPeer({ userId, accountId, peer }) {
  return request(accountChatPath(accountId, "peer", { userId, peer }), { timeoutMs: 30000 });
}

// --- M4.3: Go Telegram Core 写路径与会话详情 ---
// sendText / clickMessageButton / chatDetails 三个函数在 native 账号下没有 GramJS 客户端可用，
// 统一改由 Go 原生 MTProto 提供（对应 downloader/cmd/feigram-downloader/chatwrite.go）。

function accountSend({ userId, accountId, peer, text }) {
  return request(accountChatPath(accountId, "send", { userId, peer }), {
    method: "POST",
    body: JSON.stringify({ text: String(text || "") }),
    timeoutMs: 45000
  });
}

function accountButton({ userId, accountId, peer, message, data }) {
  return request(accountChatPath(accountId, "button", { userId, peer, message }), {
    method: "POST",
    body: JSON.stringify({ data: String(data || "") }),
    timeoutMs: 45000
  });
}

function accountDetails({ userId, accountId, peer, limit = 0, before = 0 }) {
  return request(accountChatPath(accountId, "details", { userId, peer, limit, before }), { timeoutMs: 45000 });
}

// 头像是二进制，不能用 JSON request()，单独走 arrayBuffer。
async function accountAvatar({ userId, accountId, peer }) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
    const response = await fetch(`${baseUrl()}${accountChatPath(accountId, "avatar", { userId, peer })}`, {
      signal: controller.signal
    });
    if (!response.ok) {
      throw new Error(`Go downloader ${response.status}`);
    }
    const buffer = Buffer.from(await response.arrayBuffer());
    if (!buffer.length) throw new Error("暂无头像");
    return {
      buffer,
      contentType: response.headers.get("content-type") || "image/jpeg"
    };
  } finally {
    clearTimeout(timer);
  }
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
  authQRStatus,
  migrateAccount,
  accountDialogs,
  accountFolders,
  accountMessages,
  accountMedia,
  accountPeer,
  accountAvatar,
  accountSend,
  accountButton,
  accountDetails
};
