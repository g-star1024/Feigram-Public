package main

// M4.2 单元测试：覆盖 peer 解析、消息序列化、文件夹匹配与参数钳制。
// 这些函数是纯函数，可在无 Telegram 凭据的环境下离线验证（铁律 4 的「真实验证」）。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestPeerIDFromPeerClass(t *testing.T) {
	cases := []struct {
		name string
		peer tg.PeerClass
		want string
	}{
		{"user", &tg.PeerUser{UserID: 123}, "User:123"},
		{"chat", &tg.PeerChat{ChatID: 456}, "Chat:456"},
		{"channel", &tg.PeerChannel{ChannelID: 789}, "Channel:789"},
		{"empty", &tg.PeerUser{}, "User:0"},
		{"nil interface", nil, ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := peerIDFromPeerClass(item.peer); got != item.want {
				t.Fatalf("peerIDFromPeerClass(%T) = %q, want %q", item.peer, got, item.want)
			}
		})
	}
}

func TestInputPeerIDString(t *testing.T) {
	if got := inputPeerIDString(&tg.InputPeerChannel{ChannelID: 100500}); got != "Channel:100500" {
		t.Fatalf("inputPeerIDString = %q, want Channel:100500", got)
	}
	if got := inputPeerIDString(&tg.InputPeerSelf{}); got != "" {
		t.Fatalf("inputPeerIDString(self) = %q, want empty", got)
	}
}

func TestNativeInputPeerFromInfo(t *testing.T) {
	peer, err := nativeInputPeerFromInfo(nativePeerInfo{Type: "channel", ID: "42", AccessHash: "99"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		t.Fatalf("expected InputPeerChannel, got %T", peer)
	}
	if channel.ChannelID != 42 || channel.AccessHash != 99 {
		t.Fatalf("unexpected channel peer: %+v", channel)
	}
	if _, err := nativeInputPeerFromInfo(nativePeerInfo{Type: "user", ID: "42", AccessHash: "not-a-number"}); err == nil {
		t.Fatal("expected error for invalid access hash")
	}
	if _, err := nativeInputPeerFromInfo(nativePeerInfo{Type: "unknown", ID: "42"}); err == nil {
		t.Fatal("expected error for unsupported peer type")
	}
}

func TestIndexNativePeers(t *testing.T) {
	users := []tg.UserClass{
		&tg.User{ID: 7, AccessHash: 777, FirstName: "Ada", LastName: "Lovelace", Username: "ada", Bot: false, Contact: true},
	}
	chats := []tg.ChatClass{
		&tg.Channel{ID: 8, AccessHash: 888, Title: "Feigram", Username: "feigram", Broadcast: true},
		&tg.Chat{ID: 9, Title: "Legacy Group"},
	}
	index := indexNativePeers(users, chats)
	user, ok := index["User:7"]
	if !ok {
		t.Fatal("user peer missing")
	}
	if user.AccessHash != "777" || user.Title != "Ada Lovelace" || !user.Contact {
		t.Fatalf("unexpected user info: %+v", user)
	}
	channel, ok := index["Channel:8"]
	if !ok || channel.Kind != "channel" || channel.AccessHash != "888" {
		t.Fatalf("unexpected channel info: %+v", channel)
	}
	chat, ok := index["Chat:9"]
	if !ok || chat.Type != "chat" || chat.AccessHash != "" {
		t.Fatalf("unexpected chat info: %+v", chat)
	}
}

func TestSerializeNativeMessage(t *testing.T) {
	message := &tg.Message{
		ID:      55,
		Date:    1700000000,
		Message: "see https://t.me/feigram and @ada",
		FromID:  &tg.PeerUser{UserID: 7},
		PeerID:  &tg.PeerChannel{ChannelID: 8},
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityURL{Offset: 4, Length: 20},
			&tg.MessageEntityMention{Offset: 29, Length: 4},
			&tg.MessageEntityBold{Offset: 0, Length: 3},
		},
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonURL{Text: "打开", URL: "https://example.com"},
				&tg.KeyboardButtonCallback{Text: "刷新", Data: []byte{1, 2, 3}},
			}},
		}},
	}
	index := map[string]nativePeerInfo{
		"User:7": {PeerID: "User:7", ID: "7", Title: "Ada", Username: "ada", Kind: "private"},
	}
	item := serializeNativeMessage(message, index)
	if chatString(item["senderId"]) != "User:7" {
		t.Fatalf("unexpected senderId: %v", item["senderId"])
	}
	if chatString(item["date"]) != "2023-11-14T22:13:20Z" {
		t.Fatalf("unexpected date: %v", item["date"])
	}
	entities, _ := item["entities"].([]map[string]any)
	if len(entities) != 2 {
		t.Fatalf("expected 2 link entities (bold has no url), got %d: %+v", len(entities), entities)
	}
	if chatString(entities[0]["url"]) != "https://t.me/feigram" {
		t.Fatalf("unexpected first entity url: %v", entities[0]["url"])
	}
	if chatString(entities[1]["url"]) != "https://t.me/ada" {
		t.Fatalf("unexpected mention url: %v", entities[1]["url"])
	}
	buttons, _ := item["buttons"].([][]map[string]any)
	if len(buttons) != 1 || len(buttons[0]) != 2 {
		t.Fatalf("unexpected buttons: %+v", item["buttons"])
	}
	if chatString(buttons[0][0]["type"]) != "url" || chatString(buttons[0][1]["type"]) != "callback" {
		t.Fatalf("unexpected button types: %+v", buttons[0])
	}
	if chatString(buttons[0][1]["data"]) != "AQID" {
		t.Fatalf("callback data should be base64 encoded, got %q", buttons[0][1]["data"])
	}
	// 注意：nil map 装进 any 之后与 nil 比较不相等，只能判长度。
	if media, _ := item["media"].(map[string]any); len(media) != 0 {
		t.Fatalf("expected empty media for plain message, got %+v", media)
	}
}

