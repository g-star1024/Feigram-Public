// R4.5 · 日志分级与清空能力测试（node --test）
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { parseLogEntries, classifyLogLevel, clearLog } = require("../src/diagnostics");

test("classifyLogLevel 按关键字分级", () => {
  assert.strictEqual(classifyLogLevel("something ERROR happened"), "error");
  assert.strictEqual(classifyLogLevel("连接超时 timed out"), "error");
  assert.strictEqual(classifyLogLevel("2026-01-01 WARN deprecated api"), "warn");
  assert.strictEqual(classifyLogLevel("FLOOD_WAIT 请稍后再试"), "warn");
  assert.strictEqual(classifyLogLevel("info: server started ok"), "info");
});

test("parseLogEntries 拆分、去空白行并分级", () => {
  const entries = parseLogEntries("line one\n\nERROR boom\nWARN slow");
  assert.strictEqual(entries.length, 3);
  assert.strictEqual(entries[0].text, "line one");
  assert.strictEqual(entries[1].level, "error");
  assert.strictEqual(entries[2].level, "warn");
});

test("parseLogEntries 空文本返回空数组", () => {
  assert.deepStrictEqual(parseLogEntries(""), []);
  assert.deepStrictEqual(parseLogEntries(null), []);
});

test("clearLog 截断已知目标文件", async () => {
  const tmp = path.join(os.tmpdir(), `feigram-logtest-${Date.now()}.log`);
  fs.writeFileSync(tmp, "old log content\nsecond line\n");
  const prev = process.env.LOG_FILE;
  process.env.LOG_FILE = tmp;
  try {
    const result = await clearLog("node");
    assert.strictEqual(result.ok, true);
    assert.strictEqual(fs.readFileSync(tmp, "utf8"), "");
  } finally {
    if (prev === undefined) delete process.env.LOG_FILE;
    else process.env.LOG_FILE = prev;
    fs.rmSync(tmp, { force: true });
  }
});

test("clearLog 拒绝非法目标", async () => {
  await assert.rejects(() => clearLog("evil"), /无效的日志目标/);
});
