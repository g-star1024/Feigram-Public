const fs = require("fs-extra");
const fsp = require("fs/promises");
const os = require("os");
const path = require("path");
const { dataDir, downloadTasksPath, silentCachePath } = require("./store");
const { metaPath } = require("./migrations");
const { readSettings } = require("./settings");
const downloaderSidecar = require("./downloaderSidecar");

async function dirSize(target) {
  let total = 0;
  const entries = await fs.readdir(target).catch(() => []);
  for (const entry of entries) {
    const filePath = path.join(target, entry);
    const stat = await fs.stat(filePath).catch(() => null);
    if (!stat) continue;
    if (stat.isDirectory()) total += await dirSize(filePath);
    else total += stat.size;
  }
  return total;
}

async function tail(filePath, maxBytes = 16000) {
  if (!filePath || !(await fs.pathExists(filePath))) return "";
  const stat = await fs.stat(filePath);
  const start = Math.max(0, stat.size - maxBytes);
  const handle = await fsp.open(filePath, "r");
  try {
    const buffer = Buffer.alloc(stat.size - start);
    await handle.read(buffer, 0, buffer.length, start);
    return buffer.toString("utf8");
  } finally {
    await handle.close();
  }
}

// R4.5 · 日志分级：把原始日志行按内容归类为 error / warn / info，供前端着色与过滤。
// Node 侧日志为 console 文本、Go 侧为 slog JSON，二者格式不一，故用内容关键字分类而非解析结构，
// 保证不依赖具体日志格式也能稳定着色（纯函数，可单测）。
function classifyLogLevel(text) {
  if (/\b(ERROR|FATAL|ERR|PANIC|CRITICAL|FAIL|失败|错误|异常|timeout|timed out|ECONNREFUSED|ECONNRESET|SIGPIPE|panic|rejected)\b/i.test(text)) return "error";
  if (/\b(WARN|WARNING|警告|DEPRECATED|FLOOD_WAIT|rate.?limit)\b/i.test(text)) return "warn";
  return "info";
}

// parseLogEntries 把多行日志文本切成结构化条目；空文本返回空数组。
function parseLogEntries(text) {
  if (!text) return [];
  return text
    .split("\n")
    .map((line) => line.replace(/\s+$/, ""))
    .filter((line) => line.length > 0)
    .map((text) => ({ level: classifyLogLevel(text), text }));
}

async function diagnostics() {
  const settings = await readSettings();
  const [downloadData, silentData, meta, downloader] = await Promise.all([
    fs.readJson(downloadTasksPath).catch(() => ({ tasks: [] })),
    fs.readJson(silentCachePath).catch(() => ({ tasks: [] })),
    fs.readJson(metaPath).catch(() => ({})),
    downloaderSidecar.health()
  ]);
  const cacheBase = settings.cacheBaseDir || process.env.DOWNLOAD_DIR || path.join(dataDir, "downloads");
  const logFile = process.env.LOG_FILE || "";
  const downloaderLogFile = process.env.FEIGRAM_DOWNLOADER_LOG || "";
  const logTail = await tail(logFile);
  const downloaderLogTail = await tail(downloaderLogFile, 8000);
  return {
    app: {
      name: "Feigram",
      edition: process.env.APP_EDITION || meta.edition || "Feigram 2.0 fnOS Client Edition",
      version: process.env.APP_VERSION || meta.version || "dev",
      changelog: process.env.APP_CHANGELOG || "",
      uptime: Math.round(process.uptime()),
      pid: process.pid,
      node: process.version,
      platform: `${os.platform()} ${os.arch()}`
    },
    paths: {
      dataDir,
      cacheBase,
      logFile,
      downloaderLogFile
    },
    downloader,
    cache: {
      bytes: await dirSize(cacheBase),
      downloadTasks: (downloadData.tasks || []).length,
      silentCacheTasks: (silentData.tasks || []).length
    },
    logTail,
    downloaderLogTail,
    nodeLogEntries: parseLogEntries(logTail),
    downloaderLogEntries: parseLogEntries(downloaderLogTail)
  };
}

// R4.5 · 清空日志文件：仅允许 node / downloader 两个已知目标，路径必须来自环境变量且为绝对路径，
// 杜绝任意路径截断。截断前确保文件存在（运行中进程可能尚未写出该文件）。
async function clearLog(target) {
  if (target !== "node" && target !== "downloader") throw new Error("无效的日志目标");
  const file = target === "downloader" ? process.env.FEIGRAM_DOWNLOADER_LOG : process.env.LOG_FILE;
  if (!file) throw new Error("未配置日志文件路径，无法清空");
  if (!path.isAbsolute(file)) throw new Error("日志路径非法，已拒绝清空");
  await fs.ensureFile(file);
  await fsp.truncate(file, 0);
  return { ok: true, target, file };
}

async function checkForUpdates() {
  const current = process.env.APP_VERSION || "dev";
  const endpoint = "https://api.github.com/repos/g-star1024/Feigram-Public/releases/latest";
  try {
    const response = await fetch(endpoint, { headers: { "User-Agent": "Feigram" } });
    if (!response.ok) throw new Error(`GitHub ${response.status}`);
    const latest = await response.json();
    const latestVersion = String(latest.tag_name || latest.name || "").replace(/^v/i, "");
    return {
      current,
      latest: latestVersion,
      url: latest.html_url || "https://github.com/g-star1024/Feigram-Public/releases",
      updateAvailable: Boolean(latestVersion && latestVersion !== current)
    };
  } catch (error) {
    return {
      current,
      latest: "",
      url: "https://github.com/g-star1024/Feigram-Public/releases",
      updateAvailable: false,
      error: error.message
    };
  }
}

module.exports = {
  checkForUpdates,
  diagnostics,
  clearLog,
  parseLogEntries,
  classifyLogLevel
};
