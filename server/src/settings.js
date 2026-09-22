const fs = require("fs-extra");
const path = require("path");
const { dataDir, ensureStore } = require("./store");
const { normalizeProxyUrl } = require("./proxyConfig");

const settingsPath = path.join(dataDir, "settings.json");

// 单一事实源：登录前展示的 Telegram 服务条款与账号观察提示。
const TELEGRAM_LEGAL_NOTICE = "使用本应用即表示你已阅读并同意遵守 Telegram 服务条款。第三方客户端登录的账号会被 Telegram 自动观察；请勿滥用、刷量或违反平台规则，否则可能导致账号受限或封禁。你必须提供自有的 api_id / api_hash（于 my.telegram.org 申请），本应用绝不内置任何默认凭据。";

function envDefaults() {
  const dataRoot = process.env.DATA_DIR || "/data";
  return {
    appPassword: process.env.APP_PASSWORD || "",
    publicBaseUrl: process.env.PUBLIC_BASE_URL || `http://127.0.0.1:${process.env.APP_PORT || 3088}`,
    telegramApiId: process.env.TELEGRAM_API_ID || "",
    telegramApiHash: process.env.TELEGRAM_API_HASH || "",
    cacheBaseDir: process.env.CACHE_BASE_DIR || process.env.DOWNLOAD_DIR || `${dataRoot}/downloads`,
    imageCacheDir: process.env.IMAGE_CACHE_DIR || "",
    videoCacheDir: process.env.VIDEO_CACHE_DIR || "",
    fileCacheDir: process.env.FILE_CACHE_DIR || "",
    cacheRetentionDays: process.env.CACHE_RETENTION_DAYS || "30",
    notificationEnabled: process.env.NOTIFICATION_ENABLED !== "false",
    notificationPreview: process.env.NOTIFICATION_PREVIEW !== "false",
    privacyOpenTelegramLinksInApp: process.env.PRIVACY_OPEN_TG_LINKS_IN_APP !== "false",
    privacyMediaPreview: process.env.PRIVACY_MEDIA_PREVIEW !== "false",
    messageShowSender: process.env.MESSAGE_SHOW_SENDER !== "false",
    foldersEnabled: process.env.FOLDERS_ENABLED !== "false",
    foldersShowArchived: process.env.FOLDERS_SHOW_ARCHIVED === "true",
    foldersAutoSelectFirst: process.env.FOLDERS_AUTO_SELECT_FIRST !== "false",
    playerMode: process.env.PLAYER_MODE || "browser",
    downloaderEngine: process.env.DOWNLOADER_ENGINE || "go-sidecar",
    downloaderSidecarUrl: process.env.FEIGRAM_DOWNLOADER_URL || "http://127.0.0.1:3090",
    // 网络代理：空表示回落到环境变量（ALL_PROXY / HTTPS_PROXY ...），都没有则直连。
    // 真正生效的是 Go 侧，这里保存的值会在保存设置时推送给 Go（见 index.js 的 pushProxyToSidecar）。
    proxyUrl: process.env.FEIGRAM_PROXY_URL || ""
  };
}

function sanitize(input = {}) {
  const retention = Math.max(1, Math.min(3650, Number(input.cacheRetentionDays || 30)));
  const bool = (value, fallback) => value === undefined ? fallback : Boolean(value);
  return {
    appPassword: String(input.appPassword ?? "").trim(),
    publicBaseUrl: String(input.publicBaseUrl ?? "").trim(),
    telegramApiId: String(input.telegramApiId ?? "").trim(),
    telegramApiHash: String(input.telegramApiHash ?? "").trim(),
    cacheBaseDir: String(input.cacheBaseDir ?? "").trim(),
    imageCacheDir: String(input.imageCacheDir ?? "").trim(),
    videoCacheDir: String(input.videoCacheDir ?? "").trim(),
    fileCacheDir: String(input.fileCacheDir ?? "").trim(),
    cacheRetentionDays: String(Number.isFinite(retention) ? retention : 30),
    notificationEnabled: bool(input.notificationEnabled, true),
    notificationPreview: bool(input.notificationPreview, true),
    privacyOpenTelegramLinksInApp: bool(input.privacyOpenTelegramLinksInApp, true),
    privacyMediaPreview: bool(input.privacyMediaPreview, true),
    messageShowSender: bool(input.messageShowSender, true),
    foldersEnabled: bool(input.foldersEnabled, true),
    foldersShowArchived: bool(input.foldersShowArchived, false),
    foldersAutoSelectFirst: bool(input.foldersAutoSelectFirst, true),
    playerMode: ["browser", "local"].includes(input.playerMode) ? input.playerMode : "browser",
    downloaderEngine: ["node", "go-sidecar"].includes(input.downloaderEngine) ? input.downloaderEngine : "go-sidecar",
    downloaderSidecarUrl: String(input.downloaderSidecarUrl ?? "http://127.0.0.1:3090").trim(),
    // 校验通过则存规范化结果；不通过则原样保留用户输入，
    // 让 Go 侧把具体原因通过 /api/state 的 proxy.error 报给界面，避免这里吞掉错误。
    proxyUrl: normalizeProxyUrl(input.proxyUrl).value
  };
}

async function readSettings() {
  await ensureStore();
  if (!(await fs.pathExists(settingsPath))) {
    return envDefaults();
  }
  const saved = await fs.readJson(settingsPath);
  return sanitize({ ...envDefaults(), ...saved });
}

async function writeSettings(input) {
  await ensureStore();
  const current = await readSettings();
  const next = sanitize({ ...current, ...input });
  await fs.writeJson(settingsPath, next, { spaces: 2 });
  return next;
}

async function settingsFileExists() {
  await ensureStore();
  return fs.pathExists(settingsPath);
}

function publicSettings(settings) {
  return {
    appPasswordSet: Boolean(settings.appPassword),
    publicBaseUrl: settings.publicBaseUrl,
    // M1.3 合规要求：登录弹窗必须展示 Telegram 服务条款与账号观察提示。
    // 此前字段从未输出，前端 legalNotice 恒为空——合规提示静默失效（2026-09-22 复盘发现）。
    telegramLegalNotice: TELEGRAM_LEGAL_NOTICE,
    telegramApiId: "",
    telegramApiIdSet: Boolean(settings.telegramApiId),
    telegramApiHashSet: Boolean(settings.telegramApiHash && !settings.telegramApiHash.includes("put-your")),
    cacheBaseDir: settings.cacheBaseDir,
    imageCacheDir: settings.imageCacheDir,
    videoCacheDir: settings.videoCacheDir,
    fileCacheDir: settings.fileCacheDir,
    cacheRetentionDays: settings.cacheRetentionDays,
    notificationEnabled: settings.notificationEnabled,
    notificationPreview: settings.notificationPreview,
    privacyOpenTelegramLinksInApp: settings.privacyOpenTelegramLinksInApp,
    privacyMediaPreview: settings.privacyMediaPreview,
    messageShowSender: settings.messageShowSender,
    foldersEnabled: settings.foldersEnabled,
    foldersShowArchived: settings.foldersShowArchived,
    foldersAutoSelectFirst: settings.foldersAutoSelectFirst,
    playerMode: settings.playerMode,
    downloaderEngine: settings.downloaderEngine,
    downloaderSidecarUrl: settings.downloaderSidecarUrl,
    proxyUrl: settings.proxyUrl
  };
}

module.exports = {
  publicSettings,
  readSettings,
  settingsFileExists,
  writeSettings
};
