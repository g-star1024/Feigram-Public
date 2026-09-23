package main

// M4.2 单元测试：覆盖 peer 解析、消息序列化、文件夹匹配与参数钳制。
// 这些函数是纯函数，可在无 Telegram 凭据的环境下离线验证（铁律 4 的「真实验证」）。

import (
	"errors"
	"testing"

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
	if transientSourceError(errors.New("invalid native channel id: parsing \"\"")) {
		t.Fatal("非瞬态错误不应被误判")
	}
}
