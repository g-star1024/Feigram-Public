const path = require("path");
const crypto = require("crypto");
const fs = require("fs-extra");
const mime = require("mime-types");
const bigInt = require("big-integer");
const { dataDir, downloadTasksPath, readAccounts, removeAccount, safeId, silentCachePath, upsertAccount } = require("./store");
const { readSettings } = require("./settings");
const { decryptText, encryptText } = require("./cryptoBox");
const downloaderSidecar = require("./downloaderSidecar");
const { avatarFallbackBuffer } = require("./avatarFallback");
const { goMessageToNodeMessage, findNativeMessage } = require("./nativeMediaAdapter");

const clients = new Map();
const cacheClients = new Map();
const clientConnectLocks = new Map();
const peerCache = new Map();
const pendingLogins = new Map();
const downloadTasks = new Map();
const silentCacheRecords = new Map();
const silentCacheTasks = new Map();
const silentCacheQueue = [];
const foregroundAccountOps = new Map();
let silentCacheActive = 0;
let downloadPersistTimer = null;
let silentPersistTimer = null;
let realtimeIo = null;
let silentCacheEnabled = true;
let silentCacheRateLimitBps = 0;
let silentCacheConcurrency = 1;
let silentCacheMode = "conservative";
let silentRateWindowStart = Date.now();
let silentRateWindowBytes = 0;
let silentRateChain = Promise.resolve();
const VIDEO_CACHE_THRESHOLD = 100 * 1024 * 1024;
const MAX_SILENT_CACHE_CONCURRENCY = 10;
const TELEGRAM_MIN_CHUNK_SIZE = 4096;
const MAX_TELEGRAM_CHUNK_SIZE = 512 * 1024;
const DOWNLOAD_RETRY_LIMIT = 3;
const SILENT_RETRY_DELAY_MS = 60 * 1000;
const STALE_TASK_MS = 90 * 1000;
const STALE_REQUEUE_DELAY_MS = 60 * 1000;
const SLOW_TASK_MS = 3 * 60 * 1000;
const MIN_HEALTHY_SPEED_BPS = 128 * 1024;
const FAST_ZERO_SPEED_DEGRADE_MS = 45 * 1000;
const IDLE_SPEED_RESET_MS = 20 * 1000;
// 会话列表总量上限。R4.36：Go 侧此前只发一次 messages.getDialogs，而 MTProto
// 单页硬上限是 100——用户会话数超过 100 时列表就显示不全（第 100 条之后的群组
// 永不出现）。Go 侧已改为真分页累加，此上限现在才真正生效。
// R4.52：500 → 2000，与 Go 侧 maxChatDialogLimit 对齐——用户实测 545 条会话
// 只显示 500 条，分页深翻能力（30 轮×100）本就足够，此前是总量上限卡住了。
const DIALOG_FETCH_LIMIT = 2000;

function stableId(prefix, ...parts) {
  return `${prefix}_${crypto.createHash("sha1").update(parts.map((part) => String(part ?? "")).join("|")).digest("hex").slice(0, 24)}`;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}







function normalizedSilentCacheMode(value) {
  return value === "fast" ? "fast" : "conservative";
}








function hasRunningSilentCacheForAccount(accountId) {
  for (const task of silentCacheRecords.values()) {
    if (task.accountId === accountId && task.status === "running" && silentCacheTasks.has(task.id)) return true;
  }
  return false;
}


function resetSilentRateWindow() {
  silentRateWindowStart = Date.now();
  silentRateWindowBytes = 0;
  silentRateChain = Promise.resolve();
}














async function cacheSettings() {
  const settings = await readSettings();
  const base = settings.cacheBaseDir || process.env.DOWNLOAD_DIR || path.join(process.env.DATA_DIR || "/data", "downloads");
  return {
    base,
    image: settings.imageCacheDir || path.join(base, "images"),
    video: settings.videoCacheDir || path.join(base, "videos"),
    file: settings.fileCacheDir || path.join(base, "files"),
    avatars: path.join(base, "avatars"),
    thumbs: path.join(base, "thumbs"),
    retentionDays: Math.max(1, Number(settings.cacheRetentionDays || 30))
  };
}