func TestSerializeNativeMedia(t *testing.T) {
	// gotd 的条件字段需要显式 SetXxx 才会写入 Flags，直接赋值字面量会被 GetXxx 判定为缺失。
	videoMedia := &tg.MessageMediaDocument{}
	videoMedia.SetDocument(&tg.Document{
		ID:            555,
		AccessHash:    12345,
		FileReference: []byte{9, 9},
		DCID:          2,
		MimeType:      "video/mp4",
		Size:          4096,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "movie.mp4"},
			&tg.DocumentAttributeVideo{Duration: 12, W: 640, H: 360},
		},
	})
	video := &tg.Message{ID: 1}
	video.SetMedia(videoMedia)
	media := serializeNativeMedia(video)
	if chatString(media["kind"]) != "video" || chatString(media["fileName"]) != "movie.mp4" {
		t.Fatalf("unexpected video media: %+v", media)
	}
	if chatString(media["size"]) != "4096" || chatInt(media["duration"]) != 12 || chatInt(media["width"]) != 640 {
		t.Fatalf("unexpected video metadata: %+v", media)
	}
	if chatString(media["fileId"]) != "555" || chatString(media["accessHash"]) != "12345" {
		t.Fatalf("unexpected file identity: %+v", media)
	}

	photoMediaObject := &tg.MessageMediaPhoto{}
	photoMediaObject.SetPhoto(&tg.Photo{ID: 77, DCID: 2, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{W: 100, H: 200}}})
	photo := &tg.Message{ID: 2}
	photo.SetMedia(photoMediaObject)
	photoMedia := serializeNativeMedia(photo)
	if chatString(photoMedia["kind"]) != "image" || chatInt(photoMedia["height"]) != 200 {
		t.Fatalf("unexpected photo media: %+v", photoMedia)
	}

	if got := serializeNativeMedia(&tg.Message{ID: 3}); got != nil {
		t.Fatalf("expected nil media for plain message, got %+v", got)
	}
}

func TestNativeFilterMatchesChat(t *testing.T) {
	filter := map[string]any{
		"id":             2,
		"includePeerIds": []string{},
		"pinnedPeerIds":  []string{},
		"excludePeerIds": []string{"Channel:9"},
		"flags":          map[string]bool{"groups": true, "excludeArchived": true},
	}
	group := map[string]any{
		"id":          "Chat:5",
		"folderIds":   []int{},
		"type":        "group",
		"archived":    false,
		"muted":       false,
		"unreadCount": 3,
		"bot":         false,
		"contact":     false,
	}
	if !nativeFilterMatchesChat(filter, group) {
		t.Fatal("group chat should match groups flag")
	}
	archived := map[string]any{"id": "Chat:6", "folderIds": []int{1}, "type": "group", "archived": true, "unreadCount": 1}
	if nativeFilterMatchesChat(filter, archived) {
		t.Fatal("archived chat should be excluded by excludeArchived")
	}
	excluded := map[string]any{"id": "Channel:9", "folderIds": []int{}, "type": "channel", "unreadCount": 1}
	if nativeFilterMatchesChat(filter, excluded) {
		t.Fatal("explicitly excluded chat must not match")
	}
	byFolder := map[string]any{"id": "Channel:11", "folderIds": []int{2}, "type": "channel", "unreadCount": 0}
	if !nativeFilterMatchesChat(filter, byFolder) {
		t.Fatal("chat in the same folder should match")
	}
}

