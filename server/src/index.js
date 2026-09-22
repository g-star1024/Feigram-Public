require("dotenv").config();

const path = require("path");
const fs = require("fs");
const express = require("express");
const cors = require("cors");
const { createServer } = require("http");
const { Server } = require("socket.io");
const {
  adminOnly,
  authMiddleware,
  bootstrap,
  bootstrapStatus,
  login,
  publicUser,
  verifyUserToken
} = require("./auth");
const { ensureStore, findUserByUsername, readUsers, safeId, upsertUser } = require("./store");
const { hashPassword } = require("./cryptoBox");
const { publicSettings, readSettings, writeSettings } = require("./settings");
const { maskProxyUrl } = require("./proxyConfig");
const { readPolicies } = require("./policies");
const { readAbout, readAnnouncements } = require("./releaseContent");
const { checkForUpdates, diagnostics } = require("./diagnostics");
const downloaderSidecar = require("./downloaderSidecar");
const { migrateStore, schemaVersion } = require("./migrations");
const { rateLimit } = require("./rateLimit");
const { pickAccountsSummary, sidecarTaskCount } = require("./healthSummary");
const { startDefaultMonitor } = require("./nativeHealthMonitor");
const { startDefaultAutoUpgrade } = require("./transportAutoUpgrade");
const tg = require("./telegramService");

const port = Number(process.env.APP_PORT || 3088);
// M5.2 修复：/api/health 曾引用未定义的 serverVersion，导致该接口恒定 500。
const serverVersion = process.env.APP_VERSION || require("../package.json").version || "dev";
const app = express();
const server = createServer(app);
const io = new Server(server, {
  cors: { origin: true, credentials: true }
});

app.use(cors({ origin: true, credentials: true }));
app.use(express.json({ limit: "1mb" }));

function asyncRoute(handler) {
  return async (req, res, next) => {
    try {
      await handler(req, res, next);
    } catch (error) {
      next(error);
    }
  };
}

// 网络出口在 Go 侧（MTProto 建连与媒体下载都在那里），
// 因此应用内配置的代理必须推送给 Go 才真正生效。
// 失败只告警不阻断：cmd/main 是「先起 Node、再起 Go sidecar」，
// 启动时 sidecar 可能还没就绪，所以启动路径带重试，保存设置时只试一次。
async function pushProxyToSidecar(settings, { attempts = 1, delayMs = 2000 } = {}) {
  const proxyUrl = String(settings?.proxyUrl || "");
  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    const result = await downloaderSidecar.updateConfig({ proxyUrl });
    if (result && result.ok !== false) {
      console.log(
        `[proxy] 已同步代理到 Go 下载服务：${proxyUrl ? maskProxyUrl(proxyUrl) : "空（回落环境变量或直连）"}`
      );
      return true;
    }
    if (attempt === attempts) {
      console.warn(`[proxy] 同步代理到 Go 下载服务失败（已尝试 ${attempts} 次）：${result?.error || "未知原因"}`);
      return false;
    }
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  return false;
}

// M5.2：健康接口扩充 schemaVersion / transport / sidecar 状态 / 账户健康分布。
// M5.3：汇总逻辑已抽到 healthSummary.js（纯函数，可单测），此处只做装配。
app.get("/api/health", asyncRoute(async (_req, res) => {
  const [sidecarState, sidecarAccounts] = await Promise.all([
    downloaderSidecar.state(),
    downloaderSidecar.nativeAccounts()
  ]);
  const reachable = Boolean(sidecarState.ok);
  const transport = sidecarState.ok ? sidecarState.transport : undefined;
  const accounts = pickAccountsSummary(sidecarState, sidecarAccounts);
  res.json({
    ok: true,
    version: serverVersion,
    schemaVersion: reachable && sidecarState.schemaVersion !== undefined ? sidecarState.schemaVersion : schemaVersion(),
    transport,
    sidecar: {
      reachable,
      url: sidecarState.url || downloaderSidecar.baseUrl(),
      version: sidecarState.ok ? sidecarState.version : undefined,
      taskCount: sidecarTaskCount(sidecarState),
      running: sidecarState.ok ? sidecarState.running : undefined
    },
    // 网络代理的权威状态来自 Go 侧（MTProto dialer 与媒体 transport 都在那里）。
    // source: settings / env:<KEY> / none / invalid；address 已脱敏。
    proxy: sidecarState.ok ? sidecarState.proxy : undefined,
    accounts
  });
}));
app.get("/api/bootstrap/status", asyncRoute(bootstrapStatus));
app.post("/api/bootstrap", rateLimit({ windowMs: 60000, max: 5 }), asyncRoute(bootstrap));
app.post("/api/login", rateLimit({ windowMs: 60000, max: 12 }), asyncRoute(login));
app.get("/api/policies", asyncRoute(async (_req, res) => res.json(await readPolicies())));
app.get("/api/about", asyncRoute(async (_req, res) => res.json(readAbout())));