function safeName(value) {
  return String(value || "telegram-file").replace(/[\\/:*?"<>|]/g, "_").slice(0, 180);
}




async function telegramConfig() {
  const settings = await readSettings();
  const apiId = Number(settings.telegramApiId || 0);
  const apiHash = settings.telegramApiHash || "";
  if (!apiId || !apiHash || apiHash.includes("put-your")) {
    const err = new Error("Telegram API 凭据未配置：必须通过 fnOS 安装向导或管理员后台填写你自有的 api_id / api_hash（my.telegram.org 申请）。本应用禁止使用内置默认凭据，缺失凭据时拒绝启动登录。");
    err.status = 500;
    throw err;
  }
  return { apiId, apiHash };
}

function toText(value) {
  if (value === undefined || value === null) return "";
  return typeof value === "bigint" ? value.toString() : String(value);
}

function peerKey(entity) {
  const klass = entity.className || entity.constructor?.name || "Peer";
  return `${klass}:${toText(entity.id)}`;
}


function serializeEntity(entity) {
  const title = entity.title || [entity.firstName, entity.lastName].filter(Boolean).join(" ") || entity.username || "Unknown";
  return {
    id: peerKey(entity),
    rawId: toText(entity.id),
    title,
    username: entity.username || "",
    type: entity.broadcast ? "channel" : entity.megagroup || entity.gigagroup ? "group" : entity.className === "User" ? "private" : "chat"
  };
}









function mediaKind(message, mimeType = "") {
  if (message.photo) return "image";
  if (mimeType.startsWith("image/")) return "image";
  if (mimeType.startsWith("video/") || message.video) return "video";
  return "file";
}


function linkDomain(url) {
  const value = String(url || "").trim();
  if (!value) return "";
  if (value.startsWith("@")) return value.slice(1);
  if (value.startsWith("tg://resolve")) return new URL(value).searchParams.get("domain") || "";
  const normalized = value.startsWith("t.me/") ? `https://${value}` : value;
  try {
    const parsed = new URL(normalized);
    if (!["t.me", "telegram.me", "www.t.me", "www.telegram.me"].includes(parsed.hostname)) return "";
    const [domain] = parsed.pathname.split("/").filter(Boolean);
    if (!domain || domain === "c" || domain === "joinchat" || domain.startsWith("+")) return "";
    return domain;
  } catch {
    return "";
  }
}

function linkMessageId(url) {
  try {
    const value = String(url || "").trim();
    const normalized = value.startsWith("t.me/") ? `https://${value}` : value;
    const parsed = new URL(normalized);
    const parts = parsed.pathname.split("/").filter(Boolean);
    const numeric = [...parts].reverse().find((part) => /^\d+$/.test(part));
    return numeric ? Number(numeric) : 0;
  } catch {
    return 0;
  }
}

function linkPrivatePeerId(url) {
  try {
    const value = String(url || "").trim();
    const normalized = value.startsWith("t.me/") ? `https://${value}` : value;
    const parsed = new URL(normalized);
    if (!["t.me", "telegram.me", "www.t.me", "www.telegram.me"].includes(parsed.hostname)) return "";
    const parts = parsed.pathname.split("/").filter(Boolean);
    if (parts[0] !== "c" || !/^\d+$/.test(parts[1] || "")) return "";
    return `Channel:${parts[1]}`;
  } catch {
    return "";
  }
}




async function listAccounts(userId) {
  const accounts = await readAccounts();
  const owned = accounts.filter((account) => account.userId === userId);
  const synced = await Promise.all(owned.map((account) => syncGoNativeAccount(account).catch(() => null)));
  // M2.4：向前端暴露迁移状态，供账号卡片判断是否展示「迁移到 Go」按钮。
  return owned.map(({ session, ...safe }, index) => {
    const go = (synced[index] && (synced[index].account || synced[index])) || null;
    const authMode = safe.authMode || (session ? "gramjs" : "unknown");
    return {
      ...safe,
      authMode,
      needsMigration: authMode !== "native",
      goReady: Boolean(go && go.ready),
      goStatus: (go && go.status) || "",
      connected: clients.has(safe.id) || Boolean(go && go.ready)
    };
  });
}

async function syncGoNativeAccount(account) {
  if (!account?.userId || !account?.id) return null;
  const { apiId, apiHash } = await telegramConfig();
  // M2.4：已迁移到 Go 的账号不能被降级为 needs-relogin，
  // 否则每次刷新账号列表都会把健康的 Go session 打回「需重新登录」。
  return downloaderSidecar.upsertNativeAccount({
    userId: account.userId,
    accountId: account.id,
    phone: account.phone || "",
    displayName: account.label || account.username || account.phone || account.id,
    apiId,
    apiHash,
    status: account.authMode === "native" ? "" : "needs-relogin"
  });
}




async function resetCacheClient(accountId) {
  const client = cacheClients.get(accountId);
  if (client) {
    try {
      await client.disconnect();
    } catch {}
  }
  cacheClients.delete(accountId);
}

async function resetTelegramClient(accountId) {
  await resetCacheClient(accountId);
  const client = clients.get(accountId);
  if (client) {
    try {
      await client.disconnect();
    } catch {}
  }
  clients.delete(accountId);
}

async function startLogin(userId, { label, phoneNumber }) {
  const { apiId, apiHash } = await telegramConfig();
  const accountId = safeId("account");
  const result = await downloaderSidecar.authStart({
    userID: userId,
    accountID: accountId,
    phone: phoneNumber,
    apiId,
    apiHash
  });
  const loginId = result.loginId;
  pendingLogins.set(loginId, {
    loginId,
    accountId,
    userId,
    label: label || phoneNumber,
    phoneNumber
  });
  return { loginId, isCodeViaApp: false, passwordRequired: Boolean(result.passwordRequired) };
}

// M2.2：验证码提交改由 Go Telegram Core 处理（/api/auth/code）。
// 注意：pendingLogins 中已不再保存 GramJS client，这里不能再引用 pending.client。
async function completeCode({ loginId, code }, io) {
  const pending = pendingLogins.get(loginId);
  if (!pending) throw Object.assign(new Error("登录流程已过期，请重新发送验证码"), { status: 400 });
  const result = await downloaderSidecar.authSubmitCode({ loginId, code });
  if (result.passwordRequired) return { passwordRequired: true };
  if (!result.done) throw Object.assign(new Error("验证码提交后登录未完成，请重试"), { status: 400 });
  return saveLoggedInClient(loginId, io, result.account);
}

// M2.2：两步验证密码提交同样经 Go（/api/auth/password）。
async function completePassword({ loginId, password }, io) {
  const pending = pendingLogins.get(loginId);
  if (!pending) throw Object.assign(new Error("登录流程已过期，请重新开始"), { status: 400 });
  const result = await downloaderSidecar.authSubmitPassword({ loginId, password });
  if (result.done) return saveLoggedInClient(loginId, io, result.account);
  throw Object.assign(new Error("密码提交后登录未完成"), { status: 400 });
}

// M2.2/M4.0：登录成功后 Node 只保存账号展示信息与「已就绪」状态，
// 不再保存或持有 GramJS session —— session 由 Go 侧 gotd 独占，
// 以避免双端同时握有同一 auth_key 触发 AUTH_KEY_DUPLICATED。
async function saveLoggedInClient(loginId, io, goAccount) {
  const pending = pendingLogins.get(loginId);
  if (!pending) throw Object.assign(new Error("登录流程已过期，请重新开始"), { status: 400 });
  const account = {
    id: pending.accountId,
    userId: pending.userId,
    label: pending.label,
    phoneNumber: pending.phoneNumber || (goAccount && goAccount.phone) || "",
    displayName: (goAccount && goAccount.displayName) || pending.label,
    username: "",
    rawUserId: "",
    authMode: "native",
    migratedAt: new Date().toISOString(),
    createdAt: new Date().toISOString()
  };
  await upsertAccount(account);
  pendingLogins.delete(loginId);
  return { account: { ...account, connected: Boolean(goAccount && goAccount.ready) } };
}

async function logout(userId, accountId) {
  const client = clients.get(accountId);
  if (client) {
    try {
      await client.disconnect();
    } catch {}
  }
  clients.delete(accountId);
  await resetCacheClient(accountId);
  peerCache.delete(accountId);
  const account = (await readAccounts()).find((item) => item.id === accountId);
  if (!account || account.userId !== userId) throw Object.assign(new Error("账号不存在"), { status: 404 });
  await removeAccount(accountId);
  // 登出即清理 Go 侧记录：否则 native-sessions 里留下孤儿记录，
  // 诊断页会一直显示一条「failed」的同号账号（2026-09-22 用户截图实证）。
  await downloaderSidecar.deleteNativeAccount(userId, accountId).catch(() => null);
}

// --- M4.2：已迁移到 Go 的账号（authMode=native）没有 GramJS 客户端，
// 聊天/文件夹/消息/媒体/头像/peer 解析统一经 Go Telegram Core（downloaderSidecar）---

async function nativeAccountRecord(userId, accountId) {
  const accounts = await readAccounts();
  const account = accounts.find((item) => item.id === accountId && item.userId === userId);
  if (!account) return null;
  // 兼容旧记录：有 GramJS session 且未标记 native 的账号继续走 GramJS。
  return account.authMode === "native" ? account : null;
}

function syntheticNativeEntity(chat) {
  const [type, id] = String(chat?.id || chat?.peerId || "").split(":");
  if (!type || !id) return null;
  const className = type === "Channel" ? "Channel" : type === "Chat" ? "Chat" : "User";
  return {
    className,
    id,
    rawId: id,
    accessHash: chat.accessHash ? String(chat.accessHash) : "",
    title: chat.title || "",
    username: chat.username || "",
    broadcast: chat.type === "channel" || className === "Channel"
  };
}

function rememberNativePeers(accountId, chats) {
  if (!Array.isArray(chats) || !chats.length) return;
  if (!peerCache.has(accountId)) peerCache.set(accountId, new Map());
  const cache = peerCache.get(accountId);
  chats.forEach((chat) => {
    const entity = syntheticNativeEntity(chat);
    if (!entity) return;
    // R4.27：消息发送者缓存（listNativeMessages 传入的 {id,title,username}）不带
    // accessHash。若用这种贫信息覆盖「会话列表」带 accessHash 的完整缓存，
    // Go 侧按 channel accessHash 刷新 file_reference 时就会拿到空串并失败
    //（2.6.4 实测「invalid native channel access hash: parsing \"\"」的源头之一）。
    // 已有完整条目时，贫信息只做合并、不降级覆盖。
    const existing = cache.get(chat.id);
    if (existing && existing.accessHash && !entity.accessHash) {
      cache.set(chat.id, { ...existing, title: entity.title || existing.title, username: entity.username || existing.username });
      return;
    }
    cache.set(chat.id, entity);
  });
}

async function listNativeChats(userId, accountId, query = "", includeArchived = false) {
  let items;
  try {
    items = await downloaderSidecar.accountDialogs({
      userId,
      accountId,
      limit: DIALOG_FETCH_LIMIT,
      query,
      includeArchived
    });
  } catch (error) {
    if (error && error.name === "AbortError") {
      // R4.0a：裸 AbortError（"The operation was aborted"）对用户无意义，
      // 换成可读文案并给出可操作的排查方向。
      throw new Error("会话列表加载超时（超过 65 秒）。网络可能不通，请在「设置 → 网络代理」核对地址与端口，或稍后重试");
    }
    throw error;
  }
  const chats = Array.isArray(items) ? items : [];
  rememberNativePeers(accountId, chats);
  return chats;
}

async function listNativeFolders(userId, accountId) {
  let items;
  try {
    items = await downloaderSidecar.accountFolders({ userId, accountId });
  } catch (error) {
    if (error && error.name === "AbortError") {
      // R4.34：启动窗口 Go 侧忙（首个任务 + 健康检查 + 媒体连接竞争）时
      // 45s 超时 abort，裸 DOMException 对用户无意义——转可读文案。
      // 2.6.11 实测：启动即打出整段 "DOMException [AbortError]" 噪音。
      throw new Error("文件夹列表加载超时（超过 45 秒）。Go 下载服务可能正在启动或繁忙，请稍后重试");
    }
    throw error;
  }
  return Array.isArray(items) ? items : [];
}

async function listNativeMessages(userId, accountId, peerId, limit = 50, before = 0, around = 0) {
  const items = await downloaderSidecar.accountMessages({
    userId,
    accountId,
    peer: peerId,
    limit,
    before,
    around
  });
  const messages = Array.isArray(items) ? items : [];
  messages.forEach((message) => {
    const sender = message.sender || {};
    if (sender.id) rememberNativePeers(accountId, [{ id: sender.id, title: sender.title, username: sender.username }]);
  });
  return messages;
}

async function resolveNativePeerEntity(userId, accountId, peerId) {
  const cached = peerCache.get(accountId)?.get(peerId);
  if (cached) return cached;
  const info = await downloaderSidecar.accountPeer({ userId, accountId, peer: peerId });
  const entity = syntheticNativeEntity({ ...info, peerId });
  if (!entity) throw Object.assign(new Error("找不到会话，可能已退出该群组、会话已被删除，或 Telegram 暂时无法解析该会话"), { status: 404 });
  if (!peerCache.has(accountId)) peerCache.set(accountId, new Map());
  peerCache.get(accountId).set(peerId, entity);
  return entity;
}

async function listChats(userId, accountId, query = "", includeArchived = false) {
  // M4.2：native 账号经 Go 原生 MTProto 读取会话列表，返回结构与 GramJS 路径一致。
  if (await nativeAccountRecord(userId, accountId)) {
    return listNativeChats(userId, accountId, query, includeArchived);
  }
  throw reloginError(accountId);
}

async function listFolders(userId, accountId) {
  // M4.2：native 账号经 Go 读取 Telegram 文件夹（dialog filters）。
  if (await nativeAccountRecord(userId, accountId)) {
    return listNativeFolders(userId, accountId);
  }
  throw reloginError(accountId);
}

async function resolvePeer(userId, accountId, peerId) {
  // M4.2：native 账号没有 GramJS 实体，改由 Go 提供 peer 元数据（含 accessHash）。
  if (await nativeAccountRecord(userId, accountId)) {
    return resolveNativePeerEntity(userId, accountId, peerId);
  }
  throw reloginError(accountId);
}

async function listMessages(userId, accountId, peerId, limit = 50, before = 0, around = 0) {
  // M4.2：native 账号经 Go 读取消息历史，返回结构与 GramJS 序列化一致。
  if (await nativeAccountRecord(userId, accountId)) {
    return listNativeMessages(userId, accountId, peerId, limit, before, around);
  }
  throw reloginError(accountId);
}

async function chatDetails(userId, accountId, peerId) {
  // M4.3：native 账号没有 GramJS 客户端（getClient 抛 409），会话详情改由 Go 原生 MTProto 提供，
  // 返回字段与 GramJS 分支完全一致（id/rawId/title/username/type/about/participantsCount/
  // mediaSummary/files/nextMediaBefore/hasMoreMedia），保证 UI 行为不变（铁律 3）。
  if (await nativeAccountRecord(userId, accountId)) {
    return downloaderSidecar.accountDetails({ userId, accountId, peer: peerId, limit: 30 });
  }
  throw reloginError(accountId);
}

async function chatMedia(userId, accountId, peerId, { before = 0, limit = 30 } = {}) {
  // M4.2：native 账号的媒体列表改由 Go 提供（含下载所需的 nativeFile 元数据）。
  if (await nativeAccountRecord(userId, accountId)) {
    const pageSize = Math.max(1, Math.min(60, Number(limit) || 30));
    const files = await downloaderSidecar.accountMedia({ userId, accountId, peer: peerId, limit: pageSize, before });
    const list = Array.isArray(files) ? files : [];
    return {
      files: list,
      nextBefore: list.length ? Math.min(...list.map((item) => Number(item.id))) : 0,
      hasMore: list.length >= pageSize
    };
  }
  throw reloginError(accountId);
}

async function sendText(userId, accountId, peerId, text) {
  // M4.3：native 账号走 Go 原生 MTProto 写路径；GramJS 分支保持原样（铁律 3：不改行为）。
  if (await nativeAccountRecord(userId, accountId)) {
    return downloaderSidecar.accountSend({ userId, accountId, peer: peerId, text });
  }
  throw reloginError(accountId);
}

async function clickMessageButton(userId, accountId, peerId, messageId, data) {
  if (!data) throw Object.assign(new Error("这个按钮暂不支持点击"), { status: 400 });
  // M4.3：native 账号的按钮回调由 Go 的 messages.getBotCallbackAnswer 提供。
  if (await nativeAccountRecord(userId, accountId)) {
    return downloaderSidecar.accountButton({ userId, accountId, peer: peerId, message: messageId, data });
  }
  throw reloginError(accountId);
}

async function resolveTelegramLink(userId, accountId, url) {
  const domain = linkDomain(url);
  const privatePeerId = linkPrivatePeerId(url);
  const messageId = linkMessageId(url);
  const ensurePeerCache = async () => {
    if (!peerCache.has(accountId)) await listChats(userId, accountId);
  };
  // M4.4：native 账号改走 Go 原生 MTProto（contacts.resolveUsername）；
  // 私有频道链接（t.me/c/<id>）先在已缓存会话里命中，命中不了再交给 Go。
  if (await nativeAccountRecord(userId, accountId)) {
    await ensurePeerCache();
    if (privatePeerId) {
      const cached = peerCache.get(accountId)?.get(privatePeerId) || null;
      if (cached) {
        const chat = serializeEntity(cached);
        return { ...chat, avatarKey: chat.id, messageId };
      }
    }
    return downloaderSidecar.accountResolve({ userId, accountId, link: url, messageId });
  }
  throw reloginError(accountId);
}

async function search(userId, accountId, query) {
  // M4.4：native 账号的会话检索已由 Go 提供，全局消息搜索改走 Go 的 messages.search。
  if (await nativeAccountRecord(userId, accountId)) {
    const dialogs = await listChats(userId, accountId, query);
    const messages = await downloaderSidecar
      .accountSearch({ userId, accountId, query })
      .catch(() => []);
    return { chats: dialogs, messages };
  }
  throw reloginError(accountId);
}

async function mediaFileInfo(userId, accountId, message, contentType, kind) {
  const directories = await cacheSettings();
  const extension = mime.extension(contentType);
  const guessedName = safeName(message.file?.name || `telegram-${accountId}-${message.id}${extension ? `.${extension}` : ""}`);
  const downloadDir = path.join(kind === "image" ? directories.image : kind === "video" ? directories.video : directories.file, userId);
  return {
    fileName: guessedName,
    filePath: path.join(downloadDir, guessedName),
    downloadDir
  };
}

function downloadTaskId(userId, accountId, peerId, messageId) {
  return stableId("download", userId, accountId, peerId, messageId);
}


function silentDedupKey(userId, accountId, peerId, fileName, size) {
  return [userId, accountId, peerId, String(fileName || "").trim().toLowerCase(), Number(size || 0)].join("|");
}












// M4.1 子步：原生账号（authMode=native）不再持有 GramJS 客户端——getClient 对 native 直接抛 409，
// 因此原生下载元数据不能走 mediaMessage（GramJS），必须改经 Go 原生 MTProto（/messages + /peer）获取。
// 这里复用 M4.2 的 resolveNativePeerEntity（内部用 syntheticNativeEntity 合成 entity），
// 再用 nativeMediaAdapter 把 Go 序列化消息映射为与 mediaMessage 兼容的 { entity, message } 形状，
// 供 ensureGoDownloadTask / mediaNativeMetadata / runAutoCacheScan 直接消费。
async function nativeMediaMeta(userId, accountId, peerId, messageId) {
  const entity = await resolveNativePeerEntity(userId, accountId, peerId);
  const items = await downloaderSidecar.accountMessages({
    userId, accountId, peer: peerId, limit: 20, around: Number(messageId)
  });
  const item = findNativeMessage(items, messageId);
  if (!item || !item.media || !item.media.hasPreview) {
    throw Object.assign(new Error("这条消息没有可下载媒体"), { status: 404 });
  }
  const message = goMessageToNodeMessage(item);
  return { client: null, entity, message };
}






















async function mediaThumbnail(userId, accountId, peerId, messageId) {
  // B：原生账号媒体由 Go blob 端点提供；遗留 GramJS 账号需重新登录转原生。
  if (await nativeAccountRecord(userId, accountId)) {
    return { blobUrl: goBlobSourceUrl(userId, accountId, peerId, messageId), contentType: "image/jpeg", fileName: `${accountId}-${peerId}-${messageId}.jpg` };
  }
  throw reloginError(accountId);
}



async function downloadMedia(userId, accountId, peerId, messageId, options = {}) {
  // B：原生账号媒体由 Go blob 端点提供（返回 blobUrl 由路由代理）；遗留 GramJS 账号需重新登录。
  if (await nativeAccountRecord(userId, accountId)) {
    const meta = await mediaNativeMetadata(userId, accountId, peerId, messageId).catch(() => null);
    const message = meta?.message;
    const isPhoto = Boolean(message?.photo);
    const contentType = message?.document?.mimeType || (isPhoto ? "image/jpeg" : "application/octet-stream");
    const fileName = message?.file?.name || `telegram-${accountId}-${messageId}`;
    const size = Number(message?.file?.size || message?.document?.size || 0);
    return {
      blobUrl: goBlobSourceUrl(userId, accountId, peerId, messageId),
      fileName,
      contentType,
      size,
      kind: isPhoto ? "image" : (String(contentType).startsWith("video") ? "video" : "file"),
      cacheable: false
    };
  }
  throw reloginError(accountId);
}

async function cacheMedia(userId, accountId, peerId, messageId) {
  return downloadMedia(userId, accountId, peerId, messageId, { forceCache: true });
}

async function streamVideoMedia(userId, accountId, peerId, messageId, rangeHeader, res) {
  // B：原生账号视频走 Go blob 端点（支持 Range）；遗留 GramJS 账号需重新登录转原生。
  if (await nativeAccountRecord(userId, accountId)) {
    return proxyBlob(res, goBlobSourceUrl(userId, accountId, peerId, messageId), { inline: true, fileName: `media-${messageId}`, range: rangeHeader });
  }
  throw reloginError(accountId);
}


async function profilePhoto(userId, accountId, peerId = "__self") {
  // M4.2：native 账号的头像由 Go 原生 MTProto 下载，直接返回内存缓冲。
  if (await nativeAccountRecord(userId, accountId)) {
    try {
      const avatar = await downloaderSidecar.accountAvatar({ userId, accountId, peer: peerId });
      return {
        buffer: avatar.buffer,
        contentType: avatar.contentType || "image/jpeg",
        fileName: `${safeId("avatar")}.jpg`
      };
    } catch (error) {
      // 2.4.4 实测反馈：账号未就绪（failed）或头像暂时拉不到时，
      // 聊天列表每次渲染都会在日志里刷一行 "Error: Go downloader 404"。
      // 改为返回内置 SVG 占位图：前端正常显示，账号恢复后自动回到真实头像。
      const fallback = avatarFallbackBuffer(error?.message);
      return {
        buffer: fallback.buffer,
        contentType: fallback.contentType,
        fileName: `${safeId("avatar")}.svg`
      };
    }
  }
  throw reloginError(accountId);
}

function goDownloaderTaskId(userId, accountId, peerId, messageId) {
  return downloadTaskId(userId, accountId, peerId, messageId);
}

// B：遗留 GramJS 账号已不再受支持；统一引导重新登录转原生模式。
function reloginError(accountId) {
  return Object.assign(new Error("该账号需重新登录以升级到原生模式（Feigram 已移除旧版 GramJS 客户端）"), {
    status: 409,
    needsRelogin: true,
    accountId
  });
}

// B：原生账号媒体字节流统一经 Go 原生 blob 端点（支持 Range / FILE_MIGRATE / file_reference 续期），
// Node 侧仅做服务端代理，不整份落地。
async function proxyBlob(res, blobUrl, { inline = true, fileName = "media", range } = {}) {
  const upstream = await fetch(blobUrl, { headers: range ? { Range: String(range) } : {} });
  if (!upstream.ok && upstream.status !== 206) return false;
  const status = upstream.status === 206 ? 206 : 200;
  const headers = {};
  const ct = upstream.headers.get("content-type");
  const cl = upstream.headers.get("content-length");
  const cr = upstream.headers.get("content-range");
  if (ct) headers["Content-Type"] = ct;
  if (cl) headers["Content-Length"] = cl;
  if (cr) headers["Content-Range"] = cr;
  headers["Accept-Ranges"] = "bytes";
  headers["Cache-Control"] = "no-store";
  headers["Content-Disposition"] = `${inline ? "inline" : "attachment"}; filename="${encodeURIComponent(fileName)}"`;
  res.writeHead(status, headers);
  for await (const chunk of upstream.body) {
    if (res.destroyed) break;
    if (!res.write(chunk)) await new Promise((resolve) => res.once("drain", resolve));
  }
  res.end();
  return true;
}

// M4.1：Go 下载器的 http-bridge 回退源直接指向 Go 原生 blob 端点，
// 不再经由 Node 侧 GramJS 内部媒体桥（该桥已移除）。
function goBlobSourceUrl(userId, accountId, peerId, messageId) {
  const search = new URLSearchParams({
    userId: String(userId),
    peer: String(peerId),
    messageId: String(messageId)
  });
  return `${downloaderSidecar.baseUrl()}/api/accounts/${encodeURIComponent(accountId)}/blob?${search.toString()}`;
}

function base64Buffer(value) {
  if (!value) return "";
  if (Buffer.isBuffer(value)) return value.toString("base64");
  if (value instanceof Uint8Array) return Buffer.from(value).toString("base64");
  if (Array.isArray(value)) return Buffer.from(value).toString("base64");
  return "";
}

function nativeFileLocation(peerId, message, contentType, fileName, kind) {
  const document = message.document || null;
  const file = message.file || {};
  return {
    peerId,
    messageId: Number(message.id || 0),
    kind,
    fileId: document?.id ? toText(document.id) : "",
    accessHash: document?.accessHash ? toText(document.accessHash) : "",
    fileReference: base64Buffer(document?.fileReference),
    dcId: Number(document?.dcId || file.dcId || 0),
    size: Number(file.size || document?.size || 0),
    mimeType: contentType || document?.mimeType || "",
    fileName: fileName || file.name || "",
    updatedAt: new Date().toISOString()
  };
}

function nativePeerLocation(peerId, entity) {
  const klass = entity?.className || entity?.constructor?.name || "";
  const [fallbackType, fallbackId] = String(peerId || "").split(":");
  const id = toText(entity?.id || fallbackId || "");
  const accessHash = entity?.accessHash ? toText(entity.accessHash) : "";
  if (klass.includes("Channel") || fallbackType === "Channel") {
    return { type: "channel", id, accessHash };
  }
  if (klass.includes("User") || fallbackType === "User") {
    return { type: "user", id, accessHash };
  }
  if (klass.includes("Chat") || fallbackType === "Chat") {
    return { type: "chat", id, accessHash };
  }
  return { type: "", id, accessHash };
}

function normalizeGoDownloadTask(task) {
  const next = {
    id: task.id,
    accountId: task.accountId,
    peerId: task.peerId,
    messageId: task.messageId,
    fileName: task.fileName,
    kind: task.kind,
    contentType: task.contentType,
    status: task.status === "running" ? "downloading" : task.status,
    size: Number(task.size || 0),
    downloaded: Number(task.downloaded || 0),
    speedBps: Number(task.speedBps || 0),
    source: task.source || "manual",
    autoCache: Boolean(task.autoCache || task.source === "auto"),
    order: Number(task.order || 0),
    error: task.error || "",
    createdAt: task.createdAt,
    updatedAt: task.updatedAt,
    inlineUrl: task.inlineUrl || ""
  };
  if (!next.inlineUrl && next.status === "completed") {
    next.inlineUrl = `/api/media/${next.accountId}/${encodeURIComponent(next.peerId)}/${next.messageId}?inline=1`;
  }
  return next;
}

function normalizeGoSilentTask(task) {
  const next = normalizeGoDownloadTask(task);
  return {
    ...next,
    status: next.status === "downloading" ? "running" : next.status,
    lastProgressAt: task.lastProgressAt || task.updatedAt || "",
    lastObservedAt: task.lastObservedAt || task.updatedAt || ""
  };
}

async function goAllTasks() {
  const tasks = await downloaderSidecar.listTasks();
  return Array.isArray(tasks) ? tasks : [];
}

// R4.42：媒体字节大小必须尽量给出——Go 侧拿不到声明大小就无法校验完整性，
// 会（正确地）拒绝把文件当完整交付，于是任务永远完不成。
// 此前只取 message.file?.size || message.document?.size：Go 原生元数据里
// **没有** message.file，照片也**没有** document.size，于是所有照片任务的
// size 恒为 0 —— 这正是 2.6.18 实测「日志显示全部下载完成、实际都没下载下来」
// 的入口（未声明大小会让下游完成判定退化成「文件与自己比较」）。
function mediaByteSize(message) {
  const direct = Number(message?.file?.size || message?.document?.size || 0);
  if (direct > 0) return direct;
  // 照片：取各档 PhotoSize 中最大的一档（Go 原生元数据为 photo.sizes）。
  const sizes = message?.photo?.sizes;
  if (Array.isArray(sizes)) {
    return sizes.reduce((max, entry) => Math.max(max, Number(entry?.size || 0)), 0);
  }
  return 0;
}

async function ensureGoDownloadTask(userId, accountId, peerId, messageId, options = {}) {
  // M4.1 子步：原生账号经 Go 原生 MTProto 取消息元数据（nativeMediaMeta），避免 GramJS 的 409。
  const native = await nativeAccountRecord(userId, accountId);
  if (!native) throw reloginError(accountId);
  // R4.59：允许调用方传入扫描时已拿到的 { entity, message }——后台缓存批量入队
  // 每个视频再走一次 nativeMediaMeta（peer 解析 + around 拉取 20 条）会把整批
  // 拖到分钟级（代理差时必然整体超时、0 个入队），元数据在扫描结果里本来就有。
  const meta = options.meta || await nativeMediaMeta(userId, accountId, peerId, messageId);
  const { entity, message } = meta;
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = mediaByteSize(message);
  const { fileName, filePath, downloadDir } = await mediaFileInfo(userId, accountId, message, contentType, kind);
  await fs.ensureDir(downloadDir);
  const id = goDownloaderTaskId(userId, accountId, peerId, messageId);
  const source = options.source || "manual";
  // R4.23：单一传输层。此前这里按「全局配置 / 体积≥100MB 且账号就绪」挑选
  // http-bridge，但 M4.1 后 http-bridge 的源只能指向 Go 自己的 blob 端点（自环），
  // 账号未就绪时把「尚未就绪」包装成 404 终态错误，后台缓存任务全挂。
  // 现在一律 native-mtproto；账号未就绪由 Go 调度层等待恢复（自动续传）。
  const transport = "native-mtproto";
  const task = await downloaderSidecar.enqueueTask({
    id,
    userId,
    accountId,
    peerId,
    messageId: Number(messageId),
    fileName,
    filePath,
    partPath: `${filePath}.part`,
    kind,
    contentType: contentType || mime.lookup(fileName) || "application/octet-stream",
    size,
    source,
    autoCache: Boolean(options.autoCache || source === "auto"),
    transport,
    sourceUrl: goBlobSourceUrl(userId, accountId, peerId, messageId),
    inlineUrl: `/api/media/${accountId}/${encodeURIComponent(peerId)}/${messageId}?inline=1`,
    nativeFile: nativeFileLocation(peerId, message, contentType, fileName, kind),
    nativePeer: nativePeerLocation(peerId, entity),
    order: Number(options.order || 0) || Date.now(),
    dedupKey: options.dedupKey || ""
  });
  return normalizeGoDownloadTask(task);
}

async function mediaNativeMetadata(userId, accountId, peerId, messageId) {
  // M4.1 子步：原生账号经 Go 原生 MTProto 取消息元数据（nativeMediaMeta）。
  const native = await nativeAccountRecord(userId, accountId);
  if (!native) throw reloginError(accountId);
  const { entity, message } = await nativeMediaMeta(userId, accountId, peerId, messageId);
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const file = message.file || {};
  const fileName = safeFileName(file.name || message.document?.mimeType || `telegram-${messageId}`);
  return {
    accountId,
    peerId,
    messageId: Number(messageId),
    nativeFile: nativeFileLocation(peerId, message, contentType, fileName, kind),
    nativePeer: nativePeerLocation(peerId, entity)
  };
}

async function startGoDownloadTask(userId, accountId, peerId, messageId, _io, options = {}) {
  return ensureGoDownloadTask(userId, accountId, peerId, messageId, options);
}

async function listGoDownloadTasks(userId) {
  const tasks = await goAllTasks().catch(() => []);
  return tasks
    .filter((task) => task.userId === userId && !task.autoCache && task.source !== "auto")
    .sort((a, b) => String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt)))
    .map(normalizeGoDownloadTask);
}

