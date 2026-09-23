package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R4.16：failed 账号的「尚未就绪」错误必须附带健康检查落库的真实原因，
// 否则用户只看到状态词，无从判断代理是否配对、DC 是否超时。
func TestNotReadyAccountErrorIncludesReason(t *testing.T) {
	failed := &NativeAccount{UserID: "u", AccountID: "a", Session: "s", Ready: false, Status: "failed", Error: "dial tcp: i/o timeout —— 请检查代理设置"}
	err := notReadyAccountError("a", failed)
	if !strings.Contains(err.Error(), "dial tcp: i/o timeout") {
		t.Fatalf("failed 账号错误应附带真实原因，got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "尚未就绪（failed）") {
		t.Fatalf("错误应保留状态词，got %q", err.Error())
	}

	// 非 failed 状态（如 qr-waiting）不附带可能误导的旧错误文案。
	waiting := &NativeAccount{UserID: "u", AccountID: "b", Session: "", Ready: false, Status: "qr-waiting", Error: "上一次的失败原因"}
	err = notReadyAccountError("b", waiting)
	if strings.Contains(err.Error(), "上一次的失败原因") {
		t.Fatalf("非 failed 状态不应附带历史错误，got %q", err.Error())
	}

	// failed 但 Error 为空时不应出现孤立的冒号。
	blank := &NativeAccount{UserID: "u", AccountID: "c", Session: "s", Ready: false, Status: "failed", Error: ""}
	err = notReadyAccountError("c", blank)
	if strings.HasSuffix(err.Error(), "：") {
		t.Fatalf("Error 为空时不应留下孤立冒号，got %q", err.Error())
	}
}

// resolveChatAccountLocked 对 failed 账号返回的错误应透出健康检查的真实原因。
func TestResolveChatAccountLockedSurfacesReason(t *testing.T) {
	app := &App{
		proxy:  &proxyRuntime{},
		native: map[string]*NativeAccount{},
	}
	app.native[nativeAccountKey("u", "a")] = &NativeAccount{
		UserID: "u", AccountID: "a", Session: "s", Ready: false,
		Status: "failed", Error: "context deadline exceeded —— 代理可能无法连通 Telegram",
	}
	app.mu.Lock()
	_, err := app.resolveChatAccountLocked("u", "a")
	app.mu.Unlock()
	if err == nil {
		t.Fatal("failed 账号应返回错误")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("错误应透出健康检查真实原因，got %q", err.Error())
	}
}

// R4.17：DC_ID_INVALID 属媒体 DC 层错误，应映射为「账号可用但抽样失败」的可读文案，
// 而不是裸 RPC 错误让用户误判为账号失效。
func TestClassifyNativeReadErrorDCIDInvalid(t *testing.T) {
	classified := classifyNativeReadError(errors.New("rpc error code 400: DC_ID_INVALID"))
	msg := classified.Error()
	if !strings.Contains(msg, "DC_ID_INVALID") || !strings.Contains(msg, "账号本身可用") {
		t.Fatalf("DC_ID_INVALID 应映射为可读提示，got %q", msg)
	}
	// 其他错误不受影响。
	if got := classifyNativeReadError(errors.New("connection refused")).Error(); strings.Contains(got, "账号本身可用") {
		t.Fatalf("普通网络错误不应命中 DC_ID_INVALID 提示，got %q", got)
	}
}

// R4.18：nativePrimaryDC 应从落库的加密 session 中解析主 DC；
// 无会话/解析失败时返回 0（未知），调用方据此保持既有 MediaOnly 行为。
func TestNativePrimaryDC(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "native-secret")
	if err := os.WriteFile(secretFile, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEIGRAM_DOWNLOADER_SECRET_FILE", secretFile)

	app := &App{proxy: &proxyRuntime{}, native: map[string]*NativeAccount{}}

	// 无会话 → 0。
	if got := app.nativePrimaryDC("u", "none"); got != 0 {
		t.Fatalf("无会话应返回 0，got %d", got)
	}

	// 加密落库 {"DC":5} → 解析为 5。
	raw := []byte(`{"DC":5,"Addr":"149.154.171.5:443","AuthKey":"aGk=","AuthKeyID":"aGk=","Salt":1}`)
	encoded, err := app.encryptNativeSession(raw)
	if err != nil {
		t.Fatalf("encryptNativeSession: %v", err)
	}
	// 注意：nativePrimaryDC 内部自持 a.mu，调用方不得再持锁（sync.Mutex 非重入，
	// 否则本用例直接死锁）。
	app.native[nativeAccountKey("u", "a")] = &NativeAccount{UserID: "u", AccountID: "a", Session: encoded}
	if got := app.nativePrimaryDC("u", "a"); got != 5 {
		t.Fatalf("应解析出主 DC 5，got %d", got)
	}

	// 另一个账号主 DC 为 4：调用方据此判定「媒体 DC 是否等于主 DC」。
	raw4 := []byte(`{"DC":4,"Addr":"149.154.167.51:443","AuthKey":"aGk=","AuthKeyID":"aGk=","Salt":2}`)
	encoded4, err := app.encryptNativeSession(raw4)
	if err != nil {
		t.Fatalf("encryptNativeSession: %v", err)
	}
	app.native[nativeAccountKey("u", "b")] = &NativeAccount{UserID: "u", AccountID: "b", Session: encoded4}
	if got := app.nativePrimaryDC("u", "b"); got != 4 {
		t.Fatalf("应解析出主 DC 4，got %d", got)
	}

	// 会话损坏 → 0（未知），调用方据此保持既有 MediaOnly 行为。
	app.native[nativeAccountKey("u", "c")] = &NativeAccount{UserID: "u", AccountID: "c", Session: "garbage"}
	if got := app.nativePrimaryDC("u", "c"); got != 0 {
		t.Fatalf("损坏 session 应返回 0，got %d", got)
	}
}
