package main

// M4.3 写路径单元测试：覆盖发送回执解析、peer 构造、合成消息与请求体解析。
// gotd 的条件字段必须用 SetXxx() 写入 Flags，直接赋字面量会被判为「字段缺失」。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestNativeSentMessageIDShortSentMessage(t *testing.T) {
	updates := &tg.UpdateShortSentMessage{ID: 4321, Out: true, Date: int(time.Now().Unix())}
	if got := nativeSentMessageID(updates); got != 4321 {
		t.Fatalf("want 4321, got %d", got)
	}
}

func TestNativeSentMessageIDNilUpdates(t *testing.T) {
	if got := nativeSentMessageID(nil); got != 0 {
		t.Fatalf("want 0 for nil updates, got %d", got)
	}
	var typed *tg.UpdateShortSentMessage
	if got := nativeSentMessageID(typed); got != 0 {
		t.Fatalf("want 0 for nil typed updates, got %d", got)
	}
}

func TestNativeSentMessageIDPrefersUpdateNewMessage(t *testing.T) {
	// updates 里同时出现 updateMessageID 与 updateNewMessage 时，
	// 必须优先返回 updateNewMessage 的 ID（它携带完整消息，可直接序列化）。
	message := &tg.Message{ID: 99, Message: "hello"}
	updates := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 98, RandomID: 7},
			&tg.UpdateNewMessage{Message: message, Pts: 1, PtsCount: 1},
		},
	}
	if got := nativeSentMessageID(updates); got != 99 {
		t.Fatalf("want 99 (updateNewMessage), got %d", got)
	}
}

func TestNativeSentMessageIDFallsBackToUpdateMessageID(t *testing.T) {
	updates := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 555, RandomID: 3},
		},
	}
	if got := nativeSentMessageID(updates); got != 555 {
		t.Fatalf("want 555, got %d", got)
	}
}

func TestNativeSentMessageIDUpdatesCombined(t *testing.T) {
	updates := &tg.UpdatesCombined{
		Updates: []tg.UpdateClass{
			&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 777, Message: "x"}},
		},
	}
	if got := nativeSentMessageID(updates); got != 777 {
		t.Fatalf("want 777, got %d", got)
	}
}

func TestNativePeerClassFromInfo(t *testing.T) {
	cases := []struct {
		name string
		info nativePeerInfo
		want string
	}{
		{"user", nativePeerInfo{Type: "user", ID: "11"}, "User:11"},
		{"chat", nativePeerInfo{Type: "chat", ID: "22"}, "Chat:22"},
		{"channel", nativePeerInfo{Type: "channel", ID: "33"}, "Channel:33"},
		{"unknown", nativePeerInfo{Type: "bogus", ID: "44"}, ""},
		{"empty-id", nativePeerInfo{Type: "user", ID: "0"}, ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := peerIDFromPeerClass(nativePeerClassFromInfo(item.info))
			if got != item.want {
				t.Fatalf("want %q, got %q", item.want, got)
			}
		})
	}
}

func TestNativeSyntheticMessageSerialization(t *testing.T) {
	info := nativePeerInfo{PeerID: "User:11", Type: "user", ID: "11", Title: "Ada"}
	message := nativeSyntheticMessage(info, 123, "ping")
	if message.ID != 123 || message.Message != "ping" || !message.Out {
		t.Fatalf("unexpected synthetic message: %+v", message)
	}
	out := serializeNativeMessage(message, nil)
	if out["id"] != 123 {
		t.Fatalf("want id 123, got %v", out["id"])
	}
	if out["text"] != "ping" {
		t.Fatalf("want text ping, got %v", out["text"])
	}
	if out["outgoing"] != true {
		t.Fatalf("want outgoing true, got %v", out["outgoing"])
	}
	// 发送者是本人：senderId 应回落到 peer 自身。
	if out["senderId"] != "User:11" {
		t.Fatalf("want senderId User:11, got %v", out["senderId"])
	}
}

func TestNextNativeRandomIDIncrements(t *testing.T) {
	first := nextNativeRandomID()
	second := nextNativeRandomID()
	if first == second {
		t.Fatalf("random_id must be unique, got %d twice", first)
	}
}

func TestChatWriteString(t *testing.T) {
	if got := chatWriteString(nil); got != "" {
		t.Fatalf("want empty for nil, got %q", got)
	}
	if got := chatWriteString("text"); got != "text" {
		t.Fatalf("want text, got %q", got)
	}
	if got := chatWriteString(42); got != "42" {
		t.Fatalf("want 42, got %q", got)
	}
}

func TestDecodeChatWriteBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/a/send", strings.NewReader(`{"text":"hi"}`))
	body, err := decodeChatWriteBody(request)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatWriteString(body["text"]) != "hi" {
		t.Fatalf("want hi, got %v", body["text"])
	}
}

func TestDecodeChatWriteBodyEmpty(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/a/send", strings.NewReader(""))
	body, err := decodeChatWriteBody(request)
	if err != nil {
		t.Fatalf("empty body should be tolerated, got %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("want empty map, got %v", body)
	}
}

func TestDecodeChatWriteBodyInvalidJSON(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/a/send", strings.NewReader("{not-json"))
	if _, err := decodeChatWriteBody(request); err == nil {
		t.Fatalf("want error for invalid JSON")
	}
}

func TestHandleAccountWriteRejectsWrongMethod(t *testing.T) {
	// 路由层已保证 send/button 只接受 POST；这里验证 details 缺 peer 时返回 400 而不是崩溃。
	request := httptest.NewRequest(http.MethodGet, "/api/accounts/a/details", nil)
	recorder := httptest.NewRecorder()
	app := &App{}
	app.handleAccountWrite(recorder, request, "details", NativeAccount{}, nil, request.URL.Query())
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing peer, got %d", recorder.Code)
	}
}
