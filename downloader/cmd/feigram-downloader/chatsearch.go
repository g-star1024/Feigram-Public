package main

// M4.4 · Go Telegram Core 链接解析与全局搜索
//
// M4.1 阻塞链的剩余缺口：resolveTelegramLink、search 两个函数在 native 账号下
// 只能走 GramJS getClient（对 authMode=native 抛 409）。本文件把这两条路径平移到
// Go 原生 MTProto，使卸载 telegram npm 后这两条链路不回归。
//
// 路由（挂在 /api/accounts/ 子树，均为 GET）：
//   GET /api/accounts/{accountID}/resolve?userId=&peer=<@user|t.me 链接>
//   GET /api/accounts/{accountID}/search?userId=&query=&limit=
//
// 输出字段与 Node telegramService 的 resolveTelegramLink / search 保持一致（铁律 3）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gotd/td/tg"
)

// nativeUsernameFromLink 从 @username / tg://resolve?domain=x / t.me/x 形式的输入里
// 提取可解析的用户名；无法识别时返回空串（调用方据此降级）。
func nativeUsernameFromLink(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "@") {
		return strings.TrimPrefix(value, "@")
	}
	if strings.HasPrefix(value, "tg://") {
		if parsed, err := url.Parse(value); err == nil {
			return parsed.Query().Get("domain")
		}
		return ""
	}
	normalized := value
	if strings.HasPrefix(value, "t.me/") {
		normalized = "https://" + value
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return ""
	}
	switch parsed.Hostname() {
	case "t.me", "telegram.me", "www.t.me", "www.telegram.me":
	default:
		// 不是链接，可能是调用方直接传的用户名。
		if !strings.Contains(value, "/") && !strings.Contains(value, ":") {
			return value
		}
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return ""
	}
	domain := segments[0]
	// /c/<id> 私有频道链接与 joinchat 邀请链接没有可解析的用户名。
	if domain == "" || domain == "c" || domain == "joinchat" || strings.HasPrefix(domain, "+") {
		return ""
	}
	return domain
}

// fetchNativeResolveUsername 解析 t.me 链接指向的会话，返回与 Node resolveTelegramLink
// 相同形状的 { id, rawId, title, username, type, avatarKey, messageId }。
func (a *App) fetchNativeResolveUsername(ctx context.Context, api *tg.Client, account NativeAccount, link string, messageID int) (map[string]any, error) {
	username := nativeUsernameFromLink(link)
	if username == "" {
		return nil, errors.New("这个链接暂不支持在客户端内打开，或你当前账号无权访问")
	}
	result, err := api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("解析链接失败：%w", err)
	}
	if result == nil {
		return nil, errors.New("解析链接失败")
	}
	index := indexNativePeers(result.Users, result.Chats)
	// 解析结果里的实体也要入索引，后续发消息/下载才能拿到 accessHash。
	storeNativePeerIndex(account.UserID, account.AccountID, index)
	peerID := peerIDFromPeerClass(result.Peer)
	if peerID == "" {
		return nil, errors.New("这个链接暂不支持在客户端内打开，或你当前账号无权访问")
	}
	info, ok := index[peerID]
	if !ok {
		// 兜底：即使没有实体详情，也返回可跳转的 peerId，避免前端直接报错。
		info = nativePeerInfo{PeerID: peerID}
	}
	return map[string]any{
		"id":        peerID,
		"rawId":     info.ID,
		"title":     chatTitle(info, peerID),
		"username":  info.Username,
		"type":      coalesce(info.Kind, "chat"),
		"avatarKey": peerID,
		"messageId": messageID,
	}, nil
}

// fetchNativeSearchMessages 执行全局消息搜索（InputPeerEmpty 表示跨会话搜索），
// 返回与 Node search 的 messages 字段一致的序列化消息列表。
func (a *App) fetchNativeSearchMessages(ctx context.Context, api *tg.Client, account NativeAccount, query string, limit int) ([]map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []map[string]any{}, nil
	}
	if limit <= 0 || limit > maxChatMessageLimit {
		limit = 30
	}
	result, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer:  &tg.InputPeerEmpty{},
		Q:     query,
		Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("搜索失败：%w", err)
	}
	messages, users, chats, ok := flattenNativeMessages(result)
	if !ok {
		return []map[string]any{}, nil
	}
	index := indexNativePeers(users, chats)
	storeNativePeerIndex(account.UserID, account.AccountID, index)
	items := []map[string]any{}
	for _, entry := range messages {
		if message, ok := entry.(*tg.Message); ok && message != nil {
			items = append(items, serializeNativeMessage(message, index))
		}
	}
	return items, nil
}

// handleAccountSearch 处理 /api/accounts/{accountID}/resolve|search，
// 由 handleAccountAPI 完成账号解析与客户端构建后转交。
func (a *App) handleAccountSearch(w http.ResponseWriter, r *http.Request, action string, account NativeAccount, runner chatRunner, query url.Values) {
	ctx, cancel := context.WithTimeout(r.Context(), chatQueryTimeout)
	defer cancel()

	switch action {
	case "resolve":
		link := query.Get("peer")
		if link == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少链接参数"})
			return
		}
		messageID := intFromQuery(query.Get("message"))
		out, err := runner(ctx, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeResolveUsername(ctx, api, account, link, messageID)
		})
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "暂不支持在客户端内打开") {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, out)
	case "search":
		searchQuery := query.Get("query")
		limit := clampChatLimit(query.Get("limit"), 30, maxChatMessageLimit)
		out, err := runner(ctx, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeSearchMessages(ctx, api, account, searchQuery, limit)
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
