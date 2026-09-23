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
// R4.30：重大修正——gotd session.Loader 落库的是嵌套包装
// {"Version":1,"Data":{"DC":N,...}}，此前测试用手工平铺 {"DC":N} 构造夹具，
// 测试绿而生产恒解析 0（健康探测全跳过、主 DC 短路失效 → DC_ID_INVALID）。
// 现在夹具改用 gotd 真实嵌套形状，平铺仅作历史兼容保留。
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

	// gotd 真实嵌套形状 {"Version":1,"Data":{"DC":5,...}} → 解析为 5。
	raw := []byte(`{"Version":1,"Data":{"Config":{"ThisDC":5},"DC":5,"Addr":"149.154.171.5:443","AuthKey":"aGk=","AuthKeyID":"aGk=","Salt":1}}`)
	encoded, err := app.encryptNativeSession(raw)
	if err != nil {
		t.Fatalf("encryptNativeSession: %v", err)
	}
	// 注意：nativePrimaryDC 内部自持 a.mu，调用方不得再持锁（sync.Mutex 非重入，
	// 否则本用例直接死锁）。
	app.native[nativeAccountKey("u", "a")] = &NativeAccount{UserID: "u", AccountID: "a", Session: encoded}
	if got := app.nativePrimaryDC("u", "a"); got != 5 {
		t.Fatalf("gotd 嵌套 session 应解析出主 DC 5，got %d", got)
	}

	// 另一个账号主 DC 为 4（嵌套形状）：调用方据此判定「媒体 DC 是否等于主 DC」。
	raw4 := []byte(`{"Version":1,"Data":{"Config":{"ThisDC":4},"DC":4,"Addr":"149.154.167.51:443","AuthKey":"aGk=","AuthKeyID":"aGk=","Salt":2}}`)
	encoded4, err := app.encryptNativeSession(raw4)
	if err != nil {
		t.Fatalf("encryptNativeSession: %v", err)
	}
	app.native[nativeAccountKey("u", "b")] = &NativeAccount{UserID: "u", AccountID: "b", Session: encoded4}
	if got := app.nativePrimaryDC("u", "b"); got != 4 {
		t.Fatalf("应解析出主 DC 4，got %d", got)
	}

	// 历史平铺形状（R4.30 之前的测试夹具）仍应兼容解析。
	rawFlat := []byte(`{"DC":5,"Addr":"149.154.171.5:443","AuthKey":"aGk=","AuthKeyID":"aGk=","Salt":1}`)
	encodedFlat, err := app.encryptNativeSession(rawFlat)
	if err != nil {
		t.Fatalf("encryptNativeSession: %v", err)
	}
	app.native[nativeAccountKey("u", "flat")] = &NativeAccount{UserID: "u", AccountID: "flat", Session: encodedFlat}
	if got := app.nativePrimaryDC("u", "flat"); got != 5 {
		t.Fatalf("平铺历史 session 应兼容解析出主 DC 5，got %d", got)
	}

	// 会话损坏 → 0（未知），调用方据此保持既有 MediaOnly 行为。
	app.native[nativeAccountKey("u", "c")] = &NativeAccount{UserID: "u", AccountID: "c", Session: "garbage"}
	if got := app.nativePrimaryDC("u", "c"); got != 0 {
		t.Fatalf("损坏 session 应返回 0，got %d", got)
	}
}

// R4.30：sessionFingerprint 取 AuthKeyID 作为认证指纹。
// 同一 AUTH_KEY 的 session 重复回写（盐值变化）指纹不变；重新登录（新 AUTH_KEY）
// 指纹必变——媒体池据此区分「回写」与「换号」，不再误重建常驻连接。
func TestSessionFingerprint(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "native-secret")
	if err := os.WriteFile(secretFile, []byte(strings.Repeat("cd", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEIGRAM_DOWNLOADER_SECRET_FILE", secretFile)

	app := &App{proxy: &proxyRuntime{}}

	if got := app.sessionFingerprint(""); got != "" {
		t.Fatalf("空 session 指纹应为空，got %q", got)
	}
	if got := app.sessionFingerprint("garbage"); got != "" {
		t.Fatalf("损坏 session 指纹应为空，got %q", got)
	}

	// gotd 嵌套形状：同 key 不同 Salt → 指纹相同。
	write1, err := app.encryptNativeSession([]byte(`{"Version":1,"Data":{"DC":5,"AuthKey":"a2F1dGg=","AuthKeyID":"a2V5aWQx","Salt":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	write2, err := app.encryptNativeSession([]byte(`{"Version":1,"Data":{"DC":5,"AuthKey":"a2F1dGg=","AuthKeyID":"a2V5aWQx","Salt":999}}`))
	if err != nil {
		t.Fatal(err)
	}
	fp1 := app.sessionFingerprint(write1)
	fp2 := app.sessionFingerprint(write2)
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("同一 AUTH_KEY 回写后指纹应不变，got %q vs %q", fp1, fp2)
	}

	// 换 AUTH_KEY（重新登录）→ 指纹必变。
	relogin, err := app.encryptNativeSession([]byte(`{"Version":1,"Data":{"DC":5,"AuthKey":"bmV3a2V5","AuthKeyID":"a2V5aWQy","Salt":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if fp3 := app.sessionFingerprint(relogin); fp3 == fp1 {
		t.Fatalf("重新登录后指纹必须变化，got %q", fp3)
	}

	// 历史平铺形状兼容。
	flat, err := app.encryptNativeSession([]byte(`{"DC":5,"AuthKey":"a2F1dGg=","AuthKeyID":"a2V5aWQx","Salt":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if fp := app.sessionFingerprint(flat); fp != fp1 {
		t.Fatalf("平铺历史 session 指纹应与嵌套形状一致，got %q vs %q", fp, fp1)
	}
}
