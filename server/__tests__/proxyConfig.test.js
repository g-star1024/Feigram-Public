const test = require("node:test");
const assert = require("node:assert/strict");

const { describeProxySource, maskProxyUrl, normalizeProxyUrl } = require("../src/proxyConfig");

test("normalizeProxyUrl: 空值视为未配置且不报错", () => {
  for (const input of ["", "   ", null, undefined]) {
    assert.deepEqual(normalizeProxyUrl(input), { value: "", error: "" });
  }
});

test("normalizeProxyUrl: 省略 scheme 时按 socks5 补全", () => {
  assert.deepEqual(normalizeProxyUrl("127.0.0.1:7890"), {
    value: "socks5://127.0.0.1:7890",
    error: ""
  });
});

test("normalizeProxyUrl: 接受四种支持的协议并保留账号密码", () => {
  const cases = [
    ["socks5://127.0.0.1:7890", "socks5://127.0.0.1:7890"],
    ["socks5h://127.0.0.1:1080", "socks5h://127.0.0.1:1080"],
    ["http://proxy.local:3128", "http://proxy.local:3128"],
    ["https://proxy.local:8443", "https://proxy.local:8443"],
    ["socks5://user:pw@127.0.0.1:7890", "socks5://user:pw@127.0.0.1:7890"],
    ["SOCKS5://127.0.0.1:7890", "socks5://127.0.0.1:7890"]
  ];
  for (const [input, expected] of cases) {
    const result = normalizeProxyUrl(input);
    assert.equal(result.error, "", `${input} 不应报错：${result.error}`);
    assert.equal(result.value, expected);
  }
});

test("normalizeProxyUrl: 非法输入给出可读错误并保留原值", () => {
  const cases = [
    ["socks4://127.0.0.1:1080", /不支持的代理协议 socks4/],
    ["socks5://127.0.0.1", /缺少端口/],
    ["socks5://127.0.0.1:7890/extra", /不应包含路径/]
  ];
  for (const [input, pattern] of cases) {
    const result = normalizeProxyUrl(input);
    assert.match(result.error, pattern, `${input} 应报错`);
    assert.equal(result.value, input, `${input} 校验失败时应原样保留，便于界面回显`);
  }
});

test("maskProxyUrl: 抹掉账号密码，只留协议主机端口", () => {
  assert.equal(maskProxyUrl("socks5://user:supersecret@127.0.0.1:7890"), "socks5://127.0.0.1:7890");
  assert.equal(maskProxyUrl("http://proxy.local:3128"), "http://proxy.local:3128");
  assert.equal(maskProxyUrl(""), "");
  assert.equal(maskProxyUrl("这不是地址"), "");
  assert.ok(!maskProxyUrl("socks5://user:supersecret@127.0.0.1:7890").includes("supersecret"));
});

test("describeProxySource: 各类来源都有可读文案", () => {
  assert.equal(describeProxySource("settings"), "应用内设置");
  assert.equal(describeProxySource("env:ALL_PROXY"), "环境变量 ALL_PROXY");
  assert.equal(describeProxySource("none"), "未启用（直连）");
  assert.equal(describeProxySource("invalid"), "配置无效");
  assert.equal(describeProxySource(""), "未启用（直连）");
});

test("normalizeProxyUrl 与 Go 侧规则一致：同输入同结果", () => {
  // 这些用例在 Go 侧 proxy_test.go 的 TestNormalizeProxyURLAcceptsSupportedSchemes
  // / RejectsBadInput 中有对应用例，两边必须给出同样的判定结论。
  for (const input of ["127.0.0.1:7890", "socks5h://127.0.0.1:1080", "http://proxy.local:3128"]) {
    assert.equal(normalizeProxyUrl(input).error, "", `${input} 两端都应通过`);
  }
  for (const input of ["socks4://127.0.0.1:1080", "socks5://127.0.0.1", "socks5://127.0.0.1:7890/extra"]) {
    assert.notEqual(normalizeProxyUrl(input).error, "", `${input} 两端都应拒绝`);
  }
});
