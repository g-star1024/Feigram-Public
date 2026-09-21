package main

import "testing"

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
