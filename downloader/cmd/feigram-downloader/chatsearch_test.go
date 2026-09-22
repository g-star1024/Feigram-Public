package main

import "testing"

func TestNativeUsernameFromLink(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"at-prefix", "@durov", "durov"},
		{"plain-username", "durov", "durov"},
		{"t.me-with-scheme", "https://t.me/durov", "durov"},
		{"t.me-schemeless", "t.me/durov", "durov"},
		{"t.me-telegram-me", "https://telegram.me/durov", "durov"},
		{"tg-resolve", "tg://resolve?domain=durov", "durov"},
		{"private-channel", "https://t.me/c/123456/7", ""},
		{"joinchat", "https://t.me/joinchat/AAAA", ""},
		{"plus-invite", "https://t.me/+abcdef", ""},
		{"other-host", "https://example.com/durov", ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := nativeUsernameFromLink(item.in); got != item.want {
				t.Fatalf("input %q: want %q, got %q", item.in, item.want, got)
			}
		})
	}
}

func TestNativeUsernameFromLinkWithMessage(t *testing.T) {
	if got := nativeUsernameFromLink("https://t.me/durov/42"); got != "durov" {
		t.Fatalf("want durov, got %q", got)
	}
}