func TestSyntheticNativeFolders(t *testing.T) {
	chats := []map[string]any{
		{"id": "Chat:1", "folderId": 1},
		{"id": "Channel:2", "folderId": 1},
		{"id": "User:3", "folderId": 0},
	}
	folders := syntheticNativeFolders(chats)
	if len(folders) != 1 {
		t.Fatalf("expected 1 synthetic folder, got %d", len(folders))
	}
	if chatString(folders[0]["title"]) != "归档" {
		t.Fatalf("unexpected folder title: %v", folders[0]["title"])
	}
	chatIDs, _ := folders[0]["chatIds"].([]string)
	if len(chatIDs) != 2 {
		t.Fatalf("unexpected chatIds: %v", chatIDs)
	}
}

func TestClampChatLimitAndSort(t *testing.T) {
	if got := clampChatLimit("", 50, 200); got != 50 {
		t.Fatalf("default limit = %d, want 50", got)
	}
	if got := clampChatLimit("1000", 50, 200); got != 200 {
		t.Fatalf("limit should be clamped, got %d", got)
	}
	if got := clampChatLimit("abc", 30, 100); got != 30 {
		t.Fatalf("invalid limit should fall back, got %d", got)
	}
	items := []map[string]any{{"id": 3}, {"id": 1}, {"id": 2}}
	sortMessagesByID(items)
	if chatInt(items[0]["id"]) != 1 || chatInt(items[2]["id"]) != 3 {
		t.Fatalf("messages not sorted ascending: %+v", items)
	}
}

func TestFormatGroupedID(t *testing.T) {
	if got := formatGroupedID(0); got != "" {
		t.Fatalf("grouped id 0 should be empty string, got %q", got)
	}
	if got := formatGroupedID(1234567890123); got != "1234567890123" {
		t.Fatalf("unexpected grouped id: %q", got)
	}
}

// R4.30：peer 索引必须落盘并在启动时恢复。此前索引是纯内存结构，服务重启即空，
// 下载任务刷新 file_reference 时「找不到会话 Channel:xxx」终态失败。
// 本用例验证：写入 → 落盘 → 清空内存 → 重新绑定路径加载 → 条目仍在。
func TestNativePeerIndexPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	// 隔离全局状态，避免污染其他用例。
	nativePeerIndexMu.Lock()
	savedFile, savedIndex := nativePeerIndexFile, nativePeerIndex
	nativePeerIndex = map[string]map[string]nativePeerInfo{}
	nativePeerIndexMu.Unlock()
	defer func() {
		nativePeerIndexMu.Lock()
		nativePeerIndexFile, nativePeerIndex = savedFile, savedIndex
		nativePeerIndexMu.Unlock()
	}()

	initNativePeerIndexStore(dir)
	storeNativePeerIndex("u", "restart", map[string]nativePeerInfo{
		"Channel:2052039292": {PeerID: "Channel:2052039292", Type: "channel", ID: "2052039292", AccessHash: "770077", Title: "Deep"},
	})
	if info, ok := loadNativePeerIndex("u", "restart")["Channel:2052039292"]; !ok || info.AccessHash != "770077" {
		t.Fatalf("写入后应能立即命中：%+v", info)
	}

	// 模拟重启：清空内存 + 重新初始化（initNativePeerIndexStore 会从磁盘恢复）。
	nativePeerIndexMu.Lock()
	nativePeerIndex = map[string]map[string]nativePeerInfo{}
	nativePeerIndexMu.Unlock()
	initNativePeerIndexStore(dir)
	info, ok := loadNativePeerIndex("u", "restart")["Channel:2052039292"]
	if !ok || info.AccessHash != "770077" || info.Title != "Deep" {
		t.Fatalf("重启后应从磁盘恢复 peer 索引，got %+v (ok=%v)", info, ok)
	}
}

