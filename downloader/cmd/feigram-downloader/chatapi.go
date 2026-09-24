package main

// M4.2 · Go Telegram Core 聊天/文件夹/消息/头像 API
//
// 把 Node 侧 GramJS 的会话（dialogs）、文件夹（dialog filters）、消息历史（history）、
// 头像（profile photo）与 peer 解析能力平移到 Go 原生 MTProto，使已迁移到 Go 的账号
// （authMode=native）在无 GramJS 客户端的前提下仍可浏览聊天与读取媒体元数据。
//
// 路由（挂载在既有 3090 mux 的 /api/accounts/ 子树下，GET）：
//   GET /api/accounts/{accountID}/dialogs?userId=&limit=&query=   -> 会话列表
//   GET /api/accounts/{accountID}/folders?userId=                 -> 文件夹（含归属 chatIds）
//   GET /api/accounts/{accountID}/messages?userId=&peer=&limit=&before=&around=
//   GET /api/accounts/{accountID}/avatar?userId=&peer=            -> image/jpeg 二进制
//   GET /api/accounts/{accountID}/peer?userId=&peer=              -> peer 元数据（含 accessHash）
//
// 输出 JSON 字段与 Node telegramService 的 serializeEntity/serializeMessage/
// serializeDialogFilterShallow 保持一致，便于 Node 网关直接透传、UI 行为不变（铁律 3）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const (
	defaultChatDialogLimit = 200
	// maxChatDialogLimit 是「会话列表总量」上限（客户端可见条数），不是单次 RPC 的
	// 可拉取量——MTProto 对 messages.getDialogs 的单页 limit 硬上限是 100，
	// 传 500 服务端也只返回 100 条（R4.36 实证：此前把两者混为一谈，导致
	// 每次都只拿到前 100 个会话，第 100 条之后的群组在列表里永不出现，
	// 深翻分页也因「返回 100 < 500」被误判为已到列表末尾而从未真正翻页）。
	maxChatDialogLimit = 500
	// dialogPageSize 是单次 messages.getDialogs 的请求页大小，必须 ≤ Telegram
	// 的硬上限 100，否则无法区分「本页已到末尾」与「服务端截断」。
	dialogPageSize = 100
	// nativeDialogPageRounds 是单个文件夹最多翻多少页（100×30 = 3000 会话）。
	nativeDialogPageRounds  = 30
	defaultChatMessageLimit = 50
	maxChatMessageLimit     = 200
	avatarChunkSize         = 256 * 1024
	maxAvatarBytes          = 8 * 1024 * 1024
	chatQueryTimeout        = 60 * time.Second
)