async function listGoSilentCacheTasks(userId) {
  const tasks = await goAllTasks().catch(() => []);
  return tasks
    .filter((task) => task.userId === userId && task.status !== "cancelled" && (task.autoCache || task.source === "auto"))
    .sort((a, b) => {
      const orderDiff = Number(a.order || 0) - Number(b.order || 0);
      if (orderDiff) return orderDiff;
      return String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt));
    })
    .map(normalizeGoSilentTask);
}

async function goSilentCacheState(userId) {
  const state = await downloaderSidecar.state();
  const config = state?.config || {};
  const tasks = (Array.isArray(state?.tasks) ? state.tasks : [])
    .filter((task) => task.userId === userId && task.status !== "cancelled" && (task.autoCache || task.source === "auto"))
    .sort((a, b) => Number(a.order || 0) - Number(b.order || 0))
    .map(normalizeGoSilentTask);
  return {
    enabled: config.enabled !== false,
    rateLimitBps: Number(config.rateLimitBps || 0),
    concurrency: Math.max(1, Number(config.concurrency || 1)),
    configuredConcurrency: Math.max(1, Number(config.concurrency || 1)),
    // R4.46：effectiveConcurrency 此前误用 Go 的全局 running 计数（含手动任务），
    // 且保守模式实际强制并发 1（main.go pumpOnce：Mode != "fast" → limit=1），
    // 面板却显示「运行中 0/5」误导用户。改为按模式计算真实并发上限。
    effectiveConcurrency: (config.mode || "conservative") === "fast"
      ? Math.max(1, Number(config.concurrency || 1))
      : 1,
    mode: config.mode || "conservative",
    transport: config.transport || state?.transport || "native-mtproto",
    running: tasks.filter((task) => task.status === "running").length,
    tasks,
    engine: "go-sidecar"
  };
}