// R4.30：「找不到会话」的 peer 解析失败应视为瞬态错误（自动续传），
// 而不是一票终态——索引落盘 + 深翻分页后，下一轮自愈大概率能命中。
func TestTransientSourceErrorPeerResolution(t *testing.T) {
	err := errors.New("file_reference 失效：自动刷新消息元数据失败：native peer 元数据缺失且自动解析失败：找不到会话 Channel:2052039292")
	if !transientSourceError(err) {
		t.Fatal("peer 解析失败应按瞬态处理，自动续传")
	}
	// R4.31：DC_ID_INVALID 是授权层不同步，重试常能自愈，不得一票终态。
	if !transientSourceError(errors.New("DC_ID_INVALID: 媒体 DC 授权导出被拒：connect Telegram media DC 5: export auth to 5: rpc error code 400: DC_ID_INVALID")) {
		t.Fatal("DC_ID_INVALID 应按瞬态处理，自动续传")
	}
	if transientSourceError(errors.New("invalid native channel id: parsing \"\"")) {
		t.Fatal("非瞬态错误不应被误判")
	}
}

// --- R4.39：会话列表分页游标守卫 -----------------------------------------
//
// 背景（2.6.16 实测）：messages.getDialogs 的 offset_date 在 TL 里是**无条件字段**
// （gotd 生成代码 EncodeBare 里是裸 `b.PutInt(g.OffsetDate)`，无 flag 守卫），官方
// dialogs.Iterator（telegram/query/dialogs/iter.go:153-164）每页都会带上它。此前只传
// offset_peer + offset_id、offset_date 恒为 0，服务端拿 (date=0, id, peer) 定位不到
// 「上一页末尾」，于是每页都从列表开头重发 → 页间大面积重叠：4 页取回 400 条，
// 按 peer 去重后只剩 102 条，用户看到的群组列表自然永远「不全」。

func channelInfo(id int64, accessHash int64) nativePeerInfo {
	return nativePeerInfo{
		PeerID:     "Channel:" + strconv.FormatInt(id, 10),
		Type:       "channel",
		ID:         strconv.FormatInt(id, 10),
		AccessHash: strconv.FormatInt(accessHash, 10),
		Kind:       "group",
	}
}

func TestNextDialogCursorCarriesIDAndDate(t *testing.T) {
	index := map[string]nativePeerInfo{"Channel:1": channelInfo(1, 1001)}
	dialogs := []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 1}, TopMessage: 777}}
	messages := lastMessageByID([]tg.MessageClass{&tg.Message{ID: 777, Date: 1700000000}})

	cursor := nextDialogCursor(dialogs, index, messages)
	if !cursor.OK {
		t.Fatal("有实体、有消息时应能算出可续翻游标")
	}
	if cursor.ID != 777 {
		t.Fatalf("游标 ID 应为该会话 top_message，实际 %d", cursor.ID)
	}
	if cursor.Date != 1700000000 {
		t.Fatalf("游标必须带上日期（offset_date 是 TL 无条件字段，缺失会让服务端定位不到上一页末尾 → 翻页重叠），实际 %+v", cursor)
	}
	channel, ok := cursor.Peer.(*tg.InputPeerChannel)
	if !ok || channel.ChannelID != 1 {
		t.Fatalf("游标 peer 应为 Channel:1，实际 %#v", cursor.Peer)
	}
}

func TestNextDialogCursorKeepsLastKnownDateWhenTopMessageMissing(t *testing.T) {
	index := map[string]nativePeerInfo{
		"Channel:1": channelInfo(1, 1001),
		"Channel:2": channelInfo(2, 2002),
	}
	dialogs := []tg.DialogClass{
		&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 1}, TopMessage: 100},
		// 最后一个会话的 top message 不在本页 messages 里。
		&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 2}, TopMessage: 200},
	}
	messages := lastMessageByID([]tg.MessageClass{&tg.Message{ID: 100, Date: 1699999999}})

	cursor := nextDialogCursor(dialogs, index, messages)
	if cursor.ID != 100 || cursor.Date != 1699999999 {
		t.Fatalf("消息缺失时应沿用上一个已知的 ID/日期（与官方迭代器同），实际 %+v", cursor)
	}
	channel, ok := cursor.Peer.(*tg.InputPeerChannel)
	if !ok || channel.ChannelID != 2 {
		t.Fatalf("peer 仍应推进到本页最后一个可解析会话（Channel:2），实际 %#v", cursor.Peer)
	}
}

