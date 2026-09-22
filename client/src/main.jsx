import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { io } from "socket.io-client";
import {
  Bell,
  Download,
  ExternalLink,
  Folder,
  Info,
  LogOut,
  MessageSquare,
  Moon,
  Play,
  Plus,
  RefreshCw,
  Search,
  Send,
  Settings,
  Shield,
  SlidersHorizontal,
  Sun,
  ArrowLeft,
  Trash2,
  UserRound,
  Users,
  X
} from "lucide-react";
import { api, appLogin, getToken, setToken as saveToken } from "./api";
import "./styles/tokens.css";
import "./styles/app.css";

function cx(...items) {
  return items.filter(Boolean).join(" ");
}

/*
 * fnOS 主题引导（04-feigram-ui-upgrade.md §7）：
 * ?theme=dark|light → localStorage 手动选择 → fnOS 注入的 cookie → 系统偏好 → 亮色兜底。
 */
function resolveInitialTheme() {
  const fromQuery = new URLSearchParams(window.location.search).get("theme");
  if (fromQuery === "dark" || fromQuery === "light") return fromQuery;
  const stored = localStorage.getItem("feigrame.theme");
  if (stored === "dark" || stored === "light") return stored;
  const cookie = (document.cookie.match(/fnos_theme=(\w+)/) || [])[1];
  if (cookie === "dark" || cookie === "light") return cookie;
  if (window.matchMedia?.("(prefers-color-scheme: dark)").matches) return "dark";
  return "light";
}

function formatTime(value) {
  if (!value) return "";
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(new Date(value));
}

function formatBytes(value) {
  const size = Number(value || 0);
  if (!size) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const index = Math.min(units.length - 1, Math.floor(Math.log(size) / Math.log(1024)));
  return `${(size / (1024 ** index)).toFixed(index ? 1 : 0)} ${units[index]}`;
}

function formatDuration(value) {
  const seconds = Math.max(0, Math.round(Number(value || 0)));
  if (!seconds) return "";
  const mins = Math.floor(seconds / 60);
  const secs = seconds % 60;
  const hours = Math.floor(mins / 60);
  const restMins = mins % 60;
  if (hours) return `${hours}:${String(restMins).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
  return `${mins}:${String(secs).padStart(2, "0")}`;
}

function downloadDisplayKey(item) {
  const mediaKey = `${item.accountId}:${item.peerId}:${item.messageId}`;
  if (item.fileName && item.size) return `${item.accountId}:${item.peerId}:${item.fileName}:${item.size}`;
  return mediaKey;
}

function mergeDownloads(items) {
  return [...items.reduce((map, item) => {
    const key = item.status === "completed" ? downloadDisplayKey(item) : `${item.accountId}:${item.peerId}:${item.messageId}`;
    const existing = map.get(key);
    if (!existing || String(item.updatedAt).localeCompare(String(existing.updatedAt)) > 0 || item.status === "completed") {
      map.set(key, item);
    }
    return map;
  }, new Map()).values()].sort((a, b) => String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt)));
}

function sortSilentCaches(items) {
  return [...items].sort((a, b) => {
    const orderDiff = Number(a.order || 0) - Number(b.order || 0);
    if (orderDiff) return orderDiff;
    return String(b.createdAt || b.updatedAt).localeCompare(String(a.createdAt || a.updatedAt));
  });
}

function normalizeLink(value) {
  if (value.startsWith("@")) return `https://t.me/${value.slice(1)}`;
  if (value.startsWith("t.me/")) return `https://${value}`;
  return value;
}

function avatarUrl(accountId, peerId) {
  if (!accountId) return "";
  const path = peerId ? `/api/avatar/${accountId}/${encodeURIComponent(peerId)}` : `/api/avatar/${accountId}`;
  return `${path}?token=${encodeURIComponent(getToken())}`;
}

function Avatar({ accountId, peerId, label, size = 40 }) {
  const [failed, setFailed] = useState(false);
  const initials = (label || "?").trim().slice(0, 1).toUpperCase();
  if (!accountId || failed) {
    return <span className="avatar" style={{ height: size, width: size }}>{initials || <UserRound size={18} />}</span>;
  }
  return (
    <span className="avatar avatar-image" style={{ height: size, width: size }}>
      <img src={avatarUrl(accountId, peerId)} alt={label || "avatar"} loading="lazy" onError={() => setFailed(true)} />
    </span>
  );
}

function LinkAnchor({ href, children, onOpenLink }) {
  const normalized = normalizeLink(href);
  return (
    <a
      href={normalized}
      onClick={(event) => {
        if (!onOpenLink) return;
        event.preventDefault();
        onOpenLink(normalized);
      }}
      target="_blank"
      rel="noreferrer"
    >
      {children}<ExternalLink size={12} />
    </a>
  );
}

function renderPlainLinks(text, keyPrefix, onOpenLink) {
  const pattern = /(https?:\/\/[^\s]+|tg:\/\/[^\s]+|t\.me\/[^\s]+|@[A-Za-z0-9_]{5,32})/g;
  const parts = text.split(pattern);
  return parts.map((part, index) => {
    if (!part.match(pattern)) return <React.Fragment key={`${keyPrefix}-plain-${index}`}>{part}</React.Fragment>;
    const trailing = part.match(/[，。！？、；：,.!?;:)）】]+$/)?.[0] || "";
    const clean = trailing ? part.slice(0, -trailing.length) : part;
    return (
      <React.Fragment key={`${keyPrefix}-link-${index}`}>
        <LinkAnchor href={clean} onOpenLink={onOpenLink}>{clean}</LinkAnchor>
        {trailing}
      </React.Fragment>
    );
  });
}

function MessageText({ text, entities = [], onOpenLink }) {
  if (!text) return null;
  const links = entities
    .filter((entity) => entity.url && Number(entity.length) > 0)
    .sort((a, b) => a.offset - b.offset);
  const rendered = [];
  let cursor = 0;
  links.forEach((entity, index) => {
    const start = Number(entity.offset);
    const end = start + Number(entity.length);
    if (start < cursor || start > text.length) return;
    if (start > cursor) rendered.push(...renderPlainLinks(text.slice(cursor, start), `pre-${index}`, onOpenLink));
    const label = text.slice(start, Math.min(end, text.length));
    rendered.push(<LinkAnchor key={`entity-${index}`} href={entity.url} onOpenLink={onOpenLink}>{label}</LinkAnchor>);
    cursor = Math.min(end, text.length);
  });
  if (cursor < text.length) rendered.push(...renderPlainLinks(text.slice(cursor), "tail", onOpenLink));
  return (
    <p className="message-text">
      {rendered}
    </p>
  );
}

function mediaUrl(accountId, chatId, messageId, inline = false) {
  const params = new URLSearchParams({ token: getToken() });
  if (inline) params.set("inline", "1");
  return `/api/media/${accountId}/${encodeURIComponent(chatId)}/${messageId}?${params.toString()}`;
}

function thumbnailMediaUrl(accountId, chatId, messageId) {
  const params = new URLSearchParams({ token: getToken() });
  return `/api/media/${accountId}/${encodeURIComponent(chatId)}/${messageId}/thumbnail?${params.toString()}`;
}

function FeigramVideo({ src, onError }) {
  const ref = useRef(null);
  useEffect(() => {
    const video = ref.current;
    if (!video) return undefined;
    video.src = src;
    return undefined;
  }, [src]);
  return <video ref={ref} controls autoPlay preload="metadata" playsInline onError={onError} />;
}

function MessageMedia({ accountId, chatId, message, compact = false, onCache, task, playerMode = "browser" }) {
  const [failed, setFailed] = useState(false);
  const [active, setActive] = useState(false);
  const [localStatus, setLocalStatus] = useState("");
  const media = message.media;
  if (!media) return null;
  const previewUrl = mediaUrl(accountId, chatId, message.id, true);
  const thumbUrl = thumbnailMediaUrl(accountId, chatId, message.id);
  const downloadUrl = mediaUrl(accountId, chatId, message.id);
  const label = media.fileName || media.mimeType || "下载媒体";
  const cacheStatus = task?.status || localStatus;
  const cacheLabel = cacheStatus === "completed" ? "已缓存" : cacheStatus === "downloading" ? "下载中" : cacheStatus === "queued" ? "排队中" : cacheStatus === "cancelled" ? "继续缓存" : "缓存";
  if (media.kind === "image") {
    return (
      <a className={cx("media-preview image-preview", compact && "compact-media")} href={previewUrl} target="_blank" rel="noreferrer" title="打开原图">
        <img src={previewUrl} alt={label} loading="lazy" />
      </a>
    );
  }
  if (media.kind === "video") {
    const ratio = media.width && media.height ? `${media.width} / ${media.height}` : "16 / 9";
    const orientation = Number(media.height || 0) > Number(media.width || 0) ? "portrait" : "landscape";
    return (
      <div className={cx("media-preview", "video-preview", orientation, compact && "compact-media")} style={{ "--video-ratio": ratio }}>
        <div className="video-stage">
          <button
            className={cx("video-cache-button", cacheStatus === "completed" && "done", cacheStatus === "downloading" && "busy")}
            type="button"
            disabled={cacheStatus === "downloading" || cacheStatus === "queued"}
            onClick={async (event) => {
              event.stopPropagation();
              setLocalStatus("queued");
              try {
                const result = await onCache?.(message);
                setLocalStatus(result?.status || "queued");
                setFailed(false);
              } catch {
                setLocalStatus("");
              }
            }}
            title={cacheStatus === "completed" ? "已缓存到下载目录" : "缓存视频到下载列表"}
          >
            <Download size={14} />{cacheLabel}
          </button>
          {!active && !failed && playerMode !== "local" ? <button className="video-load-button" type="button" onClick={() => setActive(true)}>
            <img src={thumbUrl} alt="" loading="lazy" onError={(event) => { event.currentTarget.style.display = "none"; }} />
            <span><Play size={18} />点击播放视频</span>
            {!!media.duration && <b>{formatDuration(media.duration)}</b>}
          </button> : null}
          {active && !failed && playerMode !== "local" ? <FeigramVideo src={previewUrl} onError={() => setFailed(true)} /> : null}
          {playerMode === "local" ? <a className="video-load-button local-player-link" href={downloadUrl}>下载后用本地播放器打开</a> : null}
          {failed ? <div className="video-fallback">当前视频编码无法直接在线播放，请先缓存后下载到本地播放。</div> : null}
        </div>
      </div>
    );
  }
  return <a className="media-chip" href={downloadUrl}><Download size={15} />{label}</a>;
}

function buildMessageItems(messages) {
  const items = [];
  for (let index = 0; index < messages.length; index += 1) {
    const message = messages[index];
    if (message.groupedId && message.media) {
      const group = [message];
      while (index + 1 < messages.length && messages[index + 1].groupedId === message.groupedId && messages[index + 1].media) {
        group.push(messages[index + 1]);
        index += 1;
      }
      items.push({ type: "group", id: `group-${message.groupedId}-${message.id}`, messages: group });
    } else {
      items.push({ type: "message", id: `${message.id}-${message.date}`, message });
    }
  }
  return items;
}

