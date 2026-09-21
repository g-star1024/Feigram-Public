package main

// M4.1 · Go 原生媒体字节流（替换 Node 侧 GramJS downloadMedia 与 /api/internal/media 桥）
//
// 背景：M4.2 之后已迁移到 Go 的账号（authMode=native）在 Node 侧拿不到 GramJS 客户端，
// getClientUnlocked 会以 409 直接拒绝，导致 downloadMedia / streamVideoMedia / mediaThumbnail
// 对 native 账号全部不可用——这是 M4.2 引入的功能回归。本文件把「按消息读取媒体字节」
// 平移到 Go 原生 MTProto，使 native 账号无需 GramJS 即可播放、预览与下载媒体。
//
// 路由（GET，挂在既有 /api/accounts/ 子树下）：
//   GET /api/accounts/{accountID}/blob?userId=&peer=&messageId=&thumb=1
//
// 支持 HTTP Range：带 Range 头时返回 206 + Content-Range，供视频拖动与分片播放；
// 不带 Range 时返回 200 全量字节（按块流式写出，不在内存中整份缓存）。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const (
	mediaBlobChunkSize = 512 * 1024
	mediaBlobTimeout   = 180 * time.Second
	// maxMediaBlobBytes 是单次非 Range 请求允许读取的上限，避免大文件打爆内存。
	maxMediaBlobBytes = 256 * 1024 * 1024
)

// nativeMediaSource 描述一条消息媒体可供 upload.getFile 使用的定位信息。
type nativeMediaSource struct {
	Location tg.InputFileLocationClass
	Size     int64
	MimeType string
	FileName string
	DCID     int
}

// mediaLocationFromMessage 从消息媒体构造 InputFileLocation。
//
// thumb=true 时取缩略图（供列表封面使用）；文档无缩略图时返回错误，
// 由调用方回退到整图拉取，与 Node 侧 mediaThumbnail 的既有行为保持一致。
func mediaLocationFromMessage(message *tg.Message, thumb bool) (nativeMediaSource, error) {
	if message == nil {
		return nativeMediaSource{}, errors.New("消息不存在")
	}
	media, ok := message.GetMedia()
	if !ok || media == nil {
		return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
	}
	switch typed := media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := typed.GetDocument()
		if !ok {
			return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
		}
		value, ok := doc.(*tg.Document)
		if !ok || value == nil {
			return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
		}
		thumbSize := ""
		size := value.Size
		if thumb {
			thumbSize, size = documentThumbSize(value)
			if thumbSize == "" {
				return nativeMediaSource{}, errors.New("暂无缩略图")
			}
		}
		return nativeMediaSource{
			Location: &tg.InputDocumentFileLocation{
				ID:            value.ID,
				AccessHash:    value.AccessHash,
				FileReference: value.FileReference,
				ThumbSize:     thumbSize,
			},
			Size:     size,
			MimeType: value.MimeType,
			FileName: documentFileName(value),
			DCID:     value.DCID,
		}, nil
	case *tg.MessageMediaPhoto:
		photo, ok := typed.GetPhoto()
		if !ok {
			return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
		}
		value, ok := photo.(*tg.Photo)
		if !ok || value == nil {
			return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
		}
		_, thumbSize, sizeBytes := photoTargetSize(value, thumb)
		if thumbSize == "" {
			return nativeMediaSource{}, errors.New("暂无缩略图")
		}
		return nativeMediaSource{
			Location: &tg.InputPhotoFileLocation{
				ID:            value.ID,
				AccessHash:    value.AccessHash,
				FileReference: value.FileReference,
				ThumbSize:     thumbSize,
			},
			Size:     sizeBytes,
			MimeType: "image/jpeg",
			FileName: fmt.Sprintf("photo-%d.jpg", value.ID),
			DCID:     value.DCID,
		}, nil
	}
	return nativeMediaSource{}, errors.New("这条消息没有可下载媒体")
}

// documentThumbSize 选取文档缩略图：Node 侧取 thumbs 的最后一项，这里保持一致。
func documentThumbSize(doc *tg.Document) (string, int64) {
	if len(doc.Thumbs) == 0 {
		return "", 0
	}
	if size, ok := doc.Thumbs[len(doc.Thumbs)-1].(*tg.PhotoSize); ok && size != nil {
		return size.Type, int64(size.Size)
	}
	if size, ok := doc.Thumbs[0].(*tg.PhotoSize); ok && size != nil {
		return size.Type, int64(size.Size)
	}
	return "", 0
}

func documentFileName(doc *tg.Document) string {
	for _, attribute := range doc.Attributes {
		if typed, ok := attribute.(*tg.DocumentAttributeFilename); ok && typed != nil {
			return typed.FileName
		}
	}
	return fmt.Sprintf("document-%d", doc.ID)
}