async function setGoSilentCacheControl(userId, payload = {}) {
  const patch = {};
  if (payload.enabled !== undefined) patch.enabled = Boolean(payload.enabled);
  if (payload.rateLimitBps !== undefined) patch.rateLimitBps = Math.max(0, Math.min(1024 * 1024 * 1024, Number(payload.rateLimitBps || 0)));
  if (payload.concurrency !== undefined) patch.concurrency = Math.max(1, Math.min(10, Number(payload.concurrency || 1)));
  if (payload.mode !== undefined) patch.mode = normalizedSilentCacheMode(payload.mode);
  if (payload.transport !== undefined) patch.transport = payload.transport === "native-mtproto" ? "native-mtproto" : "http-bridge";
  await downloaderSidecar.updateConfig(patch);
  return goSilentCacheState(userId);
}

async function reorderGoSilentCacheTasks(userId, orderedIds = []) {
  const ids = Array.isArray(orderedIds) ? orderedIds : [];
  const tasks = await goAllTasks();
  const byId = new Map(tasks.map((task) => [task.id, task]));
  for (const [index, id] of ids.entries()) {
    const task = byId.get(id);
    if (task && task.userId === userId) {
      await downloaderSidecar.enqueueTask({ ...task, order: index + 1 });
    }
  }
  return goSilentCacheState(userId);
}