function MessageBubble({ item, accountId, chatId, showSender, showMedia, playerMode, onOpenLink, onInlineButton, onCacheMedia, downloadTasks, highlighted = false, messageRef }) {
  const messages = item.type === "group" ? item.messages : [item.message];
  const first = messages[0];
  const caption = messages.find((message) => message.text) || first;
  const taskFor = (message) => downloadTasks.find((task) => (
    task.accountId === accountId && task.peerId === chatId && Number(task.messageId) === Number(message.id)
  ));
  return (
    <article ref={messageRef} className={cx("message-row", first.outgoing && "mine", highlighted && "jump-highlight")}>
      {showSender && !first.outgoing && <Avatar accountId={accountId} peerId={first.sender?.id} label={first.sender?.title || first.senderId} size={34} />}
      <div className={cx("bubble", first.outgoing && "mine", item.type === "group" && "media-group-bubble")}>
        {showSender && !first.outgoing && <div className="sender-line">{first.sender?.title || first.senderId}<span>{first.sender?.username ? `@${first.sender.username}` : first.senderId}</span></div>}
        {caption.text && <MessageText text={caption.text} entities={caption.entities} onOpenLink={onOpenLink} />}
        {showMedia && item.type === "group" ? <div className={cx("media-grid", messages.length > 1 && "multi")}>
          {messages.map((message) => <MessageMedia key={message.id} accountId={accountId} chatId={chatId} message={message} compact onCache={onCacheMedia} task={taskFor(message)} playerMode={playerMode} />)}
        </div> : showMedia && <MessageMedia accountId={accountId} chatId={chatId} message={first} onCache={onCacheMedia} task={taskFor(first)} playerMode={playerMode} />}
        {!!caption.buttons?.length && <div className="inline-buttons">
          {caption.buttons.map((row, rowIndex) => <div className="inline-button-row" key={`${caption.id}-row-${rowIndex}`}>
            {row.map((button, buttonIndex) => <button type="button" key={`${button.text}-${buttonIndex}`} onClick={() => onInlineButton(caption, button)} disabled={button.type === "unsupported"}>
              {button.text || "按钮"}
            </button>)}
          </div>)}
        </div>}
        <time>{formatTime(messages[messages.length - 1].date)}</time>
      </div>
    </article>
  );
}

function useSocket(token) {
  return useMemo(() => token ? io("/", { auth: { token } }) : null, [token]);
}

function AuthGate({ onReady }) {
  const [mode, setMode] = useState("checking");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api("/api/bootstrap/status")
      .then((state) => setMode(state.required ? "bootstrap" : "login"))
      .catch(() => setMode("login"));
  }, []);

  async function submit(event) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const endpoint = mode === "bootstrap" ? "/api/bootstrap" : "/api/login";
      const result = mode === "bootstrap"
        ? await api(endpoint, { method: "POST", body: JSON.stringify({ username, password }) })
        : await appLogin({ username, password });
      saveToken(result.token);
      onReady(result.token, result.user);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  if (mode === "checking") return <main className="auth-page"><div className="auth-panel">加载中</div></main>;

  return (
    <main className="auth-page">
      <form className="auth-panel" onSubmit={submit}>
        <div className="brand-row">
          <div className="brand-mark"><MessageSquare size={24} /></div>
          <div>
            <h1>Feigram</h1>
            <p>{mode === "bootstrap" ? "创建第一个管理员" : "公开版登录"}</p>
          </div>
        </div>
        <label><span>飞牛账户</span><input autoFocus value={username} onChange={(e) => setUsername(e.target.value)} required /></label>
        <label><span>密码</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={8} required /></label>
        {error && <p className="error">{error}</p>}
        <button className="primary" disabled={busy}><Shield size={18} />{busy ? "处理中" : mode === "bootstrap" ? "创建管理员" : "登录"}</button>
      </form>
    </main>
  );
}

function AccountLogin({ socket, onDone, legalNotice }) {
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState("phone");
  const [label, setLabel] = useState("");
  const [phoneNumber, setPhoneNumber] = useState("");
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [loginId, setLoginId] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  function call(event, payload) {
    return new Promise((resolve, reject) => {
      if (!socket?.connected) {
        reject(new Error("实时连接未建立，请刷新页面后重试"));
        return;
      }
      socket.timeout(20000).emit(event, payload, (err, reply) => {
        if (err) {
          reject(new Error("请求超时，请检查 Telegram API 配置、网络或稍后重试"));
          return;
        }
        reply?.ok ? resolve(reply.data) : reject(new Error(reply?.error || "操作失败"));
      });
    });
  }

  async function start(event) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const data = await call("login:start", { label, phoneNumber });
      setLoginId(data.loginId);
      setStep("code");
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function submitCode(event) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const data = await call("login:code", { loginId, code });
      if (data.passwordRequired) setStep("password");
      else {
        setOpen(false);
        onDone(data.account);
      }
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function submitPassword(event) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const data = await call("login:password", { loginId, password });
      setOpen(false);
      onDone(data.account);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  if (!open) return <button className="secondary action-button" onClick={() => setOpen(true)} title="添加 Telegram 账号"><Plus size={18} />添加 Telegram 账号</button>;

  return (
    <div className="modal-backdrop">
      <div className="modal">
        <button className="close" onClick={() => setOpen(false)} title="关闭"><X size={18} /></button>
        <h2>登录 Telegram</h2>
        {legalNotice ? <p className="legal-notice">{legalNotice}</p> : null}
        {step === "phone" && <form onSubmit={start} className="stack">
          <label><span>账号名称</span><input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="我的账号" /></label>
          <label><span>手机号</span><input value={phoneNumber} onChange={(e) => setPhoneNumber(e.target.value)} placeholder="+86..." required /></label>
          <button className="primary" disabled={busy}><Send size={18} />{busy ? "发送中" : "发送验证码"}</button>
        </form>}
        {step === "code" && <form onSubmit={submitCode} className="stack">
          <label><span>验证码</span><input value={code} onChange={(e) => setCode(e.target.value)} required /></label>
          <button className="primary" disabled={busy}><Send size={18} />{busy ? "验证中" : "完成登录"}</button>
        </form>}
        {step === "password" && <form onSubmit={submitPassword} className="stack">
          <label><span>两步验证密码</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required /></label>
          <button className="primary" disabled={busy}><Shield size={18} />{busy ? "验证中" : "验证密码"}</button>
        </form>}
        {error && <p className="error">{error}</p>}
      </div>
    </div>
  );
}