// M4.1：移除 Node 侧 GramJS 内部媒体桥（原 internal/media* 路由）；
// 媒体字节流改由 Go 原生 blob 端点（/api/accounts/:id/blob）提供，元数据由 Go 账户 API 提供。

app.use("/api", authMiddleware());

app.get("/api/me", asyncRoute(async (req, res) => {
  res.json(publicUser(req.user));
}));

app.get("/api/settings", asyncRoute(async (_req, res) => {
  res.json(publicSettings(await readSettings()));
}));

app.put("/api/settings", adminOnly, asyncRoute(async (req, res) => {
  const next = await writeSettings(req.body || {});
  await pushProxyToSidecar(next);
  res.json({ settings: publicSettings(next) });
}));

app.get("/api/admin/users", adminOnly, asyncRoute(async (_req, res) => {
  res.json((await readUsers()).map(publicUser));
}));

app.get("/api/admin/diagnostics", adminOnly, asyncRoute(async (_req, res) => {
  res.json(await diagnostics());
}));

app.get("/api/admin/downloader", adminOnly, asyncRoute(async (_req, res) => {
  res.json(await downloaderSidecar.state());
}));

app.put("/api/admin/downloader/config", adminOnly, asyncRoute(async (req, res) => {
  res.json(await downloaderSidecar.updateConfig(req.body || {}));
}));

app.get("/api/admin/native-accounts", adminOnly, asyncRoute(async (_req, res) => {
  res.json(await tg.nativeAccounts());
}));

// 清理诊断页的残留 Go 记录（无对应应用内账号的孤儿记录）。
app.delete("/api/admin/native-accounts/:account", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.deleteOrphanNativeAccount(req.user.id, req.params.account));
}));

app.post("/api/admin/native-accounts/:account/health", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountHealth(req.user.id, req.params.account));
}));

// M2.4：GramJS → Go 一键迁移（路径 A）。
// 失败时显式回传 needsRelogin，前端据此降级到「路径 B：重新登录」。
// 这里自行捕获而不用 asyncRoute，是为了保留 needsRelogin 标记（统一错误中间件会丢弃它）。
app.post("/api/admin/native-accounts/:account/migrate", adminOnly, asyncRoute(async (req, res) => {
  try {
    res.json(await tg.migrateAccountToGo(req.user.id, req.params.account));
  } catch (error) {
    res.status(error.status || 500).json({ error: error.message, needsRelogin: Boolean(error.needsRelogin) });
  }
}));

app.post("/api/admin/native-accounts/:account/login/start", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountLoginStart(req.user.id, req.params.account, req.body || {}));
}));

app.post("/api/admin/native-accounts/:account/login/qr-start", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountQRLoginStart(req.user.id, req.params.account, req.body || {}));
}));

app.post("/api/admin/native-accounts/:account/login/qr-status", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountQRLoginStatus(req.user.id, req.params.account, req.body || {}));
}));

app.post("/api/admin/native-accounts/:account/login/code", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountLoginCode(req.user.id, req.params.account, req.body || {}));
}));

app.post("/api/admin/native-accounts/:account/login/password", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.nativeAccountLoginPassword(req.user.id, req.params.account, req.body || {}));
}));

app.post("/api/admin/cache-speed-diagnostics", adminOnly, asyncRoute(async (req, res) => {
  res.json(await tg.silentCacheSpeedDiagnostics(req.user.id, req.body || {}));
}));

app.get("/api/admin/update-check", adminOnly, asyncRoute(async (_req, res) => {
  res.json(await checkForUpdates());
}));

