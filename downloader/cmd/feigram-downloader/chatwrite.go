package main

// M4.3 · Go Telegram Core 写路径与会话详情
//
// 背景（M4.1 的真正前置）：Node telegramService 的 sendText / clickMessageButton /
// chatDetails 三个函数在 native 账号下没有 Go 对等实现，只能走 GramJS getClient，
// 而 getClient 对 authMode=native 直接抛 409。因此卸载 telegram npm 会让
// 「发消息 / 点按钮 / 会话详情」对全部账号失效（重大回归）。
// 本文件把三条写/交互路径平移到 Go 原生 MTProto，使卸载 telegram 后功能不回归。
//
// 路由（挂在既有 /api/accounts/ 子树）：
//   POST /api/accounts/{accountID}/send?userId=&peer=             body {"text": "..."}
//   POST /api/accounts/{accountID}/button?userId=&peer=&message=  body {"data": base64}
//   GET  /api/accounts/{accountID}/details?userId=&peer=&limit=&before=
//
// 输出字段与 Node telegramService 的 serializeMessage / serializeEntity 对齐（铁律 3：
// 行为稳定优先，UI 契约不变）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"
)

// nativeRandomIDSeq 用于生成 messages.sendMessage 的 random_id，
// 初值取启动时刻纳秒，避免与上一次进程产生重复。
var nativeRandomIDSeq = time.Now().UnixNano()

func nextNativeRandomID() int64 {
	return atomic.AddInt64(&nativeRandomIDSeq, 1)
}

// --- 发送消息 --------------------------------------------------------------

// fetchNativeSendMessage 发送文本消息，并把服务端回执还原为与 Node sendText
// 相同形状的消息对象。
func (a *App) fetchNativeSendMessage(ctx context.Context, api *tg.Client, account NativeAccount, peerID string, text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("消息内容不能为空")
	}
	// Telegram 单条消息上限 4096 个字符（按 Unicode 码点计）。
	if len([]rune(text)) > 4096 {
		return nil, errors.New("消息内容过长（最多 4096 个字符）")
	}
	info, err := a.resolveNativePeer(ctx, api, account, peerID)
	if err != nil {
		return nil, err
	}
	peer, err := nativeInputPeerFromInfo(info)
	if err != nil {
		return nil, err
	}
	updates, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: nextNativeRandomID(),
	})
	if err != nil {
		return nil, fmt.Errorf("发送消息失败：%w", err)
	}
	messageID := nativeSentMessageID(updates)
	if messageID <= 0 {
		return nil, errors.New("消息已发送，但 Telegram 未返回可解析的回执")
	}
	index := loadNativePeerIndex(account.UserID, account.AccountID)
	message, err := a.fetchNativeMessageByID(ctx, api, info, messageID)
	if err != nil {
		// 发送已成功，仅回执读取失败；降级为最小消息对象，避免前端误判为发送失败。
		return serializeNativeMessage(nativeSyntheticMessage(info, messageID, text), index), nil
	}
	return serializeNativeMessage(message, index), nil
}

// nativeSyntheticMessage 在拿不到服务端回执时构造一个仅含必要字段的消息对象，
// 字段形状与 serializeNativeMessage 一致。
func nativeSyntheticMessage(info nativePeerInfo, messageID int, text string) *tg.Message {
	message := &tg.Message{
		ID:      messageID,
		Date:    int(time.Now().Unix()),
		Message: text,
		Out:     true,
	}
	message.PeerID = nativePeerClassFromInfo(info)
	message.FromID = message.PeerID
	return message
}

func nativePeerClassFromInfo(info nativePeerInfo) tg.PeerClass {
	id := int64FromString(info.ID)
	if id == 0 {
		return nil
	}
	switch info.Type {
	case "user":
		return &tg.PeerUser{UserID: id}
	case "chat":
		return &tg.PeerChat{ChatID: id}
	case "channel":
		return &tg.PeerChannel{ChannelID: id}
	}
	return nil
}

// nativeSentMessageID 从 messages.sendMessage 的 updates 回执中解析出新消息 ID。
// 优先取 updateNewMessage（携带完整消息），其次 updateMessageID。
func nativeSentMessageID(updates tg.UpdatesClass) int {
	switch typed := updates.(type) {
	case *tg.UpdateShortSentMessage:
		if typed == nil {
			return 0
		}
		return typed.ID
	case *tg.UpdateShortMessage:
		if typed == nil {
			return 0
		}
		return typed.ID
	case *tg.UpdateShortChatMessage:
		if typed == nil {
			return 0
		}
		return typed.ID
	case *tg.Updates:
		if typed == nil {
			return 0
		}
		return nativeMessageIDFromUpdateList(typed.Updates)
	case *tg.UpdatesCombined:
		if typed == nil {
			return 0
		}
		return nativeMessageIDFromUpdateList(typed.Updates)
	}
	return 0
}

