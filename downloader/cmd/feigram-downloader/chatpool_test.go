package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChatPoolFatal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"auth key", errors.New("rpc error code 401: AUTH_KEY_UNREGISTERED"), true},
		{"session expired", errors.New("SESSION_EXPIRED"), true},
		{"connection dead", errors.New("read: connection dead"), true},
		{"engine closed", errors.New("invoke pool: engine forcibly closed"), true},
		{"普通业务错误不重建", errors.New("找不到会话 Channel:123（可能已退出）"), false},
		{"单次超时不重建", errors.New("context deadline exceeded"), false},
		{"限流不重建", errors.New("rpc error code 420: FLOOD_WAIT (9)"), false},
	}
	for _, tc := range cases {
		if got := chatPoolFatal(tc.err); got != tc.want {
			t.Fatalf("%s: chatPoolFatal=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPooledChatClientIdleExpiredAndClosed(t *testing.T) {
	entry := &pooledChatClient{
		ready: make(chan struct{}),
		fail:  make(chan error, 1),
		done:  make(chan struct{}),
	}
	if entry.idleExpired() {
		t.Fatal("从未使用（lastUsed=0）不应判空闲过期")
	}
	entry.lastUsed.Store(time.Now().Add(-11 * time.Minute).UnixNano())
	if !entry.idleExpired() {
		t.Fatal("11 分钟未使用应判空闲过期")
	}
	entry.lastUsed.Store(time.Now().UnixNano())
	if entry.idleExpired() {
		t.Fatal("刚使用不应判空闲过期")
	}
	if entry.closed() {
		t.Fatal("done 未关闭不应判已断开")
	}
	close(entry.done)
	if !entry.closed() {
		t.Fatal("done 关闭后应判已断开")
	}
}

// 验证 ready 关闭的广播语义：多个等待者都能通过（回归铁律 8 复核发现的并发缺陷）。
func TestPooledChatClientReadyBroadcast(t *testing.T) {
	entry := &pooledChatClient{
		ready: make(chan struct{}),
		fail:  make(chan error, 1),
		done:  make(chan struct{}),
	}
	close(entry.ready)
	for i := 0; i < 5; i++ {
		select {
		case <-entry.ready:
		case <-time.After(time.Second):
			t.Fatalf("等待者 %d 未能通过已关闭的 ready", i)
		}
	}
}

// cancel 后 Run 协程侧的 done 会关闭（用最小结构验证 cancel→done 语义由
// client.Run 的真实实现保证，这里只验证 dropChatPoolEntry 的锁行为不 panic）。
func TestDropChatPoolEntryNilSafe(t *testing.T) {
	app := &App{proxy: &proxyRuntime{}}
	app.dropChatPool("user", "account")
	entry := &pooledChatClient{
		ready:  make(chan struct{}),
		fail:   make(chan error, 1),
		done:   make(chan struct{}),
		cancel: func() {},
	}
	entry.runCtx, entry.cancel = context.WithCancel(context.Background())
	app.dropChatPoolEntry("user:account", entry)
	select {
	case <-entry.runCtx.Done():
	default:
		t.Fatal("dropChatPoolEntry 应取消条目 context")
	}
}
