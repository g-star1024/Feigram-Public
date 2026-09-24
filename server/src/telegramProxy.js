const net = require("net");

const V2RAYA_HOST = process.env.V2RAYA_SOCKS5_HOST || "127.0.0.1";
const V2RAYA_PORT = Number(process.env.V2RAYA_SOCKS5_PORT || 20170);

function socks5Available(host, port, timeoutMs = 1200) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port });
    let settled = false;
    const finish = (available) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      resolve(available);
    };
    socket.setTimeout(timeoutMs, () => finish(false));
    socket.once("error", () => finish(false));
    socket.once("connect", () => socket.write(Buffer.from([0x05, 0x01, 0x00])));
    socket.once("data", (data) => finish(data.length >= 2 && data[0] === 0x05 && data[1] !== 0xff));
  });
}

async function resolveTelegramProxy(settings = {}) {
  const mode = settings.telegramProxyMode || (settings.telegramProxyEnabled ? "manual" : "auto");
  if (mode === "direct") {
    return { enabled: false, mode, source: "direct", host: "", port: 0, username: "", password: "" };
  }
  if (mode === "manual") {
    return {
      enabled: Boolean(settings.telegramProxyEnabled && settings.telegramProxyHost),
      mode,
      source: "manual",
      host: settings.telegramProxyHost || "",
      port: Number(settings.telegramProxyPort || 1080),
      username: settings.telegramProxyUsername || "",
      password: settings.telegramProxyPassword || ""
    };
  }
  const available = await socks5Available(V2RAYA_HOST, V2RAYA_PORT);
  return {
    enabled: available,
    mode: "auto",
    source: available ? "v2rayA" : "direct-fallback",
    host: available ? V2RAYA_HOST : "",
    port: available ? V2RAYA_PORT : 0,
    username: "",
    password: ""
  };
}

module.exports = { resolveTelegramProxy, socks5Available };