func nativeMessageIDFromUpdateList(items []tg.UpdateClass) int {
	fallback := 0
	for _, entry := range items {
		switch typed := entry.(type) {
		case *tg.UpdateNewMessage:
			if typed == nil {
				continue
			}
			if message, ok := typed.Message.(*tg.Message); ok && message != nil {
				return message.ID
			}
		case *tg.UpdateNewChannelMessage:
			if typed == nil {
				continue
			}
			if message, ok := typed.Message.(*tg.Message); ok && message != nil {
				return message.ID
			}
		case *tg.UpdateMessageID:
			if typed == nil {
				continue
			}
			if fallback == 0 {
				fallback = typed.ID
			}
		}
	}
	return fallback
}

// --- 机器人按钮 ------------------------------------------------------------

// fetchNativeButtonAnswer 调用 messages.getBotCallbackAnswer 触发内联按钮回调，
// 返回与 Node clickMessageButton 相同的 { message, url, alert }。
func (a *App) fetchNativeButtonAnswer(ctx context.Context, api *tg.Client, account NativeAccount, peerID string, messageID int, dataBase64 string) (map[string]any, error) {
	if messageID <= 0 {
		return nil, errors.New("缺少消息 ID")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataBase64))
	if err != nil {
		return nil, errors.New("按钮数据不是合法内容")
	}
	if len(raw) == 0 {
		return nil, errors.New("这个按钮暂不支持点击")
	}
	info, err := a.resolveNativePeer(ctx, api, account, peerID)
	if err != nil {
		return nil, err
	}
	peer, err := nativeInputPeerFromInfo(info)
	if err != nil {
		return nil, err
	}
	answer, err := api.MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{
		Peer:  peer,
		MsgID: messageID,
		Data:  raw,
	})
	if err != nil {
		return nil, fmt.Errorf("机器人响应失败：%w", err)
	}
	return map[string]any{
		"message": answer.Message,
		"url":     answer.URL,
		"alert":   answer.Alert,
	}, nil
}

// --- 会话详情 --------------------------------------------------------------

// fetchNativeChatDetails 返回会话详情（简介、成员数、媒体摘要与媒体列表），
// 字段与 Node chatDetails 的返回保持一致。
func (a *App) fetchNativeChatDetails(ctx context.Context, api *tg.Client, account NativeAccount, peerID string, limit int, before int) (map[string]any, error) {
	info, err := a.resolveNativePeer(ctx, api, account, peerID)
	if err != nil {
		return nil, err
	}
	about, participantsCount := a.fetchNativeChatFull(ctx, api, info)
	files, err := a.fetchNativeMedia(ctx, api, account, peerID, limit, before)
	if err != nil {
		// 详情主体可用即可，媒体列表失败不应让整页 500（与 Node 侧 try/catch 行为一致）。
		files = []map[string]any{}
	}
	summary := map[string]any{"images": 0, "videos": 0, "files": 0}
	for _, item := range files {
		switch chatString(item["kind"]) {
		case "image":
			summary["images"] = chatInt(summary["images"]) + 1
		case "video":
			summary["videos"] = chatInt(summary["videos"]) + 1
		default:
			summary["files"] = chatInt(summary["files"]) + 1
		}
	}
	// R4.57：媒体统计不再只数扫描窗口——旧实现 details 默认只扫 30 条消息，
	// 千人大群实际几百个视频却显示「30」。改用 messages.search 官方过滤器
	// （photos/video/document）拿服务端权威 Count；任一查询失败回退窗口计数。
	if peer, peerErr := nativeInputPeerFromInfo(info); peerErr == nil {
		if totals := a.fetchNativeMediaTotals(ctx, api, peer); totals != nil {
			summary = totals
		}
	}
	nextBefore := 0
	for _, item := range files {
		if id := chatInt(item["id"]); id > 0 && (nextBefore == 0 || id < nextBefore) {
			nextBefore = id
		}
	}
	return map[string]any{
		"id":                info.PeerID,
		"rawId":             info.ID,
		"title":             chatTitle(info, info.PeerID),
		"username":          info.Username,
		"type":              info.Kind,
		"about":             about,
		"participantsCount": participantsCount,
		"mediaSummary":      summary,
		"files":             files,
		"nextMediaBefore":   nextBefore,
		"hasMoreMedia":      len(files) >= limit,
	}, nil
}

