const assert = require("node:assert/strict");
const net = require("node:net");
const test = require("node:test");
const { resolveTelegramProxy, socks5Available } = require("./telegramProxy");

test("detects a SOCKS5 endpoint by protocol handshake", async (t) => {
  const server = net.createServer((socket) => socket.once("data", () => socket.write(Buffer.from([0x05, 0x00]))));
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.equal(await socks5Available("127.0.0.1", address.port), true);
});

test("rejects a non-SOCKS endpoint", async (t) => {
  const server = net.createServer((socket) => socket.once("data", () => socket.write("HTTP/1.1 200 OK\r\n\r\n")));
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.equal(await socks5Available("127.0.0.1", address.port), false);
});

test("manual and direct modes resolve without probing", async () => {
  assert.deepEqual(await resolveTelegramProxy({ telegramProxyMode: "direct" }), {
    enabled: false, mode: "direct", source: "direct", host: "", port: 0, username: "", password: ""
  });
  const manual = await resolveTelegramProxy({
    telegramProxyMode: "manual",
    telegramProxyEnabled: true,
    telegramProxyHost: "10.0.0.2",
    telegramProxyPort: "1088",
    telegramProxyUsername: "u",
    telegramProxyPassword: "p"
  });
  assert.equal(manual.enabled, true);
  assert.equal(manual.host, "10.0.0.2");
  assert.equal(manual.port, 1088);
});
