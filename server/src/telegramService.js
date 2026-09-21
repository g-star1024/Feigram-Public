const path = require("path");
const crypto = require("crypto");
const fs = require("fs-extra");
const mime = require("mime-types");
const bigInt = require("big-integer");
const { Api, TelegramClient } = require("telegram");
const { StringSession } = require("telegram/sessions");
const { NewMessage } = require("telegram/events");
const { dataDir, downloadTasksPath, readAccounts, removeAccount, safeId, silentCachePath, upsertAccount } = require("./store");
const { readSettings } = require("./settings");
const { decryptText, encryptText } = require("./cryptoBox");
const downloaderSidecar = require("./downloaderSidecar");

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
const DIALOG_FETCH_LIMIT = 500;

function stableId(prefix, ...parts) {
  return `${prefix}_${crypto.createHash("sha1").update(parts.map((part) => String(part ?? "")).join("|")).digest("hex").slice(0, 24)}`;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function isTransientDownloadError(error) {
  const message = String(error?.message || error || "");
  return /FILE_REFERENCE_EXPIRED|File reference expired|Request was unsuccessful|TIMEOUT|timeout|retryUntilAck|retry limit reached|unexpected EOF|ECONN|ETIMEDOUT|EPIPE|socket|network|disconnect|CONNECTION_NOT_INITED|AUTH_KEY_UNREGISTERED|AUTH_KEY_DUPLICATED|Not connected/i.test(message);
}

function isDuplicatedAuthKey(error) {
  return /AUTH_KEY_DUPLICATED/i.test(String(error?.message || error || ""));
}

function isFileReferenceExpired(error) {
  return /FILE_REFERENCE_EXPIRED|File reference expired/i.test(String(error?.message || error || ""));
}

function nextSilentOrder() {
  return Math.max(0, ...[...silentCacheRecords.values()].map((task) => Number(task.order || 0))) + 1;
}

function enqueueSilentTask(task) {
  if (!task || ["completed", "cancelled"].includes(task.status)) return;
  if (silentCacheQueue.some((item) => item.id === task.id)) return;
  silentCacheQueue.push(task);
  silentCacheQueue.sort((a, b) => Number(a.order || 0) - Number(b.order || 0));
}

function silentConcurrencyLimit() {
  return Math.max(1, Math.min(MAX_SILENT_CACHE_CONCURRENCY, Number(silentCacheConcurrency || 1)));
}

function normalizedSilentCacheMode(value) {
  return value === "fast" ? "fast" : "conservative";
}

function syncSilentCacheActive() {
  const running = [...silentCacheRecords.values()].filter((task) => task.status === "running" && silentCacheTasks.has(task.id)).length;
  if (silentCacheActive !== running) silentCacheActive = running;
  return running;
}

function activeSilentAccountCount(userId = "") {
  const accounts = new Set();
  for (const task of downloadTasks.values()) {
    if (userId && task.userId !== userId) continue;
    if (["completed", "cancelled"].includes(task.status)) continue;
    accounts.add(task.accountId);
  }
  return accounts.size;
}

function effectiveSilentConcurrency(userId = "") {
  const configured = silentConcurrencyLimit();
  return Math.max(1, Math.min(configured, activeSilentAccountCount(userId) || 1));
}

function fileSizeComplete(actualSize, expectedSize) {
  const actual = Math.max(0, Number(actualSize || 0));
  const expected = Math.max(0, Number(expectedSize || 0));
  return actual > 0 && (!expected || actual >= expected);
}

function formatByteCount(bytes) {
  const value = Math.max(0, Number(bytes || 0));
  if (value >= 1024 * 1024 * 1024) return `${(value / 1024 / 1024 / 1024).toFixed(1)} GB`;
  if (value >= 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)} MB`;
  if (value >= 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${value} B`;
}

function lowSpeedThresholdBps() {
  const limit = Number(silentCacheRateLimitBps || 0);
  if (!limit) return MIN_HEALTHY_SPEED_BPS;
  return Math.max(16 * 1024, Math.min(MIN_HEALTHY_SPEED_BPS, Math.floor(limit / Math.max(1, effectiveSilentConcurrency()))));
}

function hasForegroundAccountOp(accountId) {
  return Number(foregroundAccountOps.get(accountId) || 0) > 0;
}

function hasRunningSilentCacheForAccount(accountId) {
  for (const task of silentCacheRecords.values()) {
    if (task.accountId === accountId && task.status === "running" && silentCacheTasks.has(task.id)) return true;
  }
  return false;
}

function markForegroundAccountOp(accountId, delta) {
  const next = Math.max(0, Number(foregroundAccountOps.get(accountId) || 0) + delta);
  if (next) foregroundAccountOps.set(accountId, next);
  else foregroundAccountOps.delete(accountId);
}

function resetSilentRateWindow() {
  silentRateWindowStart = Date.now();
  silentRateWindowBytes = 0;
  silentRateChain = Promise.resolve();
}

function rebalanceSilentConcurrency(io = realtimeIo) {
  const limit = silentConcurrencyLimit();
  const running = [...silentCacheRecords.values()]
    .filter((task) => task.status === "running" && task.cancelToken)
    .sort((a, b) => Number(a.order || 0) - Number(b.order || 0));
  running.slice(limit).forEach((task) => {
    task.cancelToken.paused = true;
    task.cancelToken.requeue = true;
    task.status = "queued";
    task.speedBps = 0;
    task.updatedAt = new Date().toISOString();
    enqueueSilentTask(task);
    emitSilentCacheTask(io, task);
  });
}

function detachRunningSilentTask(task, reason, io = realtimeIo, { resetConnection = false, retryDelayMs = 0 } = {}) {
  if (!task || task.status !== "running") return false;
  if (task.cancelToken) {
    task.cancelToken.paused = true;
    task.cancelToken.requeue = true;
    task.cancelToken.detached = true;
  }
  if (task.runToken) task.runToken.detached = true;
  if (silentCacheTasks.has(task.id)) {
    silentCacheTasks.delete(task.id);
    silentCacheActive = Math.max(0, silentCacheActive - 1);
  }
  task.runToken = null;
  task.cancelToken = null;
  task.status = "queued";
  task.error = reason || "后台缓存已重新排队";
  task.speedBps = 0;
  task.lowSpeedSince = "";
  if (retryDelayMs) task.retryAfter = Date.now() + retryDelayMs;
  task.updatedAt = new Date().toISOString();
  enqueueSilentTask(task);
  emitSilentCacheTask(io, task);
  if (resetConnection) resetTelegramClient(task.accountId).catch(() => {});
  return true;
}

function suspendSilentCacheForAccount(accountId, io = realtimeIo, reason = "前台正在读取聊天，后台缓存稍后续传") {
  let changed = false;
  for (const task of silentCacheRecords.values()) {
    if (task.accountId !== accountId) continue;
    if (detachRunningSilentTask(task, reason, io, { resetConnection: false })) changed = true;
  }
  if (changed) pumpSilentCacheQueue(io);
  return changed;
}

function enforceConservativeSilentCacheMode(io = realtimeIo) {
  if (silentCacheMode === "fast") return false;
  const seenAccounts = new Set();
  let changed = false;
  const running = [...silentCacheRecords.values()]
    .filter((task) => task.status === "running" && silentCacheTasks.has(task.id))
    .sort((a, b) => Number(a.order || 0) - Number(b.order || 0));
  for (const task of running) {
    if (!seenAccounts.has(task.accountId)) {
      seenAccounts.add(task.accountId);
      continue;
    }
    if (detachRunningSilentTask(task, "保守模式限制同账号单任务，已重新排队", io, { resetConnection: false })) changed = true;
  }
  if (changed) pumpSilentCacheQueue(io);
  return changed;
}

function degradeFastModeIfStalled(now = Date.now(), io = realtimeIo) {
  if (silentCacheMode !== "fast") return false;
  const running = [...silentCacheRecords.values()]
    .filter((task) => task.status === "running" && silentCacheTasks.has(task.id));
  if (running.length < 2) return false;
  const byAccount = new Map();
  for (const task of running) {
    if (!byAccount.has(task.accountId)) byAccount.set(task.accountId, []);
    byAccount.get(task.accountId).push(task);
  }
  let changed = false;
  for (const tasks of byAccount.values()) {
    if (tasks.length < 2) continue;
    const allZeroSpeed = tasks.every((task) => Number(task.speedBps || 0) <= 0);
    const allStalled = tasks.every((task) => {
      const lastObserved = Date.parse(task.lastObservedAt || task.lastProgressAt || task.createdAt || 0) || 0;
      const startedAt = Number(task.runToken?.startedAt || 0);
      const anchor = lastObserved || startedAt;
      return anchor && now - anchor > FAST_ZERO_SPEED_DEGRADE_MS;
    });
    if (allZeroSpeed && allStalled) {
      silentCacheMode = "conservative";
      for (const task of tasks.slice(1)) {
        if (detachRunningSilentTask(task, "高速模式连续 0 速，已自动降级保守并重新排队", io, { resetConnection: false, retryDelayMs: STALE_REQUEUE_DELAY_MS })) changed = true;
      }
    }
  }
  if (changed) {
    for (const task of silentCacheRecords.values()) {
      if (task.status === "running") {
        task.error = task.error || "高速模式连续 0 速，已自动降级保守";
        emitSilentCacheTask(io, task);
      }
    }
    schedulePersistSilent();
    pumpSilentCacheQueue(io);
  }
  return changed;
}

async function completeSilentTaskFromPart(task, io = realtimeIo) {
  if (!task?.partPath || !task?.filePath) return false;
  const partStat = await fs.stat(task.partPath).catch(() => null);
  const partSize = Math.max(0, Number(partStat?.size || 0));
  const total = Math.max(0, Number(task.size || 0));
  if (!fileSizeComplete(partSize, total)) return false;
  if (task.cancelToken) {
    task.cancelToken.paused = true;
    task.cancelToken.detached = true;
  }
  if (task.runToken) task.runToken.detached = true;
  if (silentCacheTasks.has(task.id)) {
    silentCacheTasks.delete(task.id);
    silentCacheActive = Math.max(0, silentCacheActive - 1);
  }
  await fs.ensureDir(path.dirname(task.filePath));
  await fs.move(task.partPath, task.filePath, { overwrite: true });
  const stat = await fs.stat(task.filePath).catch(() => null);
  task.downloaded = Number(stat?.size || partSize);
  task.size = task.size || task.downloaded;
  if (!fileSizeComplete(task.downloaded, task.size)) return false;
  task.status = "completed";
  task.error = "";
  task.speedBps = 0;
  task.runToken = null;
  task.cancelToken = null;
  task.lastProgressAt = new Date().toISOString();
  task.updatedAt = task.lastProgressAt;
  emitSilentCacheTask(io, task);
  schedulePersistSilent();
  return true;
}

async function throttleSilentCache(deltaBytes) {
  const delta = Math.max(0, Number(deltaBytes || 0));
  const rate = Number(silentCacheRateLimitBps || 0);
  if (!delta || !rate) return;
  silentRateChain = silentRateChain.catch(() => {}).then(async () => {
    const currentRate = Number(silentCacheRateLimitBps || 0);
    if (!currentRate) return;
    const now = Date.now();
    if (now - silentRateWindowStart > 60 * 1000) {
      silentRateWindowStart = now;
      silentRateWindowBytes = 0;
    }
    silentRateWindowBytes += delta;
    const expectedElapsed = (silentRateWindowBytes / currentRate) * 1000;
    const actualElapsed = Date.now() - silentRateWindowStart;
    if (expectedElapsed > actualElapsed) {
      await sleep(expectedElapsed - actualElapsed);
    }
  });
  return silentRateChain;
}

function schedulePersistDownloads() {
  if (downloadPersistTimer) return;
  downloadPersistTimer = setTimeout(async () => {
    downloadPersistTimer = null;
    await persistDownloadTasks().catch((error) => console.warn("Persist downloads failed:", error.message));
  }, 500);
  downloadPersistTimer.unref?.();
}

function schedulePersistSilent() {
  if (silentPersistTimer) return;
  silentPersistTimer = setTimeout(async () => {
    silentPersistTimer = null;
    await persistSilentCacheTasks().catch((error) => console.warn("Persist silent cache failed:", error.message));
  }, 500);
  silentPersistTimer.unref?.();
}

