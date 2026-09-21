export function getToken() {
  return localStorage.getItem("feigrame.token") || "";
}

export function setToken(token) {
  localStorage.setItem("feigrame.token", token);
}

export async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
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
}

export async function appLogin(password) {
  const payload = await api("/api/login", {
    method: "POST",
    body: JSON.stringify(password)
  });
  setToken(payload.token);
  return payload;
}
