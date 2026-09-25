export function getToken() {
  return localStorage.getItem("feigrame.token") || "";
}

export function setToken(token) {
  localStorage.setItem("feigrame.token", token);
}

export async function api(path, options = {}) {
  // R4.51：前端请求超时兜底。此前浏览器→Node 一段是裸 fetch，没有任何超时——
  // webview 半开连接或 Node 僵死时，调用方 Promise 永不落定，「加载更早消息」等
  // 按钮会永久停在「加载中」。timeoutMs 超时后中止并翻译成中文提示。
  const timeoutMs = Number(options.timeoutMs) > 0 ? Number(options.timeoutMs) : 0;
  const controller = new AbortController();
  const external = options.signal;
  const relayAbort = () => controller.abort(external.reason);
  if (external) {
    if (external.aborted) controller.abort(external.reason);
    else external.addEventListener("abort", relayAbort, { once: true });
  }
  const timer = timeoutMs ? setTimeout(() => controller.abort(new DOMException("timeout", "TimeoutError")), timeoutMs) : null;
  try {
    const response = await fetch(path, {
      ...options,
      signal: controller.signal,
      // R4.54（方案 C）：会话/API 请求标记高优先级，让浏览器调度器优先分配
      // 连接与带宽（预览图已统一降为 low 并经 mediaQueue 排队，见方案 A）。
      // 不支持该提示的实现会忽略此字段，无副作用。
      priority: "high",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${getToken()}`,
        ...(options.headers || {})
      }
    });
    const contentType = response.headers.get("content-type") || "";
    const payload = contentType.includes("application/json") ? await response.json() : await response.text();
    if (!response.ok) {
      const error = new Error(payload?.error || payload || "请求失败");
      // M2.4：保留服务端下发的降级标记（如 needsRelogin），
      // 否则前端无法判断迁移失败后应自动切换到「重新登录」。
      if (payload && typeof payload === "object") {
        error.status = response.status;
        error.needsRelogin = Boolean(payload.needsRelogin);
      }
      throw error;
    }
    return payload;
  } catch (err) {
    if (err?.name === "TimeoutError" || (err?.name === "AbortError" && timeoutMs && !external?.aborted)) {
      throw new Error("请求超时，请检查网络后重试");
    }
    throw err;
  } finally {
    if (timer) clearTimeout(timer);
    if (external) external.removeEventListener("abort", relayAbort);
  }
}

export async function appLogin(password) {
  const payload = await api("/api/login", {
    method: "POST",
    body: JSON.stringify(password)
  });
  setToken(payload.token);
  return payload;
}