// photoTargetSize 选取照片的目标尺寸：缩略图取最小，原图取最大。
// Telegram 的 InputPhotoFileLocation 必须给出一个真实存在的尺寸类型。
func photoTargetSize(photo *tg.Photo, thumb bool) (*tg.PhotoSize, string, int64) {
	var best *tg.PhotoSize
	for _, entry := range photo.Sizes {
		size, ok := entry.(*tg.PhotoSize)
		if !ok || size == nil || size.Type == "" {
			continue
		}
		if best == nil {
			best = size
			continue
		}
		better := size.W*size.H > best.W*best.H
		if thumb {
			better = size.W*size.H < best.W*best.H
		}
		if better {
			best = size
		}
	}
	if best == nil {
		return nil, "", 0
	}
	return best, best.Type, int64(best.Size)
}

// readFileRange 按 [offset, offset+limit) 读取若干字节。
// 末端返回不足一块即视为读完；已读到数据时的读错误按「读到此为止」处理，
// 避免把最后一块的边界错误放大成整次请求失败。
func readFileRange(ctx context.Context, api *tg.Client, location tg.InputFileLocationClass, offset int64, limit int64) ([]byte, error) {
	data := make([]byte, 0, limit)
	for int64(len(data)) < limit {
		want := limit - int64(len(data))
		chunk := int64(mediaBlobChunkSize)
		if want < chunk {
			chunk = want
		}
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: location,
			Offset:   offset + int64(len(data)),
			Limit:    int(chunk),
		})
		if err != nil {
			if len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			if len(data) > 0 {
				return data, nil
			}
			return nil, fmt.Errorf("unexpected upload.getFile response: %T", result)
		}
		data = append(data, file.Bytes...)
		if len(file.Bytes) < int(chunk) {
			break
		}
	}
	return data, nil
}

// readFileRangeWithMigration 在命中 FILE_MIGRATE_x 时切到媒体 DC 重读。
func readFileRangeWithMigration(ctx context.Context, client *telegram.Client, location tg.InputFileLocationClass, offset int64, limit int64) ([]byte, error) {
	data, err := readFileRange(ctx, client.API(), location, offset, limit)
	if err == nil {
		return data, nil
	}
	dc := migrationDC(err)
	if dc <= 0 {
		return nil, err
	}
	invoker, invokerErr := client.MediaOnly(ctx, dc, 1)
	if invokerErr != nil {
		return nil, err
	}
	defer invoker.Close()
	return readFileRange(ctx, tg.NewClient(invoker), location, offset, limit)
}

// fetchNativeMediaMeta 只解析定位信息，不下载字节，用于确定文件大小与 MIME。
func (a *App) fetchNativeMediaMeta(ctx context.Context, client *telegram.Client, account NativeAccount, peerID string, messageID int, thumb bool) (nativeMediaSource, error) {
	var meta nativeMediaSource
	var err error
	runErr := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		info, resolveErr := a.resolveNativePeer(ctx, api, account, peerID)
		if resolveErr != nil {
			return resolveErr
		}
		if err = ensureNativeMediaPeer(a, account, info); err != nil {
			return err
		}
		message, msgErr := a.fetchNativeMessageByID(ctx, api, info, messageID)
		if msgErr != nil {
			return msgErr
		}
		meta, err = mediaLocationFromMessage(message, thumb)
		return err
	})
	if runErr != nil {
		return nativeMediaSource{}, runErr
	}
	return meta, nil
}

// ensureNativeMediaPeer 占位：peer 解析已在 resolveNativePeer 完成，
// 保留此函数是为了让调用点显式表达「媒体读取依赖 peer 元数据」这一约束。
func ensureNativeMediaPeer(_ *App, _ NativeAccount, info nativePeerInfo) error {
	if strings.TrimSpace(info.PeerID) == "" {
		return errors.New("缺少 peer 参数")
	}
	return nil
}

// fetchNativeMediaBytes 读取 [offset, offset+limit) 区间，带 file_reference 过期续期。
// 续期方式沿用 M3.3 的思路：重新拉取消息拿到新的 file_reference 后重建定位信息，
// 并受 allowFileReferenceRefresh 预算封顶，避免无限重试。
func (a *App) fetchNativeMediaBytes(ctx context.Context, client *telegram.Client, account NativeAccount, peerID string, messageID int, thumb bool, offset int64, limit int64) ([]byte, nativeMediaSource, error) {
	var meta nativeMediaSource
	var data []byte
	runErr := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		info, resolveErr := a.resolveNativePeer(ctx, api, account, peerID)
		if resolveErr != nil {
			return resolveErr
		}
		message, msgErr := a.fetchNativeMessageByID(ctx, api, info, messageID)
		if msgErr != nil {
			return msgErr
		}
		meta, msgErr = mediaLocationFromMessage(message, thumb)
		if msgErr != nil {
			return msgErr
		}
		for attempt := 1; ; attempt++ {
			var readErr error
			data, readErr = readFileRangeWithMigration(ctx, client, meta.Location, offset, limit)
			if readErr == nil {
				return nil
			}
			if !isFileReferenceError(readErr) || !allowFileReferenceRefresh(attempt) {
				return readErr
			}
			refreshed, refreshErr := a.fetchNativeMessageByID(ctx, api, info, messageID)
			if refreshErr != nil {
				return readErr
			}
			newMeta, metaErr := mediaLocationFromMessage(refreshed, thumb)
			if metaErr != nil {
				return readErr
			}
			meta = newMeta
			if nativeFileRefRefreshHook != nil {
				if hookErr := nativeFileRefRefreshHook(attempt); hookErr != nil {
					return hookErr
				}
			}
		}
	})
	if runErr != nil {
		return nil, nativeMediaSource{}, runErr
	}
	return data, meta, nil
}