async function persistDownloadTasks() {
  await fs.ensureDir(dataDir);
  const tasks = [...downloadTasks.values()].map(({ cancelToken, ...task }) => task);
  await fs.writeJson(downloadTasksPath, { tasks }, { spaces: 2 });
}

async function persistSilentCacheTasks() {
  await fs.ensureDir(dataDir);
  await fs.writeJson(silentCachePath, {
    enabled: silentCacheEnabled,
    rateLimitBps: silentCacheRateLimitBps,
    concurrency: silentConcurrencyLimit(),
    mode: normalizedSilentCacheMode(silentCacheMode),
    tasks: []
  }, { spaces: 2 });
}

async function loadPersistentTasks() {
  const savedDownloads = await fs.readJson(downloadTasksPath).catch(() => ({ tasks: [] }));
  for (const task of savedDownloads.tasks || []) {
    if (!task?.id || !task.userId || !task.accountId || !task.peerId || !task.messageId) continue;
    const completedStat = task.status === "completed" && task.filePath ? await fs.stat(task.filePath).catch(() => null) : null;
    const completedFileExists = completedStat && fileSizeComplete(completedStat.size, task.size);
    if (completedStat && !completedFileExists && task.partPath) {
      await fs.move(task.filePath, task.partPath, { overwrite: true }).catch(() => {});
    }
    const next = {
      ...task,
      cancelToken: null,
      retryCount: Number(task.retryCount || 0),
      source: task.source || "manual",
      autoCache: Boolean(task.autoCache),
      order: Number(task.order || 0) || nextDownloadOrder(),
      speedBps: 0,
      downloaded: completedFileExists ? completedStat.size : Number(task.downloaded || 0),
      lastProgressAt: task.lastProgressAt || task.updatedAt || new Date().toISOString(),
      lastObservedAt: task.lastObservedAt || task.lastProgressAt || task.updatedAt || new Date().toISOString(),
      lastObservedPartSize: Number(task.lastObservedPartSize || task.downloaded || 0),
      retryAfter: 0,
      status: completedFileExists ? "completed" : "queued",
      error: completedFileExists ? task.error || "" : "本地文件不完整，已等待续传",
      updatedAt: new Date().toISOString()
    };
    downloadTasks.set(next.id, next);
  }

  const savedSilent = await fs.readJson(silentCachePath).catch(() => ({ tasks: [] }));
  silentCacheEnabled = savedSilent.enabled !== false;
  silentCacheRateLimitBps = Math.max(0, Number(savedSilent.rateLimitBps || 0));
  silentCacheConcurrency = Math.max(1, Math.min(MAX_SILENT_CACHE_CONCURRENCY, Number(savedSilent.concurrency || 1)));
  silentCacheMode = normalizedSilentCacheMode(savedSilent.mode);
  for (const task of savedSilent.tasks || []) {
    if (!task?.id || !task.userId || !task.accountId || !task.peerId || !task.messageId) continue;
    if (task.status === "cancelled") continue;
    const id = downloadTaskId(task.userId, task.accountId, task.peerId, task.messageId);
    const completedStat = task.status === "completed" && task.filePath ? await fs.stat(task.filePath).catch(() => null) : null;
    const completedFileExists = completedStat && fileSizeComplete(completedStat.size, task.size);
    if (completedStat && !completedFileExists && task.partPath) {
      await fs.move(task.filePath, task.partPath, { overwrite: true }).catch(() => {});
    }
    const partPath = `${task.filePath}.part`;
    if (task.partPath && task.partPath !== partPath && await fs.pathExists(task.partPath) && !(await fs.pathExists(partPath))) {
      await fs.move(task.partPath, partPath, { overwrite: true }).catch(() => {});
    }
    const shouldResume = !["completed", "cancelled"].includes(task.status);
    const next = {
      ...task,
      id,
      cancelToken: null,
      partPath,
      order: Number(task.order || (task.createdAt ? Date.parse(task.createdAt) : 0) || nextSilentOrder()),
      source: "auto",
      autoCache: true,
      kind: task.kind || "video",
      retryCount: Number(task.retryCount || 0),
      lastProgressAt: task.lastProgressAt || task.updatedAt || new Date().toISOString(),
      lastObservedAt: task.lastObservedAt || task.lastProgressAt || task.updatedAt || new Date().toISOString(),
      lastObservedPartSize: Number(task.lastObservedPartSize || task.downloaded || 0),
      retryAfter: 0,
      speedBps: 0,
      downloaded: completedFileExists ? completedStat.size : Number(task.downloaded || 0),
      status: completedFileExists ? "completed" : shouldResume || task.status === "completed" ? "queued" : task.status,
      error: completedFileExists ? "" : task.status === "completed" ? "本地缓存文件不完整，已等待续传" : "",
      updatedAt: new Date().toISOString()
    };
    const existing = downloadTasks.get(next.id);
    if (existing) {
      existing.autoCache = true;
      existing.source = existing.source || "auto";
      existing.order = Number(existing.order || next.order || nextDownloadOrder());
    } else {
      downloadTasks.set(next.id, next);
    }
  }
  schedulePersistDownloads();
  schedulePersistSilent();
}