app.post("/api/admin/users", adminOnly, asyncRoute(async (req, res) => {
  const { username, password, displayName, role } = req.body || {};
  if (!username || !password || String(password).length < 8) {
    res.status(400).json({ error: "请输入飞牛账户和至少 8 位密码" });
    return;
  }
  if (await findUserByUsername(username)) {
    res.status(409).json({ error: "飞牛账户已存在" });
    return;
  }
  const user = await upsertUser({
    id: safeId("user"),
    username: String(username).trim(),
    displayName: displayName || username,
    role: role === "admin" ? "admin" : "user",
    disabled: false,
    passwordHash: hashPassword(password),
    createdAt: new Date().toISOString()
  });
  res.json(publicUser(user));
}));

app.put("/api/admin/users/:id", adminOnly, asyncRoute(async (req, res) => {
  const users = await readUsers();
  const user = users.find((item) => item.id === req.params.id);
  if (!user) {
    res.status(404).json({ error: "飞牛账户不存在" });
    return;
  }
  const next = {
    ...user,
    displayName: req.body.displayName ?? user.displayName,
    role: req.body.role === "admin" ? "admin" : "user",
    disabled: req.body.disabled === undefined ? user.disabled : Boolean(req.body.disabled)
  };
  if (req.body.password) next.passwordHash = hashPassword(req.body.password);
  await upsertUser(next);
  res.json(publicUser(next));
}));

app.get("/api/announcements", asyncRoute(async (_req, res) => res.json(await readAnnouncements())));

app.get("/api/accounts", asyncRoute(async (req, res) => {
  res.json(await tg.listAccounts(req.user.id));
}));

app.delete("/api/accounts/:id", asyncRoute(async (req, res) => {
  await tg.logout(req.user.id, req.params.id);
  res.json({ ok: true });
}));

app.get("/api/chats", asyncRoute(async (req, res) => {
  const includeArchived = req.query.includeArchived === "1" || req.query.includeArchived === "true";
  res.json(await tg.listChats(req.user.id, req.query.account, req.query.query || "", includeArchived));
}));

app.get("/api/chats/:account/:peer/details", asyncRoute(async (req, res) => {
  res.json(await tg.chatDetails(req.user.id, req.params.account, req.params.peer));
}));

app.get("/api/chats/:account/:peer/media", asyncRoute(async (req, res) => {
  res.json(await tg.chatMedia(req.user.id, req.params.account, req.params.peer, {
    before: req.query.before,
    limit: req.query.limit
  }));
}));

app.post("/api/chats/:account/:peer/cache-large-videos", asyncRoute(async (req, res) => {
  res.json(await tg.cacheLargeVideosInChat(req.user.id, req.params.account, req.params.peer, io));
}));

app.get("/api/folders", asyncRoute(async (req, res) => {
  res.json(await tg.listFolders(req.user.id, req.query.account));
}));

app.get("/api/messages", asyncRoute(async (req, res) => {
  res.json(await tg.listMessages(req.user.id, req.query.account, req.query.peer, req.query.limit, req.query.before, req.query.around));
}));

app.post("/api/messages", asyncRoute(async (req, res) => {
  const { account, peer, text } = req.body || {};
  if (!text || !text.trim()) {
    res.status(400).json({ error: "消息不能为空" });
    return;
  }
  res.json(await tg.sendText(req.user.id, account, peer, text.trim()));
}));

app.post("/api/messages/callback", asyncRoute(async (req, res) => {
  const { account, peer, messageId, data } = req.body || {};
  res.json(await tg.clickMessageButton(req.user.id, account, peer, messageId, data));
}));

app.post("/api/resolve-link", asyncRoute(async (req, res) => {
  const { account, url } = req.body || {};
  res.json(await tg.resolveTelegramLink(req.user.id, account, url));
}));

app.get("/api/search", asyncRoute(async (req, res) => {
  res.json(await tg.search(req.user.id, req.query.account, req.query.query || ""));
}));

app.get("/api/avatar/:account/:peer?", asyncRoute(async (req, res) => {
  const avatar = await tg.profilePhoto(req.user.id, req.params.account, req.params.peer || "__self");
  res.setHeader("Content-Type", avatar.contentType);
  res.setHeader("Cache-Control", "private, max-age=86400");
  // M4.2：Go 原生头像直接是内存缓冲，不经磁盘缓存。
  if (avatar.buffer) {
    res.send(avatar.buffer);
    return;
  }
  res.sendFile(avatar.filePath);
}));