// nativePeerInfo 描述一个 Telegram peer 的最小可用元数据。
// AccessHash 是解析 InputPeer/InputChannel 的必备材料（M3.2 依赖此项）。
type nativePeerInfo struct {
	PeerID     string `json:"peerId"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	AccessHash string `json:"accessHash"`
	Title      string `json:"title"`
	Username   string `json:"username"`
	Kind       string `json:"kind"`
	PhotoID    int64  `json:"photoId"`
	HasPhoto   bool   `json:"hasPhoto"`
	Bot        bool   `json:"bot"`
	Contact    bool   `json:"contact"`
}

var (
	nativePeerIndexMu sync.Mutex
	nativePeerIndex   = map[string]map[string]nativePeerInfo{}
	// R4.30：peer 索引落盘路径。空表示未初始化（单元测试），不读写磁盘。
	nativePeerIndexFile string
)

// initNativePeerIndexStore 绑定落盘路径并加载历史索引。
// R4.30：peer 索引此前是纯内存结构，服务重启即清空——下载任务刷新
// file_reference 时找不到频道 accessHash（会话深翻也未必救得回来），
// 表现为「找不到会话 Channel:xxx」终态失败。现在索引随更新写盘、启动加载。
func initNativePeerIndexStore(dataDir string) {
	nativePeerIndexMu.Lock()
	defer nativePeerIndexMu.Unlock()
	nativePeerIndexFile = filepath.Join(dataDir, "native-peer-index.json")
	raw, err := os.ReadFile(nativePeerIndexFile)
	if err != nil {
		return // 首次启动无文件属正常
	}
	var stored map[string]map[string]nativePeerInfo
	if json.Unmarshal(raw, &stored) != nil {
		log.Printf("native peer index 文件损坏，忽略重建：%s", nativePeerIndexFile)
		return
	}
	for key, entries := range stored {
		current := nativePeerIndex[key]
		if current == nil {
			current = map[string]nativePeerInfo{}
			nativePeerIndex[key] = current
		}
		for id, info := range entries {
			current[id] = info
		}
	}
	total := 0
	for _, entries := range nativePeerIndex {
		total += len(entries)
	}
	log.Printf("native peer index 已从磁盘恢复（%d 个账号，%d 个 peer）", len(stored), total)
}

// persistNativePeerIndexLocked 把内存索引快照写盘（调用方须持 nativePeerIndexMu）。
// 原子写：临时文件 + rename，避免写一半崩溃留下损坏 JSON。
func persistNativePeerIndexLocked() {
	if nativePeerIndexFile == "" || len(nativePeerIndex) == 0 {
		return
	}
	snapshot := make(map[string]map[string]nativePeerInfo, len(nativePeerIndex))
	for key, entries := range nativePeerIndex {
		inner := make(map[string]nativePeerInfo, len(entries))
		for id, info := range entries {
			inner[id] = info
		}
		snapshot[key] = inner
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	tmp := nativePeerIndexFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("native peer index 写盘失败：%v", err)
		return
	}
	if err := os.Rename(tmp, nativePeerIndexFile); err != nil {
		log.Printf("native peer index 落盘失败：%v", err)
	}
}

func nativePeerIndexKey(userID, accountID string) string {
	return nativeAccountKey(userID, accountID)
}

func storeNativePeerIndex(userID, accountID string, index map[string]nativePeerInfo) {
	if len(index) == 0 {
		return
	}
	nativePeerIndexMu.Lock()
	defer nativePeerIndexMu.Unlock()
	key := nativePeerIndexKey(userID, accountID)
	current := nativePeerIndex[key]
	if current == nil {
		current = map[string]nativePeerInfo{}
		nativePeerIndex[key] = current
	}
	for id, info := range index {
		if existing, ok := current[id]; ok && info.AccessHash == "" && existing.AccessHash != "" {
			continue
		}
		current[id] = info
	}
	persistNativePeerIndexLocked()
}

func loadNativePeerIndex(userID, accountID string) map[string]nativePeerInfo {
	nativePeerIndexMu.Lock()
	defer nativePeerIndexMu.Unlock()
	current := nativePeerIndex[nativePeerIndexKey(userID, accountID)]
	out := make(map[string]nativePeerInfo, len(current))
	for id, info := range current {
		out[id] = info
	}
	return out
}

// handleAccountAPI 是 /api/accounts/ 子树的统一入口。
// 精确路径 /api/accounts/migrate 由 mux 优先匹配，不会进入这里。
// M4.3：写路径（send/button）接受 POST，详情（details）沿用 GET，其余只读动作保持 GET。
// 这样 Node 网关无需为写操作单独开一套端口或鉴权。
func (a *App) handleAccountAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	segments := strings.Split(rest, "/")
	accountID, err := url.PathUnescape(segments[0])
	if err != nil || accountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少账号 ID"})
		return
	}
	action := ""
	if len(segments) > 1 {
		action = segments[1]
	}
	// M4.1：blob 走二进制直出（支持 Range），不进入 JSON 分支。
	switch action {
	case "dialogs", "folders", "messages", "media", "avatar", "peer", "blob", "details", "resolve", "search":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
			return
		}
	case "send", "button":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown account action: " + action})
		return
	}

	query := r.URL.Query()
	account, err := a.resolveChatAccount(query.Get("userId"), accountID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	apiHash, err := a.nativeAPIHash(account)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	client, err := a.newTelegramClient(account, apiHash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// M4.3：写路径（send/button）与会话详情（details）转交 chatwrite.go，
	// 它自带超时控制与 JSON body 解析，避免在此处重复分支。
	// M4.4：链接解析（resolve）与全局搜索（search）转交 chatsearch.go。
	if action == "send" || action == "button" || action == "details" {
		a.handleAccountWrite(w, r, action, account, client, r.URL.Query())
		return
	}
	if action == "resolve" || action == "search" {
		a.handleAccountSearch(w, r, action, account, client, r.URL.Query())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), chatQueryTimeout)
	defer cancel()

	switch action {
	case "dialogs":
		limit := clampChatLimit(query.Get("limit"), defaultChatDialogLimit, maxChatDialogLimit)
		// R4.0a：默认不显示归档时，没必要每次都额外拉 folder 1（双倍 RPC 开销，
		// 结果还会被前端过滤掉）。只有调用方显式要归档才拉。
		includeArchived := query.Get("includeArchived") == "1" || query.Get("includeArchived") == "true"
		items, err := a.runNativeChatQuery(ctx, client, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeDialogs(ctx, api, account, limit, query.Get("query"), includeArchived)
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "folders":
		items, err := a.runNativeChatQuery(ctx, client, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeFolders(ctx, api, account)
		})
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "messages":
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		limit := clampChatLimit(query.Get("limit"), defaultChatMessageLimit, maxChatMessageLimit)
		before := intFromQuery(query.Get("before"))
		around := intFromQuery(query.Get("around"))
		items, err := a.runNativeChatQuery(ctx, client, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeMessages(ctx, api, account, peer, limit, before, around)
		})
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "找不到会话") {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "media":
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		limit := clampChatLimit(query.Get("limit"), 30, maxChatMessageLimit/4)
		before := intFromQuery(query.Get("before"))
		items, err := a.runNativeChatQuery(ctx, client, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.fetchNativeMedia(ctx, api, account, peer, limit, before)
		})
		if err != nil {
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "找不到会话") {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "peer":
		peer := query.Get("peer")
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer 参数"})
			return
		}
		info, err := a.runNativeChatQuery(ctx, client, func(ctx context.Context, api *tg.Client) (any, error) {
			return a.resolveNativePeer(ctx, api, account, peer)
		})
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": compactError(err)})
			return
		}
		writeJSON(w, http.StatusOK, info)
	case "avatar":
		data, err := a.fetchNativeAvatar(ctx, client, account, query.Get("peer"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": compactError(err)})
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	case "blob":
		// 媒体字节流耗时远长于一次聊天查询，换成独立超时后再交给专用处理器。
		cancel()
		a.serveNativeMediaBlob(w, r, client, account, query)
	}
}

// runNativeChatQuery 在一次 gotd 会话内执行查询，避免重复握手。
func (a *App) runNativeChatQuery(ctx context.Context, client *telegram.Client, fn func(context.Context, *tg.Client) (any, error)) (any, error) {
	var (
		out any
		err error
	)
	runErr := client.Run(ctx, func(ctx context.Context) error {
		out, err = fn(ctx, client.API())
		return err
	})
	if runErr != nil {
		if err == nil {
			err = runErr
		}
		return nil, classifyNativeReadError(runErr)
	}
	return out, nil
}

// resolveChatAccount 按 userId+accountId 定位 Go 原生账号；userId 缺省时回退到唯一匹配账号。
func (a *App) resolveChatAccount(userID, accountID string) (NativeAccount, error) {
	a.mu.Lock()
	account, err := a.resolveChatAccountLocked(userID, accountID)
	a.mu.Unlock()
	if err != nil {
		// 业务请求撞到「有会话但未就绪」的账号时顺手补一次健康检查（带冷却去重），
		// 避免代理恢复后账号还要等最长 30 分钟的定时巡检才能自愈。
		a.maybeRecheckNotReadyAccount(userID, accountID)
	}
	return account, err
}

// notReadyAccountError 构造「账号未就绪」错误。failed 状态时必须附带健康检查
// 落库的真实原因（account.Error），否则用户只看到状态词，无从判断是代理不通、
// DC 超时还是 session 失效——R4.16：这是 2.5.0 实测「会话列表不恢复却看不到为什么」的根因。
func notReadyAccountError(accountID string, account *NativeAccount) error {
	message := fmt.Sprintf("Go 原生账号 %s 尚未就绪（%s），请先完成登录或健康检查", accountID, coalesce(account.Status, "unknown"))
	if strings.TrimSpace(account.Status) == "failed" {
		if reason := strings.TrimSpace(account.Error); reason != "" {
			message += "：" + reason
		}
	}
	return errors.New(message)
}

func (a *App) resolveChatAccountLocked(userID, accountID string) (NativeAccount, error) {
	if userID != "" {
		account := a.native[nativeAccountKey(userID, accountID)]
		if account != nil {
			if account.Session == "" || !account.Ready {
				return NativeAccount{}, notReadyAccountError(accountID, account)
			}
			return *account, nil
		}
	}
	var fallback *NativeAccount
	for _, account := range a.native {
		if account.AccountID != accountID {
			continue
		}
		if fallback == nil || (account.Ready && !fallback.Ready) {
			fallback = account
		}
	}
	if fallback == nil {
		return NativeAccount{}, fmt.Errorf("Go 原生账号 %s 未注册，请先登录或迁移", accountID)
	}
	if fallback.Session == "" || !fallback.Ready {
		return NativeAccount{}, notReadyAccountError(accountID, fallback)
	}
	return *fallback, nil
}

// --- 会话列表 -------------------------------------------------------------

func (a *App) fetchNativeDialogs(ctx context.Context, api *tg.Client, account NativeAccount, limit int, query string, includeArchived bool) ([]map[string]any, error) {
	items := []map[string]any{}
	index := map[string]nativePeerInfo{}
	seen := map[string]bool{}
	normalized := strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 {
		limit = defaultChatDialogLimit
	}
	if limit > maxChatDialogLimit {
		limit = maxChatDialogLimit
	}
	// R4.36：带查询词时要扫描更多会话才可能命中（返回条数仍以 limit 封顶）。
	scanLimit := limit
	if normalized != "" {
		scanLimit = maxChatDialogLimit
	}

	for _, folderID := range dialogFolderIDs(includeArchived) {
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		offsetID := 0
		scanned := 0
		// truncated 记录「本文件夹是否因达到上限而提前收工」——用于在触及
		// 总量上限时给出可操作提示（用户看不到某个群组时能立刻判断是不是
		// 列表被截断，而不是继续怀疑账号/同步）。
		truncated := true
		// R4.36 根因修复：此前只发一次 messages.getDialogs（Limit=limit）就收工，
		// 而 MTProto 单页硬上限是 100——服务端只回 100 条，第 100 条之后的会话
		// （大量群组）在列表里永不出现，深翻分页也因「返回 < 请求量」误判到底。
		// 现在按 offset_peer + offset_id 真分页累加，直到达到上限或列表末尾。
		for page := 0; page < nativeDialogPageRounds && scanned < scanLimit && len(items) < limit; page++ {
			pageSize := dialogPageSize
			if remain := scanLimit - scanned; remain < pageSize {
				pageSize = remain
			}
			request := &tg.MessagesGetDialogsRequest{
				OffsetPeer: offsetPeer,
				OffsetID:   offsetID,
				Limit:      pageSize,
			}
			if folderID > 0 {
				request.SetFolderID(folderID)
			}
			result, err := api.MessagesGetDialogs(ctx, request)
			if err != nil {
				if folderID == 0 {
					return nil, fmt.Errorf("获取会话列表失败：%w", err)
				}
				log.Printf("chatapi: archived dialogs fetch failed for %s/%s: %v", account.UserID, account.AccountID, err)
				break
			}
			dialogs, messages, chats, users, ok := flattenNativeDialogs(result)
			if !ok {
				break
			}
			for id, info := range indexNativePeers(users, chats) {
				index[id] = info
			}
			lastMessages := map[int]*tg.Message{}
			for _, item := range messages {
				if message, ok := item.(*tg.Message); ok {
					lastMessages[message.ID] = message
				}
			}
			// 分页游标取「本页最后一条 dialog」（即使它已因去重被跳过）；
			// 拿不到它的 input peer（实体列表缺 accessHash）就无法继续翻页，
			// 到此为止，避免盲目空翻。
			pageDialogs := 0
			var nextPeer tg.InputPeerClass
			nextOffsetID := 0
			for _, entry := range dialogs {
				dialog, ok := entry.(*tg.Dialog)
				if !ok || dialog == nil {
					continue
				}
				pageDialogs++
				if info, found := index[peerIDFromPeerClass(dialog.Peer)]; found {
					if peer, peerErr := nativeInputPeerFromInfo(info); peerErr == nil {
						nextPeer = peer
						nextOffsetID = dialog.TopMessage
					}
				}
				peerID := peerIDFromPeerClass(dialog.Peer)
				if peerID == "" || seen[peerID] {
					continue
				}
				folder, _ := dialog.GetFolderID()
				info := index[peerID]
				item := map[string]any{
					"id":          peerID,
					"rawId":       info.ID,
					"accessHash":  info.AccessHash,
					"title":       chatTitle(info, peerID),
					"username":    info.Username,
					"type":        info.Kind,
					"avatarKey":   peerID,
					"folderId":    folder,
					"folderIds":   folderIDs(folder),
					"unreadCount": dialog.UnreadCount,
					"pinned":      dialog.Pinned,
					"archived":    folder == 1,
					"muted":       nativeDialogMuted(dialog),
					"bot":         info.Bot,
					"contact":     info.Contact,
					"lastMessage": nil,
				}
				if message := lastMessages[dialog.TopMessage]; message != nil {
					item["lastMessage"] = serializeNativeMessage(message, index)
				}
				if normalized == "" || chatMatchesQuery(item, normalized) {
					items = append(items, item)
				}
				seen[peerID] = true
			}
			scanned += pageDialogs
			// 本页不满一页 = 已到列表末尾；无下一页游标同样终止。
			if pageDialogs < pageSize || nextPeer == nil {
				truncated = false
				break
			}
			offsetPeer = nextPeer
			offsetID = nextOffsetID
		}
		if truncated && len(items) >= limit {
			log.Printf("chatapi: 会话列表已达上限 %d 条（folder %d 仍有更多会话）——若有群组未显示，即为列表截断而非同步失败", limit, folderID)
		}
	}
	storeNativePeerIndex(account.UserID, account.AccountID, index)
	return items, nil
}

// dialogFolderIDs 决定本次会话列表要拉取的归档文件夹。
// R4.0a：默认（不显示归档）只拉 folder 0，省掉一次必然被前端过滤的 folder 1 拉取；
// 需要归档时才同时拉 0 和 1。
func dialogFolderIDs(includeArchived bool) []int {
	if includeArchived {
		return []int{0, 1}
	}
	return []int{0}
}

func flattenNativeDialogs(result tg.MessagesDialogsClass) ([]tg.DialogClass, []tg.MessageClass, []tg.ChatClass, []tg.UserClass, bool) {
	switch typed := result.(type) {
	case *tg.MessagesDialogs:
		return typed.Dialogs, typed.Messages, typed.Chats, typed.Users, true
	case *tg.MessagesDialogsSlice:
		return typed.Dialogs, typed.Messages, typed.Chats, typed.Users, true
	default:
		return nil, nil, nil, nil, false
	}
}

func indexNativePeers(users []tg.UserClass, chats []tg.ChatClass) map[string]nativePeerInfo {
	index := map[string]nativePeerInfo{}
	for _, item := range users {
		user, ok := item.(*tg.User)
		if !ok || user == nil {
			continue
		}
		title := strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName))
		photoID, hasPhoto := userProfilePhotoID(user.Photo)
		index["User:"+strconv.FormatInt(user.ID, 10)] = nativePeerInfo{
			PeerID:     "User:" + strconv.FormatInt(user.ID, 10),
			Type:       "user",
			ID:         strconv.FormatInt(user.ID, 10),
			AccessHash: strconv.FormatInt(user.AccessHash, 10),
			Title:      title,
			Username:   user.Username,
			Kind:       "private",
			PhotoID:    photoID,
			HasPhoto:   hasPhoto,
			Bot:        user.Bot,
			Contact:    user.Contact,
		}
	}
	for _, item := range chats {
		switch entity := item.(type) {
		case *tg.Channel:
			if entity == nil {
				continue
			}
			kind := "group"
			if entity.Broadcast {
				kind = "channel"
			}
			photoID, hasPhoto := chatPhotoID(entity.Photo)
			index["Channel:"+strconv.FormatInt(entity.ID, 10)] = nativePeerInfo{
				PeerID:     "Channel:" + strconv.FormatInt(entity.ID, 10),
				Type:       "channel",
				ID:         strconv.FormatInt(entity.ID, 10),
				AccessHash: strconv.FormatInt(entity.AccessHash, 10),
				Title:      entity.Title,
				Username:   entity.Username,
				Kind:       kind,
				PhotoID:    photoID,
				HasPhoto:   hasPhoto,
			}
		case *tg.Chat:
			if entity == nil {
				continue
			}
			photoID, hasPhoto := chatPhotoID(entity.Photo)
			index["Chat:"+strconv.FormatInt(entity.ID, 10)] = nativePeerInfo{
				PeerID:   "Chat:" + strconv.FormatInt(entity.ID, 10),
				Type:     "chat",
				ID:       strconv.FormatInt(entity.ID, 10),
				Title:    entity.Title,
				Kind:     "group",
				PhotoID:  photoID,
				HasPhoto: hasPhoto,
			}
		}
	}
	return index
}

func userProfilePhotoID(photo tg.UserProfilePhotoClass) (int64, bool) {
	typed, ok := photo.(*tg.UserProfilePhoto)
	if !ok || typed == nil {
		return 0, false
	}
	return typed.PhotoID, true
}

func chatPhotoID(photo tg.ChatPhotoClass) (int64, bool) {
	switch typed := photo.(type) {
	case *tg.ChatPhoto:
		if typed == nil {
			return 0, false
		}
		return typed.PhotoID, true
	case *tg.ChatPhotoEmpty:
		return 0, false
	default:
		return 0, false
	}
}

func chatTitle(info nativePeerInfo, fallback string) string {
	if strings.TrimSpace(info.Title) != "" {
		return info.Title
	}
	if strings.TrimSpace(info.Username) != "" {
		return info.Username
	}
	return coalesce(fallback, "Unknown")
}

func folderIDs(folder int) []int {
	if folder == 0 {
		return []int{}
	}
	return []int{folder}
}

func chatMatchesQuery(item map[string]any, query string) bool {
	title := strings.ToLower(chatString(item["title"]))
	username := strings.ToLower(chatString(item["username"]))
	return strings.Contains(title, query) || strings.Contains(username, query)
}

func nativeDialogMuted(dialog *tg.Dialog) bool {
	if dialog == nil {
		return false
	}
	return dialog.NotifySettings.MuteUntil > int(time.Now().Unix())
}

// --- 文件夹 ---------------------------------------------------------------

func (a *App) fetchNativeFolders(ctx context.Context, api *tg.Client, account NativeAccount) ([]map[string]any, error) {
	chats, err := a.fetchNativeDialogs(ctx, api, account, maxChatDialogLimit, "", true)
	if err != nil {
		return nil, err
	}
	filtersResult, err := api.MessagesGetDialogFilters(ctx)
	if err != nil {
		// 文件夹不可用时退化为「按 folderId 合成」，与 Node 的兜底行为一致。
		log.Printf("chatapi: dialog filters unavailable for %s/%s: %v", account.UserID, account.AccountID, err)
		return syntheticNativeFolders(chats), nil
	}
	index := loadNativePeerIndex(account.UserID, account.AccountID)
	folders := []map[string]any{}
	for _, entry := range filtersResult.Filters {
		filter, ok := entry.(*tg.DialogFilter)
		if !ok || filter == nil || filter.ID == 0 || strings.TrimSpace(filter.Title) == "" {
			continue
		}
		item := map[string]any{
			"id":             filter.ID,
			"title":          filter.Title,
			"emoticon":       filter.Emoticon,
			"includePeerIds": nativePeerIDs(filter.IncludePeers, index),
			"pinnedPeerIds":  nativePeerIDs(filter.PinnedPeers, index),
			"excludePeerIds": nativePeerIDs(filter.ExcludePeers, index),
			"flags": map[string]bool{
				"contacts":        filter.Contacts,
				"nonContacts":     filter.NonContacts,
				"groups":          filter.Groups,
				"broadcasts":      filter.Broadcasts,
				"bots":            filter.Bots,
				"excludeMuted":    filter.ExcludeMuted,
				"excludeRead":     filter.ExcludeRead,
				"excludeArchived": filter.ExcludeArchived,
			},
		}
		chatIDs := []string{}
		for _, chat := range chats {
			if nativeFilterMatchesChat(item, chat) {
				chatIDs = append(chatIDs, chatString(chat["id"]))
			}
		}
		item["chatIds"] = chatIDs
		if len(chatIDs) > 0 || len(item["includePeerIds"].([]string)) > 0 || len(item["pinnedPeerIds"].([]string)) > 0 {
			folders = append(folders, item)
		}
	}
	if len(folders) > 0 {
		return folders, nil
	}
	return syntheticNativeFolders(chats), nil
}

func syntheticNativeFolders(chats []map[string]any) []map[string]any {
	byFolder := map[int][]string{}
	order := []int{}
	for _, chat := range chats {
		folder := chatInt(chat["folderId"])
		if folder == 0 {
			continue
		}
		if _, ok := byFolder[folder]; !ok {
			order = append(order, folder)
		}
		byFolder[folder] = append(byFolder[folder], chatString(chat["id"]))
	}
	folders := []map[string]any{}
	for _, id := range order {
		title := fmt.Sprintf("文件夹 %d", id)
		if id == 1 {
			title = "归档"
		}
		folders = append(folders, map[string]any{
			"id":             id,
			"title":          title,
			"emoticon":       "",
			"includePeerIds": []string{},
			"pinnedPeerIds":  []string{},
			"excludePeerIds": []string{},
			"flags":          map[string]bool{},
			"chatIds":        byFolder[id],
		})
	}
	return folders
}

func nativePeerIDs(peers []tg.InputPeerClass, index map[string]nativePeerInfo) []string {
	ids := []string{}
	for _, peer := range peers {
		id := inputPeerIDString(peer)
		if id == "" {
			continue
		}
		if info, ok := index[id]; ok && info.PeerID != "" {
			ids = append(ids, info.PeerID)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func nativeFilterMatchesChat(filter map[string]any, chat map[string]any) bool {
	include := map[string]bool{}
	for _, id := range filter["includePeerIds"].([]string) {
		include[id] = true
	}
	for _, id := range filter["pinnedPeerIds"].([]string) {
		include[id] = true
	}
	exclude := map[string]bool{}
	for _, id := range filter["excludePeerIds"].([]string) {
		exclude[id] = true
	}
	chatID := chatString(chat["id"])
	if exclude[chatID] {
		return false
	}
	if include[chatID] {
		return true
	}
	for _, id := range chat["folderIds"].([]int) {
		if id == chatInt(filter["id"]) {
			return true
		}
	}
	flags, _ := filter["flags"].(map[string]bool)
	if flags["excludeArchived"] && chatBool(chat["archived"]) {
		return false
	}
	if flags["excludeMuted"] && chatBool(chat["muted"]) {
		return false
	}
	if flags["excludeRead"] && chatInt(chat["unreadCount"]) == 0 {
		return false
	}
	kind := chatString(chat["type"])
	if kind == "group" && flags["groups"] {
		return true
	}
	if kind == "channel" && flags["broadcasts"] {
		return true
	}
	if kind == "private" && flags["bots"] && chatBool(chat["bot"]) {
		return true
	}
	if kind == "private" && flags["contacts"] && chatBool(chat["contact"]) {
		return true
	}
	if kind == "private" && flags["nonContacts"] && !chatBool(chat["contact"]) && !chatBool(chat["bot"]) {
		return true
	}
	return false
}

// --- 消息历史 -------------------------------------------------------------

func (a *App) fetchNativeMessages(ctx context.Context, api *tg.Client, account NativeAccount, peerID string, limit int, before int, around int) ([]map[string]any, error) {
	info, err := a.resolveNativePeer(ctx, api, account, peerID)
	if err != nil {
		return nil, err
	}
	peer, err := nativeInputPeerFromInfo(info)
	if err != nil {
		return nil, err
	}
	request := &tg.MessagesGetHistoryRequest{Peer: peer, Limit: limit}
	switch {
	case around > 0:
		request.OffsetID = around
		request.AddOffset = -limit / 2
	case before > 0:
		request.OffsetID = before
	}
	result, err := api.MessagesGetHistory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("获取消息失败：%w", err)
	}
	index := loadNativePeerIndex(account.UserID, account.AccountID)
	messages, users, chats, ok := flattenNativeMessages(result)
	if !ok {
		return nil, errors.New("Telegram 返回了不支持的消息结果")
	}
	for id, peerInfo := range indexNativePeers(users, chats) {
		index[id] = peerInfo
	}
	storeNativePeerIndex(account.UserID, account.AccountID, index)

	items := []map[string]any{}
	found := false
	for _, entry := range messages {
		message, ok := entry.(*tg.Message)
		if !ok || message == nil {
			continue
		}
		if around > 0 && message.ID == around {
			found = true
		}
		items = append(items, serializeNativeMessage(message, index))
	}
	if around > 0 && !found {
		target, targetErr := a.fetchNativeMessageByID(ctx, api, info, around)
		if targetErr == nil && target != nil {
			items = append(items, serializeNativeMessage(target, index))
		}
	}
	sortMessagesByID(items)
	return items, nil
}

// fetchNativeMedia 返回会话内的媒体资源列表，字段与 Node serializeMediaResource 对齐，
// 并额外附带 nativeFile 元数据（fileId/accessHash/fileReference/dcId），
// 供 Node 直接构造 Go 原生下载任务（M3.2 的前置依赖）。
func (a *App) fetchNativeMedia(ctx context.Context, api *tg.Client, account NativeAccount, peerID string, limit int, before int) ([]map[string]any, error) {
	requestLimit := limit * 4
	if requestLimit > maxChatMessageLimit {
		requestLimit = maxChatMessageLimit
	}
	messages, err := a.fetchNativeMessages(ctx, api, account, peerID, requestLimit, before, 0)
	if err != nil {
		return nil, err
	}
	files := []map[string]any{}
	for _, item := range messages {
		media, _ := item["media"].(map[string]any)
		if len(media) == 0 || !chatBool(media["hasPreview"]) {
			continue
		}
		files = append(files, map[string]any{
			"id":            item["id"],
			"date":          item["date"],
			"text":          item["text"],
			"fileName":      mediaFileName(media),
			"kind":          chatString(media["kind"]),
			"size":          chatString(media["size"]),
			"width":         chatInt(media["width"]),
			"height":        chatInt(media["height"]),
			"duration":      chatInt(media["duration"]),
			"mimeType":      chatString(media["mimeType"]),
			"fileId":        chatString(media["fileId"]),
			"accessHash":    chatString(media["accessHash"]),
			"fileReference": chatString(media["fileReference"]),
			"dcId":          chatInt(media["dcId"]),
		})
		if len(files) >= limit {
			break
		}
	}
	// 与 Node chatMedia 保持一致：最新的媒体在前，便于前端分页取 min(id) 作为 nextBefore。
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}
	return files, nil
}

func mediaFileName(media map[string]any) string {
	if name := chatString(media["fileName"]); name != "" {
		return name
	}
	if mime := chatString(media["mimeType"]); mime != "" {
		return mime
	}
	return "Telegram 媒体"
}

func (a *App) fetchNativeMessageByID(ctx context.Context, api *tg.Client, info nativePeerInfo, messageID int) (*tg.Message, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}}
	var result tg.MessagesMessagesClass
	var err error
	if info.Type == "channel" {
		accessHash, parseErr := strconv.ParseInt(info.AccessHash, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid channel access hash: %w", parseErr)
		}
		result, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: int64FromString(info.ID), AccessHash: accessHash},
			ID:      ids,
		})
	} else {
		result, err = api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, err
	}
	messages, _, _, ok := flattenNativeMessages(result)
	if !ok {
		return nil, errors.New("unexpected messages result")
	}
	for _, entry := range messages {
		if message, ok := entry.(*tg.Message); ok && message != nil && message.ID == messageID {
			return message, nil
		}
	}
	return nil, errors.New("message not found")
}

func flattenNativeMessages(result tg.MessagesMessagesClass) ([]tg.MessageClass, []tg.UserClass, []tg.ChatClass, bool) {
	switch typed := result.(type) {
	case *tg.MessagesMessages:
		return typed.Messages, typed.Users, typed.Chats, true
	case *tg.MessagesMessagesSlice:
		return typed.Messages, typed.Users, typed.Chats, true
	case *tg.MessagesChannelMessages:
		return typed.Messages, typed.Users, typed.Chats, true
	default:
		return nil, nil, nil, false
	}
}

func sortMessagesByID(items []map[string]any) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && chatInt(items[j]["id"]) < chatInt(items[j-1]["id"]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func formatGroupedID(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func serializeNativeMessage(message *tg.Message, index map[string]nativePeerInfo) map[string]any {
	senderID := peerIDFromPeerClass(message.FromID)
	if senderID == "" {
		senderID = peerIDFromPeerClass(message.PeerID)
	}
	sender := serializeNativeSender(index[senderID], senderID)
	return map[string]any{
		"id":        message.ID,
		"date":      time.Unix(int64(message.Date), 0).UTC().Format(time.RFC3339),
		"text":      message.Message,
		"entities":  serializeNativeEntities(message),
		"outgoing":  message.Out,
		"senderId":  senderID,
		"sender":    sender,
		"groupedId": formatGroupedID(message.GroupedID),
		"buttons":   serializeNativeButtons(message.ReplyMarkup),
		"media":     serializeNativeMedia(message),
	}
}

func serializeNativeSender(info nativePeerInfo, fallback string) map[string]any {
	id := info.PeerID
	if id == "" {
		id = fallback
	}
	title := chatTitle(info, fallback)
	return map[string]any{
		"id":       id,
		"title":    title,
		"username": info.Username,
		"rawId":    coalesce(info.ID, fallback),
	}
}

func serializeNativeEntities(message *tg.Message) []map[string]any {
	out := []map[string]any{}
	for _, entry := range message.Entities {
		if entry == nil {
			continue
		}
		entity, ok := entityOffsets(entry)
		if !ok {
			continue
		}
		value := message.Message
		if entity.offset < len(value) && entity.length > 0 {
			end := entity.offset + entity.length
			if end > len(value) {
				end = len(value)
			}
			value = value[entity.offset:end]
		} else {
			value = ""
		}
		url := entityNativeURL(entity, value)
		if url == "" {
			continue
		}
		if strings.HasPrefix(url, "t.me/") {
			url = "https://" + url
		}
		out = append(out, map[string]any{
			"offset": entity.offset,
			"length": entity.length,
			"url":    url,
			"type":   entity.typeName,
		})
	}
	return out
}

type nativeEntity struct {
	typeName string
	offset   int
	length   int
	url      string
	userID   int64
}

func entityOffsets(entry tg.MessageEntityClass) (nativeEntity, bool) {
	typeName := strings.TrimPrefix(fmt.Sprintf("%T", entry), "*tg.")
	entity := nativeEntity{typeName: typeName}
	switch typed := entry.(type) {
	case *tg.MessageEntityURL:
		entity.offset, entity.length = typed.Offset, typed.Length
	case *tg.MessageEntityTextURL:
		entity.offset, entity.length, entity.url = typed.Offset, typed.Length, typed.URL
	case *tg.MessageEntityMention:
		entity.offset, entity.length = typed.Offset, typed.Length
	case *tg.MessageEntityMentionName:
		entity.offset, entity.length, entity.userID = typed.Offset, typed.Length, typed.UserID
	case *tg.MessageEntityEmail:
		entity.offset, entity.length = typed.Offset, typed.Length
	case *tg.MessageEntityHashtag:
		entity.offset, entity.length = typed.Offset, typed.Length
	case *tg.MessageEntityBotCommand:
		entity.offset, entity.length = typed.Offset, typed.Length
	case *tg.MessageEntityCashtag:
		entity.offset, entity.length = typed.Offset, typed.Length
	default:
		if provider, ok := entry.(interface {
			GetOffset() int
			GetLength() int
		}); ok {
			entity.offset, entity.length = provider.GetOffset(), provider.GetLength()
		} else {
			return entity, false
		}
	}
	return entity, true
}

func entityNativeURL(entity nativeEntity, value string) string {
	if entity.url != "" {
		return entity.url
	}
	// gotd 生成的类型名是全大写的 MessageEntityURL，GramJS 侧是 MessageEntityUrl，
	// 统一转成小写比较，避免大小写差异导致链接实体被静默丢弃。
	switch strings.ToLower(entity.typeName) {
	case "messageentityurl":
		return value
	case "messageentitymention":
		if strings.HasPrefix(value, "@") {
			return "https://t.me/" + value[1:]
		}
	case "messageentitymentionname":
		if entity.userID != 0 {
			return fmt.Sprintf("tg://user?id=%d", entity.userID)
		}
	}
	return ""
}

func serializeNativeButtons(markup tg.ReplyMarkupClass) [][]map[string]any {
	rows := [][]map[string]any{}
	inline, ok := markup.(*tg.ReplyInlineMarkup)
	if !ok || inline == nil {
		return rows
	}
	for _, row := range inline.Rows {
		buttons := []map[string]any{}
		for _, entry := range row.Buttons {
			button := map[string]any{"text": "", "url": "", "data": "", "type": "unsupported"}
			switch typed := entry.(type) {
			case *tg.KeyboardButtonURL:
				button["text"], button["url"], button["type"] = typed.Text, typed.URL, "url"
			case *tg.KeyboardButtonCallback:
				button["text"], button["type"] = typed.Text, "callback"
				button["data"] = base64.StdEncoding.EncodeToString(typed.Data)
			case *tg.KeyboardButton:
				button["text"] = typed.Text
			default:
				continue
			}
			buttons = append(buttons, button)
		}
		if len(buttons) > 0 {
			rows = append(rows, buttons)
		}
	}
	return rows
}

func serializeNativeMedia(message *tg.Message) map[string]any {
	media, ok := message.GetMedia()
	if !ok || media == nil {
		return nil
	}
	className := strings.TrimPrefix(fmt.Sprintf("%T", media), "*tg.")
	switch typed := media.(type) {
	case *tg.MessageMediaPhoto:
		item := map[string]any{
			"className":     className,
			"hasPreview":    true,
			"mimeType":      "image/jpeg",
			"kind":          "image",
			"width":         0,
			"height":        0,
			"duration":      0,
			"fileName":      "",
			"size":          "0",
			"dcId":          0,
			"fileId":        "",
			"accessHash":    "",
			"fileReference": "",
		}
		if photo, ok := typed.GetPhoto(); ok {
			if value, ok := photo.(*tg.Photo); ok && value != nil {
				for _, size := range value.Sizes {
					if concrete, ok := size.(*tg.PhotoSize); ok && concrete != nil {
						item["width"], item["height"] = concrete.W, concrete.H
					}
				}
				item["fileName"] = fmt.Sprintf("photo-%d.jpg", value.ID)
				item["dcId"] = value.DCID
			}
		}
		return item
	case *tg.MessageMediaDocument:
		doc, ok := typed.GetDocument()
		if !ok {
			return nil
		}
		value, ok := doc.(*tg.Document)
		if !ok || value == nil {
			return nil
		}
		kind := "file"
		switch {
		case strings.HasPrefix(value.MimeType, "image/"):
			kind = "image"
		case strings.HasPrefix(value.MimeType, "video/") || value.MimeType == "application/x-tgsticker":
			kind = "video"
		}
		item := map[string]any{
			"className":     className,
			"hasPreview":    true,
			"mimeType":      value.MimeType,
			"kind":          kind,
			"width":         0,
			"height":        0,
			"duration":      0,
			"fileName":      "",
			"size":          strconv.FormatInt(value.Size, 10),
			"dcId":          value.DCID,
			"fileId":        strconv.FormatInt(value.ID, 10),
			"accessHash":    strconv.FormatInt(value.AccessHash, 10),
			"fileReference": base64.StdEncoding.EncodeToString(value.FileReference),
		}
		for _, attribute := range value.Attributes {
			switch typedAttr := attribute.(type) {
			case *tg.DocumentAttributeFilename:
				item["fileName"] = typedAttr.FileName
			case *tg.DocumentAttributeVideo:
				item["duration"] = typedAttr.Duration
				item["width"] = typedAttr.W
				item["height"] = typedAttr.H
				if kind == "file" {
					kind = "video"
					item["kind"] = "video"
				}
			case *tg.DocumentAttributeImageSize:
				item["width"] = typedAttr.W
				item["height"] = typedAttr.H
			}
		}
		return item
	}
	return map[string]any{
		"className":     className,
		"hasPreview":    false,
		"mimeType":      "",
		"kind":          "file",
		"width":         0,
		"height":        0,
		"duration":      0,
		"fileName":      "",
		"size":          "0",
		"dcId":          0,
		"fileId":        "",
		"accessHash":    "",
		"fileReference": "",
	}
}

// --- peer 解析 ------------------------------------------------------------

// peerIndexUsable 判断索引条目是否「可直接使用」。channel 必须带 accessHash——
// 否则 main.go 的 file_reference 刷新链路必然以「native peer 元数据缺失」失败，
// 等于没有命中；这类贫信息（由消息发送者缓存产生）若在索引里短路返回，会让
// 「回拉会话列表自愈」永远不被触发，解析逻辑在空 accessHash 上原地绕圈
// （R4.36 核证）。
func peerIndexUsable(info nativePeerInfo) bool {
	if strings.TrimSpace(info.Type) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(info.Type), "channel") {
		return strings.TrimSpace(info.AccessHash) != ""
	}
	return true
}

// lookupNativePeer 在索引中查找可用条目；未命中或条目不可用时返回 false，
// 让调用方继续走「回拉会话列表 + 深翻分页」自愈路径。
func lookupNativePeer(index map[string]nativePeerInfo, peerID string) (nativePeerInfo, bool) {
	if len(index) == 0 {
		return nativePeerInfo{}, false
	}
	info, ok := index[peerID]
	if !ok || !peerIndexUsable(info) {
		return nativePeerInfo{}, false
	}
	return info, true
}

// resolveNativePeer 把 "User:123" / "Chat:123" / "Channel:123" 解析为带 accessHash 的元数据。
// accessHash 只能从 dialogs 返回的 entity 中获得，故本地索引未命中时回拉一次会话列表。
// nativePeerBackfillRounds 深翻分页轮数上限：每轮 dialogPageSize(100) 会话，
// 30 轮 ≈ 3000/文件夹。R4.36 修正——此前按「每轮 500 会话」设计，而 MTProto
// 单页硬上限只有 100，每轮都会被误判为「不满页 = 已到末尾」，分页从未真正推进，
// 这正是「第 100 条之后的频道永远解析不到」的根因。
const nativePeerBackfillRounds = nativeDialogPageRounds

// backfillNativePeerIndex 深翻会话列表分页，为 peer 索引补齐「不在首批 500 个
// 会话」的频道/用户。R4.30：resolveNativePeer 此前只拉一次会话列表（上限 500），
// 会话数超过上限的账号（实测存在）解析不到目标频道，file_reference 刷新以
// 「找不到会话」终态失败。这里按 Telegram 标准分页（offset_peer + offset_id）
// 逐轮后翻，每轮把拿到的 peers 并入索引并落盘——即使中途失败，已翻到的部分
// 也保留。folder 0（主列表）与 folder 1（归档）各自翻满轮数。
func (a *App) backfillNativePeerIndex(ctx context.Context, api *tg.Client, account NativeAccount) error {
	for _, folderID := range []int{0, 1} {
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		offsetID := 0
		for round := 0; round < nativePeerBackfillRounds; round++ {
			request := &tg.MessagesGetDialogsRequest{
				OffsetPeer: offsetPeer,
				OffsetID:   offsetID,
				Limit:      dialogPageSize,
			}
			if folderID > 0 {
				request.SetFolderID(folderID)
			}
			result, err := api.MessagesGetDialogs(ctx, request)
			if err != nil {
				return fmt.Errorf("深翻会话列表（folder %d 第 %d 轮）失败：%w", folderID, round+1, err)
			}
			dialogs, _, chats, users, ok := flattenNativeDialogs(result)
			if !ok {
				break
			}
			index := indexNativePeers(users, chats)
			storeNativePeerIndex(account.UserID, account.AccountID, index)
			if len(dialogs) < dialogPageSize {
				break // 本轮不满一页：已到列表末尾
			}
			// 以最后一个会话构造下一轮 offset。找不到（类型异常/索引缺 accessHash）
			// 就到此为止，避免盲目空翻。
			var last *tg.Dialog
			for _, entry := range dialogs {
				if dialog, isDialog := entry.(*tg.Dialog); isDialog && dialog != nil {
					last = dialog
				}
			}
			if last == nil {
				break
			}
			roundIndex := index
			info, found := roundIndex[peerIDFromPeerClass(last.Peer)]
			if !found {
				break
			}
			nextPeer, peerErr := nativeInputPeerFromInfo(info)
			if peerErr != nil {
				break
			}
			offsetPeer = nextPeer
			offsetID = last.TopMessage
		}
	}
	return nil
}

func (a *App) resolveNativePeer(ctx context.Context, api *tg.Client, account NativeAccount, peerID string) (nativePeerInfo, error) {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return nativePeerInfo{}, errors.New("缺少 peer 参数")
	}
	if info, ok := lookupNativePeer(loadNativePeerIndex(account.UserID, account.AccountID), peerID); ok {
		return info, nil
	}
	// R4.35：解析闸门——singleflight（并发任务只发一轮 dialogs 拉取/深翻）
	// + 失败冷却（被 Telegram 限流后账号级退避，不再 5~10s 就重翻）。
	// R4.36：冷却分级——结构性失败只退避该 peer，并把连续失败升级为可操作终态。
	return a.peerResolveGate(nativeAccountKey(account.UserID, account.AccountID), peerID, func() (nativePeerInfo, error) {
		return a.resolveNativePeerUncached(ctx, api, account, peerID)
	})
}

// resolveNativePeerUncached 单次真实解析：回拉首批会话 → 深翻分页 → 再查索引。
// 调用方须经 peerResolveGate（闸门保证同账号同一时刻只跑一轮）。
func (a *App) resolveNativePeerUncached(ctx context.Context, api *tg.Client, account NativeAccount, peerID string) (nativePeerInfo, error) {
	if _, err := a.fetchNativeDialogs(ctx, api, account, maxChatDialogLimit, "", true); err != nil {
		return nativePeerInfo{}, err
	}
	if info, ok := lookupNativePeer(loadNativePeerIndex(account.UserID, account.AccountID), peerID); ok {
		return info, nil
	}
	// R4.30：首批 500 会话没有目标 peer 时深翻分页（最多 6 轮/文件夹）。
	// 深翻失败不阻断——已翻到的部分可能已包含目标，最后再查一次索引；
	// R4.35：失败原因保留进最终错误，让 FLOOD_WAIT 秒数能被 floodWaitFromError
	// 提取（此前深翻的 FLOOD_WAIT 只写日志，任务按通用退避 5~10s 重试，
	// 在限流窗口内反复撞墙、把限流喂得更大）。
	var backfillErr error
	if err := a.backfillNativePeerIndex(ctx, api, account); err != nil {
		backfillErr = err
		log.Printf("chatapi: peer %s 深翻会话列表未完成：%v", peerID, err)
	}
	if info, ok := lookupNativePeer(loadNativePeerIndex(account.UserID, account.AccountID), peerID); ok {
		return info, nil
	}
	if backfillErr != nil {
		return nativePeerInfo{}, fmt.Errorf("找不到会话 %s，可能已退出该群组、会话已被删除，或 Telegram 暂时无法解析该会话；索引刷新未完成：%w", peerID, backfillErr)
	}
	return nativePeerInfo{}, fmt.Errorf("找不到会话 %s，可能已退出该群组、会话已被删除，或 Telegram 暂时无法解析该会话", peerID)
}

// peerResolveCall 一次在途解析的共享结果（singleflight）。
type peerResolveCall struct {
	done chan struct{}
	info nativePeerInfo
	err  error
}

// peer 解析冷却：失败后按失败性质分级退避，至少 60s；FLOOD_WAIT 时按 Telegram
// 给的秒数等待（封顶 15 分钟），避免在限流窗口内反复深翻。
const (
	peerResolveBaseCooldown = 60 * time.Second
	peerResolveMaxCooldown  = 15 * time.Minute
	// peerStructFailThreshold 连续多少次「结构性找不到会话」后判定为不可达
	//（频道已退出/被删除）。深翻分页 + 索引落盘都覆盖过了，再重试不会变好。
	peerStructFailThreshold = 3
)

// peerUnreachableError 表示该 peer 结构性不可达：连续多轮解析（含深翻分页）
// 都找不到它，说明它已不在账号会话列表里（已退出该群组/频道被删除）。
// 这不是瞬态错误——无限重试没有意义，必须转成带操作指引的终态。
// 手动重试会重置计数，用户重新加入频道后可以立刻恢复。
type peerUnreachableError struct {
	PeerID string
	Count  int
}

func (e *peerUnreachableError) Error() string {
	return fmt.Sprintf("频道 %s 已不在你的会话列表中（连续 %d 次解析失败，已含会话列表深翻分页）：可能你已退出该群组，或该频道已被删除。"+
		"该文件的下载无法继续——请先把该频道重新加入你的 Telegram 账号，然后点「重试」恢复（重试会重置解析计数）", e.PeerID, e.Count)
}

// isRateLimitPeerError 区分「限流/传输类失败」与「结构性失败」：
// 前者账号级退避（保护账号不再撞 FLOOD_WAIT），后者只退避该 peer
// （否则一个不可达频道会把其他健康频道的解析一起冻结 60s，R4.36 核证）。
func isRateLimitPeerError(err error) bool {
	if err == nil {
		return false
	}
	if floodWaitFromError(err) > 0 {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"flood_wait", "floodwait", "too many requests", "timeout", "deadline exceeded",
		"connection", "retry limit reached", "retryuntilack", "engine was closed", "eof",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// peerResolveCooldownDuration 依据失败原因计算冷却时长。纯函数便于单测。
func peerResolveCooldownDuration(err error) time.Duration {
	d := peerResolveBaseCooldown
	if wait := floodWaitFromError(err); wait > 0 {
		if wd := time.Duration(wait) * time.Second; wd > d {
			d = wd
		}
	}
	if d > peerResolveMaxCooldown {
		d = peerResolveMaxCooldown
	}
	return d
}

// peerResolveGate peer 解析闸门（R4.35 引入，R4.36 分级）：
//   - 账号级冷却：仅限流/传输类失败后设置，保护账号不再撞 FLOOD_WAIT；
//   - peer 级冷却：结构性「找不到会话」只退避该 peer，不连坐其他频道的解析；
//   - singleflight：同账号并发解析共享一轮结果，不重复深翻；
//   - 连续 peerStructFailThreshold 次结构性失败 → peerUnreachableError（终态）。
//
// 实测背景（2.6.12）：三个任务同时解析同一个缺失 accessHash 的频道，
// 各自全量深翻会话列表 → FLOOD_WAIT(9)/(6) → 秒级重试再翻 → 限流自噬。
// 实测背景（2.6.13）：同一个不可达频道让整个账号的解析冻结 60s。
func (a *App) peerResolveGate(accountKey, peerID string, fn func() (nativePeerInfo, error)) (nativePeerInfo, error) {
	peerKey := peerResolveKeyOf(accountKey, peerID)
	a.peerResolveMu.Lock()
	// 惰性初始化：测试与部分构造点用 App 字面量，未走 newApp 初始化。
	if a.peerResolveInflight == nil {
		a.peerResolveInflight = map[string]*peerResolveCall{}
	}
	if a.peerResolveCooldown == nil {
		a.peerResolveCooldown = map[string]time.Time{}
	}
	if a.peerResolvePeerCooldown == nil {
		a.peerResolvePeerCooldown = map[string]time.Time{}
	}
	if a.peerResolveFailCount == nil {
		a.peerResolveFailCount = map[string]int{}
	}
	if until, ok := a.peerResolveCooldown[accountKey]; ok {
		if remain := time.Until(until); remain > 0 {
			a.peerResolveMu.Unlock()
			return nativePeerInfo{}, fmt.Errorf("会话索引解析在冷却中（剩余 %s）——上一轮解析被 Telegram 限流，冷却结束会自动重试", remain.Round(time.Second))
		}
		delete(a.peerResolveCooldown, accountKey)
	}
	if until, ok := a.peerResolvePeerCooldown[peerKey]; ok {
		if remain := time.Until(until); remain > 0 {
			a.peerResolveMu.Unlock()
			return nativePeerInfo{}, fmt.Errorf("该频道解析退避中（剩余 %s）——上一轮解析没找到这个会话，稍后会自动重试", remain.Round(time.Second))
		}
		delete(a.peerResolvePeerCooldown, peerKey)
	}
	if call := a.peerResolveInflight[accountKey]; call != nil {
		a.peerResolveMu.Unlock()
		<-call.done
		return call.info, call.err
	}
	call := &peerResolveCall{done: make(chan struct{})}
	a.peerResolveInflight[accountKey] = call
	a.peerResolveMu.Unlock()

	info, err := fn()

	a.peerResolveMu.Lock()
	delete(a.peerResolveInflight, accountKey)
	if err == nil {
		// 解析成功：结构性失败计数归零（频道重新可达后立刻恢复常态）。
		delete(a.peerResolveFailCount, peerKey)
	} else if isRateLimitPeerError(err) {
		cooldown := peerResolveCooldownDuration(err)
		a.peerResolveCooldown[accountKey] = time.Now().Add(cooldown)
		log.Printf("peer resolve gate: account %s 解析失败（限流/传输类），%s 内不再发起解析：%v", accountKey, cooldown, err)
	} else {
		cooldown := peerResolveCooldownDuration(err)
		a.peerResolvePeerCooldown[peerKey] = time.Now().Add(cooldown)
		a.peerResolveFailCount[peerKey]++
		count := a.peerResolveFailCount[peerKey]
		log.Printf("peer resolve gate: %s 解析失败（结构性，第 %d/%d 次），%s 内不再解析该 peer：%v", peerID, count, peerStructFailThreshold, cooldown, err)
		if count >= peerStructFailThreshold {
			// 深翻分页 + 索引落盘都覆盖过仍找不到 → 结构性不可达，转可操作终态。
			err = &peerUnreachableError{PeerID: peerID, Count: count}
		}
	}
	call.info, call.err = info, err
	close(call.done)
	a.peerResolveMu.Unlock()
	return info, err
}

// peerResolveKeyOf 组合「账号 + peer」的解析退避键。
func peerResolveKeyOf(accountKey, peerID string) string {
	return accountKey + "|" + peerID
}

// peerResolveCooldownRemaining 返回该任务 peer 的解析冷却剩余时间（账号级限流冷却
// 或 peer 级结构性退避，取先到期的那个）。供调度层在启动前判断——冷却期内启动
// 注定失败，只是白建连接与白选 DC（R4.36-C）。
func (a *App) peerResolveCooldownRemaining(userID, accountID, peerID string) (time.Duration, bool) {
	if userID == "" || accountID == "" || peerID == "" {
		return 0, false
	}
	accountKey := nativeAccountKey(userID, accountID)
	peerKey := peerResolveKeyOf(accountKey, peerID)
	a.peerResolveMu.Lock()
	defer a.peerResolveMu.Unlock()
	now := time.Now()
	if until, ok := a.peerResolveCooldown[accountKey]; ok {
		if remain := until.Sub(now); remain > 0 {
			return remain, true
		}
	}
	if until, ok := a.peerResolvePeerCooldown[peerKey]; ok {
		if remain := until.Sub(now); remain > 0 {
			return remain, true
		}
	}
	return 0, false
}

// resetPeerResolveFailures 手动重试时清除该 peer 的结构性失败计数与退避，
// 让「重新加入频道 → 点重试」能立即恢复解析（R4.36）。
func (a *App) resetPeerResolveFailures(userID, accountID, peerID string) {
	if userID == "" || accountID == "" || peerID == "" {
		return
	}
	accountKey := nativeAccountKey(userID, accountID)
	peerKey := peerResolveKeyOf(accountKey, peerID)
	a.peerResolveMu.Lock()
	defer a.peerResolveMu.Unlock()
	delete(a.peerResolveFailCount, peerKey)
	delete(a.peerResolvePeerCooldown, peerKey)
}

func nativeInputPeerFromInfo(info nativePeerInfo) (tg.InputPeerClass, error) {
	id := int64FromString(info.ID)
	if id == 0 {
		return nil, errors.New("invalid peer id")
	}
	switch info.Type {
	case "user":
		accessHash, err := strconv.ParseInt(info.AccessHash, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid user access hash: %w", err)
		}
		return &tg.InputPeerUser{UserID: id, AccessHash: accessHash}, nil
	case "chat":
		return &tg.InputPeerChat{ChatID: id}, nil
	case "channel":
		accessHash, err := strconv.ParseInt(info.AccessHash, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid channel access hash: %w", err)
		}
		return &tg.InputPeerChannel{ChannelID: id, AccessHash: accessHash}, nil
	}
	return nil, fmt.Errorf("unsupported peer type %q", info.Type)
}

func inputPeerIDString(peer tg.InputPeerClass) string {
	switch typed := peer.(type) {
	case *tg.InputPeerUser:
		if typed == nil {
			return ""
		}
		return "User:" + strconv.FormatInt(typed.UserID, 10)
	case *tg.InputPeerChat:
		if typed == nil {
			return ""
		}
		return "Chat:" + strconv.FormatInt(typed.ChatID, 10)
	case *tg.InputPeerChannel:
		if typed == nil {
			return ""
		}
		return "Channel:" + strconv.FormatInt(typed.ChannelID, 10)
	}
	return ""
}

func peerIDFromPeerClass(peer tg.PeerClass) string {
	switch typed := peer.(type) {
	case *tg.PeerUser:
		if typed == nil {
			return ""
		}
		return "User:" + strconv.FormatInt(typed.UserID, 10)
	case *tg.PeerChat:
		if typed == nil {
			return ""
		}
		return "Chat:" + strconv.FormatInt(typed.ChatID, 10)
	case *tg.PeerChannel:
		if typed == nil {
			return ""
		}
		return "Channel:" + strconv.FormatInt(typed.ChannelID, 10)
	}
	return ""
}

// --- 头像 -----------------------------------------------------------------

func (a *App) fetchNativeAvatar(ctx context.Context, client *telegram.Client, account NativeAccount, peerID string) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	runErr := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		info := nativePeerInfo{PeerID: peerID}
		if peerID == "" || peerID == "__self" {
			self, selfErr := a.nativeSelfInfo(ctx, api)
			if selfErr != nil {
				return selfErr
			}
			info = self
		} else {
			resolved, resolveErr := a.resolveNativePeer(ctx, api, account, peerID)
			if resolveErr != nil {
				return resolveErr
			}
			info = resolved
		}
		if !info.HasPhoto || info.PhotoID == 0 {
			return errors.New("暂无头像")
		}
		peer, peerErr := nativeInputPeerFromInfo(info)
		if peerErr != nil {
			return peerErr
		}
		location := &tg.InputPeerPhotoFileLocation{Peer: peer, PhotoID: info.PhotoID}
		data, err = downloadNativeFile(ctx, client, location)
		return err
	})
	if runErr != nil {
		return nil, runErr
	}
	if len(data) == 0 {
		return nil, errors.New("暂无头像")
	}
	return data, nil
}

func (a *App) nativeSelfInfo(ctx context.Context, api *tg.Client) (nativePeerInfo, error) {
	users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return nativePeerInfo{}, fmt.Errorf("获取账号信息失败：%w", err)
	}
	for _, entry := range users {
		user, ok := entry.(*tg.User)
		if !ok || user == nil {
			continue
		}
		photoID, hasPhoto := userProfilePhotoID(user.Photo)
		title := strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName))
		return nativePeerInfo{
			PeerID:     "User:" + strconv.FormatInt(user.ID, 10),
			Type:       "user",
			ID:         strconv.FormatInt(user.ID, 10),
			AccessHash: strconv.FormatInt(user.AccessHash, 10),
			Title:      title,
			Username:   user.Username,
			Kind:       "private",
			PhotoID:    photoID,
			HasPhoto:   hasPhoto,
			Bot:        user.Bot,
			Contact:    user.Contact,
		}, nil
	}
	return nativePeerInfo{}, errors.New("获取账号信息失败：返回为空")
}

// downloadNativeFile 通过 upload.getFile 读取小文件（头像/缩略图），
// 命中 FILE_MIGRATE_x 时按既有 readNativeSample 的方式切到媒体 DC。
func downloadNativeFile(ctx context.Context, client *telegram.Client, location tg.InputFileLocationClass) ([]byte, error) {
	data, err := readNativeFileChunks(ctx, client.API(), location)
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
	return readNativeFileChunks(ctx, tg.NewClient(invoker), location)
}

func readNativeFileChunks(ctx context.Context, api *tg.Client, location tg.InputFileLocationClass) ([]byte, error) {
	data := []byte{}
	for int64(len(data)) < maxAvatarBytes {
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: location,
			Offset:   int64(len(data)),
			Limit:    avatarChunkSize,
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
		if len(file.Bytes) < avatarChunkSize {
			break
		}
	}
	return data, nil
}

// --- 小工具 ---------------------------------------------------------------

func clampChatLimit(raw string, fallback int, maximum int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func intFromQuery(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func int64FromString(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// chatInt / chatBool / chatString 是 chatapi 内部的宽松取值辅助：
// 既有 int64Value/boolValue/stringValue 返回 (值, ok) 二元组且对空串判定为 false，
// 不适合直接读取本文件构造的 map，故在此提供单返回值版本，不改动下载主链路的既有函数。
func chatInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		return int(int64FromString(typed))
	}
	return 0
}

func chatBool(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func chatString(value any) string {
	typed, _ := value.(string)
	return typed
}