async function restoreBackgroundTasks(io) {
  realtimeIo = io;
  await loadPersistentTasks();
  for (const task of downloadTasks.values()) {
    if (task.status !== "completed") {
      task.status = "queued";
      task.speedBps = 0;
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
    }
  }
  pumpUnifiedDownloadQueue(io);
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

function videoDimensions(message) {
  const attr = message.document?.attributes?.find?.((item) => (
    item.className === "DocumentAttributeVideo" || item.constructor?.name === "DocumentAttributeVideo"
  ));
  return {
    width: Number(attr?.w || 0),
    height: Number(attr?.h || 0),
    duration: Number(attr?.duration || 0)
  };
}

function withTimeout(promise, ms, message) {
  let timer;
  return Promise.race([
    promise.finally(() => clearTimeout(timer)),
    new Promise((_resolve, reject) => {
      timer = setTimeout(() => reject(Object.assign(new Error(message), { status: 504 })), ms);
    })
  ]);
}

async function foregroundTelegramOperation(accountId, operation) {
  markForegroundAccountOp(accountId, 1);
  suspendSilentCacheForAccount(accountId, realtimeIo);
  try {
    return await operation();
  } finally {
    markForegroundAccountOp(accountId, -1);
    setTimeout(() => pumpSilentCacheQueue(realtimeIo), 3000).unref?.();
  }
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

function peerIdFromPeer(peer) {
  if (!peer) return "";
  const klass = peer.className || peer.constructor?.name || "";
  const id = peer.userId || peer.chatId || peer.channelId || peer.id;
  if (!id) return "";
  if (klass.includes("User")) return `User:${toText(id)}`;
  if (klass.includes("Channel")) return `Channel:${toText(id)}`;
  if (klass.includes("Chat")) return `Chat:${toText(id)}`;
  return "";
}

function peerFromId(peerId) {
  const [type, rawId] = String(peerId || "").split(":");
  if (!type || !rawId || !/^-?\d+$/.test(rawId)) return null;
  const id = bigInt(rawId);
  if (type === "User") return new Api.PeerUser({ userId: id });
  if (type === "Chat") return new Api.PeerChat({ chatId: id });
  if (type === "Channel") return new Api.PeerChannel({ channelId: id });
  return null;
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

function serializeSender(entity, fallbackId = "") {
  if (!entity) {
    return {
      id: fallbackId,
      title: fallbackId || "Unknown",
      username: "",
      rawId: fallbackId
    };
  }
  const chat = serializeEntity(entity);
  return {
    id: chat.id,
    title: chat.title,
    username: chat.username,
    rawId: chat.rawId
  };
}

async function filterPeerToChat(client, peer) {
  const entity = await client.getEntity(peer).catch(() => null);
  if (!entity) return null;
  return serializeEntity(entity);
}

async function serializeDialogFilter(client, filter) {
  const id = Number(filter.id || 0);
  const title = filter.title?.text || filter.title || "";
  if (!id || !title) return null;
  const includePeers = await Promise.all((filter.includePeers || []).map((peer) => filterPeerToChat(client, peer)));
  const pinnedPeers = await Promise.all((filter.pinnedPeers || []).map((peer) => filterPeerToChat(client, peer)));
  const excludePeers = await Promise.all((filter.excludePeers || []).map((peer) => filterPeerToChat(client, peer)));
  return {
    id,
    title: String(title),
    emoticon: filter.emoticon || "",
    includePeerIds: includePeers.filter(Boolean).map((chat) => chat.id),
    pinnedPeerIds: pinnedPeers.filter(Boolean).map((chat) => chat.id),
    excludePeerIds: excludePeers.filter(Boolean).map((chat) => chat.id),
    flags: {
      contacts: Boolean(filter.contacts),
      nonContacts: Boolean(filter.nonContacts),
      groups: Boolean(filter.groups),
      broadcasts: Boolean(filter.broadcasts),
      bots: Boolean(filter.bots),
      excludeMuted: Boolean(filter.excludeMuted),
      excludeRead: Boolean(filter.excludeRead),
      excludeArchived: Boolean(filter.excludeArchived)
    }
  };
}

function serializeDialogFilterShallow(filter) {
  const id = Number(filter.id || 0);
  const title = filter.title?.text || filter.title || "";
  if (!id || !title) return null;
  const includePeerIds = (filter.includePeers || []).map(peerIdFromPeer).filter(Boolean);
  const pinnedPeerIds = (filter.pinnedPeers || []).map(peerIdFromPeer).filter(Boolean);
  const excludePeerIds = (filter.excludePeers || []).map(peerIdFromPeer).filter(Boolean);
  return {
    id,
    title: String(title),
    emoticon: filter.emoticon || "",
    includePeerIds,
    pinnedPeerIds,
    excludePeerIds,
    flags: {
      contacts: Boolean(filter.contacts),
      nonContacts: Boolean(filter.nonContacts),
      groups: Boolean(filter.groups),
      broadcasts: Boolean(filter.broadcasts),
      bots: Boolean(filter.bots),
      excludeMuted: Boolean(filter.excludeMuted),
      excludeRead: Boolean(filter.excludeRead),
      excludeArchived: Boolean(filter.excludeArchived)
    }
  };
}

function normalizeDialogFilters(result) {
  const filters = Array.isArray(result) ? result : result?.filters || [];
  return filters.filter((filter) => {
    const klass = filter.className || filter.constructor?.name || "";
    return klass !== "DialogFilterDefault";
  });
}

function dialogMuted(dialog) {
  const muteUntil = Number(dialog.notifySettings?.muteUntil || dialog.dialog?.notifySettings?.muteUntil || 0);
  return muteUntil > Math.floor(Date.now() / 1000);
}

function filterMatchesChat(filter, chat, dialog) {
  const include = new Set([...(filter.includePeerIds || []), ...(filter.pinnedPeerIds || [])]);
  const exclude = new Set(filter.excludePeerIds || []);
  const flags = filter.flags || {};
  if (exclude.has(chat.id)) return false;
  if (include.has(chat.id)) return true;
  if ((chat.folderIds || []).some((id) => String(id) === String(filter.id))) return true;
  if (flags.excludeArchived && chat.archived) return false;
  if (flags.excludeMuted && dialogMuted(dialog)) return false;
  if (flags.excludeRead && !chat.unreadCount) return false;
  const entity = dialog.entity || {};
  if (chat.type === "group" && flags.groups) return true;
  if (chat.type === "channel" && flags.broadcasts) return true;
  if (chat.type === "private" && entity.bot && flags.bots) return true;
  if (chat.type === "private" && entity.contact && flags.contacts) return true;
  if (chat.type === "private" && !entity.contact && !entity.bot && flags.nonContacts) return true;
  return false;
}

function messageText(message) {
  return message.message || "";
}

function mediaKind(message, mimeType = "") {
  if (message.photo) return "image";
  if (mimeType.startsWith("image/")) return "image";
  if (mimeType.startsWith("video/") || message.video) return "video";
  return "file";
}

function inputDocumentFileLocation(doc) {
  if (!doc) return null;
  return new Api.InputDocumentFileLocation({
    id: doc.id,
    accessHash: doc.accessHash,
    fileReference: doc.fileReference,
    thumbSize: ""
  });
}

function inputFileLocation(message) {
  if (message.document) return inputDocumentFileLocation(message.document);
  return message.photo || message.media;
}

function serializeMessageEntities(message) {
  const text = messageText(message);
  return (message.entities || []).map((entity) => {
    const type = entity.className || entity.constructor?.name || "";
    const offset = Number(entity.offset || 0);
    const length = Number(entity.length || 0);
    const value = text.slice(offset, offset + length);
    let url = "";
    if (entity.url) url = entity.url;
    else if (type === "MessageEntityUrl") url = value;
    else if (type === "MessageEntityMention" && value.startsWith("@")) url = `https://t.me/${value.slice(1)}`;
    else if (type === "MessageEntityMentionName" && entity.userId) url = `tg://user?id=${toText(entity.userId)}`;
    if (!url) return null;
    if (url.startsWith("t.me/")) url = `https://${url}`;
    return { offset, length, url, type };
  }).filter(Boolean);
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

function serializeMessage(message) {
  const mimeType = message.document?.mimeType || "";
  const kind = mediaKind(message, mimeType);
  const media = message.media ? {
    className: message.media.className || message.media.constructor?.name || "Media",
    hasPreview: Boolean(message.photo || message.document),
    mimeType: kind === "image" && !mimeType ? "image/jpeg" : mimeType,
    kind,
    ...videoDimensions(message),
    fileName: message.file?.name || message.document?.attributes?.find?.((a) => a.fileName)?.fileName || "",
    size: toText(message.file?.size || message.document?.size || "")
  } : null;

  const buttons = message.replyMarkup?.rows?.map((row) => (
    row.buttons.map((button) => ({
      text: button.text || "",
      url: button.url || "",
      data: button.data ? Buffer.from(button.data).toString("base64") : "",
      type: button.url ? "url" : button.data ? "callback" : "unsupported"
    }))
  )).filter((row) => row.length) || [];

  return {
    id: message.id,
    date: message.date ? new Date(message.date * 1000).toISOString() : "",
    text: messageText(message),
    entities: serializeMessageEntities(message),
    outgoing: Boolean(message.out),
    senderId: toText(message.senderId),
    sender: serializeSender(message.sender, toText(message.senderId)),
    groupedId: toText(message.groupedId),
    buttons,
    media
  };
}

function serializeMediaResource(message) {
  const item = serializeMessage(message);
  return {
    id: item.id,
    date: item.date,
    text: item.text,
    fileName: item.media?.fileName || item.media?.mimeType || "Telegram 媒体",
    kind: item.media?.kind || "file",
    size: item.media?.size || "",
    width: item.media?.width || 0,
    height: item.media?.height || 0,
    duration: item.media?.duration || 0
  };
}

function rememberMessageSenders(accountId, messages) {
  if (!peerCache.has(accountId)) peerCache.set(accountId, new Map());
  const cache = peerCache.get(accountId);
  messages.forEach((message) => {
    if (message.sender) cache.set(peerKey(message.sender), message.sender);
  });
}

async function createClient(sessionString = "") {
  const { apiId, apiHash } = await telegramConfig();
  const client = new TelegramClient(new StringSession(sessionString), apiId, apiHash, {
    connectionRetries: 3
  });
  await withTimeout(client.connect(), 20000, "连接 Telegram 超时，请检查网络");
  return client;
}

async function loadSavedClients(io) {
  realtimeIo = io || realtimeIo;
  const accounts = await readAccounts();
  for (const account of accounts) {
    try {
      await getClient(account.userId, account.id);
    } catch (error) {
      console.warn(`Failed to connect account ${account.label}:`, error.message);
    }
  }
}

function registerUpdates(io, accountId, client) {
  if (client.__feigrameUpdatesRegistered) return;
  client.__feigrameUpdatesRegistered = true;
  client.addEventHandler((event) => {
    const message = event.message;
    if (!message) return;
    io.to(`account:${accountId}`).emit("message:new", {
      accountId,
      message: serializeMessage(message),
      peerId: message.chatId ? toText(message.chatId) : ""
    });
  }, new NewMessage({}));
}

async function listAccounts(userId) {
  const accounts = await readAccounts();
  const owned = accounts.filter((account) => account.userId === userId);
  await Promise.all(owned.map((account) => syncGoNativeAccount(account).catch(() => null)));
  return owned.map(({ session, ...safe }) => ({
    ...safe,
    connected: clients.has(safe.id)
  }));
}

async function syncGoNativeAccount(account) {
  if (!account?.userId || !account?.id) return null;
  const { apiId, apiHash } = await telegramConfig();
  return downloaderSidecar.upsertNativeAccount({
    userId: account.userId,
    accountId: account.id,
    phone: account.phone || "",
    displayName: account.label || account.username || account.phone || account.id,
    apiId,
    apiHash,
    status: "needs-relogin"
  });
}

async function getClient(userId, accountId) {
  const lockKey = String(accountId || "");
  const existingLock = clientConnectLocks.get(lockKey);
  if (existingLock) return existingLock;
  const lock = getClientUnlocked(userId, accountId).finally(() => {
    if (clientConnectLocks.get(lockKey) === lock) clientConnectLocks.delete(lockKey);
  });
  clientConnectLocks.set(lockKey, lock);
  return lock;
}

async function getClientUnlocked(userId, accountId) {
  const existing = clients.get(accountId);
  if (existing) {
    try {
      if (existing.connected === false) await existing.connect();
      if (await existing.checkAuthorization()) return existing;
    } catch (error) {
      await resetTelegramClient(accountId);
      if (!isDuplicatedAuthKey(error)) {
        // Fall through and rebuild the client from the saved session.
      } else {
        await sleep(1500);
      }
    }
  }
  const account = (await readAccounts()).find((item) => item.id === accountId && item.userId === userId);
  if (!account) throw Object.assign(new Error("账号不存在"), { status: 404 });
  let client;
  try {
    client = await createClient(await decryptText(account.session));
    if (!(await client.checkAuthorization())) throw Object.assign(new Error("账号登录已失效"), { status: 401 });
  } catch (error) {
    if (isDuplicatedAuthKey(error)) {
      throw Object.assign(new Error("Telegram 检测到账号重复连接。请先安装 2.0.24 并等待服务重启；如果仍出现，请在账号管理里退出后重新登录 Telegram。"), { status: 409 });
    }
    throw error;
  }
  clients.set(accountId, client);
  if (realtimeIo) registerUpdates(realtimeIo, accountId, client);
  return client;
}

async function getCacheClient(userId, accountId) {
  return getClient(userId, accountId);
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

async function completeCode({ loginId, code }, io) {
  const pending = pendingLogins.get(loginId);
  if (!pending) throw Object.assign(new Error("登录流程已过期，请重新发送验证码"), { status: 400 });
  try {
    await pending.client.invoke(new Api.auth.SignIn({
      phoneNumber: pending.phoneNumber,
      phoneCodeHash: pending.phoneCodeHash,
      phoneCode: code
    }));
  } catch (error) {
    if (error.message && error.message.includes("SESSION_PASSWORD_NEEDED")) {
      return { passwordRequired: true };
    }
    throw error;
  }
  return saveLoggedInClient(loginId, io);
}

async function completePassword({ loginId, password }, io) {
  const pending = pendingLogins.get(loginId);
  if (!pending) throw Object.assign(new Error("登录流程已过期，请重新开始"), { status: 400 });
  const result = await downloaderSidecar.authSubmitPassword({ loginId, password });
  if (result.done) return saveLoggedInClient(loginId, io);
  throw Object.assign(new Error("密码提交后登录未完成"), { status: 400 });
}

async function saveLoggedInClient(loginId, io) {
  const pending = pendingLogins.get(loginId);
  const me = await pending.client.getMe();
  const id = safeId("account");
  const account = {
    id,
    userId: pending.userId,
    label: pending.label,
    phoneNumber: pending.phoneNumber,
    displayName: [me.firstName, me.lastName].filter(Boolean).join(" ") || me.username || pending.label,
    username: me.username || "",
    rawUserId: toText(me.id),
    session: await encryptText(pending.client.session.save()),
    createdAt: new Date().toISOString()
  };
  await upsertAccount(account);
  clients.set(id, pending.client);
  registerUpdates(io, id, pending.client);
  pendingLogins.delete(loginId);
  return { account: { ...account, session: undefined, connected: true } };
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
}

async function listChats(userId, accountId, query = "") {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const dialogs = await withTimeout(
    client.getDialogs({ limit: DIALOG_FETCH_LIMIT }),
    20000,
    "连接 Telegram 超时，请检查网络或稍后重试"
  );
  const accountPeers = new Map();
  const normalizedQuery = query.trim().toLowerCase();
  const items = dialogs
    .map((dialog) => {
      const entity = dialog.entity;
      const chat = serializeEntity(entity);
      accountPeers.set(chat.id, entity);
      return {
        ...chat,
        avatarKey: chat.id,
        folderId: Number(dialog.folderId || 0),
        folderIds: Number(dialog.folderId || 0) ? [Number(dialog.folderId)] : [],
        unreadCount: dialog.unreadCount || 0,
        pinned: Boolean(dialog.pinned),
        archived: Boolean(dialog.archived),
        lastMessage: dialog.message ? serializeMessage(dialog.message) : null
      };
    })
    .filter((chat) => !normalizedQuery || chat.title.toLowerCase().includes(normalizedQuery) || chat.username.toLowerCase().includes(normalizedQuery));
  peerCache.set(accountId, accountPeers);
  return items;
  });
}

async function listFolders(userId, accountId) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const [filterResult, dialogs] = await Promise.all([
    withTimeout(client.invoke(new Api.messages.GetDialogFilters()), 15000, "获取 Telegram 分组超时").catch(() => []),
    withTimeout(client.getDialogs({ limit: DIALOG_FETCH_LIMIT }), 20000, "获取 Telegram 会话超时").catch(() => [])
  ]);
  const accountPeers = new Map();
  const chatsById = new Map();
  const chatDialogs = [];
  dialogs.forEach((dialog) => {
    if (!dialog.entity) return;
    const chat = serializeEntity(dialog.entity);
    const folderId = Number(dialog.folderId || 0);
    const item = {
      ...chat,
      folderId,
      folderIds: folderId ? [folderId] : [],
      unreadCount: dialog.unreadCount || 0,
      pinned: Boolean(dialog.pinned),
      archived: Boolean(dialog.archived),
      lastMessage: dialog.message ? serializeMessage(dialog.message) : null
    };
    accountPeers.set(chat.id, dialog.entity);
    chatsById.set(chat.id, item);
    chatDialogs.push({ chat: item, dialog });
  });
  if (accountPeers.size) peerCache.set(accountId, accountPeers);

  const filters = normalizeDialogFilters(filterResult);
  const shallow = filters.map(serializeDialogFilterShallow).filter(Boolean).map((filter) => ({
    ...filter,
    chatIds: chatDialogs
      .filter(({ chat, dialog }) => filterMatchesChat(filter, chat, dialog))
      .map(({ chat }) => chat.id)
  })).filter((filter) => filter.chatIds.length || filter.includePeerIds.length || filter.pinnedPeerIds.length);
  if (shallow.length) {
    return shallow;
  }
  const folderIds = [...new Set(dialogs.map((dialog) => Number(dialog.folderId || 0)).filter(Boolean))];
  return folderIds.map((id) => ({
    id,
    title: id === 1 ? "归档" : `文件夹 ${id}`,
    emoticon: "",
    includePeerIds: [],
    pinnedPeerIds: [],
    excludePeerIds: [],
    flags: {},
    chatIds: [...chatsById.values()].filter((chat) => Number(chat.folderId || 0) === id).map((chat) => chat.id)
  }));
  });
}

