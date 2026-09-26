/*
 * R4.53：聊天/群组内容本地缓存（IndexedDB）。
 *
 * 设计（stale-while-revalidate）：
 * - 打开会话时先用缓存秒开（不再干等网络），随后网络刷新覆盖界面并回写缓存；
 * - 「加载更早消息」「刷新」成功后把合并结果回写缓存（每会话最多 600 条）；
 * - 会话列表按账号缓存，网络失败时兜底显示上次缓存，不再整页报错；
 * - 缓存读写全部静默兜底：IndexedDB 不可用（隐私模式等）时所有函数返回空值，
 *   绝不影响主流程（与 localStorage 降级策略一致）。
 */

const DB_NAME = "feigrame-cache";
// R4.66：+1 建 details store（群组信息面板缓存）。
const DB_VERSION = 2;
const STORE_CHATS = "chats";
const STORE_MESSAGES = "messages";
const STORE_DETAILS = "details";
const MAX_CACHED_MESSAGES = 600;
const MAX_CACHED_CHATS = 2000;

let dbPromise = null;

function openCacheDb() {
  if (dbPromise) return dbPromise;
  dbPromise = new Promise((resolve, reject) => {
    if (typeof indexedDB === "undefined") {
      reject(new Error("indexedDB unavailable"));
      return;
    }
    const request = indexedDB.open(DB_NAME, DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(STORE_CHATS)) db.createObjectStore(STORE_CHATS);
      if (!db.objectStoreNames.contains(STORE_MESSAGES)) db.createObjectStore(STORE_MESSAGES);
      if (!db.objectStoreNames.contains(STORE_DETAILS)) db.createObjectStore(STORE_DETAILS);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error || new Error("indexedDB open failed"));
  });
  return dbPromise;
}

// 单事务 readwrite：get → 合并 → put，避免并发写相互覆盖。
async function withReadwriteStore(storeName, key, mutate) {
  try {
    const db = await openCacheDb();
    await new Promise((resolve, reject) => {
      const tx = db.transaction(storeName, "readwrite");
      const store = tx.objectStore(storeName);
      const getRequest = store.get(key);
      getRequest.onsuccess = () => {
        try {
          store.put(mutate(getRequest.result), key);
        } catch (error) {
          reject(error);
        }
      };
      tx.oncomplete = () => resolve(undefined);
      tx.onerror = () => reject(tx.error || new Error("indexedDB tx failed"));
      tx.onabort = () => reject(tx.error || new Error("indexedDB tx aborted"));
    });
    return true;
  } catch {
    return false;
  }
}

async function withReadStore(storeName, key) {
  try {
    const db = await openCacheDb();
    return await new Promise((resolve, reject) => {
      const tx = db.transaction(storeName, "readonly");
      const getRequest = tx.objectStore(storeName).get(key);
      getRequest.onsuccess = () => resolve(getRequest.result || null);
      tx.onerror = () => reject(tx.error || new Error("indexedDB read failed"));
      tx.onabort = () => reject(tx.error || new Error("indexedDB read aborted"));
    });
  } catch {
    return null;
  }
}

function cacheKey(accountId, peerId) {
  return `${accountId}:${peerId}`;
}

// 返回 { items(新→旧), reachedEnd, savedAt } 或 null。
export async function cachedMessages(accountId, peerId) {
  if (!accountId || !peerId) return null;
  const entry = await withReadStore(STORE_MESSAGES, cacheKey(accountId, peerId));
  if (!entry || !Array.isArray(entry.items) || !entry.items.length) return null;
  return {
    items: entry.items,
    reachedEnd: Boolean(entry.reachedEnd),
    savedAt: Number(entry.savedAt) || 0
  };
}

// items：当前已知窗口（新→旧或乱序均可，内部按 id 去重合并排序）。
// reachedEnd：调用方当前是否已知「没有更早消息」（loadOlder 空页时为 true）。
export async function saveMessages(accountId, peerId, items, reachedEnd = false) {
  if (!accountId || !peerId || !Array.isArray(items) || !items.length) return;
  await withReadwriteStore(STORE_MESSAGES, cacheKey(accountId, peerId), (existing) => {
    const merged = new Map();
    for (const item of [...((existing && existing.items) || []), ...items]) {
      if (item && item.id !== undefined && item.id !== null) merged.set(String(item.id), item);
    }
    const list = [...merged.values()]
      .sort((a, b) => Number(b.id) - Number(a.id))
      .slice(0, MAX_CACHED_MESSAGES);
    return { items: list, reachedEnd: Boolean(reachedEnd), savedAt: Date.now() };
  });
}

// 返回缓存的会话列表（数组）或 null。
export async function cachedChats(accountId) {
  if (!accountId) return null;
  const entry = await withReadStore(STORE_CHATS, accountId);
  if (!entry || !Array.isArray(entry.items) || !entry.items.length) return null;
  return entry.items;
}

export async function saveChats(accountId, list) {
  if (!accountId || !Array.isArray(list) || !list.length) return;
  await withReadwriteStore(STORE_CHATS, accountId, () => ({
    items: list.slice(0, MAX_CACHED_CHATS),
    savedAt: Date.now()
  }));
}

// R4.66：群组信息面板缓存（成员数/媒体统计/最近资源列表）。
// 返回 { info, savedAt } 或 null。
export async function cachedChatDetails(accountId, peerId) {
  if (!accountId || !peerId) return null;
  const entry = await withReadStore(STORE_DETAILS, cacheKey(accountId, peerId));
  if (!entry || !entry.info) return null;
  return { info: entry.info, savedAt: Number(entry.savedAt) || 0 };
}

// details：/details 接口完整返回（含 files 分页字段）；
// loadMore 追加媒体后由调用方合并好再存（这里只整体覆盖）。
export async function saveChatDetails(accountId, peerId, info) {
  if (!accountId || !peerId || !info || typeof info !== "object") return;
  await withReadwriteStore(STORE_DETAILS, cacheKey(accountId, peerId), () => ({
    info,
    savedAt: Date.now()
  }));
}