func TestNextDialogCursorStopsWhenNoResolvablePeer(t *testing.T) {
	dialogs := []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 9}, TopMessage: 5}}
	if cursor := nextDialogCursor(dialogs, map[string]nativePeerInfo{}, nil); cursor.OK {
		t.Fatalf("实体缺失 accessHash 时应判定不可续翻（避免盲目空翻），实际 %+v", cursor)
	}
}

func TestNativeDialogTotal(t *testing.T) {
	total, complete, ok := nativeDialogTotal(&tg.MessagesDialogsSlice{
		Count:   850,
		Dialogs: []tg.DialogClass{&tg.Dialog{}},
	})
	if !ok || complete || total != 850 {
		t.Fatalf("dialogsSlice 应给出总数 850 且标记未完成，实际 total=%d complete=%v ok=%v", total, complete, ok)
	}

	total, complete, ok = nativeDialogTotal(&tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{&tg.Dialog{}, &tg.Dialog{}},
	})
	if !ok || !complete || total != 2 {
		t.Fatalf("dialogs（非 slice）表示列表一次给全，实际 total=%d complete=%v ok=%v", total, complete, ok)
	}

	if _, _, ok := nativeDialogTotal(&tg.MessagesDialogsNotModified{}); ok {
		t.Fatal("未修改响应不应被当成有效计数来源")
	}
}

func TestSleepCtxCancelsOnDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("上下文已取消时应立刻返回错误，而不是傻等")
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("时长 <= 0 应直接返回：%v", err)
	}
}

// TestGetDialogsRequestsAlwaysCarryCursorFields 是「游标字段完整性」守卫。
// 上面那个 bug 无法在单测里用真实 RPC 复现（它依赖 Telegram 服务端的定位行为），
// 所以守卫直接锚在请求字面量上：任何一处 messages.getDialogs 请求构造都必须显式
// 给出 OffsetPeer / OffsetID / OffsetDate 三个字段。
//
// 负例核证（判据必须能红）：
//
//	cp chatapi.go /tmp/broken.go && （删掉其中一行 OffsetDate: offsetDate）
//	FG_DIALOGS_SOURCE=/tmp/broken.go go test -run TestGetDialogsRequestsAlwaysCarryCursorFields
func TestGetDialogsRequestsAlwaysCarryCursorFields(t *testing.T) {
	path := os.Getenv("FG_DIALOGS_SOURCE")
	if path == "" {
		path = "chatapi.go"
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}

	blocks := dialogRequestLiterals(source)
	if len(blocks) < 2 {
		t.Fatalf("只找到 %d 处 messages.getDialogs 请求构造，预期至少 2 处（会话列表分页 + peer 索引深翻）——守卫失效会静默放过整类游标 bug", len(blocks))
	}
	for i, block := range blocks {
		for _, field := range []string{"OffsetPeer:", "OffsetID:", "OffsetDate:"} {
			if !bytes.Contains(block, []byte(field)) {
				t.Errorf("第 %d 处 messages.getDialogs 请求缺少 %s（offset_date 是 TL 无条件字段，漏传会导致翻页重叠）：\n%s", i+1, field, block)
			}
		}
	}
}

// dialogRequestLiterals 从源码里取出每个 MessagesGetDialogsRequest{...} 字面量：
// 从 `{` 起按花括号配平扫描，跳过字符串字面量与行注释（避免注释/文案里的括号干扰）。
func dialogRequestLiterals(source []byte) [][]byte {
	const marker = "MessagesGetDialogsRequest{"
	var out [][]byte
	for cursor := 0; cursor < len(source); {
		offset := bytes.Index(source[cursor:], []byte(marker))
		if offset < 0 {
			break
		}
		open := cursor + offset + len(marker) - 1
		depth := 0
		inString := false
		inComment := false
		end := -1
		for i := open; i < len(source); i++ {
			current := source[i]
			if inComment {
				if current == '\n' {
					inComment = false
				}
				continue
			}
			if inString {
				if current == '\\' {
					i++
					continue
				}
				if current == '"' {
					inString = false
				}
				continue
			}
			if current == '/' && i+1 < len(source) && source[i+1] == '/' {
				inComment = true
				i++
				continue
			}
			if current == '"' {
				inString = true
				continue
			}
			switch current {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
			if end > 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, source[open:end])
		cursor = end
	}
	return out
}