async function resolvePeer(userId, accountId, peerId) {
  const cached = peerCache.get(accountId)?.get(peerId);
  if (cached) return cached;

  const client = await getClient(userId, accountId);
  const directPeer = peerFromId(peerId);
  const directEntity = directPeer ? await client.getEntity(directPeer).catch(() => null) : null;
  if (directEntity) {
    if (!peerCache.has(accountId)) peerCache.set(accountId, new Map());
    peerCache.get(accountId).set(peerId, directEntity);
    return directEntity;
  }

  await listChats(userId, accountId);
  const listed = peerCache.get(accountId)?.get(peerId);
  if (listed) return listed;

  throw Object.assign(new Error("找不到会话，可能已退出该群组、会话已被删除，或 Telegram 暂时无法解析该会话"), { status: 404 });
}

async function listMessages(userId, accountId, peerId, limit = 50, before = 0, around = 0) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const request = { limit: Number(limit) || 50 };
  if (Number(around) > 0) {
    request.offsetId = Number(around);
    request.addOffset = -Math.floor(request.limit / 2);
  } else if (Number(before) > 0) {
    request.offsetId = Number(before);
  }
  const messages = await withTimeout(
    client.getMessages(entity, request),
    20000,
    "连接 Telegram 超时，请检查网络或稍后重试"
  );
  if (Number(around) > 0 && !messages.some((message) => Number(message.id) === Number(around))) {
    const [target] = await withTimeout(client.getMessages(entity, { ids: [Number(around)] }), 12000, "定位消息超时").catch(() => []);
    if (target) messages.push(target);
  }
  rememberMessageSenders(accountId, messages);
  return messages
    .map(serializeMessage)
    .sort((a, b) => Number(a.id) - Number(b.id));
  });
}

async function chatDetails(userId, accountId, peerId) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const chat = serializeEntity(entity);
  let full = null;
  try {
    if (chat.type === "channel" || chat.type === "group") {
      full = await client.invoke(new Api.channels.GetFullChannel({ channel: entity }));
    } else if ((entity.className || entity.constructor?.name || "") === "Chat") {
      full = await client.invoke(new Api.messages.GetFullChat({ chatId: entity.id }));
    } else {
      full = await client.invoke(new Api.users.GetFullUser({ id: entity }));
    }
  } catch {}
  const fullChat = full?.fullChat || full?.fullUser || {};
  const mediaPage = await chatMedia(userId, accountId, peerId, { limit: 30 });
  const mediaMessages = mediaPage.files;
  return {
    ...chat,
    about: fullChat.about || "",
    participantsCount: Number(fullChat.participantsCount || fullChat.membersCount || entity.participantsCount || 0),
    mediaSummary: {
      images: mediaMessages.filter((message) => message.kind === "image").length,
      videos: mediaMessages.filter((message) => message.kind === "video").length,
      files: mediaMessages.filter((message) => message.kind === "file").length
    },
    files: mediaMessages,
    nextMediaBefore: mediaPage.nextBefore,
    hasMoreMedia: mediaPage.hasMore
  };
  });
}

async function chatMedia(userId, accountId, peerId, { before = 0, limit = 30 } = {}) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const pageSize = Math.max(1, Math.min(60, Number(limit) || 30));
  const requestLimit = Math.min(200, pageSize * 4);
  const request = { limit: requestLimit };
  if (Number(before) > 0) request.offsetId = Number(before);
  const messages = await withTimeout(client.getMessages(entity, request), 20000, "获取媒体文件超时").catch(() => []);
  const mediaMessages = messages.filter((message) => message.media);
  const page = mediaMessages.slice(0, pageSize);
  return {
    files: page.map(serializeMediaResource),
    nextBefore: page.length ? Math.min(...page.map((message) => Number(message.id))) : 0,
    hasMore: messages.length >= requestLimit
  };
  });
}

async function sendText(userId, accountId, peerId, text) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const message = await withTimeout(client.sendMessage(entity, { message: text }), 15000, "发送消息超时，请稍后再试");
  rememberMessageSenders(accountId, [message]);
  return serializeMessage(message);
  });
}

async function clickMessageButton(userId, accountId, peerId, messageId, data) {
  return foregroundTelegramOperation(accountId, async () => {
  if (!data) throw Object.assign(new Error("这个按钮暂不支持点击"), { status: 400 });
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  let answer;
  try {
    answer = await withTimeout(client.invoke(new Api.messages.GetBotCallbackAnswer({
      peer: entity,
      msgId: Number(messageId),
      data: Buffer.from(data, "base64")
    })), 15000, "机器人响应超时，请稍后再试");
  } catch (error) {
    if (String(error.message || "").includes("TIMEOUT")) {
      throw Object.assign(new Error("机器人响应超时，请稍后再试"), { status: 504 });
    }
    throw error;
  }
  return {
    message: answer.message || "",
    url: answer.url || "",
    alert: Boolean(answer.alert)
  };
  });
}

async function resolveTelegramLink(userId, accountId, url) {
  return foregroundTelegramOperation(accountId, async () => {
  const domain = linkDomain(url);
  const client = await getClient(userId, accountId);
  if (!peerCache.has(accountId)) await listChats(userId, accountId);
  const privatePeerId = linkPrivatePeerId(url);
  let entity = null;
  if (privatePeerId) entity = peerCache.get(accountId)?.get(privatePeerId) || null;
  if (!entity && domain) entity = await client.getEntity(domain);
  if (!entity) throw Object.assign(new Error("这个链接暂不支持在客户端内打开，或你当前账号无权访问"), { status: 400 });
  const chat = serializeEntity(entity);
  if (!peerCache.has(accountId)) peerCache.set(accountId, new Map());
  peerCache.get(accountId).set(chat.id, entity);
  return { ...chat, avatarKey: chat.id, messageId: linkMessageId(url) };
  });
}

