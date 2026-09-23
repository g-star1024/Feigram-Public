package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPrimaryDCAddr(t *testing.T) {
	cases := []struct {
		dc   int
		want string
	}{
		{2, "149.154.167.51:443"}, // 静态地址优先
		{5, "91.108.56.173:443"},  // 用户实测主 DC（2.5.2 日志）
		{1, "149.154.175.53:443"}, // 静态地址优先
		{0, ""},                   // 未知/未解析 → 空，调用方沿用原行为
		{99, ""},                  // 不存在的 DC
	}
	for _, tc := range cases {
		if got := primaryDCAddr(tc.dc); got != tc.want {
			t.Errorf("primaryDCAddr(%d) = %q, want %q", tc.dc, got, tc.want)
		}
	}
}

func TestHealthStageDiagnosis(t *testing.T) {
	deadlineErr := errors.New("rpc error: context deadline exceeded")
	rpcErr := errors.New("rpc error code 400: DC_ID_INVALID")
	probeFail := errors.New("dial tcp: i/o timeout")

	t.Run("探测失败→出口链路问题", func(t *testing.T) {
		got := healthStageDiagnosis(5, "91.108.56.173:443", false, 10*time.Second, probeFail, deadlineErr, true)
		if !strings.Contains(got, "TCP 拨号 Telegram DC 5（91.108.56.173:443）即失败") {
			t.Fatalf("应指出 TCP 拨号阶段失败，got %q", got)
		}
		if !strings.Contains(got, "经代理") || strings.Contains(got, "MTProto 握手/授权阶段") {
			t.Fatalf("应归因出口链路且不误指 MTProto 层，got %q", got)
		}
	})

	t.Run("探测成功但Run超时→节点转发质量", func(t *testing.T) {
		got := healthStageDiagnosis(5, "91.108.56.173:443", true, 300*time.Millisecond, nil, deadlineErr, true)
		if !strings.Contains(got, "已能 TCP 连通 Telegram DC 5") {
			t.Fatalf("应指出 TCP 层已通，got %q", got)
		}
		if !strings.Contains(got, "MTProto 握手/授权阶段") || !strings.Contains(got, "更换节点") {
			t.Fatalf("应归因 MTProto 层并建议换节点，got %q", got)
		}
	})

	t.Run("探测成功且非超时错误→不给诊断（沿用原错误）", func(t *testing.T) {
		if got := healthStageDiagnosis(5, "91.108.56.173:443", true, time.Second, nil, rpcErr, true); got != "" {
			t.Fatalf("非超时失败不应套分级诊断，got %q", got)
		}
	})

	t.Run("未知DC→不给诊断", func(t *testing.T) {
		if got := healthStageDiagnosis(0, "", false, time.Second, probeFail, deadlineErr, true); got != "" {
			t.Fatalf("未知 DC 应返回空串，got %q", got)
		}
	})
}

// 已知坑：构造 App 必须显式给 proxy: &proxyRuntime{}，否则零值指针 nil panic。
func TestProbeTelegramTCPRefused(t *testing.T) {
	app := &App{proxy: &proxyRuntime{}}
	// 先起再关 listener，拿到一个确定无人监听的端口 → connection refused。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("本机无法监听临时端口：%v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	err = app.probeTelegramTCP(addr, 3*time.Second)
	if err == nil {
		t.Fatalf("对已关闭端口探测应失败，实际成功")
	}
}

func TestProbeTelegramTCPListening(t *testing.T) {
	app := &App{proxy: &proxyRuntime{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("本机无法监听临时端口：%v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if err := app.probeTelegramTCP(listener.Addr().String(), 3*time.Second); err != nil {
		t.Fatalf("对在监听端口探测应成功：%v", err)
	}
}

// 保证 dialer 路径也走得通：挂一个假代理拨号器，探测应经它转发。
func TestProbeTelegramTCPWithDialer(t *testing.T) {
	var gotAddr string
	app := &App{proxy: &proxyRuntime{}}
	app.proxy.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		gotAddr = addr
		return net.Dial(network, "127.0.0.1:1") // 拨号器自身失败也应如实透出
	}
	if err := app.probeTelegramTCP("91.108.56.173:443", 2*time.Second); err == nil {
		t.Fatalf("经失败拨号器探测应报错")
	}
	if gotAddr != "91.108.56.173:443" {
		t.Fatalf("拨号器应收到目标地址 91.108.56.173:443，got %q", gotAddr)
	}
}