app.get("/api/media/:account/:peer/:messageId", asyncRoute(async (req, res) => {
  if (req.query.inline === "1" && await tg.streamVideoMedia(req.user.id, req.params.account, req.params.peer, req.params.messageId, req.headers.range, res)) {
    return;
  }
  const media = await tg.downloadMedia(req.user.id, req.params.account, req.params.peer, req.params.messageId);
  const range = req.headers.range;
  if (media.blobUrl) {
    await tg.proxyBlob(res, media.blobUrl, { inline: req.query.inline === "1", fileName: media.fileName, range });
    return;
  }
  if (media.buffer) {
    res.setHeader("Content-Type", media.contentType);
    res.setHeader("Content-Length", media.size);
    res.setHeader("Cache-Control", media.cacheable ? "private, max-age=86400" : "no-store");
    res.setHeader("Content-Disposition", `${req.query.inline === "1" ? "inline" : "attachment"}; filename="${encodeURIComponent(media.fileName)}"`);
    res.send(media.buffer);
    return;
  }
  if (req.query.inline === "1") {
    if (range) {
      const [startText, endText] = range.replace(/bytes=/, "").split("-");
      const start = Number(startText);
      const end = Math.min(endText ? Number(endText) : media.size - 1, media.size - 1);
      if (Number.isFinite(start) && Number.isFinite(end) && start <= end) {
        res.writeHead(206, {
          "Content-Range": `bytes ${start}-${end}/${media.size}`,
          "Accept-Ranges": "bytes",
          "Content-Length": end - start + 1,
          "Content-Type": media.contentType,
          "Cache-Control": "private, max-age=86400"
        });
        fs.createReadStream(media.filePath, { start, end }).pipe(res);
        return;
      }
    }
    res.setHeader("Content-Type", media.contentType);
    res.setHeader("Content-Disposition", `inline; filename="${encodeURIComponent(media.fileName)}"`);
    res.setHeader("Accept-Ranges", "bytes");
    res.setHeader("Content-Length", media.size);
    res.setHeader("Cache-Control", "private, max-age=86400");
    res.sendFile(media.filePath);
    return;
  }
  res.download(media.filePath, media.fileName);
}));

app.get("/api/media/:account/:peer/:messageId/thumbnail", asyncRoute(async (req, res) => {
  const thumb = await tg.mediaThumbnail(req.user.id, req.params.account, req.params.peer, req.params.messageId);
  if (thumb.blobUrl) {
    await tg.proxyBlob(res, thumb.blobUrl, { inline: true, fileName: thumb.fileName });
    return;
  }
  res.setHeader("Content-Type", thumb.contentType);
  res.setHeader("Cache-Control", "private, max-age=86400");
  res.sendFile(thumb.filePath);
}));

app.post("/api/media/:account/:peer/:messageId/cache", asyncRoute(async (req, res) => {
  res.json(await tg.startDownloadTask(req.user.id, req.params.account, req.params.peer, req.params.messageId, io));
}));

app.get("/api/downloads", asyncRoute(async (req, res) => {
  res.json(await tg.listDownloadTasks(req.user.id));
}));

app.get("/api/silent-cache", asyncRoute(async (req, res) => {
  res.json(await tg.silentCacheState(req.user.id));
}));

app.put("/api/silent-cache/control", asyncRoute(async (req, res) => {
  res.json(await tg.setSilentCacheControl(req.user.id, req.body || {}, io));
}));

app.post("/api/silent-cache/reorder", asyncRoute(async (req, res) => {
  res.json(await tg.reorderSilentCacheTasks(req.user.id, req.body?.orderedIds || [], io));
}));

app.post("/api/silent-cache/cancel", asyncRoute(async (req, res) => {
  res.json(await tg.cancelSilentCacheTasks(req.user.id, req.body?.ids || [], io));
}));

app.delete("/api/silent-cache/:id", asyncRoute(async (req, res) => {
  res.json(await tg.cancelSilentCacheTask(req.user.id, req.params.id, io));
}));

app.post("/api/downloads/:id/start", asyncRoute(async (req, res) => {
  res.json(await tg.resumeDownloadTask(req.user.id, req.params.id, io));
}));

app.post("/api/downloads/:id/cancel", asyncRoute(async (req, res) => {
  res.json(await tg.cancelDownloadTask(req.user.id, req.params.id, io));
}));