async function search(userId, accountId, query) {
  return foregroundTelegramOperation(accountId, async () => {
  const client = await getClient(userId, accountId);
  const dialogs = await listChats(userId, accountId, query);
  const global = query
    ? await client.getMessages(undefined, { search: query, limit: 30 }).catch(() => [])
    : [];
  return {
    chats: dialogs,
    messages: global.map(serializeMessage)
  };
  });
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

function nextDownloadOrder() {
  return Math.max(0, ...[...downloadTasks.values()].map((task) => Number(task.order || 0))) + 1;
}

function silentDedupKey(userId, accountId, peerId, fileName, size) {
  return [userId, accountId, peerId, String(fileName || "").trim().toLowerCase(), Number(size || 0)].join("|");
}

function isAutoDownloadTask(task) {
  return task?.source === "auto" || task?.autoCache === true;
}

function activeUnifiedDownloads(userId = "") {
  return [...downloadTasks.values()].filter((task) => (
    task.status === "downloading" &&
    (!userId || task.userId === userId)
  ));
}

function canRunUnifiedTask(task, running) {
  if (!task || task.status !== "queued") return false;
  if (isAutoDownloadTask(task) && !silentCacheEnabled) return false;
  if (task.retryAfter && Date.now() < Number(task.retryAfter)) return false;
  const sameAccountRequeueing = [...downloadTasks.values()].some((item) => (
    item.id !== task.id &&
    item.userId === task.userId &&
    item.accountId === task.accountId &&
    item.cancelToken?.requeue
  ));
  if (sameAccountRequeueing) return false;
  return !running.some((item) => item.userId === task.userId && item.accountId === task.accountId);
}

function pumpUnifiedDownloadQueue(io = realtimeIo, userId = "") {
  monitorDownloadCompletions(io);
  const limit = effectiveSilentConcurrency(userId);
  const sorted = [...downloadTasks.values()]
    .filter((task) => task.status === "queued" && (!userId || task.userId === userId) && (!task.retryAfter || Date.now() >= Number(task.retryAfter)))
    .sort((a, b) => {
      const orderDiff = Number(a.order || 0) - Number(b.order || 0);
      if (orderDiff) return orderDiff;
      return String(a.createdAt || a.updatedAt).localeCompare(String(b.createdAt || b.updatedAt));
    });
  let running = activeUnifiedDownloads(userId);
  for (const task of sorted) {
    if (running.length >= limit) break;
    if (!canRunUnifiedTask(task, running)) continue;
    runDownloadTask(task, io).catch(() => {});
    running = [...running, task];
  }
}

function serializeDownloadTask(task) {
  return {
    id: task.id,
    accountId: task.accountId,
    peerId: task.peerId,
    messageId: task.messageId,
    fileName: task.fileName,
    kind: task.kind,
    contentType: task.contentType,
    status: task.status,
    size: task.size,
    downloaded: task.downloaded,
    speedBps: task.speedBps,
    source: task.source || "manual",
    autoCache: Boolean(task.autoCache),
    order: Number(task.order || 0),
    error: task.error || "",
    createdAt: task.createdAt,
    updatedAt: task.updatedAt,
    inlineUrl: task.status === "completed"
      ? `/api/media/${task.accountId}/${encodeURIComponent(task.peerId)}/${task.messageId}?inline=1`
      : ""
  };
}

function emitDownloadTask(io, task) {
  if (io) {
    io.to(`user:${task.userId}`).emit("download:update", serializeDownloadTask(task));
    io.to(`user:${task.userId}`).emit("silent-cache:update", serializeSilentCacheTask(task));
  }
  schedulePersistDownloads();
  schedulePersistSilent();
}

function serializeSilentCacheTask(task) {
  if (downloadTasks.has(task?.id) || isAutoDownloadTask(task)) {
    return {
      id: task.id,
      accountId: task.accountId,
      peerId: task.peerId,
      messageId: task.messageId,
      fileName: task.fileName,
      contentType: task.contentType,
      size: task.size,
      downloaded: task.downloaded || 0,
      speedBps: task.speedBps || 0,
      order: task.order || 0,
      status: task.status === "downloading" ? "running" : task.status,
      error: task.error || "",
      source: task.source || "manual",
      createdAt: task.createdAt,
      lastProgressAt: task.lastProgressAt || task.updatedAt || "",
      lastObservedAt: task.lastObservedAt || task.updatedAt || "",
      updatedAt: task.updatedAt
    };
  }
  return {
    id: task.id,
    accountId: task.accountId,
    peerId: task.peerId,
    messageId: task.messageId,
    fileName: task.fileName,
    contentType: task.contentType,
    size: task.size,
    downloaded: task.downloaded || 0,
    speedBps: task.speedBps || 0,
    order: task.order || 0,
    status: task.status,
    error: task.error || "",
    createdAt: task.createdAt,
    lastProgressAt: task.lastProgressAt || "",
    lastObservedAt: task.lastObservedAt || "",
    updatedAt: task.updatedAt
  };
}

function serializeSilentCacheState(userId) {
  const tasks = [...downloadTasks.values()].filter((task) => task.userId === userId && task.status !== "cancelled");
  const running = tasks.filter((task) => task.status === "downloading").length;
  const configuredConcurrency = silentConcurrencyLimit();
  const effectiveConcurrency = normalizedSilentCacheMode(silentCacheMode) === "fast"
    ? configuredConcurrency
    : Math.max(1, Math.min(configuredConcurrency, new Set(tasks.filter((task) => !["completed", "cancelled"].includes(task.status)).map((task) => task.accountId)).size || 1));
  return {
    enabled: silentCacheEnabled,
    rateLimitBps: silentCacheRateLimitBps,
    concurrency: configuredConcurrency,
    configuredConcurrency,
    effectiveConcurrency,
    mode: normalizedSilentCacheMode(silentCacheMode),
    running,
    tasks: listSilentCacheTasks(userId)
  };
}

function emitSilentCacheTask(io, task) {
  const target = io || realtimeIo;
  if (target) target.to(`user:${task.userId}`).emit("silent-cache:update", serializeSilentCacheTask(task));
  schedulePersistSilent();
}

function emitSilentCacheDelete(io, task) {
  const target = io || realtimeIo;
  if (target) target.to(`user:${task.userId}`).emit("silent-cache:delete", { id: task.id });
  schedulePersistSilent();
}

async function mediaMessage(userId, accountId, peerId, messageId) {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const [message] = await client.getMessages(entity, { ids: [Number(messageId)] });
  if (!message || !message.media) throw Object.assign(new Error("这条消息没有可下载媒体"), { status: 404 });
  return { client, entity, message };
}

function silentPartSizeKb() {
  const rate = Number(silentCacheRateLimitBps || 0);
  if (!rate) return 512;
  const perTaskRate = rate / effectiveSilentConcurrency();
  if (perTaskRate >= 768 * 1024) return 512;
  if (perTaskRate >= 256 * 1024) return 256;
  if (perTaskRate >= 128 * 1024) return 128;
  return 64;
}

async function readTelegramDocumentChunks(client, doc, offset, bytesToRead, onChunk) {
  let currentOffset = Math.max(0, Number(offset || 0));
  let remaining = Math.max(0, Number(bytesToRead || 0));
  const fileLocation = inputDocumentFileLocation(doc);
  if (!fileLocation) throw new Error("无法定位 Telegram 文档文件");
  const requestSize = MAX_TELEGRAM_CHUNK_SIZE;
  const metrics = {
    requestedChunkSize: requestSize,
    effectiveChunkSize: requestSize,
    fallbackCount: 0,
    limitInvalidCount: 0,
    dcId: doc.dcId
  };
  const limit = Math.ceil(remaining / requestSize);
  for await (const chunk of client.iterDownload({
    file: fileLocation,
    offset: bigInt(currentOffset),
    requestSize,
    chunkSize: requestSize,
    fileSize: bigInt(currentOffset + remaining),
    limit,
    dcId: doc.dcId
  })) {
    if (!chunk?.length) break;
    await onChunk(chunk, currentOffset, metrics);
    currentOffset += chunk.length;
    remaining -= chunk.length;
    if (remaining <= 0 || chunk.length < requestSize) break;
  }
  metrics.bytesRead = currentOffset - Math.max(0, Number(offset || 0));
  return metrics;
}

async function downloadSilentMedia(client, entity, message, outputFile, progressCallback) {
  if (message.document) {
    const doc = message.document;
    const total = Number(doc.size?.toString?.() || doc.size || message.file?.size || 0);
    const stat = await fs.stat(outputFile).catch(() => null);
    let downloaded = Math.max(0, Number(stat?.size || 0));
    if (total && downloaded > total) {
      await fs.remove(outputFile).catch(() => {});
      downloaded = 0;
    }
    if (downloaded % TELEGRAM_MIN_CHUNK_SIZE !== 0) {
      downloaded -= downloaded % TELEGRAM_MIN_CHUNK_SIZE;
      await fs.truncate(outputFile, downloaded).catch(() => {});
    }
    if (total && downloaded >= total) {
      await progressCallback(bigInt(downloaded), bigInt(total));
      return;
    }
    const writer = fs.createWriteStream(outputFile, { flags: downloaded > 0 ? "a" : "w" });
    try {
      const bytesToRead = total ? total - downloaded : Number.MAX_SAFE_INTEGER;
      await readTelegramDocumentChunks(client, doc, downloaded, bytesToRead, async (chunk) => {
        await new Promise((resolve, reject) => {
          writer.write(chunk, (error) => error ? reject(error) : resolve());
        });
        downloaded += chunk.length;
        await progressCallback(bigInt(downloaded), bigInt(total || downloaded));
      });
    } finally {
      await new Promise((resolve) => writer.end(resolve));
    }
    return;
  }
  await client.downloadMedia(message, { outputFile, progressCallback });
}

async function ensureDownloadTask(userId, accountId, peerId, messageId, options = {}) {
  const id = downloadTaskId(userId, accountId, peerId, messageId);
  const existing = downloadTasks.get(id);
  if (existing) {
    if (options.source === "auto" || options.autoCache) {
      existing.autoCache = true;
      existing.source = existing.source || "auto";
      existing.order = Number(existing.order || options.order || nextDownloadOrder());
      existing.dedupKey = existing.dedupKey || options.dedupKey || "";
      existing.updatedAt = new Date().toISOString();
      emitDownloadTask(realtimeIo, existing);
    }
    return existing;
  }
  const { message } = await mediaMessage(userId, accountId, peerId, messageId);
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = Number(message.file?.size || message.document?.size || 0);
  const { fileName, filePath, downloadDir } = await mediaFileInfo(userId, accountId, message, contentType, kind);
  const task = {
    id,
    userId,
    accountId,
    peerId,
    messageId: Number(messageId),
    fileName,
    filePath,
    partPath: `${filePath}.part`,
    downloadDir,
    kind,
    contentType: contentType || mime.lookup(fileName) || "application/octet-stream",
    size,
    source: options.source || "manual",
    autoCache: Boolean(options.autoCache || options.source === "auto"),
    dedupKey: options.dedupKey || "",
    order: Number(options.order || 0) || nextDownloadOrder(),
    downloaded: 0,
    speedBps: 0,
    retryCount: 0,
    retryAfter: 0,
    lastProgressAt: "",
    lastObservedAt: "",
    lastObservedPartSize: 0,
    status: "queued",
    error: "",
    cancelToken: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString()
  };
  downloadTasks.set(id, task);
  schedulePersistDownloads();
  return task;
}

async function startDownloadTask(userId, accountId, peerId, messageId, io, options = {}) {
  const task = await ensureDownloadTask(userId, accountId, peerId, messageId, options);
  if (task.userId !== userId) throw Object.assign(new Error("无权访问这个下载任务"), { status: 403 });
  if (task.status === "downloading") return serializeDownloadTask(task);
  if (await fs.pathExists(task.filePath)) {
    const stat = await fs.stat(task.filePath);
    task.downloaded = stat.size;
    task.size = task.size || stat.size;
    if (fileSizeComplete(stat.size, task.size)) {
      task.status = "completed";
      task.speedBps = 0;
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
      return serializeDownloadTask(task);
    }
    await fs.move(task.filePath, task.partPath, { overwrite: true }).catch(() => {});
    task.status = "queued";
    task.error = "本地文件不完整，已转为续传";
  }
  const partStat = await fs.stat(task.partPath).catch(() => null);
  if (partStat) task.downloaded = partStat.size;
  task.status = "queued";
  task.error = "";
  task.updatedAt = new Date().toISOString();
  emitDownloadTask(io, task);
  pumpUnifiedDownloadQueue(io, userId);
  return serializeDownloadTask(task);
}

async function runDownloadTask(task, io) {
  if (task.status === "downloading") return;
  const cancelToken = { cancelled: false, paused: false, requeue: false };
  task.cancelToken = cancelToken;
  task.status = "downloading";
  task.error = "";
  task.retryAfter = 0;
  task.updatedAt = new Date().toISOString();
  emitDownloadTask(io, task);
  try {
    const { client, message } = await mediaMessage(task.userId, task.accountId, task.peerId, task.messageId);
    await fs.ensureDir(task.downloadDir);
    if (await fs.pathExists(task.filePath)) {
      const stat = await fs.stat(task.filePath);
      task.downloaded = stat.size;
      task.size = task.size || stat.size;
      if (fileSizeComplete(stat.size, task.size)) {
        task.status = "completed";
        task.updatedAt = new Date().toISOString();
        emitDownloadTask(io, task);
        return;
      }
      await fs.move(task.filePath, task.partPath, { overwrite: true }).catch(() => {});
    }
    const total = task.size || Number(message.file?.size || message.document?.size || 0);
    task.size = total;
    let lastBytes = 0;
    let lastTick = Date.now();
    const partStat = await fs.stat(task.partPath).catch(() => null);
    task.downloaded = partStat?.size || 0;
    task.lastObservedPartSize = task.downloaded;
    task.lastObservedAt = new Date().toISOString();
    lastBytes = task.downloaded;
    const progressCallback = async (downloadedValue, totalValue) => {
      if (cancelToken.cancelled) {
        throw Object.assign(new Error(cancelToken.requeue ? "下载连接已重排" : "下载已取消"), { status: 499, requeue: Boolean(cancelToken.requeue) });
      }
      const downloaded = Number(downloadedValue?.toString?.() || downloadedValue || 0);
      const fullSize = Number(totalValue?.toString?.() || totalValue || total || 0);
      task.downloaded = downloaded;
      task.size = fullSize || task.size;
      const now = Date.now();
      if (now - lastTick >= 800 || downloaded >= fullSize) {
        task.speedBps = Math.max(0, Math.round((downloaded - lastBytes) / Math.max(0.001, (now - lastTick) / 1000)));
        const progressAt = new Date().toISOString();
        task.updatedAt = progressAt;
        task.lastProgressAt = progressAt;
        if (downloaded > lastBytes) {
          task.lastObservedAt = progressAt;
          task.lastObservedPartSize = downloaded;
          task.retryCount = 0;
          task.retryAfter = 0;
        }
        lastTick = now;
        lastBytes = downloaded;
        emitDownloadTask(io, task);
      }
    };
    await downloadSilentMedia(client, null, message, task.partPath, progressCallback);
    if (cancelToken.cancelled) return;
    const partDone = await fs.stat(task.partPath).catch(() => null);
    if (!fileSizeComplete(partDone?.size, task.size)) {
      task.downloaded = Number(partDone?.size || task.downloaded || 0);
      task.status = "queued";
      task.error = `文件尚未完整，已保留断点等待续传（${formatByteCount(task.downloaded)} / ${formatByteCount(task.size)}）`;
      task.speedBps = 0;
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
      setTimeout(() => {
        if (downloadTasks.get(task.id)?.status === "queued") pumpUnifiedDownloadQueue(io, task.userId);
      }, 3000).unref?.();
      return;
    }
    await fs.move(task.partPath, task.filePath, { overwrite: true });
    const stat = await fs.stat(task.filePath);
    task.downloaded = stat.size;
    task.size = task.size || stat.size;
    if (!fileSizeComplete(task.downloaded, task.size)) {
      task.status = "queued";
      task.error = "文件校验未完成，已等待续传";
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
      return;
    }
    task.speedBps = 0;
    task.status = "completed";
    task.updatedAt = new Date().toISOString();
    emitDownloadTask(io, task);
  } catch (error) {
    if (error.status === 499 && error.requeue) {
      task.status = "queued";
      task.error = "下载连接停滞，已重新排队续传";
      task.speedBps = 0;
      task.retryAfter = Date.now() + 3000;
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
      return;
    }
    if (error.status !== 499 && isTransientDownloadError(error) && Number(task.retryCount || 0) < DOWNLOAD_RETRY_LIMIT) {
      task.retryCount = Number(task.retryCount || 0) + 1;
      task.status = "queued";
      task.error = `网络波动，自动重试 ${task.retryCount}/${DOWNLOAD_RETRY_LIMIT}`;
      task.speedBps = 0;
      task.retryAfter = Date.now() + Math.min(30000, 3000 * task.retryCount);
      task.updatedAt = new Date().toISOString();
      emitDownloadTask(io, task);
      setTimeout(() => {
        if (downloadTasks.get(task.id)?.status === "queued") pumpUnifiedDownloadQueue(io, task.userId);
      }, Math.min(30000, 3000 * task.retryCount)).unref?.();
      return;
    }
    task.status = error.status === 499 ? "cancelled" : "error";
    task.error = error.status === 499 ? "" : error.message || "下载失败";
    task.speedBps = 0;
    task.retryAfter = isTransientDownloadError(error) ? Date.now() + SILENT_RETRY_DELAY_MS : 0;
    task.updatedAt = new Date().toISOString();
    emitDownloadTask(io, task);
  } finally {
    task.cancelToken = null;
    pumpUnifiedDownloadQueue(io, task.userId);
  }
}

async function cacheVideoSilently(userId, accountId, peerId, message) {
  const contentType = message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = Number(message.file?.size || message.document?.size || 0);
  if (kind !== "video" || size <= VIDEO_CACHE_THRESHOLD) return false;
  const mediaInfo = await mediaFileInfo(userId, accountId, message, contentType, kind);
  const dedupKey = silentDedupKey(userId, accountId, peerId, mediaInfo.fileName, size);
  const taskId = downloadTaskId(userId, accountId, peerId, message.id);
  const existingTask = downloadTasks.get(taskId);
  if (existingTask && ["queued", "downloading", "completed"].includes(existingTask.status)) {
    existingTask.autoCache = true;
    existingTask.source = existingTask.source || "auto";
    existingTask.dedupKey = existingTask.dedupKey || dedupKey;
    emitDownloadTask(realtimeIo, existingTask);
    return false;
  }
  const duplicate = [...downloadTasks.values()].find((task) => (
    task.userId === userId &&
    task.accountId === accountId &&
    task.peerId === peerId &&
    task.status !== "cancelled" &&
    (task.dedupKey === dedupKey ||
      (!task.dedupKey && silentDedupKey(task.userId, task.accountId, task.peerId, task.fileName, task.size) === dedupKey))
  ));
  if (duplicate && !["cancelled"].includes(duplicate.status)) return false;
  await startDownloadTask(userId, accountId, peerId, message.id, realtimeIo, {
    source: "auto",
    autoCache: true,
    dedupKey,
    order: nextDownloadOrder()
  });
  return true;
}

function pumpSilentCacheQueue(io = realtimeIo) {
  if (!silentCacheEnabled) return;
  syncSilentCacheActive();
  while (silentCacheActive < silentConcurrencyLimit() && silentCacheQueue.length) {
    const record = silentCacheQueue.shift();
    if (!record || silentCacheTasks.has(record.id) || ["completed", "cancelled"].includes(record.status)) continue;
    if (record.retryAfter && Date.now() < Number(record.retryAfter)) {
      enqueueSilentTask(record);
      break;
    }
    if (hasForegroundAccountOp(record.accountId) || (silentCacheMode !== "fast" && hasRunningSilentCacheForAccount(record.accountId))) {
      enqueueSilentTask(record);
      break;
    }
    silentCacheActive += 1;
    const runToken = { detached: false, startedAt: Date.now() };
    record.runToken = runToken;
    record.retryAfter = 0;
    record.status = "running";
    record.error = "";
    record.speedBps = 0;
    record.lowSpeedSince = "";
    record.updatedAt = new Date().toISOString();
    emitSilentCacheTask(io, record);
    const promise = runSilentCacheTask(record, io, runToken)
      .catch((error) => {
        if (isTransientDownloadError(error) && silentCacheRecords.has(record.id)) {
          const fileReferenceExpired = isFileReferenceExpired(error);
          resetTelegramClient(record.accountId).catch(() => {});
          record.retryCount = Number(record.retryCount || 0) + 1;
          record.status = "queued";
          record.error = fileReferenceExpired ? "文件引用过期，已刷新后重试" : "Telegram 连接波动，已重置账号连接并等待续传";
          record.speedBps = 0;
          record.updatedAt = new Date().toISOString();
          emitSilentCacheTask(io, record);
          setTimeout(() => {
            const current = silentCacheRecords.get(record.id);
            if (current && current.status === "queued") {
              enqueueSilentTask(current);
              pumpSilentCacheQueue(io);
            }
          }, fileReferenceExpired ? 3000 : Math.min(60 * 1000, 3000 * Math.min(10, record.retryCount))).unref?.();
          return;
        }
        record.status = "error";
        record.error = error.message || "后台缓存失败";
        record.speedBps = 0;
        record.updatedAt = new Date().toISOString();
        emitSilentCacheTask(io, record);
      })
      .finally(() => {
        if (!runToken.detached) silentCacheActive = Math.max(0, silentCacheActive - 1);
        if (record.runToken === runToken) {
          record.runToken = null;
          silentCacheTasks.delete(record.id);
          if (silentCacheRecords.has(record.id)) emitSilentCacheTask(io, record);
          pumpSilentCacheQueue(io);
        }
      });
    silentCacheTasks.set(record.id, promise);
  }
}

async function runSilentCacheTask(record, io = realtimeIo, runToken = null) {
  if (await fs.pathExists(record.filePath)) {
    const stat = await fs.stat(record.filePath).catch(() => null);
    if (stat) {
      record.downloaded = stat.size;
      record.size = record.size || stat.size;
    }
    if (!fileSizeComplete(record.downloaded, record.size)) {
      await fs.move(record.filePath, record.partPath, { overwrite: true }).catch(() => {});
      record.status = "queued";
      record.error = "本地缓存文件不完整，已转为续传";
      record.speedBps = 0;
      record.updatedAt = new Date().toISOString();
      emitSilentCacheTask(io, record);
      return;
    }
    record.status = "completed";
    record.error = "";
    record.speedBps = 0;
    record.updatedAt = new Date().toISOString();
    emitSilentCacheTask(io, record);
    return;
  }
  const client = await getCacheClient(record.userId, record.accountId);
  const entity = await resolvePeer(record.userId, record.accountId, record.peerId);
  const [message] = await client.getMessages(entity, { ids: [Number(record.messageId)] });
  if (!message || !message.media) throw Object.assign(new Error("这条消息没有可下载媒体"), { status: 404 });
  await fs.ensureDir(record.downloadDir);
  const total = record.size || Number(message.file?.size || message.document?.size || 0);
  record.size = total;
  const partStat = await fs.stat(record.partPath).catch(() => null);
  const existingDownloaded = Math.max(0, Number(partStat?.size || 0));
  if (existingDownloaded) record.downloaded = existingDownloaded;
  record.lastObservedPartSize = existingDownloaded;
  record.lastObservedAt = new Date().toISOString();
  let lastBytes = existingDownloaded;
  let lastTick = Date.now();
  let lastThrottleBytes = existingDownloaded;
  const cancelToken = { cancelled: false, paused: false };
  record.cancelToken = cancelToken;
  const progressCallback = async (downloadedValue, totalValue) => {
    if (cancelToken.cancelled || cancelToken.paused || !silentCacheEnabled) {
      throw Object.assign(new Error(cancelToken.detached ? "后台缓存重排" : cancelToken.cancelled ? "后台缓存已取消" : "后台缓存已暂停"), {
        status: 499,
        paused: cancelToken.paused || !silentCacheEnabled,
        requeue: cancelToken.requeue,
        detached: cancelToken.detached
      });
    }
    const downloaded = Number(downloadedValue?.toString?.() || downloadedValue || 0);
    const fullSize = Number(totalValue?.toString?.() || totalValue || total || 0);
    record.downloaded = downloaded;
    record.size = fullSize || record.size;
    await throttleSilentCache(downloaded - lastThrottleBytes);
    lastThrottleBytes = downloaded;
    if (cancelToken.cancelled || cancelToken.paused || !silentCacheEnabled) {
      throw Object.assign(new Error(cancelToken.detached ? "后台缓存重排" : cancelToken.cancelled ? "后台缓存已取消" : "后台缓存已暂停"), {
        status: 499,
        paused: cancelToken.paused || !silentCacheEnabled,
        requeue: cancelToken.requeue,
        detached: cancelToken.detached
      });
    }
    const now = Date.now();
    if (now - lastTick >= 1000 || downloaded >= fullSize) {
      record.speedBps = Math.max(0, Math.round((downloaded - lastBytes) / Math.max(0.001, (now - lastTick) / 1000)));
      const progressAt = new Date().toISOString();
      record.lastProgressAt = progressAt;
      record.lastObservedPartSize = downloaded;
      record.lastObservedAt = progressAt;
      if (record.speedBps > 0 && record.speedBps < lowSpeedThresholdBps() && downloaded < fullSize) {
        record.lowSpeedSince = record.lowSpeedSince || progressAt;
      } else {
        record.lowSpeedSince = "";
      }
      record.updatedAt = progressAt;
      lastTick = now;
      lastBytes = downloaded;
      emitSilentCacheTask(io, record);
    }
  };
  try {
    await downloadSilentMedia(client, entity, message, record.partPath, progressCallback);
    await fs.move(record.partPath, record.filePath, { overwrite: true });
    const stat = await fs.stat(record.filePath).catch(() => null);
    if (stat) record.downloaded = stat.size;
    record.status = "completed";
    record.error = "";
    record.speedBps = 0;
    record.lastProgressAt = new Date().toISOString();
    record.updatedAt = new Date().toISOString();
    emitSilentCacheTask(io, record);
  } catch (error) {
    if (error.status === 499 && (error.requeue || error.detached)) {
      if (runToken && record.runToken !== runToken) return;
      record.status = "queued";
      record.error = "";
      enqueueSilentTask(record);
    } else if (error.status === 499 && error.paused) {
      if (runToken && record.runToken !== runToken) return;
      if (silentCacheEnabled) {
        record.status = "queued";
        enqueueSilentTask(record);
      } else {
        record.status = "paused";
      }
      record.error = "";
    } else if (error.status === 499) {
      if (!silentCacheRecords.has(record.id)) return;
      if (runToken && record.runToken !== runToken) return;
      record.status = "cancelled";
      record.error = "";
    } else {
      throw error;
    }
    record.speedBps = 0;
    record.updatedAt = new Date().toISOString();
    emitSilentCacheTask(io, record);
  } finally {
    if (!runToken || record.runToken === runToken) record.cancelToken = null;
  }
}

async function cacheLargeVideosInChat(userId, accountId, peerId, io = realtimeIo) {
  realtimeIo = io || realtimeIo;
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const recent = await client.getMessages(entity, { limit: 120 }).catch(() => []);
  let queued = 0;
  for (const message of recent) {
    if (!message?.media) continue;
    if (await cacheVideoSilently(userId, accountId, peerId, message)) queued += 1;
  }
  return { queued };
}

function listDownloadTasks(userId) {
  return [...downloadTasks.values()]
    .filter((task) => task.userId === userId)
    .sort((a, b) => String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt)))
    .map(serializeDownloadTask);
}

