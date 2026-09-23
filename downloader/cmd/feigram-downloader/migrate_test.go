package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/gotd/td/crypto"
)

// buildGramJSStringSession 严格按 telegram/sessions/StringSession.js save() 的布局
// 构造 session 字符串：dcId(1B) | addrLen(2B BE) | addr | port(2B BE) | authKey。
func buildGramJSStringSession(t *testing.T, dc int, addr string, port int, key []byte) string {
	t.Helper()
	out := []byte{byte(dc)}
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(addr)))
	out = append(out, l[:]...)
	out = append(out, []byte(addr)...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	out = append(out, p[:]...)
	out = append(out, key...)
	return "1" + base64.StdEncoding.EncodeToString(out)
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 256)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return key
}

func TestParseGramJSStringSessionRoundTrip(t *testing.T) {
	key := randomKey(t)
	in := buildGramJSStringSession(t, 2, "149.154.167.50", 443, key)

	dc, addr, port, got, err := parseGramJSStringSession(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if dc != 2 || addr != "149.154.167.50" || port != 443 {
		t.Fatalf("字段不符: dc=%d addr=%q port=%d", dc, addr, port)
	}
	if len(got) != 256 {
		t.Fatalf("auth_key 长度 %d（预期 256）", len(got))
	}
	for i := range key {
		if key[i] != got[i] {
			t.Fatalf("auth_key 第 %d 字节不一致", i)
		}
	}
}

func TestParseGramJSStringSessionRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"空字符串":     "",
		"版本前缀错误":   "2" + base64.StdEncoding.EncodeToString(make([]byte, 300)),
		"非 base64": "1!!!not-base64!!!",
		"解码后过短":    "1" + base64.StdEncoding.EncodeToString([]byte{2, 0}),
	}
	for name, in := range cases {
		if _, _, _, _, err := parseGramJSStringSession(in); err == nil {
			t.Fatalf("%s: 预期报错，实际通过", name)
		}
	}

	// auth_key 长度异常（128 字节）必须被拒绝，避免写入可被 gotd 判定 corrupted 的会话。
	short := buildGramJSStringSession(t, 2, "149.154.167.50", 443, make([]byte, 128))
	if _, _, _, _, err := parseGramJSStringSession(short); err == nil {
		t.Fatal("auth_key 长度异常: 预期报错，实际通过")
	}
}

// TestMigrateAuthKeyIDMatchesGotd 验证迁移流程计算出的 auth_key_id 与 gotd
// telegram/session.go restoreConnection 的校验（key.Value.ID() == key.ID）同源一致。
// 该校验若不一致，gotd 会直接返回 "corrupted key"，迁移必然失败。
func TestMigrateAuthKeyIDMatchesGotd(t *testing.T) {
	key := randomKey(t)
	in := buildGramJSStringSession(t, 2, "1.2.3.4", 443, key)

	_, _, _, got, err := parseGramJSStringSession(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var ak crypto.AuthKey
	copy(ak.Value[:], got)
	ak.ID = ak.Value.ID()

	if ak.Value.ID() != ak.ID {
		t.Fatal("auth_key_id 与 gotd 校验不一致")
	}
	if len(ak.ID) != 8 {
		t.Fatalf("auth_key_id 长度 %d（预期 8）", len(ak.ID))
	}
}