async function cancelGoSilentCacheTask(userId, taskId) {
  const task = await downloaderSidecar.getTask(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到后台缓存任务"), { status: 404 });
  await downloaderSidecar.cancelTask(taskId);
  return { ok: true, id: taskId };
}

async function cancelGoSilentCacheTasks(userId, taskIds = []) {
  const ids = Array.isArray(taskIds) ? taskIds : [];
  const cancelled = [];
  for (const id of ids) {
    const task = await downloaderSidecar.getTask(id).catch(() => null);
    if (!task || task.userId !== userId) continue;
    await downloaderSidecar.cancelTask(id).catch(() => {});
    cancelled.push(id);
  }
  return { ok: true, ids: cancelled };
}

async function resumeGoDownloadTask(userId, taskId) {
  const task = await downloaderSidecar.getTask(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  return normalizeGoDownloadTask(await downloaderSidecar.queueTask(taskId));
}

async function cancelGoDownloadTask(userId, taskId) {
  const task = await downloaderSidecar.getTask(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  return normalizeGoDownloadTask(await downloaderSidecar.cancelTask(taskId));
}

async function clearGoDownloadTask(userId, taskId) {
  const task = await downloaderSidecar.getTask(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  await downloaderSidecar.deleteTask(taskId);
  return { ok: true };
}

async function deleteGoDownloadTask(userId, taskId) {
  const task = await downloaderSidecar.getTask(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  await downloaderSidecar.deleteTask(taskId);
  await fs.remove(task.partPath || `${task.filePath}.part`).catch(() => {});
  await fs.remove(task.filePath).catch(() => {});
  return { ok: true };
}

async function cacheVideoSilentlyGo(userId, accountId, peerId, message, entity = null) {
  const contentType = message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = Number(message.file?.size || message.document?.size || 0);
  // R4.57：返回可区分状态（queued/duplicate/skip），让调用方统计真实的
  // 「新增/重复/跳过」，不再把三种情况混成一个 false。
  if (kind !== "video" || size <= VIDEO_CACHE_THRESHOLD) return "skip";
  const mediaInfo = await mediaFileInfo(userId, accountId, message, contentType, kind);
  const dedupKey = silentDedupKey(userId, accountId, peerId, mediaInfo.fileName, size);
  const tasks = await goAllTasks().catch(() => []);
  const duplicate = tasks.find((task) => (
    task.userId === userId &&
    task.accountId === accountId &&
    task.peerId === peerId &&
    task.status !== "cancelled" &&
    (task.id === goDownloaderTaskId(userId, accountId, peerId, message.id) ||
      silentDedupKey(task.userId, task.accountId, task.peerId, task.fileName, task.size) === dedupKey)
  ));
  if (duplicate) return "duplicate";
  await ensureGoDownloadTask(userId, accountId, peerId, message.id, {
    source: "auto",
    autoCache: true,
    dedupKey,
    order: Date.now(),
    // R4.59：扫描元数据直传——不再每视频回源拉一遍（见 ensureGoDownloadTask 注释）。
    meta: entity ? { entity, message } : null
  });
  return "queued";
}

// R4.60：后台缓存扫描改异步作业。旧实现把「扫描 200 条 + 逐视频入队」整条链路
// 塞在一个同步 HTTP 请求里，代理差/盘慢时任何一环慢都会顶穿前端 120s 超时
// （2026-09-25 实测：勾选后显示「请求超时」，但 Node 后台其实还在跑，结果不可见）。
// 现在提交即受理（秒回 { started:true }），扫描在后台执行，结果经同路径 GET
// 状态接口轮询取回；各阶段打点日志，失败原因记入 job.error 不静默。
const autoCacheJobs = new Map();

function autoCacheJobKey(userId, accountId, peerId) {
  return `${userId}|${accountId}|${peerId}`;
}

function autoCacheJobSnapshot(job) {
  if (!job) return { status: "none" };
  return {
    status: job.status,
    startedAt: job.startedAt,
    result: job.result,
    error: job.error || ""
  };
}

function cacheLargeVideosStatus(userId, accountId, peerId) {
  return autoCacheJobSnapshot(autoCacheJobs.get(autoCacheJobKey(userId, accountId, peerId)));
}

function startAutoCacheScan(userId, accountId, peerId, io = realtimeIo) {
  realtimeIo = io || realtimeIo;
  const key = autoCacheJobKey(userId, accountId, peerId);
  const existing = autoCacheJobs.get(key);
  // 防重入：同一会话已有扫描在跑时直接告知，前端继续轮询同一个 job。
  if (existing && existing.status === "running") {
    return { started: false, alreadyRunning: true };
  }
  const job = { status: "running", startedAt: new Date().toISOString(), result: null, error: "" };
  autoCacheJobs.set(key, job);
  runAutoCacheScan(userId, accountId, peerId, job, key).catch((err) => {
    // 兜底：runAutoCacheScan 内部已把错误写进 job，这里只防意外同步抛出。
    job.status = "error";
    job.error = err.message || String(err);
    console.warn(`[auto-cache] ${key} 扫描异常: ${job.error}`);
  });
  return { started: true };
}

async function runAutoCacheScan(userId, accountId, peerId, job, key) {
  const startedMs = Date.now();
  try {
    if (!(await nativeAccountRecord(userId, accountId))) throw reloginError(accountId);
    // R4.57：扫描窗口 120→200（Go /messages 上限即 200）；扫描失败直接进
    // job.error（不再静默成「没有大视频」）；单个视频入队失败不中断整批。
    const scanStart = Date.now();
    const items = await downloaderSidecar.accountMessages({
      userId, accountId, peer: peerId, limit: 200
    });
    const recent = Array.isArray(items) ? items : [];
    const peerStart = Date.now();
    const entity = await resolveNativePeerEntity(userId, accountId, peerId);
    console.log(`[auto-cache] ${key} 扫描 ${recent.length} 条耗时 ${peerStart - scanStart}ms，peer 解析 ${Date.now() - peerStart}ms`);
    let queued = 0;
    let duplicates = 0;
    let skipped = 0;
    let failed = 0;
    for (const item of recent) {
      if (!item || !item.media || !item.media.hasPreview) continue;
      const message = goMessageToNodeMessage(item);
      try {
        const status = await cacheVideoSilentlyGo(userId, accountId, peerId, message, entity);
        if (status === "queued") queued += 1;
        else if (status === "duplicate") duplicates += 1;
        else skipped += 1;
      } catch (err) {
        failed += 1;
        console.warn(`[auto-cache] ${key} 消息 ${item.id} 入队失败: ${err.message}`);
      }
    }
    job.result = { queued, scanned: recent.length, duplicates, skipped, failed };
    job.status = "done";
    console.log(`[auto-cache] ${key} 完成：新增 ${queued}/重复 ${duplicates}/跳过 ${skipped}/失败 ${failed}，总耗时 ${Date.now() - startedMs}ms`);
  } catch (err) {
    job.status = "error";
    job.error = err.message || String(err);
    console.warn(`[auto-cache] ${key} 扫描失败（${Date.now() - startedMs}ms）: ${job.error}`);
  }
}

async function goSilentCacheSpeedDiagnostics(userId) {
  const state = await downloaderSidecar.state();
  const tasks = (Array.isArray(state?.tasks) ? state.tasks : []).filter((task) => task.userId === userId && (task.autoCache || task.source === "auto"));
  const running = tasks.filter((task) => ["downloading", "running"].includes(task.status));
  const primary = running[0] || tasks.find((task) => task.status === "queued") || tasks[0];
  return {
    testedAt: new Date().toISOString(),
    ok: Boolean(state?.ok),
    enabled: state?.config?.enabled !== false,
    rateLimitBps: Number(state?.config?.rateLimitBps || 0),
    concurrency: Number(state?.config?.concurrency || 1),
    configuredConcurrency: Number(state?.config?.concurrency || 1),
    effectiveConcurrency: Number(state?.running || 0),
    cacheMode: state?.config?.mode || "conservative",
    transport: state?.config?.transport || state?.transport || "native-mtproto",
    mode: "go-sidecar",
    running: running.length,
    queued: tasks.filter((task) => task.status === "queued").length,
    partSizeKb: Math.round(Number(state?.config?.partSize || (1024 * 1024)) / 1024),
    result: {
      bytesRead: 0,
      chunks: 0,
      durationMs: 0,
      speedBps: Number(state?.speedBps || 0),
      runningTasks: running.length,
      requestedChunkSize: Number(state?.config?.partSize || (1024 * 1024)),
      effectiveChunkSize: Number(state?.config?.partSize || (1024 * 1024)),
      fallbackCount: 0,
      limitInvalidCount: 0
    },
    task: primary ? normalizeGoSilentTask(primary) : null,
    activeTasks: tasks.slice(0, 20).map(normalizeGoSilentTask),
    note: "Go 下载服务已接管队列与文件写入；诊断显示 Go 任务聚合速度。"
  };
}

async function goNativeAccounts() {
  const records = await downloaderSidecar.nativeAccounts();
  const list = Array.isArray(records) ? records : [];
  // 标注每条 Go 记录是否有对应的 Node 账号：没有的即「残留记录」
  // （多半来自失败/中途放弃的登录流程），诊断页据此分组并允许清理。
  const accounts = await readAccounts();
  const linkedIds = new Set(accounts.map((item) => item.id));
  return list.map((record) => ({ ...record, linked: linkedIds.has(record.accountId) }));
}

async function deleteOrphanNativeAccount(userId, accountId) {
  const accounts = await readAccounts();
  if (accounts.some((item) => item.id === accountId && item.userId === userId)) {
    throw Object.assign(new Error("该记录仍关联着应用内账号，请用「退出登录」清理"), { status: 409 });
  }
  return downloaderSidecar.deleteNativeAccount(userId, accountId);
}

async function goNativeAccountHealth(userId, accountId) {
  const account = (await readAccounts()).find((item) => item.userId === userId && item.id === accountId);
  if (!account) throw Object.assign(new Error("账号不存在"), { status: 404 });
  await syncGoNativeAccount(account).catch(() => null);
  return downloaderSidecar.checkNativeAccount(userId, accountId);
}

async function goNativeAccountLoginStart(userId, accountId, payload = {}) {
  const account = (await readAccounts()).find((item) => item.userId === userId && item.id === accountId);
  if (!account) throw Object.assign(new Error("账号不存在"), { status: 404 });
  const { apiId, apiHash } = await telegramConfig();
  await syncGoNativeAccount(account).catch(() => null);
  return downloaderSidecar.startNativeLogin(userId, accountId, {
    phone: payload.phone || account.phone || "",
    apiId,
    apiHash
  });
}

async function goNativeAccountQRLoginStart(userId, accountId, payload = {}) {
  const account = (await readAccounts()).find((item) => item.userId === userId && item.id === accountId);
  if (!account) throw Object.assign(new Error("账号不存在"), { status: 404 });
  const { apiId, apiHash } = await telegramConfig();
  await syncGoNativeAccount(account).catch(() => null);
  return downloaderSidecar.startNativeQRLogin(userId, accountId, {
    apiId: payload.apiId || apiId,
    apiHash: payload.apiHash || apiHash
  });
}

async function goNativeAccountQRLoginStatus(userId, accountId, payload = {}) {
  return downloaderSidecar.pollNativeQRLogin(userId, accountId, {
    loginId: payload.loginId
  });
}

async function goNativeAccountLoginCode(userId, accountId, payload = {}) {
  return downloaderSidecar.submitNativeLoginCode(userId, accountId, {
    loginId: payload.loginId,
    code: payload.code
  });
}

async function goNativeAccountLoginPassword(userId, accountId, payload = {}) {
  return downloaderSidecar.submitNativeLoginPassword(userId, accountId, {
    loginId: payload.loginId,
    password: payload.password
  });
}

async function restoreGoBackgroundTasks(io) {
  realtimeIo = io;
  // GramJS 移除后 Node 侧不再持有/持久化下载任务；Go 侧后台任务由 downloader sidecar 自行恢复。
}

async function cleanupCache() {
  const settings = await cacheSettings();
  const cutoff = Date.now() - settings.retentionDays * 24 * 60 * 60 * 1000;
  for (const dir of [settings.image, settings.video, settings.file, settings.avatars, settings.thumbs]) {
    if (!(await fs.pathExists(dir))) continue;
    const entries = await fs.readdir(dir).catch(() => []);
    for (const entry of entries) {
      const entryPath = path.join(dir, entry);
      const stat = await fs.stat(entryPath).catch(() => null);
      if (!stat) continue;
      if (stat.isDirectory()) {
        const files = await fs.readdir(entryPath).catch(() => []);
        for (const file of files) {
          const filePath = path.join(entryPath, file);
          const fileStat = await fs.stat(filePath).catch(() => null);
          if (fileStat && fileStat.mtimeMs < cutoff) await fs.remove(filePath).catch(() => {});
        }
      } else if (stat.mtimeMs < cutoff) {
        await fs.remove(entryPath).catch(() => {});
      }
    }
  }
}

// M2.4：GramJS → gotd session「一键迁移」（路径 A）。
// 迁移失败统一抛出带 needsRelogin 标记的错误，由前端降级到「路径 B：重新登录」。
// 说明（M4.0）：迁移成功后 Node 会丢弃该账号的加密 GramJS session，
// session 由 gotd 独占，账户结构只保留「已就绪」状态与展示信息。
// 影响：回滚到 GramJS 不再可能，若 Go 侧后续失效需走「路径 B：重新登录」。
async function migrateAccountToGo(userId, accountId) {
  const accounts = await readAccounts();
  const account = accounts.find((item) => item.id === accountId && item.userId === userId);
  if (!account) throw Object.assign(new Error("账号不存在"), { status: 404 });
  if (account.authMode === "native") {
    return { migrated: true, alreadyMigrated: true, account: stripAccountSession(account) };
  }
  if (!account.session) {
    throw Object.assign(new Error("该账号没有可迁移的 GramJS session，请直接重新登录"), { status: 400, needsRelogin: true });
  }
  const { apiId, apiHash } = await telegramConfig();
  const gramjsSession = await decryptText(account.session);
  if (!gramjsSession) {
    throw Object.assign(new Error("GramJS session 解密结果为空，请直接重新登录"), { status: 400, needsRelogin: true });
  }
  // 先停后启：迁移前断开该账号的 GramJS 连接，避免同一 auth_key 被双端同时持有。
  await resetTelegramClient(accountId);
  const result = await downloaderSidecar.migrateAccount({
    userID: userId,
    accountID: accountId,
    gramjsSession,
    phone: account.phoneNumber || "",
    label: account.displayName || account.label || "",
    apiId,
    apiHash
  });
  if (!result.ok || !result.migrated) {
    throw Object.assign(new Error(result.error || "迁移失败，请改用重新登录"), { status: 502, needsRelogin: true });
  }
  // M4.0：丢弃 Node 侧持有的 GramJS session，账户结构不再含 session 字段。
  const { session: droppedGramjsSession } = account;
  const migrated = {
    ...account,
    authMode: "native",
    migratedAt: new Date().toISOString(),
    session: undefined
  };
  void droppedGramjsSession;
  await upsertAccount(migrated);
  return { migrated: true, dc: result.dc, account: stripAccountSession(migrated) };
}

function stripAccountSession(account) {
  const { session, ...safe } = account;
  return safe;
}

module.exports = {
  clickMessageButton,
  completeCode,
  completePassword,
  cacheMedia,
  cacheLargeVideosInChat: startAutoCacheScan,
  cacheLargeVideosStatus,
  cancelDownloadTask: cancelGoDownloadTask,
  chatDetails,
  chatMedia,
  clearDownloadTask: clearGoDownloadTask,
  deleteDownloadTask: deleteGoDownloadTask,
  downloadMedia,
  mediaThumbnail,
  listDownloadTasks: listGoDownloadTasks,
  listSilentCacheTasks: listGoSilentCacheTasks,
  silentCacheSpeedDiagnostics: goSilentCacheSpeedDiagnostics,
  silentCacheState: goSilentCacheState,
  setSilentCacheControl: setGoSilentCacheControl,
  reorderSilentCacheTasks: reorderGoSilentCacheTasks,
  monitorSilentCacheTasks: () => {},
  nativeAccounts: goNativeAccounts,
  deleteOrphanNativeAccount,
  nativeAccountHealth: goNativeAccountHealth,
  nativeAccountLoginStart: goNativeAccountLoginStart,
  nativeAccountQRLoginStart: goNativeAccountQRLoginStart,
  nativeAccountQRLoginStatus: goNativeAccountQRLoginStatus,
  nativeAccountLoginCode: goNativeAccountLoginCode,
  nativeAccountLoginPassword: goNativeAccountLoginPassword,
  mediaNativeMetadata,
  cancelSilentCacheTask: cancelGoSilentCacheTask,
  cancelSilentCacheTasks: cancelGoSilentCacheTasks,
  resumeDownloadTask: resumeGoDownloadTask,
  startDownloadTask: startGoDownloadTask,
  streamVideoMedia,
  proxyBlob,
  listAccounts,
  migrateAccountToGo,
  listChats,
  listFolders,
  listMessages,
  logout,
  cleanupCache,
  profilePhoto,
  resolveTelegramLink,
  restoreBackgroundTasks: restoreGoBackgroundTasks,
  search,
  sendText,
  startLogin
};