function listSilentCacheTasks(userId) {
  return [...downloadTasks.values()]
    .filter((task) => task.userId === userId && task.status !== "cancelled")
    .sort((a, b) => {
      const orderDiff = Number(a.order || 0) - Number(b.order || 0);
      if (orderDiff) return orderDiff;
      return String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt));
    })
    .map(serializeSilentCacheTask);
}

async function silentCacheSpeedDiagnostics(userId, payload = {}) {
  monitorSilentCacheTasks(realtimeIo);
  const maxSampleBytes = 32 * 1024 * 1024;
  const forceProbe = Boolean(payload.forceProbe);
  const sampleBytes = Math.max(512 * 1024, Math.min(maxSampleBytes, Number(payload.sampleBytes || 1024 * 1024)));
  const tasks = [...downloadTasks.values()].filter((task) => task.userId === userId && task.status !== "cancelled");
  const runningTasks = tasks.filter((task) => task.status === "downloading");
  const running = runningTasks.length;
  const candidate = tasks.find((task) => ["queued", "error"].includes(task.status)) ||
    runningTasks[0] ||
    tasks[0];
  const activeTasks = tasks
    .filter((task) => ["downloading", "queued", "error"].includes(task.status))
    .sort((a, b) => Number(a.order || 0) - Number(b.order || 0))
    .slice(0, 20)
    .map(serializeSilentCacheTask);
  const base = {
    testedAt: new Date().toISOString(),
    enabled: silentCacheEnabled,
    rateLimitBps: silentCacheRateLimitBps,
    concurrency: silentConcurrencyLimit(),
    configuredConcurrency: silentConcurrencyLimit(),
    effectiveConcurrency: effectiveSilentConcurrency(userId),
    cacheMode: normalizedSilentCacheMode(silentCacheMode),
    running,
    queued: tasks.filter((task) => task.status === "queued").length,
    partSizeKb: silentPartSizeKb(),
    directChunkSize: MAX_TELEGRAM_CHUNK_SIZE,
    sampleBytes,
    activeTasks
  };
  if (!candidate) return { ...base, ok: false, error: "暂无后台缓存任务，先在群组信息里勾选后台缓存后再测试。" };
  if (runningTasks.length && !forceProbe) {
    const aggregateSpeedBps = runningTasks.reduce((sum, task) => sum + Number(task.speedBps || 0), 0);
    const now = Date.now();
    const primary = runningTasks[0];
    const lastObservedAt = Date.parse(primary.updatedAt || primary.createdAt || 0) || 0;
    const lowSpeedSince = 0;
    return {
      ...base,
      ok: true,
      mode: "aggregate",
      task: serializeSilentCacheTask(primary),
      result: {
        bytesRead: 0,
        chunks: 0,
        durationMs: 0,
        speedBps: aggregateSpeedBps,
        runningTasks: runningTasks.length,
        lastObservedAgeMs: lastObservedAt ? now - lastObservedAt : null,
        lowSpeedThresholdBps: lowSpeedThresholdBps(),
        lowSpeedAgeMs: lowSpeedSince ? now - lowSpeedSince : 0,
        suspectedStall: Boolean(lastObservedAt && now - lastObservedAt > STALE_TASK_MS),
        suspectedSlowLink: Boolean(lowSpeedSince && now - lowSpeedSince > SLOW_TASK_MS),
        requestedChunkSize: 0,
        effectiveChunkSize: 0,
        fallbackCount: 0,
        limitInvalidCount: 0
      },
      note: "当前已有后台缓存任务运行，诊断显示运行任务状态；读取样本为 0 表示未额外抢占 Telegram 连接。"
    };
  }
  try {
    const { client, message } = await mediaMessage(candidate.userId, candidate.accountId, candidate.peerId, candidate.messageId);
    if (!message.document) return { ...base, ok: false, task: serializeSilentCacheTask(candidate), error: "当前任务不是 document 视频，无法执行 Telegram 分片测速。" };
    const total = Number(message.document.size?.toString?.() || message.document.size || message.file?.size || candidate.size || 0);
    const partStat = await fs.stat(candidate.partPath).catch(() => null);
    const downloaded = Math.max(0, Number(partStat?.size || candidate.downloaded || 0));
    let safeOffset = total ? Math.min(downloaded, Math.max(0, total - 512 * 1024)) : downloaded;
    safeOffset -= safeOffset % TELEGRAM_MIN_CHUNK_SIZE;
    const targetBytes = total ? Math.min(sampleBytes, Math.max(0, total - safeOffset)) : sampleBytes;
    const started = Date.now();
    let chunks = 0;
    const metrics = await readTelegramDocumentChunks(client, message.document, safeOffset, targetBytes, async () => {
      chunks += 1;
    });
    const durationMs = Math.max(1, Date.now() - started);
    const bytesRead = metrics.bytesRead || 0;
    return {
      ...base,
      ok: true,
      task: {
        ...serializeSilentCacheTask(candidate),
        dcId: message.document.dcId,
        mimeType: message.document.mimeType || "",
        savedPartBytes: downloaded,
        testOffset: safeOffset
      },
      result: {
        bytesRead,
        chunks,
        durationMs,
        speedBps: Math.round((bytesRead / durationMs) * 1000),
        requestedChunkSize: metrics.requestedChunkSize,
        effectiveChunkSize: metrics.effectiveChunkSize,
        fallbackCount: metrics.fallbackCount,
        limitInvalidCount: metrics.limitInvalidCount
      }
    };
  } catch (error) {
    return {
      ...base,
      ok: false,
      task: serializeSilentCacheTask(candidate),
      error: error.message || String(error)
    };
  }
}