// parseMediaRange 解析 Range 头，返回 [start, end] 闭区间与是否为 Range 请求。
// 非法或无法满足的 Range 一律退化为全量，避免向上传播 416 处理分支。
func parseMediaRange(header string, size int64) (int64, int64, bool) {
	raw := strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(raw), "bytes=") {
		return 0, 0, false
	}
	body := strings.TrimSpace(raw[len("bytes="):])
	if body == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(body, ",", 2)[0]
	parts = strings.TrimSpace(parts)
	dash := strings.Index(parts, "-")
	if dash < 0 {
		return 0, 0, false
	}
	startText := strings.TrimSpace(parts[:dash])
	endText := strings.TrimSpace(parts[dash+1:])
	start := int64(0)
	end := size - 1
	if startText != "" {
		value, err := strconv.ParseInt(startText, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = value
		if endText != "" {
			value, err := strconv.ParseInt(endText, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			end = value
		}
	} else if endText != "" {
		suffix, err := strconv.ParseInt(endText, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = size - suffix
		if start < 0 {
			start = 0
		}
	} else {
		return 0, 0, false
	}
	if size > 0 {
		if start >= size {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	if end < start {
		end = start
	}
	return start, end, true
}

// serveNativeMediaBlob 处理 /api/accounts/{accountID}/blob 请求。
func (a *App) serveNativeMediaBlob(w http.ResponseWriter, r *http.Request, client *telegram.Client, account NativeAccount, query url.Values) {
	ctx, cancel := context.WithTimeout(r.Context(), mediaBlobTimeout)
	defer cancel()

	peerID := strings.TrimSpace(query.Get("peer"))
	messageID := intFromQuery(query.Get("messageId"))
	if peerID == "" || messageID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 或 messageId 参数"})
		return
	}
	thumb := query.Get("thumb") == "1"

	meta, err := a.fetchNativeMediaMeta(ctx, client, account, peerID, messageID, thumb)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	start, end, isRange := parseMediaRange(r.Header.Get("Range"), meta.Size)
	if !isRange {
		start, end = 0, meta.Size-1
	}
	total := end - start + 1
	if !isRange && meta.Size > maxMediaBlobBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "媒体过大，请使用 Range 分片读取"})
		return
	}
	if total <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Range 范围无效"})
		return
	}

	// 先读首块：读取失败时还能返回 JSON 错误，而不是已写出 200 头的半截响应。
	first := int64(mediaBlobChunkSize)
	if total < first {
		first = total
	}
	head, _, err := a.fetchNativeMediaBytes(ctx, client, account, peerID, messageID, thumb, start, first)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if len(head) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "媒体内容为空"})
		return
	}

	contentType := strings.TrimSpace(meta.MimeType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers := w.Header()
	headers.Set("Content-Type", contentType)
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Cache-Control", "private, max-age=86400")
	headers.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", mediaBlobFileName(meta)))
	if isRange {
		headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+int64(len(head))-1, meta.Size))
		headers.Set("Content-Length", strconv.FormatInt(int64(len(head)), 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		headers.Set("Content-Length", strconv.FormatInt(int64(len(head)), 10))
		w.WriteHeader(http.StatusOK)
	}
	if _, err := w.Write(head); err != nil {
		log.Printf("write media blob head failed: %v", err)
		return
	}

	// 首块之后的剩余区间按块续读，不在内存中整份缓存。
	sent := int64(len(head))
	for sent < total {
		want := total - sent
		if want > mediaBlobChunkSize {
			want = mediaBlobChunkSize
		}
		chunk, _, err := a.fetchNativeMediaBytes(ctx, client, account, peerID, messageID, thumb, start+sent, want)
		if err != nil {
			log.Printf("write media blob chunk failed: %v", err)
			return
		}
		if len(chunk) == 0 {
			return
		}
		if _, err := w.Write(chunk); err != nil {
			log.Printf("write media blob chunk failed: %v", err)
			return
		}
		sent += int64(len(chunk))
	}
}

func mediaBlobFileName(meta nativeMediaSource) string {
	name := strings.TrimSpace(meta.FileName)
	if name == "" {
		return "telegram-media"
	}
	return name
}
