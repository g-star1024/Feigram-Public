// nativeMediaAdapter.test.js
//
// 验证「原生账号消息元数据适配器」的映射契约。该适配器把 Go 原生 MTProto 序列化的
// 消息 JSON（downloader/cmd/feigram-downloader/chatapi.go 的 serializeNativeMessage /
// serializeNativeMedia）映射为与 GramJS mediaMessage() 兼容的 { entity, message } 形状。
//
// 关键：无需任何 Telegram 凭据或运行中的 Go 服务即可验证映射正确性——
// 下列断言逐一锁定 nativeFileLocation / mediaKind / mediaFileInfo 实际读取的字段
// （document.id/accessHash/fileReference/dcId/size/mimeType、file.size/name/dcId、
// photo/video 存在性），确保 native 下载任务能拿到与 GramJS 路径等价的元数据。

const test = require("node:test");
const assert = require("node:assert/strict");
const { goMessageToNodeMessage, findNativeMessage } = require("../src/nativeMediaAdapter");

// 与 chatapi.go serializeNativeMedia 的 MessageMediaDocument 输出逐字段对齐。
function documentMessage(id, overrides = {}) {
  const fileReference = Buffer.from("sample-bytes").toString("base64");
  return {
    id,
    text: "a video",
    entities: [],
    outgoing: false,
    senderId: "User:1",
    sender: { id: "User:1", title: "Alice", username: "alice", rawId: "1" },
    groupedId: "",
    buttons: [],
    media: {
      className: "MessageMediaDocument",
      hasPreview: true,
      mimeType: "video/mp4",
      kind: "video",
      width: 1920,
      height: 1080,
      duration: 30,
      fileName: "clip.mp4",
      size: "1234567",
      dcId: 2,
      fileId: "55667788",
      accessHash: "99887766",
      fileReference,
      ...overrides
    }
  };
}

test("goMessageToNodeMessage 把 Go 文档消息映射为 nativeFileLocation 所需的字段", () => {
  const item = documentMessage(102);
  const message = goMessageToNodeMessage(item);

  // 顶层 id
  assert.equal(message.id, 102);

  // mediaKind 依据 video 存在性判定为 video
  assert.deepEqual(message.video, {});
  assert.equal(message.photo, undefined);

  // nativeFileLocation 读取的 document 字段
  assert.equal(message.document.id, "55667788");
  assert.equal(message.document.accessHash, "99887766");
  assert.equal(message.document.dcId, 2);
  assert.equal(message.document.size, 1234567);
  assert.equal(message.document.mimeType, "video/mp4");
  assert.ok(Buffer.isBuffer(message.document.fileReference));
  assert.deepEqual(message.document.fileReference, Buffer.from("sample-bytes"));

  // nativeFileLocation / mediaFileInfo 读取的 file 字段
  assert.equal(message.file.size, 1234567);
  assert.equal(message.file.name, "clip.mp4");
  assert.equal(message.file.dcId, 2);
});

test("goMessageToNodeMessage 处理照片消息：photo 存在、video 不存在", () => {
  const item = documentMessage(200, {
    className: "MessageMediaPhoto",
    mimeType: "image/jpeg",
    kind: "image",
    width: 800,
    height: 600,
    fileName: "photo-12345.jpg",
    size: "0",
    dcId: 0,
    fileId: "",
    accessHash: "",
    fileReference: ""
  });
  const message = goMessageToNodeMessage(item);

  assert.deepEqual(message.photo, {});
  assert.equal(message.video, undefined);
  // 照片在 Go 序列化里 fileId/dcId 为空，映射后保持空（照片经 Go blob 下载的元数据由 Go 侧另行处理，属既有范围）。
  assert.equal(message.document.id, "");
  assert.equal(message.document.dcId, 0);
  assert.equal(message.file.name, "photo-12345.jpg");
  assert.equal(message.file.size, 0);
});

test("goMessageToNodeMessage 对无预览（不支持媒体）返回未定义 document", () => {
  const item = {
    id: 300,
    media: {
      className: "MessageMediaUnsupported",
      hasPreview: false,
      kind: "file",
      mimeType: "",
      fileName: "",
      size: "0",
      dcId: 0,
      fileId: "",
      accessHash: "",
      fileReference: ""
    }
  };
  const message = goMessageToNodeMessage(item);
  assert.equal(message.document, undefined);
  assert.equal(message.photo, undefined);
  assert.equal(message.video, undefined);
  assert.equal(message.file.name, "telegram-300");
});

test("findNativeMessage 按 messageId 精确定位", () => {
  const items = [
    { id: 1, media: { hasPreview: true } },
    { id: 2, media: { hasPreview: true } },
    { id: 3, media: { hasPreview: true } }
  ];
  assert.equal(findNativeMessage(items, 2).id, 2);
  assert.equal(findNativeMessage(items, "3").id, 3);
  assert.equal(findNativeMessage(items, 9), null);
  assert.equal(findNativeMessage(null, 1), null);
  assert.equal(findNativeMessage(undefined, 1), null);
});
