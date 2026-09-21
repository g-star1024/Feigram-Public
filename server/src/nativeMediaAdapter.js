// nativeMediaAdapter.js
//
// 原生账号（authMode=native）不再持有 GramJS 客户端：getClient 对 native 账号直接抛 409，
// 因此 ensureGoDownloadTask / mediaNativeMetadata / cacheLargeVideosInChatGo 等原生路径
// 不能再用 GramJS 的 mediaMessage() 取消息元数据，必须改经 Go 原生 MTProto（/messages + /peer）。
//
// 本模块把 Go 序列化后的消息/peer JSON（见 downloader/cmd/feigram-downloader/chatapi.go
// 的 serializeNativeMessage / serializeNativeMedia / resolveNativePeer）映射为与 GramJS
// mediaMessage() 兼容的 { entity, message } 形状，供 nativeFileLocation / nativePeerLocation /
// mediaKind / mediaFileInfo 直接消费。
//
// 关键：本模块为纯函数、无副作用（不连接数据库、不读取凭据、不发起网络请求），
// 因此可在无凭据的沙箱里用 node --test 直接单测，验证映射契约。

/**
 * 把 Go /messages 返回的单个消息条目映射为与 GramJS 消息兼容的形状。
 * Go 的 media 子对象字段（fileId/accessHash/fileReference/dcId/size/mimeType/kind）即为
 * 原生下载任务（nativeFileLocation）所需的全部元数据，故直接平移。
 *
 * @param {object} item Go 序列化的消息条目（含 id 与 media）
 * @returns {{id:number, photo?:{}, video?:{}, document?:object, file:object}}
 */
function goMessageToNodeMessage(item) {
  const media = item && item.media ? item.media : {};
  const hasPreview = media.hasPreview === true;
  const isPhoto = media.className === "MessageMediaPhoto";
  const isVideo = media.kind === "video";
  const document = {
    id: String(media.fileId || ""),
    accessHash: String(media.accessHash || ""),
    fileReference: Buffer.from(String(media.fileReference || ""), "base64"),
    dcId: Number(media.dcId || 0),
    size: Number(media.size || 0),
    mimeType: String(media.mimeType || "")
  };
  return {
    id: Number(item && item.id ? item.id : 0),
    // mediaKind 依据 photo / video 存在性判定类型；仅在有可下载媒体时给出。
    photo: isPhoto && hasPreview ? {} : undefined,
    video: isVideo ? {} : undefined,
    document: hasPreview ? document : undefined,
    file: {
      size: Number(media.size || 0),
      name: media.fileName || `telegram-${item && item.id ? item.id : "unknown"}`,
      dcId: Number(media.dcId || 0)
    }
  };
}

/**
 * 从 Go /messages 列表里按 messageId 定位目标消息。
 * Go 侧 fetchNativeMessages 在 around 模式下已保证目标消息被返回（命中窗口或回退 fetchNativeMessageByID），
 * 这里仅做精确匹配。
 *
 * @param {Array<object>|*} items Go 返回的消息数组
 * @param {number|string} messageId 目标消息 id
 * @returns {object|null}
 */
function findNativeMessage(items, messageId) {
  const list = Array.isArray(items) ? items : [];
  const target = Number(messageId);
  return list.find((entry) => Number(entry && entry.id) === target) || null;
}

module.exports = { goMessageToNodeMessage, findNativeMessage };