function AdminPanel({ accounts, accountId, canAdmin, onAccountChange, onAccountLogout, onAccountsChanged, onSettingsChanged, open, onClose, initialTab = "accounts", socket, silentCacheState, silentCaches = [], onRefreshSilentCaches, onSilentCacheControl, onCancelSilentCache }) {
  const [tab, setTab] = useState(initialTab);
  const [users, setUsers] = useState([]);
  const [settings, setSettings] = useState({
    publicBaseUrl: "",
    telegramApiId: "",
    telegramApiHash: "",
    telegramLegalNotice: "",
    cacheBaseDir: "",
    imageCacheDir: "",
    videoCacheDir: "",
    fileCacheDir: "",
    cacheRetentionDays: "30",
    notificationEnabled: true,
    notificationPreview: true,
    privacyOpenTelegramLinksInApp: true,
    privacyMediaPreview: true,
    messageShowSender: true,
    foldersEnabled: true,
    foldersShowArchived: false,
    foldersAutoSelectFirst: true,
    playerMode: "browser",
    downloaderEngine: "go-sidecar",
    downloaderSidecarUrl: "http://127.0.0.1:3090"
  });
  const [apiIdPlaceholder, setApiIdPlaceholder] = useState("");
  const [hashPlaceholder, setHashPlaceholder] = useState("");
  const [diagnostics, setDiagnostics] = useState(null);
  const [downloaderState, setDownloaderState] = useState(null);
  const [nativeAccounts, setNativeAccounts] = useState([]);
  const [nativeQr, setNativeQr] = useState(null);
  const [cacheSpeedTest, setCacheSpeedTest] = useState(null);
  const [cacheSpeedTesting, setCacheSpeedTesting] = useState(false);
  const [updateInfo, setUpdateInfo] = useState(null);
  const [newUser, setNewUser] = useState({ username: "", password: "", displayName: "", role: "user" });
  const [dragSilentId, setDragSilentId] = useState("");
  const [selectedSilentIds, setSelectedSilentIds] = useState([]);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState("");
  const selectedSilentSet = useMemo(() => new Set(selectedSilentIds), [selectedSilentIds]);

  useEffect(() => {
    if (!open) return;
    const aliases = {
      users: "accounts",
      settings: "server",
      cache: "server",
      player: "server",
      folders: "privacy",
      notifications: "privacy",
      "silent-cache": "cache-info"
    };
    setTab(aliases[initialTab] || initialTab);
    refresh();
  }, [open, initialTab]);

  useEffect(() => {
    setSelectedSilentIds((ids) => ids.filter((id) => silentCaches.some((task) => task.id === id)));
  }, [silentCaches]);

  async function refresh() {
    setError("");
    if (canAdmin) {
      setUsers(await api("/api/admin/users").catch((err) => { setError(err.message); return []; }));
      const nextSettings = await api("/api/settings").catch(() => ({}));
      setSettings({
        publicBaseUrl: nextSettings.publicBaseUrl || "",
        telegramApiId: "",
        telegramLegalNotice: nextSettings.telegramLegalNotice || "",
        telegramApiHash: "",
        cacheBaseDir: nextSettings.cacheBaseDir || "",
        imageCacheDir: nextSettings.imageCacheDir || "",
        videoCacheDir: nextSettings.videoCacheDir || "",
        fileCacheDir: nextSettings.fileCacheDir || "",
        cacheRetentionDays: nextSettings.cacheRetentionDays || "30",
        notificationEnabled: nextSettings.notificationEnabled !== false,
        notificationPreview: nextSettings.notificationPreview !== false,
        privacyOpenTelegramLinksInApp: nextSettings.privacyOpenTelegramLinksInApp !== false,
        privacyMediaPreview: nextSettings.privacyMediaPreview !== false,
        messageShowSender: nextSettings.messageShowSender !== false,
        foldersEnabled: nextSettings.foldersEnabled !== false,
        foldersShowArchived: Boolean(nextSettings.foldersShowArchived),
        foldersAutoSelectFirst: nextSettings.foldersAutoSelectFirst !== false,
        playerMode: nextSettings.playerMode || "browser",
        downloaderEngine: nextSettings.downloaderEngine || "go-sidecar",
        downloaderSidecarUrl: nextSettings.downloaderSidecarUrl || "http://127.0.0.1:3090"
      });
      setApiIdPlaceholder(nextSettings.telegramApiIdSet ? "已保存，留空则不修改" : "请输入 Telegram API ID");
      setHashPlaceholder(nextSettings.telegramApiHashSet ? "已保存，留空则不修改" : "请输入 Telegram API Hash");
      setNativeAccounts(await api("/api/admin/native-accounts").catch(() => []));
    }
  }

  async function createUser(event) {
    event.preventDefault();
    setError("");
    try {
      await api("/api/admin/users", { method: "POST", body: JSON.stringify(newUser) });
      setNewUser({ username: "", password: "", displayName: "", role: "user" });
      refresh();
    } catch (err) {
      setError(err.message);
    }
  }

  async function updateUser(user, patch) {
    setError("");
    try {
      await api(`/api/admin/users/${user.id}`, { method: "PUT", body: JSON.stringify({ ...user, ...patch }) });
      refresh();
    } catch (err) {
      setError(err.message);
    }
  }

  async function saveSettings(event) {
    event.preventDefault();
    setSaved("");
    setError("");
    try {
      const payload = { ...settings };
      if (!payload.telegramApiId) delete payload.telegramApiId;
      if (!payload.telegramApiHash) delete payload.telegramApiHash;
      await api("/api/settings", { method: "PUT", body: JSON.stringify(payload) });
      setSaved("已保存");
      onSettingsChanged?.();
      refresh();
    } catch (err) {
      setError(err.message);
    }
  }

  async function loadDiagnostics() {
    setError("");
    setDiagnostics(await api("/api/admin/diagnostics").catch((err) => {
      setError(err.message);
      return null;
    }));
    setDownloaderState(await api("/api/admin/downloader").catch(() => null));
    setNativeAccounts(await api("/api/admin/native-accounts").catch(() => []));
  }

  async function refreshNativeAccounts() {
    if (!canAdmin) return;
    setNativeAccounts(await api("/api/admin/native-accounts").catch(() => []));
  }

  async function checkNativeAccount(accountId) {
    setError("");
    const result = await api(`/api/admin/native-accounts/${accountId}/health`, { method: "POST" }).catch((err) => {
      setError(err.message);
      return null;
    });
    if (result) {
      setNativeAccounts((items) => items.map((item) => item.accountId === result.accountId ? result : item));
      setDownloaderState(await api("/api/admin/downloader").catch(() => downloaderState));
    }
  }

  async function reloginNativeAccount(item) {
    setError("");
    const phone = prompt("输入用于 Go 原生 MTProto 登录的手机号", item.phone || "");
    if (!phone) return;
    try {
      const started = await api(`/api/admin/native-accounts/${item.accountId}/login/start`, {
        method: "POST",
        body: JSON.stringify({ phone })
      });
      const loginId = started.loginId;
      if (started.done) {
        setNativeAccounts((items) => items.map((entry) => entry.accountId === started.account.accountId ? started.account : entry));
        setDownloaderState(await api("/api/admin/downloader").catch(() => downloaderState));
        return;
      }
      const code = prompt("输入 Telegram 验证码");
      if (!code) return;
      const coded = await api(`/api/admin/native-accounts/${item.accountId}/login/code`, {
        method: "POST",
        body: JSON.stringify({ loginId, code })
      });
      let result = coded;
      if (coded.passwordRequired) {
        const password = prompt("该账号启用了两步验证，请输入 Telegram 云密码");
        if (!password) return;
        result = await api(`/api/admin/native-accounts/${item.accountId}/login/password`, {
          method: "POST",
          body: JSON.stringify({ loginId, password })
        });
      }
      if (result.account) {
        setNativeAccounts((items) => items.map((entry) => entry.accountId === result.account.accountId ? result.account : entry));
      }
      setDownloaderState(await api("/api/admin/downloader").catch(() => downloaderState));
    } catch (err) {
      setError(err.message);
    }
  }

  // M2.4：路径 A —— 一键把 GramJS session 迁移到 Go 原生 MTProto。
  // 迁移失败（含 Go 侧健康检查未通过）自动降级「路径 B」，走 reloginNativeAccount 重新登录。
  async function migrateAccountToGo(account) {
    setError("");
    try {
      await api(`/api/admin/native-accounts/${account.id}/migrate`, { method: "POST", body: JSON.stringify({}) });
      setNativeAccounts(await api("/api/admin/native-accounts").catch(() => nativeAccounts));
      setDownloaderState(await api("/api/admin/downloader").catch(() => downloaderState));
      await onAccountsChanged?.();
    } catch (err) {
      setError(`迁移失败：${err.message}`);
      if (err.needsRelogin) {
        await reloginNativeAccount({
          accountId: account.id,
          phone: account.phoneNumber,
          displayName: account.displayName || account.label
        });
      }
    }
  }

  async function startNativeQrLogin(item) {
    setError("");
    try {
      const started = await api(`/api/admin/native-accounts/${item.accountId}/login/qr-start`, { method: "POST", body: JSON.stringify({}) });
      setNativeQr({ ...started, accountId: item.accountId, title: item.displayName || item.phone || item.accountId });
    } catch (err) {
      setError(err.message);
    }
  }

  useEffect(() => {
    if (!nativeQr?.loginId || nativeQr.done || nativeQr.status === "error") return undefined;
    let cancelled = false;
    let timer = null;
    const poll = async () => {
      try {
        const result = await api(`/api/admin/native-accounts/${nativeQr.accountId}/login/qr-status`, {
          method: "POST",
          body: JSON.stringify({ loginId: nativeQr.loginId })
        });
        if (cancelled) return;
        setNativeQr((current) => current?.loginId === result.loginId ? { ...current, ...result } : current);
        if (result.account) {
          setNativeAccounts((items) => items.map((entry) => entry.accountId === result.account.accountId ? result.account : entry));
          setDownloaderState(await api("/api/admin/downloader").catch(() => downloaderState));
        }
      } catch (err) {
        if (!cancelled) setNativeQr((current) => current ? { ...current, status: "waiting-scan", error: "Telegram 连接波动，正在自动重试" } : current);
      } finally {
        if (!cancelled) timer = window.setTimeout(poll, 4000);
      }
    };
    timer = window.setTimeout(poll, 1200);
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [nativeQr?.loginId, nativeQr?.accountId, nativeQr?.done, nativeQr?.status]);

  async function saveDownloaderConfig(patch) {
    setError("");
    const result = await api("/api/admin/downloader/config", {
      method: "PUT",
      body: JSON.stringify(patch)
    }).catch((err) => {
      setError(err.message);
      return null;
    });
    if (result) setDownloaderState(result);
  }

  async function checkUpdates() {
    setError("");
    setUpdateInfo(await api("/api/admin/update-check").catch((err) => {
      setError(err.message);
      return null;
    }));
  }

  async function runCacheSpeedTest(forceProbe = false) {
    setError("");
    setCacheSpeedTesting(true);
    setCacheSpeedTest(await api("/api/admin/cache-speed-diagnostics", {
      method: "POST",
      body: JSON.stringify({ sampleBytes: 1024 * 1024, forceProbe })
    }).catch((err) => {
      setError(err.message);
      return null;
    }));
    setCacheSpeedTesting(false);
  }

  function toggleSilentSelection(id, checked) {
    setSelectedSilentIds((ids) => {
      const set = new Set(ids);
      if (checked) set.add(id);
      else set.delete(id);
      return [...set];
    });
  }

  async function cancelSelectedSilentCaches() {
    if (!selectedSilentIds.length) return;
    setError("");
    try {
      await api("/api/silent-cache/cancel", {
        method: "POST",
        body: JSON.stringify({ ids: selectedSilentIds })
      });
      setSelectedSilentIds([]);
      onRefreshSilentCaches?.();
    } catch (err) {
      setError(err.message);
    }
  }

  if (!open) return null;

  return (
    <div className="modal-backdrop">
      <div className="modal admin-modal">
        <button className="close" onClick={onClose} title="关闭"><X size={18} /></button>
        <h2>{canAdmin ? "管理员后台" : "账号后台"}</h2>
        <div className="tabs">
          <button className={cx(tab === "accounts" && "active")} onClick={() => setTab("accounts")}>账号管理</button>
          {canAdmin && <button className={cx(tab === "server" && "active")} onClick={() => setTab("server")}>服务端设置</button>}
          {canAdmin && <button className={cx(tab === "cache-info" && "active")} onClick={() => { setTab("cache-info"); onRefreshSilentCaches?.(); }}>缓存信息</button>}
          {canAdmin && <button className={cx(tab === "privacy" && "active")} onClick={() => setTab("privacy")}>隐私设置</button>}
          {canAdmin && <button className={cx(tab === "diagnostics" && "active")} onClick={() => { setTab("diagnostics"); loadDiagnostics(); }}>运行诊断</button>}
        </div>
        {error && <p className="error">{error}</p>}
        {nativeQr && <div className="qr-modal-backdrop" onMouseDown={(event) => event.target === event.currentTarget && setNativeQr(null)}>
          <div className="qr-login-panel" role="dialog" aria-modal="true" aria-label="Telegram App 扫码登录 Go">
            <button className="close mini-close" type="button" onClick={() => setNativeQr(null)} title="关闭"><X size={18} /></button>
            <h3>Telegram App 扫码登录 Go</h3>
            <p>{nativeQr.title || "Telegram 账号"} · {nativeQr.done ? "已授权" : nativeQr.status === "error" ? "登录异常" : "请用 Telegram 手机客户端扫描二维码"}</p>
            {nativeQr.qrImage && !nativeQr.done && <img src={nativeQr.qrImage} alt="Telegram QR login" />}
            {nativeQr.done && <p className="success">Go 原生 MTProto session 已生成，请关闭弹窗后连续执行两次健康检查。</p>}
            {nativeQr.error && <p className="error">{nativeQr.error}</p>}
            {nativeQr.expires && !nativeQr.done && <small>二维码有效期：{formatTime(nativeQr.expires)}，过期会自动刷新。</small>}
          </div>
        </div>}
        {tab === "accounts" && <div className="account-admin">
          {canAdmin && <>
            <h3>飞牛账号管理</h3>
            <form className="admin-grid" onSubmit={createUser}>
              <input placeholder="飞牛账户" value={newUser.username} onChange={(e) => setNewUser({ ...newUser, username: e.target.value })} required />
              <input placeholder="显示名" value={newUser.displayName} onChange={(e) => setNewUser({ ...newUser, displayName: e.target.value })} />
              <input placeholder="初始密码" type="password" value={newUser.password} onChange={(e) => setNewUser({ ...newUser, password: e.target.value })} minLength={8} required />
              <select value={newUser.role} onChange={(e) => setNewUser({ ...newUser, role: e.target.value })}><option value="user">user</option><option value="admin">admin</option></select>
              <button className="primary"><Plus size={16} />创建</button>
            </form>
            <div className="user-list compact-section">
              {users.map((user) => <div className="user-row" key={user.id}>
                <strong>{user.displayName}</strong>
                <span>{user.username}</span>
                <span>{user.role}</span>
                <button className="icon-button" onClick={() => updateUser(user, { disabled: !user.disabled })}>{user.disabled ? "启用" : "禁用"}</button>
                <button className="icon-button" onClick={() => {
                  const password = prompt("输入新密码，至少 8 位");
                  if (password) updateUser(user, { password });
                }}>重置密码</button>
              </div>)}
            </div>
          </>}
          <h3>Telegram 账号管理</h3>
          <div className="account-admin-head">
            {socket && <AccountLogin socket={socket} legalNotice={settings.telegramLegalNotice} onDone={() => onAccountsChanged?.()} />}
          </div>
          <div className="account-admin-list">
            {accounts.map((account) => {
              const native = nativeAccounts.find((item) => item.accountId === account.id);
              return <div className="account-admin-row" key={account.id}>
                <Avatar accountId={account.id} label={account.displayName || account.label} size={38} />
                <div>
                  <strong>{account.displayName || account.label}</strong>
                  <span>{account.username ? `@${account.username}` : account.phoneNumber || "Telegram 账号"}</span>
                  {canAdmin && <small className={cx("native-status", native?.ready && "ready")}>
                    Go：{native?.ready ? "原生 session 健康" : native?.error || native?.status || "等待扫码迁移"}
                  </small>}
                  {canAdmin && native?.sessionSet && <small>
                    连续检查 {native.healthPasses || 0}/2
                    {native.lastHealthBytes ? ` · ${formatBytes(native.lastHealthBytes)} · DC ${native.lastHealthDc || "-"} · ${native.lastHealthDurationMs || 0} ms` : ""}
                  </small>}
                </div>
                <button className="icon-button" onClick={() => onAccountChange(account.id)}>{account.id === accountId ? "当前" : "切换"}</button>
                {canAdmin && <button className="icon-button" type="button" onClick={() => startNativeQrLogin(native || { accountId: account.id, displayName: account.displayName || account.label, phone: account.phoneNumber })}>扫码登录 Go</button>}
                {canAdmin && <button className="icon-button" type="button" onClick={() => native ? checkNativeAccount(account.id) : refreshNativeAccounts()}>健康检查</button>}
                {canAdmin && <button className="icon-button" type="button" onClick={() => reloginNativeAccount(native || { accountId: account.id, phone: account.phoneNumber, displayName: account.displayName || account.label })}>验证码兜底</button>}
                {canAdmin && account.needsMigration && <button className="icon-button primary-button" type="button" onClick={() => migrateAccountToGo(account)}>迁移到 Go</button>}
                {canAdmin && account.authMode === "native" && <span className="native-status ready">已迁移 Go</span>}
                <button className="icon-button danger-button" onClick={() => onAccountLogout(account.id)}><LogOut size={16} />退出</button>
              </div>;
            })}
            {!accounts.length && <div className="empty">暂无 Telegram 账号</div>}
          </div>
        </div>}
        {canAdmin && tab === "server" && <form className="stack" onSubmit={saveSettings}>
          <h3>服务端设置</h3>
          <label><span>公开访问地址</span><input value={settings.publicBaseUrl} onChange={(e) => setSettings({ ...settings, publicBaseUrl: e.target.value })} placeholder="https://feigram.example.com" required /></label>
          <label><span>Telegram API ID</span><input type="password" inputMode="numeric" autoComplete="off" value={settings.telegramApiId} onChange={(e) => setSettings({ ...settings, telegramApiId: e.target.value })} placeholder={apiIdPlaceholder} /></label>
          <label><span>Telegram API Hash</span><input type="password" value={settings.telegramApiHash} onChange={(e) => setSettings({ ...settings, telegramApiHash: e.target.value })} placeholder={hashPlaceholder} /></label>
          {settings.telegramLegalNotice ? <p className="legal-notice">{settings.telegramLegalNotice}</p> : null}
          <h3>缓存下载设置</h3>
          <label><span>基础缓存下载位置</span><input value={settings.cacheBaseDir} onChange={(e) => setSettings({ ...settings, cacheBaseDir: e.target.value })} placeholder="/data/downloads" required /></label>
          <label><span>图片缓存位置</span><input value={settings.imageCacheDir} onChange={(e) => setSettings({ ...settings, imageCacheDir: e.target.value })} placeholder="留空则使用 基础缓存/images" /></label>
          <label><span>视频缓存位置</span><input value={settings.videoCacheDir} onChange={(e) => setSettings({ ...settings, videoCacheDir: e.target.value })} placeholder="留空则使用 基础缓存/videos" /></label>
          <label><span>文件缓存位置</span><input value={settings.fileCacheDir} onChange={(e) => setSettings({ ...settings, fileCacheDir: e.target.value })} placeholder="留空则使用 基础缓存/files" /></label>
          <label><span>聊天缓存自动清除天数</span><input type="number" min="1" max="3650" value={settings.cacheRetentionDays} onChange={(e) => setSettings({ ...settings, cacheRetentionDays: e.target.value })} required /></label>
          <h3>播放器设置</h3>
          <label><span>视频在线播放模式</span><select value={settings.playerMode} onChange={(e) => setSettings({ ...settings, playerMode: e.target.value })}>
            <option value="browser">原始视频在线播放（推荐）</option>
            <option value="local">本地播放器（下载后打开）</option>
          </select></label>
          <p className="hint">推荐优先使用原始视频在线播放；遇到浏览器不支持的编码时，可切换为本地播放器模式。</p>
          <h3>下载服务</h3>
          <label><span>Go 下载服务地址</span><input value={settings.downloaderSidecarUrl} onChange={(e) => setSettings({ ...settings, downloaderSidecarUrl: e.target.value })} placeholder="http://127.0.0.1:3090" /></label>
          <p className="hint">Go 下载服务已接管大文件队列、断点续传、限速、并发和文件落盘；当前媒体源仍通过本机 Telegram 桥接，后续会继续迁移到原生 Go/tdl 传输层。</p>
          {saved && <p className="success">{saved}</p>}
          <button className="primary"><Settings size={18} />保存服务端设置</button>
        </form>}
        {canAdmin && tab === "cache-info" && <div className="silent-cache-panel">
          <div className="silent-cache-head">
            <strong>缓存信息</strong>
            <button className="icon-button" type="button" onClick={onRefreshSilentCaches}><RefreshCw size={14} />刷新</button>
          </div>
          <div className="silent-cache-controls">
            <label className="check-row"><input type="checkbox" checked={silentCacheState.enabled !== false} onChange={(e) => onSilentCacheControl?.({ enabled: e.target.checked })} /><span>{silentCacheState.enabled !== false ? "已开启后台缓存" : "已暂停后台缓存"}</span></label>
            <label><span>最大缓存速率</span><select value={String(silentCacheState.rateLimitBps || 0)} onChange={(e) => onSilentCacheControl?.({ rateLimitBps: Number(e.target.value) })}>
              <option value="0">不限速</option>
              <option value={String(512 * 1024)}>512 KB/s</option>
              <option value={String(1024 * 1024)}>1 MB/s</option>
              <option value={String(2 * 1024 * 1024)}>2 MB/s</option>
              <option value={String(5 * 1024 * 1024)}>5 MB/s</option>
              <option value={String(10 * 1024 * 1024)}>10 MB/s</option>
            </select></label>
            <label><span>缓存模式</span><select value={silentCacheState.mode || "conservative"} onChange={(e) => onSilentCacheControl?.({ mode: e.target.value })}>
              <option value="conservative">保守模式（同账号单任务）</option>
              <option value="fast">跨账号高速模式</option>
            </select></label>
            <label><span>并发数量</span><select value={String(silentCacheState.concurrency || 1)} onChange={(e) => onSilentCacheControl?.({ concurrency: Number(e.target.value) })}>
              {[1, 2, 3, 4, 5, 10].map((value) => <option value={String(value)} key={value}>{value}</option>)}
            </select></label>
          </div>
          <div className="cache-runtime-summary">
            <span><b>运行</b>{silentCacheState.running || 0} / {silentCacheState.effectiveConcurrency || silentCacheState.concurrency || 1}</span>
            <span><b>并发设置</b>{silentCacheState.configuredConcurrency || silentCacheState.concurrency || 1}</span>
            <span><b>传输层</b>{silentCacheState.transport === "native-mtproto" ? "Go 原生 MTProto" : "HTTP 回退"}</span>
            <span><b>任务数</b>{silentCaches.length}</span>
          </div>
          <div className="silent-cache-bulk">
            <button type="button" className="icon-button" onClick={() => setSelectedSilentIds(silentCaches.filter((task) => task.status !== "completed").map((task) => task.id))}>全选当前</button>
            <button type="button" className="icon-button" onClick={() => setSelectedSilentIds([])} disabled={!selectedSilentIds.length}>清空选择</button>
            <button type="button" className="icon-button danger-button" onClick={cancelSelectedSilentCaches} disabled={!selectedSilentIds.length}>取消选中{selectedSilentIds.length ? ` (${selectedSilentIds.length})` : ""}</button>
          </div>
          <div className="silent-cache-list">
            {silentCaches.map((task) => {
              const progress = task.size ? Math.min(100, Math.round((Number(task.downloaded || 0) / Number(task.size)) * 100)) : 0;
              const statusText = task.status === "running" || task.status === "downloading" ? "下载中" : task.status === "queued" ? "排队中" : task.status === "paused" ? "已暂停" : task.status === "completed" ? "已完成" : task.status === "cancelled" ? "已取消" : "失败";
              return (
                <div
                  className="silent-cache-row"
                  key={task.id}
                  draggable
                  onDragStart={() => setDragSilentId(task.id)}
                  onDragOver={(event) => event.preventDefault()}
                  onDrop={(event) => {
                    event.preventDefault();
                    if (dragSilentId && dragSilentId !== task.id) onSilentCacheControl?.({ reorder: { fromId: dragSilentId, toId: task.id } });
                    setDragSilentId("");
                  }}
                  onDragEnd={() => setDragSilentId("")}
                >
                  <div className="silent-cache-title">
                    <input className="silent-cache-check" type="checkbox" checked={selectedSilentSet.has(task.id)} onChange={(event) => toggleSilentSelection(task.id, event.target.checked)} onClick={(event) => event.stopPropagation()} />
                    <strong title={task.fileName}>{task.fileName || "Telegram 视频"}</strong>
                    <span>{statusText}</span>
                    {task.status !== "completed" && task.status !== "cancelled" && <button type="button" title="取消缓存" onClick={() => onCancelSilentCache?.(task)}><X size={12} /></button>}
                  </div>
                  <div className="silent-cache-meta">
                    <span>{formatBytes(task.downloaded)} / {formatBytes(task.size)}</span>
                    <span>{task.status === "running" ? `${formatBytes(task.speedBps)}/s` : "-"}</span>
                    <span>{formatTime(task.updatedAt)}</span>
                  </div>
                  <div className="mini-progress"><i style={{ width: `${progress}%` }} /></div>
                  {task.error && <small>{task.error}</small>}
                </div>
              );
            })}
            {!silentCaches.length && <div className="empty">暂无群视频后台缓存任务</div>}
          </div>
        </div>}
        {canAdmin && tab === "privacy" && <form className="stack" onSubmit={saveSettings}>
          <h3>通知设置</h3>
          <label className="check-row"><input type="checkbox" checked={settings.notificationEnabled} onChange={(e) => setSettings({ ...settings, notificationEnabled: e.target.checked })} /><span>启用桌面通知</span></label>
          <label className="check-row"><input type="checkbox" checked={settings.notificationPreview} onChange={(e) => setSettings({ ...settings, notificationPreview: e.target.checked })} /><span>通知显示消息预览</span></label>
          <h3>隐私设置</h3>
          <label className="check-row"><input type="checkbox" checked={settings.privacyOpenTelegramLinksInApp} onChange={(e) => setSettings({ ...settings, privacyOpenTelegramLinksInApp: e.target.checked })} /><span>Telegram 链接优先在客户端内打开</span></label>
          <label className="check-row"><input type="checkbox" checked={settings.privacyMediaPreview} onChange={(e) => setSettings({ ...settings, privacyMediaPreview: e.target.checked })} /><span>聊天中显示图片和视频预览</span></label>
          <label className="check-row"><input type="checkbox" checked={settings.messageShowSender} onChange={(e) => setSettings({ ...settings, messageShowSender: e.target.checked })} /><span>群聊消息显示发言人头像和 ID</span></label>
          <h3>分组设置</h3>
          <label className="check-row"><input type="checkbox" checked={settings.foldersEnabled} onChange={(e) => setSettings({ ...settings, foldersEnabled: e.target.checked })} /><span>同步 Telegram 聊天文件夹</span></label>
          <label className="check-row"><input type="checkbox" checked={settings.foldersShowArchived} onChange={(e) => setSettings({ ...settings, foldersShowArchived: e.target.checked })} /><span>会话列表显示归档会话</span></label>
          <label className="check-row"><input type="checkbox" checked={settings.foldersAutoSelectFirst} onChange={(e) => setSettings({ ...settings, foldersAutoSelectFirst: e.target.checked })} /><span>打开账号后自动选择第一个会话</span></label>
          {saved && <p className="success">{saved}</p>}
          <button className="primary"><Settings size={18} />保存隐私设置</button>
        </form>}
        {canAdmin && tab === "diagnostics" && <div className="diagnostics-panel">
          <div className="diagnostics-actions">
            <button className="icon-button" type="button" onClick={loadDiagnostics}><RefreshCw size={16} />刷新运行诊断</button>
            <button className="icon-button" type="button" onClick={() => runCacheSpeedTest(false)} disabled={cacheSpeedTesting}><Play size={16} />{cacheSpeedTesting ? "测速中" : "缓存速度"}</button>
            <button className="icon-button" type="button" onClick={() => runCacheSpeedTest(true)} disabled={cacheSpeedTesting}><Play size={16} />抽样测速</button>
            <button className="icon-button" type="button" onClick={checkUpdates}><Download size={16} />检查更新</button>
          </div>
          {diagnostics ? <div className="diagnostics-grid">
            <span><b>版本</b>{diagnostics.app?.version}</span>
            <span><b>版本线</b>{diagnostics.app?.edition}</span>
            <span><b>运行时间</b>{Math.floor((diagnostics.app?.uptime || 0) / 60)} 分钟</span>
            <span><b>缓存大小</b>{formatBytes(diagnostics.cache?.bytes)}</span>
            <span><b>下载任务</b>{diagnostics.cache?.downloadTasks || 0}</span>
            <span><b>后台缓存</b>{diagnostics.cache?.silentCacheTasks || 0}</span>
            <span><b>Go 下载服务</b>{diagnostics.downloader?.ok ? `运行中 v${diagnostics.downloader.version}` : diagnostics.downloader?.error || "未连接"}</span>
          </div> : <div className="empty">点击刷新诊断查看系统状态</div>}
          {diagnostics && <div className="diagnostics-paths">
            <p><strong>数据目录</strong>{diagnostics.paths?.dataDir}</p>
            <p><strong>缓存目录</strong>{diagnostics.paths?.cacheBase}</p>
            <p><strong>日志文件</strong>{diagnostics.paths?.logFile || "未设置"}</p>
            <p><strong>Go 下载日志</strong>{diagnostics.paths?.downloaderLogFile || "未设置"}</p>
          </div>}
          {downloaderState && <div className="cache-speed-card">
            <div className="cache-speed-head">
              <strong>Go 下载服务</strong>
              <span>{downloaderState.ok ? "运行中" : "未连接"}</span>
            </div>
            <div className="diagnostics-grid compact">
              <span><b>版本</b>{downloaderState.version || "-"}</span>
              <span><b>PID</b>{downloaderState.pid || "-"}</span>
              <span><b>运行时间</b>{downloaderState.uptime ? `${Math.floor(downloaderState.uptime / 60)} 分钟` : "-"}</span>
              <span><b>任务数</b>{downloaderState.tasks?.length || downloaderState.taskCount || 0}</span>
              <span><b>并发</b>{downloaderState.config?.concurrency || "-"}</span>
              <span><b>限速</b>{downloaderState.config?.rateLimitBps ? `${formatBytes(downloaderState.config.rateLimitBps)}/s` : "不限速"}</span>
              <span><b>模式</b>{downloaderState.config?.mode === "fast" ? "高速" : "保守"}</span>
              <span><b>媒体源</b>{(downloaderState.config?.transport || downloaderState.transport) === "native-mtproto" ? "Go 原生 MTProto" : "HTTP 桥接"}</span>
              <span><b>数据目录</b>{downloaderState.dataDir || "-"}</span>
            </div>
            <div className="silent-cache-controls downloader-config-controls">
              <label className="check-row"><input type="checkbox" checked={downloaderState.config?.enabled !== false} onChange={(e) => saveDownloaderConfig({ enabled: e.target.checked })} /><span>{downloaderState.config?.enabled !== false ? "Go 队列已启用" : "Go 队列已暂停"}</span></label>
              <label><span>Go 并发</span><select value={String(downloaderState.config?.concurrency || 1)} onChange={(e) => saveDownloaderConfig({ concurrency: Number(e.target.value) })}>
                {[1, 2, 3, 4, 5, 10].map((value) => <option value={String(value)} key={value}>{value}</option>)}
              </select></label>
              <label><span>Go 限速</span><select value={String(downloaderState.config?.rateLimitBps || 0)} onChange={(e) => saveDownloaderConfig({ rateLimitBps: Number(e.target.value) })}>
                <option value="0">不限速</option>
                <option value={String(1024 * 1024)}>1 MB/s</option>
                <option value={String(5 * 1024 * 1024)}>5 MB/s</option>
                <option value={String(10 * 1024 * 1024)}>10 MB/s</option>
              </select></label>
              <label><span>Go 模式</span><select value={downloaderState.config?.mode || "conservative"} onChange={(e) => saveDownloaderConfig({ mode: e.target.value })}>
                <option value="conservative">保守</option>
                <option value="fast">高速</option>
              </select></label>
              <label><span>媒体源传输层</span><select value={downloaderState.config?.transport || "http-bridge"} onChange={(e) => saveDownloaderConfig({ transport: e.target.value })}>
                <option value="http-bridge">HTTP 桥接（稳定）</option>
                <option value="native-mtproto" disabled={!downloaderState.nativeMTProto?.ready}>Go 原生 MTProto（健康后可选）</option>
              </select></label>
            </div>
            {downloaderState.nativeMTProto && <p className="hint">{downloaderState.nativeMTProto.note}</p>}
            <p className="hint">{downloaderState.strategy || "Go sidecar 已就绪，等待 Telegram 下载桥接。"}</p>
            {downloaderState.error && <p className="error">{downloaderState.error}</p>}
          </div>}
          {nativeAccounts.length > 0 && <div className="cache-speed-card">
            <div className="cache-speed-head">
              <strong>Go 原生账号</strong>
              <span>{nativeAccounts.filter((item) => item.ready).length} / {nativeAccounts.length} 可用</span>
            </div>
            <div className="native-account-list">
              {nativeAccounts.map((item) => (
                <div className="native-account-row" key={`${item.userId}:${item.accountId}`}>
                  <div>
                    <strong>{item.displayName || item.phone || item.accountId}</strong>
                    <p>{item.ready ? "Go MTProto session 健康" : item.error || "等待 Go 重新登录生成原生 session"}</p>
                    <small>连续检查 {item.healthPasses || 0}/2{item.lastHealthBytes ? ` · ${formatBytes(item.lastHealthBytes)} · DC ${item.lastHealthDc || "-"} · ${item.lastHealthDurationMs || 0} ms` : ""}</small>
                  </div>
                  <span>{item.sessionSet ? item.status : "未迁移"}</span>
                  <button className="icon-button" type="button" onClick={() => checkNativeAccount(item.accountId)}>健康检查</button>
                </div>
              ))}
            </div>
          </div>}
          {cacheSpeedTest && <div className="cache-speed-card">
            <div className="cache-speed-head">
              <strong>缓存速度诊断</strong>
              <span>{cacheSpeedTest.ok ? "完成" : "异常"}</span>
            </div>
            <div className="diagnostics-grid compact">
              <span><b>实测速度</b>{formatBytes(cacheSpeedTest.result?.speedBps || 0)}/s</span>
              <span><b>诊断方式</b>{cacheSpeedTest.mode === "aggregate" ? "运行聚合" : "抽样读取"}</span>
              <span><b>读取样本</b>{formatBytes(cacheSpeedTest.result?.bytesRead || 0)}</span>
              <span><b>耗时</b>{cacheSpeedTest.result?.durationMs ? `${cacheSpeedTest.result.durationMs} ms` : "-"}</span>
              <span><b>读取分片</b>{cacheSpeedTest.result?.chunks || 0}</span>
              <span><b>限速</b>{cacheSpeedTest.rateLimitBps ? `${formatBytes(cacheSpeedTest.rateLimitBps)}/s` : "不限速"}</span>
              <span><b>缓存模式</b>{cacheSpeedTest.cacheMode === "fast" ? "高速" : "保守"}</span>
              <span><b>运行/有效上限</b>{cacheSpeedTest.running} / {cacheSpeedTest.effectiveConcurrency || cacheSpeedTest.concurrency}</span>
              <span><b>并发设置</b>{cacheSpeedTest.configuredConcurrency || cacheSpeedTest.concurrency}</span>
              <span><b>队列</b>{cacheSpeedTest.queued}</span>
              <span><b>请求分片</b>{formatBytes(cacheSpeedTest.result?.requestedChunkSize || cacheSpeedTest.directChunkSize || 0)}</span>
              <span><b>实际分片</b>{formatBytes(cacheSpeedTest.result?.effectiveChunkSize || cacheSpeedTest.directChunkSize || 0)}</span>
              <span><b>降级次数</b>{cacheSpeedTest.result?.fallbackCount || 0}</span>
              <span><b>LIMIT_INVALID</b>{cacheSpeedTest.result?.limitInvalidCount || 0}</span>
            </div>
            {cacheSpeedTest.task && <div className="diagnostics-paths">
              <p><strong>测试文件</strong>{cacheSpeedTest.task.fileName || "Telegram 视频"}</p>
              <p><strong>任务状态</strong>{cacheSpeedTest.task.status}，已缓存 {formatBytes(cacheSpeedTest.task.downloaded)} / {formatBytes(cacheSpeedTest.task.size)}</p>
              <p><strong>Telegram DC</strong>{cacheSpeedTest.task.dcId || "-"}</p>
              <p><strong>测试 offset</strong>{formatBytes(cacheSpeedTest.task.testOffset || 0)}</p>
            </div>}
            {cacheSpeedTest.error && <p className="error">{cacheSpeedTest.error}</p>}
            {cacheSpeedTest.note && <p className="hint">{cacheSpeedTest.note}</p>}
            <p className="hint">诊断速度来自 Go 队列的真实运行任务；如果运行数为 0，速度也会归零，避免排队任务残留速度造成误判。</p>
            <pre className="log-tail cache-speed-json">{JSON.stringify(cacheSpeedTest, null, 2)}</pre>
          </div>}
          {updateInfo && <div className="update-card">
            <strong>{updateInfo.updateAvailable ? "发现新版本" : "当前版本已是最新或暂未发现发布版"}</strong>
            <span>当前：{updateInfo.current || "-"} / 最新：{updateInfo.latest || "-"}</span>
            {updateInfo.error && <small>{updateInfo.error}</small>}
            <a href={updateInfo.url} target="_blank" rel="noreferrer">打开发布页</a>
          </div>}
          {diagnostics?.logTail && <pre className="log-tail system-log-tail">{diagnostics.logTail}</pre>}
          {diagnostics?.downloaderLogTail && <pre className="log-tail system-log-tail">{diagnostics.downloaderLogTail}</pre>}
        </div>}
      </div>
    </div>
  );
}

function InfoModal({ announcements, about, open, onClose }) {
  if (!open) return null;
  return (
    <div className="modal-backdrop">
      <div className="modal announcement-modal">
        <button className="close" onClick={onClose} title="关闭"><X size={18} /></button>
        <h2>公告</h2>
        <div className="announcement-list">
          {announcements.map((item) => <article className="announcement-item" key={item.id}>
            <strong>{item.title}</strong>
            <span>{item.version ? `v${item.version} · ` : ""}{formatTime(item.createdAt)}</span>
            <p>{item.body}</p>
          </article>)}
          {!announcements.length && <div className="empty">暂无公告</div>}
          <article className="announcement-item about-item">
            <strong>{about?.title || "关于 Feigram"}</strong>
            <p>{about?.body || "Feigram 是第三方开发的非官方 Telegram 客户端。"}</p>
            {about?.releaseUrl && <a href={about.releaseUrl} target="_blank" rel="noreferrer">发布仓库</a>}
            {about?.privacyPolicyUrl && <a href={about.privacyPolicyUrl} target="_blank" rel="noreferrer">隐私政策</a>}
            {about?.termsUrl && <a href={about.termsUrl} target="_blank" rel="noreferrer">服务条款</a>}
            {about?.supportEmail && <a href={`mailto:${about.supportEmail}`}>支持邮箱</a>}
          </article>
        </div>
      </div>
    </div>
  );
}

function DownloadCenter({ open, downloads, onStart, onCancel, onClear, onDelete, onPlay, onClose }) {
  if (!open) return null;
  const active = downloads.filter((item) => ["queued", "downloading"].includes(item.status)).length;
  const statusText = {
    queued: "排队中",
    downloading: "下载中",
    completed: "已完成",
    cancelled: "已暂停",
    error: "失败"
  };
  const progressFor = (item) => item.size ? Math.min(100, Math.round((Number(item.downloaded || 0) / Number(item.size)) * 100)) : 0;
  const deduped = mergeDownloads(downloads);
  return (
    <div className="download-layer">
      <section className="download-panel" onClick={(event) => event.stopPropagation()}>
        <header>
          <div>
            <h2>下载</h2>
            <p>{active ? `${active} 个任务进行中` : "暂无活动下载"}</p>
          </div>
          <button className="icon-button" onClick={onClose} title="关闭"><X size={18} /></button>
        </header>
        <div className="download-list">
          {deduped.map((item) => {
            const progress = progressFor(item);
            return (
              <article
                className={cx("download-item", item.status)}
                key={item.id}
                role={item.status === "completed" && item.kind === "video" ? "button" : undefined}
                tabIndex={item.status === "completed" && item.kind === "video" ? 0 : undefined}
                onClick={() => {
                  if (item.status === "completed" && item.kind === "video") onPlay?.(item);
                }}
                onKeyDown={(event) => {
                  if ((event.key === "Enter" || event.key === " ") && item.status === "completed" && item.kind === "video") {
                    event.preventDefault();
                    onPlay?.(item);
                  }
                }}
              >
                <div className="download-item-head">
                  <strong title={item.fileName || "Telegram 媒体"}>{item.fileName || "Telegram 媒体"}</strong>
                  <span>{statusText[item.status] || item.status}</span>
                </div>
                <div className="download-meta">
                  <span>{formatBytes(item.downloaded)} / {formatBytes(item.size)}</span>
                  <span>{formatBytes(item.speedBps)}/s</span>
                  <span>{formatTime(item.updatedAt)}</span>
                </div>
                <div className="download-progress"><i style={{ width: `${progress}%` }} /></div>
                {item.error && <p className="download-error">{item.error}</p>}
                <div className="download-actions" onClick={(event) => event.stopPropagation()}>
                  {item.status === "completed" && item.kind === "video" && <button onClick={() => onPlay(item)}><Play size={12} />播放</button>}
                  {item.status !== "downloading" && item.status !== "completed" && <button onClick={() => onStart(item)}><Play size={12} />开始</button>}
                  {item.status === "downloading" && <button onClick={() => onCancel(item)}><X size={12} />取消</button>}
                  <button onClick={() => onClear(item)}><X size={12} />清除</button>
                  <button onClick={() => onDelete(item)}><Trash2 size={12} />删缓存</button>
                </div>
              </article>
            );
          })}
          {!deduped.length && <div className="empty">点击视频右上角缓存后，任务会出现在这里。</div>}
        </div>
      </section>
    </div>
  );
}

function PlaybackModal({ item, playerMode, onClose }) {
  const [failed, setFailed] = useState(false);
  if (!item) return null;
  const src = mediaUrl(item.accountId, item.peerId, item.messageId, true);
  const download = mediaUrl(item.accountId, item.peerId, item.messageId);
  return (
    <div className="modal-backdrop playback-backdrop">
      <div className="modal playback-modal">
        <button className="close" onClick={onClose} title="关闭"><X size={18} /></button>
        <h2>{item.fileName || "视频播放"}</h2>
        <div className="playback-stage">
          {playerMode === "local"
            ? <a className="video-load-button local-player-link" href={download}>下载后用本地播放器打开</a>
            : <FeigramVideo src={src} onError={() => setFailed(true)} />}
          {failed ? <div className="video-fallback">当前视频编码无法直接在线播放，请切换本地播放器模式或下载到本地播放。</div> : null}
        </div>
      </div>
    </div>
  );
}

function ChatInfoPanel({ open, accountId, chat, details, loading, autoCache, autoCacheBusy, mediaLoadingMore, onAutoCacheChange, onClose, onOpenMedia, onLoadMoreMedia }) {
  const [mediaTab, setMediaTab] = useState("all");
  const contentRef = useRef(null);
  if (!open || !chat) return null;
  const info = details || chat;
  const resources = info.files || [];
  const visibleResources = mediaTab === "all" ? resources : resources.filter((file) => file.kind === mediaTab);
  return (
    <aside className="chat-info-panel">
      <header>
        <button className="icon-button" onClick={onClose} title="关闭"><X size={18} /></button>
        <strong>群组信息</strong>
      </header>
      <div
        className="chat-info-content"
        ref={contentRef}
        onScroll={(event) => {
          const element = event.currentTarget;
          if (element.scrollHeight - element.scrollTop - element.clientHeight < 160) onLoadMoreMedia?.();
        }}
      >
        <div className="chat-info-profile">
          <Avatar accountId={accountId} peerId={chat.id} label={chat.title} size={72} />
          <h2>{info.title}</h2>
          <p>{info.username ? `@${info.username}` : info.type}</p>
          {!!info.participantsCount && <span>{info.participantsCount.toLocaleString("zh-CN")} 位成员/订阅者</span>}
        </div>
        {loading ? <div className="empty">加载中</div> : <>
          {info.about && <section className="chat-info-section"><h3>简介</h3><p>{info.about}</p></section>}
          <section className="chat-info-section">
            <h3>文件与媒体</h3>
            <div className="media-summary">
              <button className={cx(mediaTab === "image" && "active")} type="button" onClick={() => setMediaTab("image")}><b>{info.mediaSummary?.images || 0}</b>图片</button>
              <button className={cx(mediaTab === "video" && "active")} type="button" onClick={() => setMediaTab("video")}><b>{info.mediaSummary?.videos || 0}</b>视频</button>
              <button className={cx(mediaTab === "file" && "active")} type="button" onClick={() => setMediaTab("file")}><b>{info.mediaSummary?.files || 0}</b>文件</button>
            </div>
            <label className="info-cache-toggle">
              <input type="checkbox" checked={Boolean(autoCache)} disabled={autoCacheBusy} onChange={(event) => onAutoCacheChange?.(event.target.checked)} />
              <span>{autoCacheBusy ? "正在提交后台缓存任务" : "后台自动缓存本群大于 100MB 的视频"}</span>
            </label>
          </section>
          <section className="chat-info-section">
            <div className="info-resource-head">
              <h3>{mediaTab === "all" ? "最近资源" : mediaTab === "image" ? "图片" : mediaTab === "video" ? "视频" : "文件"}</h3>
              <button type="button" onClick={() => setMediaTab("all")}>全部</button>
            </div>
            <div className={cx("info-resource-grid", mediaTab === "file" && "files")}>
              {visibleResources.map((file) => <button className={cx("info-resource-item", file.kind)} type="button" key={`${file.id}-${file.fileName}`} onClick={() => onOpenMedia?.(file)}>
                {file.kind === "image" && <img src={mediaUrl(accountId, chat.id, file.id, true)} alt="" loading="lazy" />}
                {file.kind === "video" && <>
                  <img src={thumbnailMediaUrl(accountId, chat.id, file.id)} alt="" loading="lazy" onError={(event) => { event.currentTarget.style.display = "none"; }} />
                  <Play size={18} />
                  {!!file.duration && <b>{formatDuration(file.duration)}</b>}
                </>}
                {file.kind === "file" && <Folder size={22} />}
                <span><strong>{file.fileName}</strong><small>{file.kind} · {formatBytes(file.size)} · {formatTime(file.date)}</small></span>
              </button>)}
              {!visibleResources.length && <div className="empty">暂无资源</div>}
            </div>
            {details?.hasMoreMedia && <button className="history-button" type="button" onClick={() => onLoadMoreMedia?.()} disabled={mediaLoadingMore}>{mediaLoadingMore ? "加载中" : "加载更多资源"}</button>}
          </section>
        </>}
      </div>
    </aside>
  );
}

function App() {
  const [token, setTokenState] = useState(getToken());
  const [me, setMe] = useState(null);
  const [theme, setTheme] = useState(resolveInitialTheme);
  const [accounts, setAccounts] = useState([]);
  const [accountId, setAccountId] = useState("");
  const [chats, setChats] = useState([]);
  const [folders, setFolders] = useState([]);
  const [appSettings, setAppSettings] = useState({
    notificationEnabled: true,
    notificationPreview: true,
    privacyOpenTelegramLinksInApp: true,
    privacyMediaPreview: true,
    messageShowSender: true,
    foldersEnabled: true,
    foldersShowArchived: false,
    foldersAutoSelectFirst: true,
    playerMode: "browser"
  });
  const [activeFolder, setActiveFolder] = useState("all");
  const [activeChat, setActiveChat] = useState(null);
  const [chatStack, setChatStack] = useState([]);
  const [messages, setMessages] = useState([]);
  const [hasOlder, setHasOlder] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [draft, setDraft] = useState("");
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [toast, setToast] = useState("");
  const [adminOpen, setAdminOpen] = useState(false);
  const [adminInitialTab, setAdminInitialTab] = useState("accounts");
  const [notifications, setNotifications] = useState("Notification" in window && Notification.permission === "granted");
  const [announcements, setAnnouncements] = useState([]);
  const [about, setAbout] = useState({});
  const [announcementOpen, setAnnouncementOpen] = useState(false);
  const [downloads, setDownloads] = useState([]);
  const [silentCaches, setSilentCaches] = useState([]);
  const [silentCacheState, setSilentCacheState] = useState({ enabled: true, rateLimitBps: 0, concurrency: 1, mode: "conservative" });
  const [chatInfoOpen, setChatInfoOpen] = useState(false);
  const [chatDetails, setChatDetails] = useState(null);
  const [chatDetailsLoading, setChatDetailsLoading] = useState(false);
  const [chatMediaLoadingMore, setChatMediaLoadingMore] = useState(false);
  const [autoCacheChats, setAutoCacheChats] = useState(() => JSON.parse(localStorage.getItem("feigrame.autoCacheChats") || "{}"));
  const [autoCacheBusy, setAutoCacheBusy] = useState(false);
  const [playback, setPlayback] = useState(null);
  const socket = useSocket(token);
  const messagesRef = useRef(null);
  const messageNodeRefs = useRef(new Map());
  const shouldScrollBottomRef = useRef(false);
  const pendingScrollRef = useRef(null);
  const [highlightMessageId, setHighlightMessageId] = useState(0);
  const activeAccount = accounts.find((account) => account.id === accountId);
  const newestAnnouncementId = announcements[0]?.id || "";
  const unreadAnnouncement = newestAnnouncementId && localStorage.getItem("feigrame.lastAnnouncement") !== newestAnnouncementId;
  const messageItems = useMemo(() => buildMessageItems(messages), [messages]);
  const visibleChats = useMemo(() => {
    const folder = folders.find((item) => String(item.id) === String(activeFolder));
    if (!folder) return chats;
    if (folder.chatIds?.length) {
      const ids = new Set(folder.chatIds);
      return chats.filter((chat) => ids.has(chat.id));
    }
    const include = new Set([...(folder.includePeerIds || []), ...(folder.pinnedPeerIds || [])]);
    const exclude = new Set(folder.excludePeerIds || []);
    return chats.filter((chat) => {
      if (exclude.has(chat.id)) return false;
      if ((chat.folderIds || []).some((id) => String(id) === String(folder.id))) return true;
      if (include.has(chat.id)) return true;
      const flags = folder.flags || {};
      if (chat.type === "group" && flags.groups) return true;
      if (chat.type === "channel" && flags.broadcasts) return true;
      if (chat.type === "private" && (flags.contacts || flags.nonContacts || flags.bots)) return true;
      return include.size === 0 && !flags.groups && !flags.broadcasts && !flags.contacts && !flags.nonContacts && !flags.bots;
    });
  }, [chats, folders, activeFolder]);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  function toggleTheme() {
    const next = theme === "dark" ? "light" : "dark";
    localStorage.setItem("feigrame.theme", next);
    setTheme(next);
  }

  useEffect(() => {
    if (!token) return;
    api("/api/me").then(setMe).catch(() => setTokenState(""));
    loadSettings();
    refreshAccounts();
    window.setTimeout(() => {
      loadAnnouncements();
      loadAbout();
      loadDownloads();
      loadSilentCaches();
    }, 400);
  }, [token]);

  useEffect(() => {
    if (!accountId) return;
    socket?.emit("account:join", accountId);
    loadChats();
    window.setTimeout(() => {
      if (appSettings.foldersEnabled) loadFolders();
      else {
        setFolders([]);
        setActiveFolder("all");
      }
    }, 250);
  }, [accountId, socket, appSettings.foldersEnabled]);

  useEffect(() => {
    if (!socket) return;
    const handler = ({ accountId: incomingAccount, message }) => {
      if (incomingAccount !== accountId) return;
      const stick = isNearBottom(messagesRef.current);
      if (notifications && appSettings.notificationEnabled && !message.outgoing && message.text) {
        new Notification("Feigram 新消息", { body: appSettings.notificationPreview ? message.text.slice(0, 120) : "收到一条新消息" });
      }
      shouldScrollBottomRef.current = stick;
      setMessages((current) => [...current, message]);
    };
    socket.on("message:new", handler);
    return () => socket.off("message:new", handler);
  }, [socket, accountId, notifications, appSettings.notificationEnabled, appSettings.notificationPreview]);

  useEffect(() => {
    if (!socket) return;
    const update = (task) => {
      setDownloads((current) => {
        const index = current.findIndex((item) => item.id === task.id);
        if (index === -1) return mergeDownloads([task, ...current]);
        const next = [...current];
        next[index] = task;
        return mergeDownloads(next);
      });
    };
    const remove = ({ id }) => setDownloads((current) => current.filter((item) => item.id !== id));
    socket.on("download:update", update);
    socket.on("download:delete", remove);
    return () => {
      socket.off("download:update", update);
      socket.off("download:delete", remove);
    };
  }, [socket]);

  useEffect(() => {
    if (!socket) return;
    const update = (task) => {
      setSilentCaches((current) => {
        const index = current.findIndex((item) => item.id === task.id);
        const next = index === -1 ? [task, ...current] : current.map((item) => item.id === task.id ? task : item);
        return sortSilentCaches(next);
      });
    };
    const remove = ({ id }) => setSilentCaches((current) => current.filter((item) => item.id !== id));
    socket.on("silent-cache:update", update);
    socket.on("silent-cache:delete", remove);
    return () => {
      socket.off("silent-cache:update", update);
      socket.off("silent-cache:delete", remove);
    };
  }, [socket]);

  useEffect(() => {
    if (!token) return;
    const timer = window.setInterval(() => {
      loadDownloads();
      loadSilentCaches();
    }, 3000);
    return () => window.clearInterval(timer);
  }, [token]);

  useEffect(() => {
    const element = messagesRef.current;
    if (!element || loadingOlder || !shouldScrollBottomRef.current) return;
    shouldScrollBottomRef.current = false;
    requestAnimationFrame(() => {
      element.scrollTop = element.scrollHeight;
    });
  }, [activeChat?.id, messages.length, loadingOlder]);

  useEffect(() => {
    const pending = pendingScrollRef.current;
    if (!pending || !activeChat) return;
    if (pending.chatId !== activeChat.id) return;
    const element = messagesRef.current;
    requestAnimationFrame(() => {
      if (!element) return;
      if (pending.messageId) {
        const node = messageNodeRefs.current.get(String(pending.messageId));
        if (node) {
          node.scrollIntoView({ block: "center" });
          setHighlightMessageId(Number(pending.messageId));
          window.clearTimeout(pendingScrollRef.highlightTimer);
          pendingScrollRef.highlightTimer = window.setTimeout(() => setHighlightMessageId(0), 2400);
          pendingScrollRef.current = null;
          return;
        }
        return;
      }
      if (Number.isFinite(pending.scrollTop)) element.scrollTop = pending.scrollTop;
      pendingScrollRef.current = null;
    });
  }, [activeChat?.id, messages.length]);

  function isNearBottom(element) {
    if (!element) return true;
    return element.scrollHeight - element.scrollTop - element.clientHeight < 96;
  }

  async function refreshAccounts(preferFirst = false) {
    setError("");
    const list = await api("/api/accounts").catch((err) => { setError(err.message); return []; });
    setAccounts(list);
    if ((preferFirst || !accountId) && list[0]) setAccountId(list[0].id);
    if (!list.length) setAccountId("");
  }

  async function loadChats(nextQuery = query) {
    if (!accountId) return;
    setBusy(true);
    setError("");
    try {
      const list = await api(`/api/chats?account=${encodeURIComponent(accountId)}&query=${encodeURIComponent(nextQuery)}`);
      const visible = appSettings.foldersShowArchived ? list : list.filter((chat) => !chat.archived);
      setChats(visible);
      if (appSettings.foldersAutoSelectFirst && !activeChat && visible[0]) selectChat(visible[0]);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function clearSearch() {
    setQuery("");
    await loadChats("");
  }

  async function loadFolders() {
    if (!accountId) return;
    const list = await api(`/api/folders?account=${encodeURIComponent(accountId)}`).catch(() => []);
    setFolders(list);
    setActiveFolder("all");
  }

  async function selectChat(chat, options = {}) {
    setActiveChat(chat);
    setChatInfoOpen(false);
    setChatDetails(null);
    messageNodeRefs.current.clear();
    const targetMessageId = Number(options.messageId || 0);
    if (targetMessageId) {
      shouldScrollBottomRef.current = false;
      pendingScrollRef.current = { chatId: chat.id, messageId: targetMessageId };
    } else if (Number.isFinite(options.restoreScrollTop)) {
      shouldScrollBottomRef.current = false;
      pendingScrollRef.current = { chatId: chat.id, scrollTop: options.restoreScrollTop };
    } else {
      shouldScrollBottomRef.current = true;
      pendingScrollRef.current = null;
    }
    setBusy(true);
    try {
      const around = targetMessageId ? `&around=${encodeURIComponent(targetMessageId)}` : "";
      const list = await api(`/api/messages?account=${encodeURIComponent(accountId)}&peer=${encodeURIComponent(chat.id)}&limit=80${around}`);
      setMessages(list);
      setHasOlder(list.length >= 80);
      if (targetMessageId && !list.some((message) => Number(message.id) === targetMessageId)) {
        pendingScrollRef.current = null;
        notify("你要访问的内容已被删除");
      }
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function returnToPreviousChat() {
    const previous = chatStack[chatStack.length - 1];
    if (!previous) return;
    setChatStack((current) => current.slice(0, -1));
    await selectChat(previous.chat || previous, { restoreScrollTop: previous.scrollTop });
  }

  async function openChatInfo() {
    if (!activeChat) return;
    setChatInfoOpen(true);
    setChatDetailsLoading(true);
    setChatMediaLoadingMore(false);
    try {
      const info = await api(`/api/chats/${encodeURIComponent(accountId)}/${encodeURIComponent(activeChat.id)}/details`);
      setChatDetails(info);
    } catch (err) {
      notify(err.message);
    } finally {
      setChatDetailsLoading(false);
    }
  }

  async function loadMoreChatMedia() {
    if (!activeChat || !chatDetails?.hasMoreMedia || chatMediaLoadingMore) return;
    setChatMediaLoadingMore(true);
    try {
      const before = chatDetails.nextMediaBefore || 0;
      const page = await api(`/api/chats/${encodeURIComponent(accountId)}/${encodeURIComponent(activeChat.id)}/media?before=${encodeURIComponent(before)}&limit=30`);
      setChatDetails((current) => {
        if (!current) return current;
        const seen = new Set((current.files || []).map((file) => String(file.id)));
        const files = [
          ...(current.files || []),
          ...(page.files || []).filter((file) => !seen.has(String(file.id)))
        ];
        return {
          ...current,
          files,
          nextMediaBefore: page.nextBefore || current.nextMediaBefore,
          hasMoreMedia: Boolean(page.hasMore)
        };
      });
    } catch (err) {
      notify(err.message);
    } finally {
      setChatMediaLoadingMore(false);
    }
  }

  function playDownload(item) {
    if (!item || item.kind !== "video") return;
    setPlayback(item);
  }

  function openInfoMedia(file) {
    if (!activeChat || !file) return;
    const item = {
      ...file,
      accountId,
      peerId: activeChat.id,
      messageId: file.id
    };
    if (file.kind === "video") {
      setPlayback(item);
    } else {
      window.open(mediaUrl(accountId, activeChat.id, file.id, file.kind === "image"), "_blank", "noopener,noreferrer");
    }
  }

  async function setChatAutoCache(enabled) {
    if (!activeChat) return;
    const key = `${accountId}:${activeChat.id}`;
    const next = { ...autoCacheChats, [key]: enabled };
    if (!enabled) delete next[key];
    setAutoCacheChats(next);
    localStorage.setItem("feigrame.autoCacheChats", JSON.stringify(next));
    if (!enabled) return;
    setAutoCacheBusy(true);
    try {
      const result = await api(`/api/chats/${encodeURIComponent(accountId)}/${encodeURIComponent(activeChat.id)}/cache-large-videos`, { method: "POST" });
      notify(result.queued ? `已提交 ${result.queued} 个后台视频缓存任务` : "没有需要后台缓存的大视频");
    } catch (err) {
      notify(err.message);
    } finally {
      setAutoCacheBusy(false);
    }
  }

  async function reloadActiveMessages() {
    if (!activeChat) return;
    const element = messagesRef.current;
    const top = element?.scrollTop || 0;
    const list = await api(`/api/messages?account=${encodeURIComponent(accountId)}&peer=${encodeURIComponent(activeChat.id)}&limit=80`);
    setMessages(list);
    setHasOlder(list.length >= 80);
    requestAnimationFrame(() => {
      if (element) element.scrollTop = top;
    });
  }

  async function loadOlderMessages() {
    if (!activeChat || !messages.length) return;
    const firstId = messages[0].id;
    const element = messagesRef.current;
    const previousHeight = element?.scrollHeight || 0;
    setLoadingOlder(true);
    setError("");
    try {
      const older = await api(`/api/messages?account=${encodeURIComponent(accountId)}&peer=${encodeURIComponent(activeChat.id)}&limit=80&before=${encodeURIComponent(firstId)}`);
      setMessages((current) => [...older, ...current]);
      setHasOlder(older.length >= 80);
      requestAnimationFrame(() => {
        if (element) element.scrollTop = element.scrollHeight - previousHeight;
      });
    } catch (err) {
      setError(err.message);
    } finally {
      setLoadingOlder(false);
    }
  }

  function notify(message) {
    setToast(message);
    window.clearTimeout(notify.timer);
    notify.timer = window.setTimeout(() => setToast(""), 4200);
  }

  async function logoutAccount(targetAccountId = accountId) {
    if (!targetAccountId || !confirm("确定退出这个 Telegram 账号？")) return;
    setError("");
    try {
      await api(`/api/accounts/${encodeURIComponent(targetAccountId)}`, { method: "DELETE" });
      if (targetAccountId === accountId) {
        setAccountId("");
        setActiveChat(null);
        setMessages([]);
      }
      await refreshAccounts(true);
    } catch (err) {
      notify(err.message);
    }
  }

  async function openTelegramLink(url) {
    const normalized = normalizeLink(url);
    if (!accountId) {
      window.open(normalized, "_blank", "noopener,noreferrer");
      return;
    }
    if (!appSettings.privacyOpenTelegramLinksInApp) {
      window.open(normalized, "_blank", "noopener,noreferrer");
      return;
    }
    try {
      const chat = await api("/api/resolve-link", {
        method: "POST",
        body: JSON.stringify({ account: accountId, url: normalized })
      });
      setChats((current) => current.some((item) => item.id === chat.id) ? current : [chat, ...current]);
      if (activeChat && activeChat.id !== chat.id) {
        setChatStack((current) => [...current, {
          chat: activeChat,
          scrollTop: messagesRef.current?.scrollTop || 0
        }].slice(-12));
      }
      await selectChat(chat, { messageId: chat.messageId });
    } catch {
      window.open(normalized, "_blank", "noopener,noreferrer");
    }
  }

  async function clickInlineButton(message, button) {
    if (button.url) {
      openTelegramLink(button.url);
      return;
    }
    if (!button.data) return;
    setError("");
    try {
      const result = await api("/api/messages/callback", {
        method: "POST",
        body: JSON.stringify({
          account: accountId,
          peer: activeChat.id,
          messageId: message.id,
          data: button.data
        })
      });
      if (result.url) openTelegramLink(result.url);
      if (result.message) notify(result.message);
      setTimeout(() => reloadActiveMessages().catch((err) => setError(err.message)), 600);
    } catch (err) {
      notify(err.message.replace(/^RPC_/, ""));
    }
  }

  async function cacheMedia(message) {
    setError("");
    try {
      const result = await api(`/api/media/${accountId}/${encodeURIComponent(activeChat.id)}/${message.id}/cache`, { method: "POST" });
      setDownloads((current) => mergeDownloads([result, ...current.filter((item) => item.id !== result.id)]));
      notify(`${result.fileName || "视频"} 已加入管理后台缓存信息`);
      return result;
    } catch (err) {
      notify(err.message);
      throw err;
    }
  }

  async function sendMessage(event) {
    event.preventDefault();
    if (!draft.trim() || !activeChat) return;
    const text = draft.trim();
    setDraft("");
    try {
      const sent = await api("/api/messages", { method: "POST", body: JSON.stringify({ account: accountId, peer: activeChat.id, text }) });
      shouldScrollBottomRef.current = true;
      setMessages((current) => [...current, sent]);
    } catch (err) {
      setError(err.message);
      setDraft(text);
    }
  }

  async function enableNotifications() {
    if ("Notification" in window && Notification.permission !== "granted") {
      setNotifications(await Notification.requestPermission() === "granted");
    }
    if (newestAnnouncementId) localStorage.setItem("feigrame.lastAnnouncement", newestAnnouncementId);
    setAnnouncementOpen(true);
  }

  async function loadAnnouncements() {
    const list = await api("/api/announcements").catch(() => []);
    setAnnouncements(list);
  }

  async function loadAbout() {
    const info = await api("/api/about").catch(() => ({}));
    setAbout(info);
  }

  async function loadDownloads() {
    const list = await api("/api/downloads").catch(() => []);
    setDownloads(mergeDownloads(list));
  }

  async function loadSilentCaches() {
    const result = await api("/api/silent-cache").catch(() => ({ enabled: true, rateLimitBps: 0, concurrency: 1, mode: "conservative", tasks: [] }));
    const tasks = Array.isArray(result) ? result : result.tasks || [];
    setSilentCacheState({
      enabled: Array.isArray(result) ? true : result.enabled !== false,
      rateLimitBps: Array.isArray(result) ? 0 : Number(result.rateLimitBps || 0),
      concurrency: Array.isArray(result) ? 1 : Number(result.concurrency || 1),
      mode: Array.isArray(result) ? "conservative" : result.mode || "conservative"
    });
    setSilentCaches(sortSilentCaches(tasks));
  }

  async function updateSilentCacheControl(patch) {
    try {
      if (patch?.reorder) {
        const { fromId, toId } = patch.reorder;
        const current = [...silentCaches];
        const from = current.findIndex((item) => item.id === fromId);
        const to = current.findIndex((item) => item.id === toId);
        if (from < 0 || to < 0) return;
        const [moved] = current.splice(from, 1);
        current.splice(to, 0, moved);
        setSilentCaches(current);
        const result = await api("/api/silent-cache/reorder", {
          method: "POST",
          body: JSON.stringify({ orderedIds: current.map((item) => item.id) })
        });
        setSilentCacheState({ enabled: result.enabled !== false, rateLimitBps: Number(result.rateLimitBps || 0), concurrency: Number(result.concurrency || 1), mode: result.mode || "conservative" });
        setSilentCaches(sortSilentCaches(result.tasks || []));
        return;
      }
      const result = await api("/api/silent-cache/control", { method: "PUT", body: JSON.stringify(patch) });
      setSilentCacheState({ enabled: result.enabled !== false, rateLimitBps: Number(result.rateLimitBps || 0), concurrency: Number(result.concurrency || 1), mode: result.mode || "conservative" });
      setSilentCaches(sortSilentCaches(result.tasks || []));
    } catch (err) {
      notify(err.message);
    }
  }

  async function cancelSilentCache(task) {
    try {
      const result = await api(`/api/silent-cache/${encodeURIComponent(task.id)}`, { method: "DELETE" });
      setSilentCaches((current) => current.filter((item) => item.id !== result.id));
    } catch (err) {
      notify(err.message);
    }
  }

  async function updateDownload(task, action, method = "POST") {
    try {
      const result = await api(`/api/downloads/${encodeURIComponent(task.id)}/${action}`, { method });
      if (result?.id) {
        setDownloads((current) => mergeDownloads([result, ...current.filter((item) => item.id !== result.id)]));
      }
      return result;
    } catch (err) {
      notify(err.message);
      throw err;
    }
  }

  async function startDownload(task) {
    return updateDownload(task, "start");
  }

  async function cancelDownload(task) {
    return updateDownload(task, "cancel");
  }

  async function clearDownload(task) {
    try {
      await api(`/api/downloads/${encodeURIComponent(task.id)}/clear`, { method: "POST" });
      setDownloads((current) => current.filter((item) => item.id !== task.id));
    } catch (err) {
      notify(err.message);
    }
  }

  async function deleteDownload(task) {
    try {
      await api(`/api/downloads/${encodeURIComponent(task.id)}`, { method: "DELETE" });
      setDownloads((current) => current.filter((item) => item.id !== task.id));
    } catch (err) {
      notify(err.message);
    }
  }

  async function loadSettings() {
    const settings = await api("/api/settings").catch(() => ({}));
    setAppSettings({
      notificationEnabled: settings.notificationEnabled !== false,
      notificationPreview: settings.notificationPreview !== false,
      privacyOpenTelegramLinksInApp: settings.privacyOpenTelegramLinksInApp !== false,
      privacyMediaPreview: settings.privacyMediaPreview !== false,
      messageShowSender: settings.messageShowSender !== false,
      foldersEnabled: settings.foldersEnabled !== false,
      foldersShowArchived: Boolean(settings.foldersShowArchived),
      foldersAutoSelectFirst: settings.foldersAutoSelectFirst !== false,
      playerMode: settings.playerMode || "browser"
    });
  }

  if (!token) return <AuthGate onReady={(nextToken, user) => { setTokenState(nextToken); setMe(user); }} />;

  return (
    <main className={cx("app-shell", activeChat && "chat-open")}>
      {toast && <div className="toast-banner">{toast}</div>}
      <aside className="sidebar">
        <div className="topbar">
          <div className="brand-compact"><MessageSquare size={20} /> Feigram</div>
          <div className="tools">
            <button className="icon-button" onClick={toggleTheme} title="切换主题">{theme === "dark" ? <Sun size={18} /> : <Moon size={18} />}</button>
            <button className={cx("icon-button", unreadAnnouncement && "has-notice")} onClick={enableNotifications} title="通知与公告"><Bell size={18} /></button>
            <button className="icon-button" onClick={() => { setAdminInitialTab("accounts"); setAdminOpen(true); }} title={me?.role === "admin" ? "管理员后台" : "账号后台"}><Users size={18} /></button>
          </div>
        </div>
        <div className="account-row current-account-row">
          {activeAccount ? <>
            <Avatar accountId={activeAccount.id} label={activeAccount.displayName || activeAccount.label} size={40} />
            <div className="current-account-copy">
              <strong>{activeAccount.displayName || activeAccount.label}</strong>
              <span>{activeAccount.username ? `@${activeAccount.username}` : activeAccount.phoneNumber || "Telegram"}</span>
            </div>
            {accounts.length > 1 && <select value={accountId} onChange={(event) => {
              setAccountId(event.target.value);
              setActiveChat(null);
              setChatStack([]);
              setMessages([]);
            }} title="切换 Telegram 账号">
              {accounts.map((account) => <option key={account.id} value={account.id}>{account.displayName || account.label}</option>)}
            </select>}
          </> : <button className="secondary action-button" onClick={() => { setAdminInitialTab("accounts"); setAdminOpen(true); }}><Plus size={18} />添加 Telegram 账号</button>}
        </div>
        <div className="sidebar-main">
          {appSettings.foldersEnabled && <nav className="folder-tabs">
            <button className={cx(activeFolder === "all" && "active")} onClick={() => setActiveFolder("all")}>
              <MessageSquare size={24} /><span>全部</span>
            </button>
            {folders.map((folder) => <button key={folder.id} className={cx(String(activeFolder) === String(folder.id) && "active")} onClick={() => setActiveFolder(folder.id)}>
              <Folder size={24} /><span>{folder.emoticon ? `${folder.emoticon} ` : ""}{folder.title}</span>
              {!!folder.chatIds?.length && <b>{folder.chatIds.length}</b>}
            </button>)}
            <button onClick={() => { setAdminInitialTab("folders"); setAdminOpen(true); }}>
              <SlidersHorizontal size={24} /><span>编辑</span>
            </button>
          </nav>}
          <div className="chat-pane">
            <form className="search" onSubmit={(event) => { event.preventDefault(); loadChats(query); }}>
              <Search size={17} />
              <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索私聊、群组、频道" />
              {query && <button type="button" title="清除搜索" onClick={clearSearch}><X size={16} /></button>}
              <button title="搜索"><RefreshCw size={16} /></button>
            </form>
            <div className="chat-list">
              {error && !activeChat && <div className="sidebar-error">
                <strong>加载失败</strong>
                <span>{error}</span>
                <button type="button" onClick={() => {
                  refreshAccounts(true);
                  if (accountId) loadChats(query);
                }}>重新加载</button>
              </div>}
              {visibleChats.map((chat) => <button key={chat.id} className={cx("chat-item", activeChat?.id === chat.id && "active")} onClick={() => selectChat(chat)}>
                <Avatar accountId={accountId} peerId={chat.id} label={chat.title} />
                <span className="chat-copy"><strong>{chat.title}</strong><small>{chat.lastMessage?.text || chat.type}</small></span>
                {chat.unreadCount > 0 && <span className="badge">{chat.unreadCount}</span>}
              </button>)}
              {!visibleChats.length && !error && <div className="empty">暂无会话</div>}
            </div>
          </div>
        </div>
      </aside>
      <section className="conversation">
        {activeChat ? <>
          <header className="conversation-head">
            <button className={cx("icon-button", chatStack.length ? "nav-back-button" : "back-button")} onClick={chatStack.length ? returnToPreviousChat : () => setActiveChat(null)} title={chatStack.length ? "返回上层位置" : "返回会话列表"}><ArrowLeft size={18} /></button>
            <button className="conversation-title-button" type="button" onClick={openChatInfo} title="查看群组信息">
              <Avatar accountId={accountId} peerId={activeChat.id} label={activeChat.title} size={42} />
              <span><h2>{activeChat.title}</h2><p>{activeChat.type} {activeChat.username ? `@${activeChat.username}` : ""}</p></span>
            </button>
            <button className="icon-button" onClick={openChatInfo} title="群组信息"><Info size={18} /></button>
          </header>
          {error && <p className="error inline">{error}</p>}
          <div className="messages" ref={messagesRef}>
            {hasOlder && <button className="history-button" onClick={loadOlderMessages} disabled={loadingOlder}>{loadingOlder ? "加载中" : "加载更早消息"}</button>}
            {messageItems.map((item) => {
              const ids = item.type === "group" ? item.messages.map((message) => String(message.id)) : [String(item.message.id)];
              return <MessageBubble
                key={item.id}
                item={item}
                accountId={accountId}
                chatId={activeChat.id}
                showSender={appSettings.messageShowSender}
                showMedia={appSettings.privacyMediaPreview}
                playerMode={appSettings.playerMode}
                onOpenLink={openTelegramLink}
                onInlineButton={clickInlineButton}
                onCacheMedia={cacheMedia}
                downloadTasks={downloads}
                highlighted={ids.includes(String(highlightMessageId))}
                messageRef={(node) => {
                  ids.forEach((id) => {
                    if (node) messageNodeRefs.current.set(id, node);
                    else messageNodeRefs.current.delete(id);
                  });
                }}
              />;
            })}
            {busy && <div className="empty">加载中</div>}
          </div>
          <form className="composer" onSubmit={sendMessage}><textarea value={draft} onChange={(event) => setDraft(event.target.value)} onKeyDown={(event) => {
            if (event.key === "Enter" && !event.shiftKey) {
              event.preventDefault();
              sendMessage(event);
            }
          }} placeholder="输入消息" rows={1} /><button className="primary" title="发送"><Send size={18} /></button></form>
        </> : <div className="blank-state">
          <MessageSquare size={40} />
          <h2>选择或添加一个 Telegram 账号</h2>
          {error && <p className="error inline">{error}</p>}
          {error && <button className="secondary action-button" type="button" onClick={() => {
            refreshAccounts(true);
            if (accountId) loadChats(query);
          }}><RefreshCw size={16} />重新加载</button>}
        </div>}
      </section>
      <ChatInfoPanel
        open={chatInfoOpen}
        accountId={accountId}
        chat={activeChat}
        details={chatDetails}
        loading={chatDetailsLoading}
        autoCache={activeChat ? autoCacheChats[`${accountId}:${activeChat.id}`] : false}
        autoCacheBusy={autoCacheBusy}
        mediaLoadingMore={chatMediaLoadingMore}
        onAutoCacheChange={setChatAutoCache}
        onLoadMoreMedia={loadMoreChatMedia}
        onOpenMedia={openInfoMedia}
        onClose={() => setChatInfoOpen(false)}
      />
      <AdminPanel
        accounts={accounts}
        accountId={accountId}
        canAdmin={me?.role === "admin"}
        onAccountChange={(nextId) => {
          setAccountId(nextId);
          setActiveChat(null);
          setChatStack([]);
          setMessages([]);
          setAdminOpen(false);
        }}
        onAccountLogout={logoutAccount}
        onAccountsChanged={() => refreshAccounts(true)}
        onSettingsChanged={loadSettings}
        open={adminOpen}
        initialTab={adminInitialTab}
        onClose={() => setAdminOpen(false)}
        socket={socket}
        silentCacheState={silentCacheState}
        silentCaches={silentCaches}
        onRefreshSilentCaches={loadSilentCaches}
        onSilentCacheControl={updateSilentCacheControl}
        onCancelSilentCache={cancelSilentCache}
      />
      <InfoModal announcements={announcements} about={about} open={announcementOpen} onClose={() => setAnnouncementOpen(false)} />
      <PlaybackModal item={playback} playerMode={appSettings.playerMode} onClose={() => setPlayback(null)} />
    </main>
  );
}

createRoot(document.getElementById("root")).render(<App />);