// fetchNativeMediaTotals 用 messages.search 按官方过滤器（photos/video/document）
// 拿各类型媒体的真实总数——响应里的 Count 是服务端权威值，与官方客户端
// 「文件与媒体」页签同源。任一查询失败返回 nil，调用方回退窗口计数。
// 注意 document 过滤器涵盖视频/音乐等一切文件，故 files = document - video
// 与旧「窗口内非图非视频」语义对齐。
func (a *App) fetchNativeMediaTotals(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) map[string]any {
	countOf := func(filter tg.MessagesFilterClass) (int, error) {
		resp, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer:   peer,
			Q:      "",
			Filter: filter,
			Limit:  1,
		})
		if err != nil {
			return 0, err
		}
		switch typed := resp.(type) {
		case *tg.MessagesChannelMessages:
			return typed.Count, nil
		case *tg.MessagesMessagesSlice:
			return typed.Count, nil
		}
		return 0, nil
	}
	photos, err := countOf(&tg.InputMessagesFilterPhotos{})
	if err != nil {
		return nil
	}
	videos, err := countOf(&tg.InputMessagesFilterVideo{})
	if err != nil {
		return nil
	}
	documents, err := countOf(&tg.InputMessagesFilterDocument{})
	if err != nil {
		return nil
	}
	files := documents - videos
	if files < 0 {
		files = 0
	}
	return map[string]any{
		"images": photos,
		"videos": videos,
		"files":  files,
		"total":  true,
	}
}

// fetchNativeChatFull 读取会话简介与成员数；失败时返回零值而不是让整页报错，
// 与 Node chatDetails 里 `try { ... } catch {}` 的降级行为保持一致。
func (a *App) fetchNativeChatFull(ctx context.Context, api *tg.Client, info nativePeerInfo) (string, int) {
	id := int64FromString(info.ID)
	if id == 0 {
		return "", 0
	}
	switch info.Type {
	case "channel":
		accessHash, err := strconv.ParseInt(info.AccessHash, 10, 64)
		if err != nil {
			return "", 0
		}
		result, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: id, AccessHash: accessHash})
		if err != nil || result == nil {
			return "", 0
		}
		if full, ok := result.FullChat.(*tg.ChannelFull); ok && full != nil {
			return full.About, full.ParticipantsCount
		}
	case "chat":
		result, err := api.MessagesGetFullChat(ctx, id)
		if err != nil || result == nil {
			return "", 0
		}
		if full, ok := result.FullChat.(*tg.ChatFull); ok && full != nil {
			count := 0
			if participants, ok := full.Participants.(*tg.ChatParticipants); ok && participants != nil {
				count = len(participants.Participants)
			}
			return full.About, count
		}
	case "user":
		accessHash, err := strconv.ParseInt(info.AccessHash, 10, 64)
		if err != nil {
			return "", 0
		}
		result, err := api.UsersGetFullUser(ctx, &tg.InputUser{UserID: id, AccessHash: accessHash})
		if err != nil || result == nil {
			return "", 0
		}
		return result.FullUser.About, 0
	}
	return "", 0
}

// --- HTTP 层 ---------------------------------------------------------------

// decodeChatWriteBody 解析写路径的 JSON body；空 body 视为空对象，
// 避免某些客户端不发送 Content-Length 时报错。
func decodeChatWriteBody(r *http.Request) (map[string]any, error) {
	payload := map[string]any{}
	if r.Body == nil {
		return payload, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		return nil, errors.New("请求体读取失败")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("请求体不是合法 JSON")
	}
	return payload, nil
}

// handleAccountWrite 处理 /api/accounts/{accountID}/send|button|details，
// 由 handleAccountAPI 在完成账号解析后转交；runner 提供已就绪的连接
// （R4.54 起为账号常驻池连接）。
func (a *App) handleAccountWrite(w http.ResponseWriter, r *http.Request, action string, account NativeAccount, runner chatRunner, query url.Values) {
	ctx, cancel := context.WithTimeout(r.Context(), chatQueryTimeout)
	defer cancel()

	switch action {
	case "send":
		body, err := decodeChatWriteBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		out, err := runner(ctx, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeSendMessage(ctx, api, account, peer, chatWriteString(body["text"]))
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, out)
	case "button":
		body, err := decodeChatWriteBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		messageID, _ := strconv.Atoi(query.Get("message"))
		out, err := runner(ctx, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeButtonAnswer(ctx, api, account, peer, messageID, chatWriteString(body["data"]))
		})
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "暂不支持点击") || strings.Contains(err.Error(), "缺少消息") ||
				strings.Contains(err.Error(), "不是合法内容") {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, out)
	case "details":
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		limit := clampChatLimit(query.Get("limit"), 30, maxChatMessageLimit/4)
		before := 0
		if value, err := strconv.Atoi(query.Get("before")); err == nil {
			before = value
		}
		out, err := runner(ctx, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeChatDetails(ctx, api, account, peer, limit, before)
		})
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "找不到会话") {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func chatWriteString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}