function setSilentCacheControl(userId, payload = {}, io = realtimeIo) {
  const wasEnabled = silentCacheEnabled;
  if (payload.enabled !== undefined) {
    silentCacheEnabled = Boolean(payload.enabled);
    if (silentCacheEnabled && !wasEnabled) resetSilentRateWindow();
  }
  if (payload.rateLimitBps !== undefined) {
    silentCacheRateLimitBps = Math.max(0, Math.min(1024 * 1024 * 1024, Number(payload.rateLimitBps || 0)));
    resetSilentRateWindow();
  }
  if (payload.concurrency !== undefined) {
    silentCacheConcurrency = Math.max(1, Math.min(MAX_SILENT_CACHE_CONCURRENCY, Number(payload.concurrency || 1)));
    resetSilentRateWindow();
  }
  if (payload.mode !== undefined) {
    silentCacheMode = normalizedSilentCacheMode(payload.mode);
  }
  if (!silentCacheEnabled) {
    for (const task of downloadTasks.values()) {
      if (task.userId !== userId) continue;
      if (!isAutoDownloadTask(task)) continue;
      if (task.cancelToken) {
        task.cancelToken.cancelled = true;
        task.cancelToken.requeue = false;
      }
      if (["queued", "downloading", "error"].includes(task.status)) {
        task.status = "cancelled";
        task.speedBps = 0;
        task.error = "";
        task.updatedAt = new Date().toISOString();
        emitDownloadTask(io, task);
      }
    }
  } else {
    for (const task of downloadTasks.values()) {
      if (task.userId !== userId) continue;
      if (!isAutoDownloadTask(task)) continue;
      if (["cancelled", "error", "queued"].includes(task.status)) {
        startDownloadTask(userId, task.accountId, task.peerId, task.messageId, io, {
          source: "auto",
          autoCache: true,
          dedupKey: task.dedupKey || ""
        }).catch((error) => {
          task.status = "error";
          task.error = error.message || "恢复缓存失败";
          task.updatedAt = new Date().toISOString();
          emitDownloadTask(io, task);
        });
      }
    }
    pumpUnifiedDownloadQueue(io, userId);
  }
  schedulePersistSilent();
  return serializeSilentCacheState(userId);
}

function reorderSilentCacheTasks(userId, orderedIds = [], io = realtimeIo) {
  const ids = Array.isArray(orderedIds) ? orderedIds : [];
  ids.forEach((id, index) => {
    const task = downloadTasks.get(id);
    if (task && task.userId === userId) {
      task.order = index + 1;
      task.updatedAt = new Date().toISOString();
    }
  });
  for (const task of downloadTasks.values()) {
    if (task.userId === userId) emitDownloadTask(io, task);
  }
  schedulePersistSilent();
  return serializeSilentCacheState(userId);
}

function monitorDownloadCompletions(io = realtimeIo) {
  const now = Date.now();
  for (const task of downloadTasks.values()) {
    if (task.status === "completed" || task.status === "cancelled") continue;
    const partStat = task.partPath && fs.existsSync(task.partPath) ? fs.statSync(task.partPath) : null;
    const fileStat = task.filePath && fs.existsSync(task.filePath) ? fs.statSync(task.filePath) : null;
    const observed = Math.max(Number(partStat?.size || 0), Number(fileStat?.size || 0), Number(task.downloaded || 0));
    if (observed > Number(task.downloaded || 0)) {
      task.downloaded = observed;
      task.lastObservedPartSize = observed;
      task.lastObservedAt = new Date(now).toISOString();
      task.updatedAt = new Date(now).toISOString();
      emitDownloadTask(io, task);
    }
    if (fileStat && fileSizeComplete(fileStat.size, task.size)) {
      task.downloaded = fileStat.size;
      task.status = "completed";
      task.speedBps = 0;
      task.error = "";
      task.updatedAt = new Date(now).toISOString();
      emitDownloadTask(io, task);
      continue;
    }
    if (partStat && fileSizeComplete(partStat.size, task.size)) {
      fs.move(task.partPath, task.filePath, { overwrite: true }).then(() => {
        task.downloaded = partStat.size;
        task.status = "completed";
        task.speedBps = 0;
        task.error = "";
        task.updatedAt = new Date().toISOString();
        emitDownloadTask(io, task);
      }).catch((error) => {
        task.error = error.message || "下载完成收口失败";
        task.updatedAt = new Date().toISOString();
        emitDownloadTask(io, task);
      });
    }
  }
}

function monitorSilentCacheTasks(io = realtimeIo) {
  monitorDownloadCompletions(io);
  const now = Date.now();
  let shouldPump = false;
  for (const task of downloadTasks.values()) {
    if (task.status === "completed" || task.status === "cancelled") continue;
    if (task.status === "downloading") {
      const lastObservedAt = Date.parse(task.lastObservedAt || task.lastProgressAt || task.updatedAt || task.createdAt || 0) || 0;
      const stale = lastObservedAt && now - lastObservedAt > STALE_TASK_MS;
      if (stale) {
        if (task.cancelToken) {
          task.cancelToken.cancelled = true;
          task.cancelToken.requeue = true;
        }
        task.status = "queued";
        task.speedBps = 0;
        task.error = "下载连接停滞，已重新排队续传";
        task.retryAfter = now + 3000;
        task.updatedAt = new Date(now).toISOString();
        emitDownloadTask(io, task);
        shouldPump = true;
      }
      continue;
    }
    if (task.status === "error" && isTransientDownloadError(task.error || "")) {
      const retryAfter = Number(task.retryAfter || 0);
      if (!retryAfter || now >= retryAfter) {
        task.status = "queued";
        task.speedBps = 0;
        task.error = "Telegram 连接波动，已重新排队续传";
        task.retryCount = 0;
        task.retryAfter = 0;
        task.updatedAt = new Date(now).toISOString();
        emitDownloadTask(io, task);
        shouldPump = true;
      }
    }
  }
  if (shouldPump) pumpUnifiedDownloadQueue(io);
}