app.post("/api/downloads/:id/clear", asyncRoute(async (req, res) => {
  res.json(await tg.clearDownloadTask(req.user.id, req.params.id, io));
}));

app.delete("/api/downloads/:id", asyncRoute(async (req, res) => {
  res.json(await tg.deleteDownloadTask(req.user.id, req.params.id, io));
}));

app.use(express.static(path.join(__dirname, "..", "public")));
app.get("*", (_req, res) => {
  res.sendFile(path.join(__dirname, "..", "public", "index.html"));
});

io.use(async (socket, next) => {
  const token = socket.handshake.auth?.token || "";
  const user = await verifyUserToken(token);
  if (user) {
    socket.user = user;
    next();
    return;
  }
  next(new Error("未授权"));
});

io.on("connection", (socket) => {
  socket.join(`user:${socket.user.id}`);

  socket.on("account:join", (accountId) => {
    if (accountId) socket.join(`account:${accountId}`);
  });

  socket.on("login:start", async (payload, reply) => {
    try {
      reply({ ok: true, data: await tg.startLogin(socket.user.id, payload || {}) });
    } catch (error) {
      reply({ ok: false, error: error.message });
    }
  });

  socket.on("login:code", async (payload, reply) => {
    try {
      reply({ ok: true, data: await tg.completeCode(payload || {}, io) });
    } catch (error) {
      reply({ ok: false, error: error.message });
    }
  });

  socket.on("login:password", async (payload, reply) => {
    try {
      reply({ ok: true, data: await tg.completePassword(payload || {}, io) });
    } catch (error) {
      reply({ ok: false, error: error.message });
    }
  });

  // M2.4：一键迁移到 Go（路径 A）。
  // 失败时回传 needsRelogin，前端据此降级为「路径 B：重新登录」。
  socket.on("account:migrate", async (payload, reply) => {
    try {
      const accountId = payload && payload.accountId;
      if (!accountId) throw Object.assign(new Error("缺少 accountId"), { status: 400 });
      reply({ ok: true, data: await tg.migrateAccountToGo(socket.user.id, accountId) });
    } catch (error) {
      reply({ ok: false, error: error.message, needsRelogin: Boolean(error.needsRelogin) });
    }
  });
});

app.use((error, _req, res, _next) => {
  console.error(error);
  res.status(error.status || 500).json({ error: error.message || "服务器错误" });
});

ensureStore()
  .then(() => migrateStore())
  .then(() => {
    setInterval(() => {
      tg.cleanupCache().catch((error) => console.warn("Cache cleanup failed:", error.message));
    }, 24 * 60 * 60 * 1000).unref?.();
    setInterval(() => {
      try {
        tg.monitorSilentCacheTasks(io);
      } catch (error) {
        console.warn("Silent cache monitor failed:", error.message);
      }
    }, 30 * 1000).unref?.();
    server.listen(port, "0.0.0.0", () => {
      console.log(`Feigram Public is listening on http://0.0.0.0:${port}`);
      // R4.1：启动账号状态巡检，状态变化经 socket 推给对应用户。
      const healthMonitor = startDefaultMonitor(io);
      healthMonitor.start();
      // R4.2：有账号 healthy 后自动把媒体源从 HTTP 桥接升级到 Go 原生 MTProto。
      const transportUpgrade = startDefaultAutoUpgrade({
        io,
        state: downloaderSidecar.state,
        nativeAccounts: downloaderSidecar.nativeAccounts,
        updateConfig: downloaderSidecar.updateConfig
      });
      const upgradeTimer = setInterval(() => {
        transportUpgrade.check().catch((error) =>
          console.warn("Transport auto-upgrade failed:", error.message));
      }, 30 * 1000);
      upgradeTimer.unref?.();
      // 把应用内代理下发给 Go（sidecar 通常还在启动中，故带重试）。
      readSettings()
        .then((settings) => pushProxyToSidecar(settings, { attempts: 8, delayMs: 2500 }))
        .catch((error) => console.warn(`[proxy] 读取设置失败：${error.message}`));
      tg.restoreBackgroundTasks(io)
        .catch((error) => console.warn("Download task restore failed:", error.message))
        .then(() => tg.cleanupCache().catch((error) => console.warn("Cache cleanup failed:", error.message)));
    });
  })
  .catch((error) => {
    console.error(error);
    process.exit(1);
  });
