package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeTransportDefaultsToNative 锁定 M3.1 语义：
// 传输层默认走 Go 原生 MTProto，只有显式指定 http-bridge（或其别名）时才降级到 Node 媒体桥。
// 空值/未知值一律按 native 处理，避免旧配置残留把用户钉死在 http-bridge 上。
func TestNormalizeTransportDefaultsToNative(t *testing.T) {
	cases := map[string]string{
		"":               "native-mtproto",
		"   ":            "native-mtproto",
		"unknown-mode":   "native-mtproto",
		"native-mtproto": "native-mtproto",
		"go-mtproto":     "native-mtproto",
		"Native-MTProto": "native-mtproto",
		"gotd":           "native-mtproto",
		"http-bridge":    "http-bridge",
		"HTTP-BRIDGE":    "http-bridge",
		"bridge":         "http-bridge",
	}
	for in, want := range cases {
		if got := normalizeTransport(in); got != want {
			t.Fatalf("normalizeTransport(%q) = %q，预期 %q", in, got, want)
		}
	}
}

// TestSanitizeConfigKeepsTransportDefault 验证配置清洗后传输层仍为 native，
// 即默认链路（Config 默认值 + sanitizeConfig）确实产出 native-mtproto。
func TestSanitizeConfigKeepsTransportDefault(t *testing.T) {
	cfg := sanitizeConfig(Config{})
	if cfg.Transport != "native-mtproto" {
		t.Fatalf("sanitizeConfig 默认 transport = %q，预期 native-mtproto", cfg.Transport)
	}
	if got := sanitizeConfig(Config{Transport: "http-bridge"}).Transport; got != "http-bridge" {
		t.Fatalf("显式降级失效：得到 %q，预期 http-bridge", got)
	}
}

// TestTaskTransportUpgradesBlobSelfLoop 锁定 R4.23 语义：
// M4.1 删除 Node 侧媒体桥后，http-bridge 的唯一现实来源是 Go blob 自环源
// （goBlobSourceUrl → /api/accounts/{id}/blob）。带自环源的任务必须升级回
// native-mtproto，否则坏账号原因被二次包装成 404 终态，账号恢复也拉不起来。
func TestTaskTransportUpgradesBlobSelfLoop(t *testing.T) {
	app := &App{config: Config{Transport: "http-bridge"}}
	blobTask := Task{
		ID: "t-blob", UserID: "u1", AccountID: "a1", Transport: "http-bridge",
		SourceURL: "http://127.0.0.1:3090/api/accounts/a1/blob?userId=u1&peer=-100123&messageId=7",
	}
	if got := app.taskTransport(blobTask); got != "native-mtproto" {
		t.Fatalf("blob 自环源任务应升级 native，得到 %q", got)
	}
	// 空任务（继承全局配置）也按自环防护后的语义走：全局 http-bridge + 无源保持 http-bridge，
	// 但 blob 源必升级。
	plainTask := Task{ID: "t-plain", UserID: "u1", AccountID: "a1", Transport: "http-bridge", SourceURL: "https://mirror.example.com/media/file.bin"}
	if got := app.taskTransport(plainTask); got != "http-bridge" {
		t.Fatalf("外部真实源的 http-bridge 任务应保持 %q，得到 %q", "http-bridge", got)
	}
	emptyTask := Task{ID: "t-empty", UserID: "u1", AccountID: "a1"}
	if got := app.taskTransport(emptyTask); got != "http-bridge" {
		t.Fatalf("无源任务应继承全局配置 http-bridge，得到 %q", got)
	}
	// 全局配置是 native 时（2.6.1 起的常态），blob 源任务自然也是 native。
	nativeApp := &App{config: Config{Transport: "native-mtproto"}}
	if got := nativeApp.taskTransport(blobTask); got != "native-mtproto" {
		t.Fatalf("native 全局配置下 blob 任务应 native，得到 %q", got)
	}
}

// TestDownloadHTTPBridgeNotReadyIsTransient 锁定 R4.23 语义：
// http-bridge 拉源遇 404 且 body 含「尚未就绪」时，必须归类为 errAccountNotReady
// （瞬态、账号恢复自动续传），而不是终态的 "source returned 404"。
func TestDownloadHTTPBridgeNotReadyIsTransient(t *testing.T) {
	var status int
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	newApp := func() *App {
		return &App{client: &http.Client{}, proxy: &proxyRuntime{}}
	}
	dir := t.TempDir()

	// 场景一：blob 未就绪 404 → 瞬态。
	status = http.StatusNotFound
	body = `{"error":"Go 原生账号 account_x 尚未就绪（failed），请先完成登录或健康检查：context deadline exceeded"}`
	app := newApp()
	// 注意用非 blob URL：blob 源会先被 taskTransport 升级拦截，走不到 HTTP 分支。
	task := &Task{
		ID: "t-1", UserID: "u1", AccountID: "a1", Transport: "http-bridge",
		FilePath: dir + "/out.bin", SourceURL: server.URL + "/media/file.bin",
	}
	err := app.download(task, nil)
	if !errors.Is(err, errAccountNotReady) {
		t.Fatalf("blob 未就绪 404 应归类 errAccountNotReady，得到 %v", err)
	}
	if !transientSourceError(err) {
		t.Fatal("errAccountNotReady 应被视为瞬态错误")
	}

	// 场景二：普通 404（非账号原因）→ 仍是终态。
	status = http.StatusNotFound
	body = `{"error":"message not found"}`
	app2 := newApp()
	task2 := &Task{
		ID: "t-2", UserID: "u1", AccountID: "a1", Transport: "http-bridge",
		FilePath: dir + "/out2.bin", SourceURL: server.URL + "/media/file.bin",
	}
	err2 := app2.download(task2, nil)
	if errors.Is(err2, errAccountNotReady) {
		t.Fatal("普通 404 不应被误判为账号未就绪")
	}
	if transientSourceError(err2) {
		t.Fatal("普通 404 应保持终态语义")
	}
	if !strings.Contains(err2.Error(), "source returned 404") {
		t.Fatalf("普通 404 错误文案应保留原始形态，得到 %v", err2)
	}
}