function cancelSilentCacheTask(userId, taskId, io = realtimeIo) {
  const task = downloadTasks.get(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到后台缓存任务"), { status: 404 });
  cancelDownloadTask(userId, taskId, io);
  emitSilentCacheDelete(io, task);
  return { ok: true, id: task.id };
}

function cancelSilentCacheTasks(userId, taskIds = [], io = realtimeIo) {
  const ids = Array.isArray(taskIds) ? taskIds : [];
  const cancelled = [];
  for (const id of ids) {
    const task = downloadTasks.get(id);
    if (!task || task.userId !== userId) continue;
    cancelDownloadTask(userId, id, io);
    cancelled.push(task.id);
    emitSilentCacheDelete(io, task);
  }
  schedulePersistSilent();
  return { ok: true, ids: cancelled };
}

async function resumeDownloadTask(userId, taskId, io) {
  const task = downloadTasks.get(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  return startDownloadTask(userId, task.accountId, task.peerId, task.messageId, io);
}

function cancelDownloadTask(userId, taskId, io) {
  const task = downloadTasks.get(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  if (task.cancelToken) {
    task.cancelToken.cancelled = true;
    task.cancelToken.requeue = false;
  }
  if (task.status !== "completed") task.status = "cancelled";
  task.speedBps = 0;
  task.updatedAt = new Date().toISOString();
  emitDownloadTask(io, task);
  pumpUnifiedDownloadQueue(io, userId);
  return serializeDownloadTask(task);
}

async function mediaThumbnail(userId, accountId, peerId, messageId) {
  const { client, message } = await mediaMessage(userId, accountId, peerId, messageId);
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const directories = await cacheSettings();
  const thumbDir = path.join(directories.thumbs, userId);
  await fs.ensureDir(thumbDir);
  const filePath = path.join(thumbDir, `${safeId(`${accountId}-${peerId}-${messageId}`)}.jpg`);
  if (await fs.pathExists(filePath)) {
    return { filePath, contentType: "image/jpeg" };
  }

  const thumbCount = message.document?.thumbs?.length || message.photo?.sizes?.length || 0;
  if (thumbCount) {
    const buffer = await client.downloadMedia(message, { thumb: thumbCount - 1 }).catch(() => null);
    if (buffer?.length) {
      await fs.writeFile(filePath, buffer);
      return { filePath, contentType: "image/jpeg" };
    }
  }

  if (kind === "image") {
    const media = await downloadMedia(userId, accountId, peerId, messageId, { forceCache: true });
    return { filePath: media.filePath, contentType: media.contentType };
  }

  throw Object.assign(new Error("暂无视频封面"), { status: 404 });
}

async function deleteDownloadTask(userId, taskId, io) {
  const task = downloadTasks.get(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  if (task.cancelToken) task.cancelToken.cancelled = true;
  await fs.remove(task.partPath).catch(() => {});
  await fs.remove(task.filePath).catch(() => {});
  downloadTasks.delete(taskId);
  schedulePersistDownloads();
  schedulePersistSilent();
  if (io) {
    io.to(`user:${userId}`).emit("download:delete", { id: taskId });
    io.to(`user:${userId}`).emit("silent-cache:delete", { id: taskId });
  }
  return { ok: true };
}

function clearDownloadTask(userId, taskId, io) {
  const task = downloadTasks.get(taskId);
  if (!task || task.userId !== userId) throw Object.assign(new Error("找不到下载任务"), { status: 404 });
  if (task.cancelToken) task.cancelToken.cancelled = true;
  downloadTasks.delete(taskId);
  schedulePersistDownloads();
  schedulePersistSilent();
  if (io) {
    io.to(`user:${userId}`).emit("download:delete", { id: taskId });
    io.to(`user:${userId}`).emit("silent-cache:delete", { id: taskId });
  }
  return { ok: true };
}

async function downloadMedia(userId, accountId, peerId, messageId, options = {}) {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const [message] = await client.getMessages(entity, { ids: [Number(messageId)] });
  if (!message || !message.media) throw Object.assign(new Error("这条消息没有可下载媒体"), { status: 404 });
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const rawSize = Number(message.file?.size || message.document?.size || 0);
  const cacheable = options.forceCache || kind === "image" || (kind === "video" && rawSize > VIDEO_CACHE_THRESHOLD);
  const { fileName, filePath, downloadDir } = await mediaFileInfo(userId, accountId, message, contentType, kind);
  if (!cacheable) {
    const buffer = await client.downloadMedia(message, {});
    return {
      buffer,
      fileName,
      contentType: contentType || mime.lookup(fileName) || "application/octet-stream",
      size: buffer.length,
      kind,
      cacheable: false
    };
  }
  await fs.ensureDir(downloadDir);
  if (!(await fs.pathExists(filePath))) {
    const buffer = await client.downloadMedia(message, {});
    await fs.writeFile(filePath, buffer);
  }
  const stat = await fs.stat(filePath);
  return {
    filePath,
    fileName,
    contentType: contentType || mime.lookup(filePath) || "application/octet-stream",
    size: stat.size,
    kind,
    cacheable: true
  };
}

async function cacheMedia(userId, accountId, peerId, messageId) {
  return downloadMedia(userId, accountId, peerId, messageId, { forceCache: true });
}

async function streamVideoMedia(userId, accountId, peerId, messageId, rangeHeader, res) {
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const [message] = await client.getMessages(entity, { ids: [Number(messageId)] });
  if (!message || !message.media) throw Object.assign(new Error("这条消息没有可播放媒体"), { status: 404 });
  const contentType = message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  if (kind !== "video") return false;
  const size = Number(message.file?.size || message.document?.size || 0);
  if (!size) return false;
  const { fileName, filePath } = await mediaFileInfo(userId, accountId, message, contentType, kind);
  if (await fs.pathExists(filePath)) {
    const stat = await fs.stat(filePath);
    if (fileSizeComplete(stat.size, size)) {
      return streamLocalFile(filePath, fileName, contentType || mime.lookup(filePath) || "video/mp4", stat.size, rangeHeader, res, "private, max-age=86400");
    }
  }
  let start = 0;
  let end = size - 1;
  let statusCode = 200;
  if (rangeHeader) {
    const match = String(rangeHeader).match(/bytes=(\d*)-(\d*)/);
    if (match) {
      if (match[1]) start = Number(match[1]);
      if (match[2]) end = Number(match[2]);
      if (!match[1] && match[2]) {
        const suffix = Number(match[2]);
        start = Math.max(0, size - suffix);
        end = size - 1;
      }
      statusCode = 206;
    }
  }
  start = Math.max(0, Math.min(start, size - 1));
  end = Math.max(start, Math.min(end, size - 1));
  const contentLength = end - start + 1;
  res.writeHead(statusCode, {
    "Accept-Ranges": "bytes",
    "Cache-Control": "no-store",
    "Content-Disposition": `inline; filename="${encodeURIComponent(fileName)}"`,
    "Content-Length": contentLength,
    "Content-Type": contentType || mime.lookup(fileName) || "video/mp4",
    ...(statusCode === 206 ? { "Content-Range": `bytes ${start}-${end}/${size}` } : {})
  });
  const chunkSize = 512 * 1024;
  const limit = Math.ceil(contentLength / chunkSize);
  let sent = 0;
  for await (const chunk of client.iterDownload({
    file: inputFileLocation(message),
    offset: bigInt(start),
    chunkSize,
    requestSize: chunkSize,
    limit,
    fileSize: bigInt(size)
  })) {
    if (res.destroyed) break;
    const remaining = contentLength - sent;
    if (remaining <= 0) break;
    const piece = chunk.length > remaining ? chunk.slice(0, remaining) : chunk;
    sent += piece.length;
    if (!res.write(piece)) {
      await new Promise((resolve) => res.once("drain", resolve));
    }
  }
  res.end();
  return true;
}

function streamLocalFile(filePath, fileName, contentType, size, rangeHeader, res, cacheControl = "private, max-age=86400") {
  let start = 0;
  let end = size - 1;
  let statusCode = 200;
  if (rangeHeader) {
    const match = String(rangeHeader).match(/bytes=(\d*)-(\d*)/);
    if (match) {
      if (match[1]) start = Number(match[1]);
      if (match[2]) end = Number(match[2]);
      if (!match[1] && match[2]) {
        const suffix = Number(match[2]);
        start = Math.max(0, size - suffix);
        end = size - 1;
      }
      statusCode = 206;
    }
  }
  start = Math.max(0, Math.min(start, size - 1));
  end = Math.max(start, Math.min(end, size - 1));
  res.writeHead(statusCode, {
    "Accept-Ranges": "bytes",
    "Cache-Control": cacheControl,
    "Content-Disposition": `inline; filename="${encodeURIComponent(fileName)}"`,
    "Content-Length": end - start + 1,
    "Content-Type": contentType,
    ...(statusCode === 206 ? { "Content-Range": `bytes ${start}-${end}/${size}` } : {})
  });
  fs.createReadStream(filePath, { start, end }).pipe(res);
  return true;
}

async function profilePhoto(userId, accountId, peerId = "__self") {
  const client = await getClient(userId, accountId);
  const entity = peerId === "__self" ? await client.getMe() : await resolvePeer(userId, accountId, peerId);
  const directories = await cacheSettings();
  const avatarDir = path.join(directories.avatars, userId);
  await fs.ensureDir(avatarDir);
  const fileName = `${safeId("avatar")}.jpg`;
  const filePath = path.join(avatarDir, fileName);
  const buffer = await client.downloadProfilePhoto(entity, { isBig: false }).catch(() => null);
  if (!buffer) throw Object.assign(new Error("暂无头像"), { status: 404 });
  await fs.writeFile(filePath, buffer);
  return {
    filePath,
    fileName,
    contentType: "image/jpeg"
  };
}

function goDownloaderTaskId(userId, accountId, peerId, messageId) {
  return downloadTaskId(userId, accountId, peerId, messageId);
}

function internalMediaSourceUrl(userId, accountId, peerId, messageId) {
  const token = process.env.FEIGRAM_INTERNAL_TOKEN || "";
  if (!token) throw Object.assign(new Error("内部下载令牌未初始化，请重启 Feigram"), { status: 500 });
  const port = Number(process.env.APP_PORT || 3088);
  return `http://127.0.0.1:${port}/api/internal/media/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/${encodeURIComponent(peerId)}/${encodeURIComponent(messageId)}?token=${encodeURIComponent(token)}`;
}

function internalMediaMetadataUrl(userId, accountId, peerId, messageId) {
  const token = process.env.FEIGRAM_INTERNAL_TOKEN || "";
  if (!token) throw Object.assign(new Error("内部下载令牌未初始化，请重启 Feigram"), { status: 500 });
  const port = Number(process.env.APP_PORT || 3088);
  return `http://127.0.0.1:${port}/api/internal/media-meta/${encodeURIComponent(userId)}/${encodeURIComponent(accountId)}/${encodeURIComponent(peerId)}/${encodeURIComponent(messageId)}?token=${encodeURIComponent(token)}`;
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

async function ensureGoDownloadTask(userId, accountId, peerId, messageId, options = {}) {
  const { entity, message } = await mediaMessage(userId, accountId, peerId, messageId);
  const contentType = message.photo ? "image/jpeg" : message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = Number(message.file?.size || message.document?.size || 0);
  const { fileName, filePath, downloadDir } = await mediaFileInfo(userId, accountId, message, contentType, kind);
  await fs.ensureDir(downloadDir);
  const id = goDownloaderTaskId(userId, accountId, peerId, messageId);
  const source = options.source || "manual";
  const downloaderState = await downloaderSidecar.state().catch(() => null);
  const configuredTransport = downloaderState?.config?.transport || "http-bridge";
  const readyAccountKeys = Array.isArray(downloaderState?.nativeMTProto?.readyAccountKeys)
    ? downloaderState.nativeMTProto.readyAccountKeys
    : [];
  const nativeEligible = size >= 100 * 1024 * 1024 && readyAccountKeys.includes(`${userId}|${accountId}`);
  const transport = configuredTransport === "native-mtproto" || nativeEligible ? "native-mtproto" : "http-bridge";
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
    sourceUrl: internalMediaSourceUrl(userId, accountId, peerId, messageId),
    metadataUrl: internalMediaMetadataUrl(userId, accountId, peerId, messageId),
    inlineUrl: `/api/media/${accountId}/${encodeURIComponent(peerId)}/${messageId}?inline=1`,
    nativeFile: nativeFileLocation(peerId, message, contentType, fileName, kind),
    nativePeer: nativePeerLocation(peerId, entity),
    order: Number(options.order || 0) || Date.now(),
    dedupKey: options.dedupKey || ""
  });
  return normalizeGoDownloadTask(task);
}

async function mediaNativeMetadata(userId, accountId, peerId, messageId) {
  const { entity, message } = await mediaMessage(userId, accountId, peerId, messageId);
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
    effectiveConcurrency: Number(state?.running || 0),
    mode: config.mode || "conservative",
    transport: config.transport || state?.transport || "http-bridge",
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

async function cacheVideoSilentlyGo(userId, accountId, peerId, message) {
  const contentType = message.document?.mimeType || "";
  const kind = mediaKind(message, contentType);
  const size = Number(message.file?.size || message.document?.size || 0);
  if (kind !== "video" || size <= VIDEO_CACHE_THRESHOLD) return false;
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
  if (duplicate) return false;
  await ensureGoDownloadTask(userId, accountId, peerId, message.id, {
    source: "auto",
    autoCache: true,
    dedupKey,
    order: Date.now()
  });
  return true;
}

async function cacheLargeVideosInChatGo(userId, accountId, peerId, io = realtimeIo) {
  realtimeIo = io || realtimeIo;
  const client = await getClient(userId, accountId);
  const entity = await resolvePeer(userId, accountId, peerId);
  const recent = await client.getMessages(entity, { limit: 120 }).catch(() => []);
  let queued = 0;
  for (const message of recent) {
    if (!message?.media) continue;
    if (await cacheVideoSilentlyGo(userId, accountId, peerId, message)) queued += 1;
  }
  return { queued };
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
    transport: state?.config?.transport || state?.transport || "http-bridge",
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
    note: "Go 下载服务已接管队列与文件写入；诊断显示 Go 任务聚合速度。媒体源传输层会在运行诊断中显示。"
  };
}

async function goNativeAccounts() {
  const accounts = await downloaderSidecar.nativeAccounts();
  return Array.isArray(accounts) ? accounts : [];
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
  await loadPersistentTasks();
  for (const task of downloadTasks.values()) {
    if (!task.userId || !task.accountId || !task.peerId || !task.messageId) continue;
    if (task.status === "completed") continue;
    await ensureGoDownloadTask(task.userId, task.accountId, task.peerId, task.messageId, {
      source: task.source || (task.autoCache ? "auto" : "manual"),
      autoCache: Boolean(task.autoCache || task.source === "auto"),
      dedupKey: task.dedupKey || "",
      order: Number(task.order || Date.parse(task.createdAt || "") || Date.now())
    }).catch((error) => {
      console.warn("Failed to migrate legacy download task:", error.message);
    });
  }
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

module.exports = {
  clickMessageButton,
  completeCode,
  completePassword,
  cacheMedia,
  cacheLargeVideosInChat: cacheLargeVideosInChatGo,
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
  listAccounts,
  listChats,
  listFolders,
  listMessages,
  loadSavedClients,
  logout,
  cleanupCache,
  profilePhoto,
  resolveTelegramLink,
  restoreBackgroundTasks: restoreGoBackgroundTasks,
  search,
  sendText,
  startLogin,
  async reconnectAll(io) {
    for (const client of clients.values()) {
      try {
        await client.disconnect();
      } catch {}
    }
    clients.clear();
    peerCache.clear();
    pendingLogins.clear();
    await loadSavedClients(io);
  }
};
