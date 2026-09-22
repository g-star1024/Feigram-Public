package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
)

// 本文件用「进程内代理桩 + 回显服务」真实验证代理 dialer：
// 拨号必须经过桩（桩能报出被请求的目标），且隧道双向可读写。
// 只断言「函数返回 nil error」是不够的——那条隧道是否真的通必须打通一次才算数。

const proxyProbe = "feigram-proxy-probe"

// clearProxyEnv 把全部代理环境变量清成空串。
// 必须逐个清：本机（含 CI/沙箱）常自带 https_proxy，只清大写变量会漏掉小写变体，
// 导致「未配置代理」的断言假阳性/假阴性。
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动回显服务失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// assertTunnelWorks 通过 dialer 建连并做一次「写入-回显」往返。
func assertTunnelWorks(t *testing.T, dial dialFunc, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("经代理拨号失败：%v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(proxyProbe)); err != nil {
		t.Fatalf("写入隧道失败：%v", err)
	}
	buf := make([]byte, len(proxyProbe))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读取隧道回显失败（隧道可能未真正建立）：%v", err)
	}
	if string(buf) != proxyProbe {
		t.Fatalf("隧道回显不匹配：%q", buf)
	}
}

func expectTarget(t *testing.T, targets <-chan string, want string) {
	t.Helper()
	select {
	case got := <-targets:
		if got != want {
			t.Fatalf("代理收到的目标是 %s，期望 %s", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("代理未收到任何目标请求，说明流量没走代理")
	}
}

// --- SOCKS5 桩 ---

type socks5Stub struct {
	addr     string
	targets  chan string
	user     string
	password string
}

func startSOCKS5Stub(t *testing.T, user, password string, requireAuth bool) *socks5Stub {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 SOCKS5 桩失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stub := &socks5Stub{addr: listener.Addr().String(), targets: make(chan string, 16), user: user, password: password}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go stub.serve(conn, requireAuth)
		}
	}()
	return stub
}

func (s *socks5Stub) serve(conn net.Conn, requireAuth bool) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return
	}
	if header[0] != 0x05 {
		return
	}
	if _, err := io.ReadFull(reader, make([]byte, int(header[1]))); err != nil {
		return
	}

	if requireAuth {
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		if !s.authenticate(conn, reader) {
			return
		}
	} else if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(reader, request); err != nil {
		return
	}
	if request[1] != 0x01 {
		_, _ = conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	host, err := readSOCKSAddr(reader, request[3])
	if err != nil {
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(portBytes)))
	s.targets <- target

	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	splice(conn, reader, upstream)
}

func (s *socks5Stub) authenticate(conn net.Conn, reader *bufio.Reader) bool {
	head := make([]byte, 2)
	if _, err := io.ReadFull(reader, head); err != nil {
		return false
	}
	user := make([]byte, int(head[1]))
	if _, err := io.ReadFull(reader, user); err != nil {
		return false
	}
	passwordLen := make([]byte, 1)
	if _, err := io.ReadFull(reader, passwordLen); err != nil {
		return false
	}
	password := make([]byte, int(passwordLen[0]))
	if _, err := io.ReadFull(reader, password); err != nil {
		return false
	}
	if string(user) != s.user || string(password) != s.password {
		_, _ = conn.Write([]byte{0x01, 0x01})
		return false
	}
	_, err := conn.Write([]byte{0x01, 0x00})
	return err == nil
}

func readSOCKSAddr(reader *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", err
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(reader, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	default:
		return "", fmt.Errorf("未知的地址类型 %d", atyp)
	}
}

// --- HTTP CONNECT 桩 ---

type httpProxyStub struct {
	addr    string
	targets chan string
	reject  bool
}

func startHTTPProxyStub(t *testing.T, reject bool) *httpProxyStub {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 HTTP 代理桩失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stub := &httpProxyStub{addr: listener.Addr().String(), targets: make(chan string, 16), reject: reject}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go stub.serve(conn)
		}
	}()
	return stub
}

func (s *httpProxyStub) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.EqualFold(fields[0], "CONNECT") {
		_, _ = conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}
	target := fields[1]
	for {
		headerLine, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(headerLine) == "" {
			break
		}
	}
	s.targets <- target
	if s.reject {
		_, _ = conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	splice(conn, reader, upstream)
}

// splice 双向搬运；若 bufio 已预读进隧道数据，先把它接回连接，
// 否则首个字节会被吞掉（这正是 bufferedConn 要解决的场景）。
func splice(conn net.Conn, reader *bufio.Reader, upstream net.Conn) {
	client := conn
	if reader.Buffered() > 0 {
		client = &bufferedConn{Conn: conn, reader: reader}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

// --- 测试用例 ---

func TestNormalizeProxyURLAcceptsSupportedSchemes(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string
	}{
		{"省略 scheme 按 socks5 处理", "127.0.0.1:7890", "socks5://127.0.0.1:7890"},
		{"socks5 保留账号密码", "socks5://user:pass@127.0.0.1:7890", "socks5://user:pass@127.0.0.1:7890"},
		{"socks5h", "socks5h://127.0.0.1:1080", "socks5h://127.0.0.1:1080"},
		{"http", "http://proxy.local:3128", "http://proxy.local:3128"},
		{"https", "https://proxy.local:8443", "https://proxy.local:8443"},
		{"大写 scheme 统一小写", "SOCKS5://127.0.0.1:7890", "socks5://127.0.0.1:7890"},
		{"IPv6 字面量", "socks5://[::1]:7890", "socks5://[::1]:7890"},
		{"punycode 域名", "http://xn--r8jz45g.jp:8080", "http://xn--r8jz45g.jp:8080"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := normalizeProxyURL(testCase.input)
			if err != nil {
				t.Fatalf("意外报错：%v", err)
			}
			if got != testCase.expect {
				t.Fatalf("得到 %q，期望 %q", got, testCase.expect)
			}
		})
	}
}

func TestNormalizeProxyURLRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"不支持的协议", "socks4://127.0.0.1:1080"},
		{"缺少端口", "socks5://127.0.0.1"},
		{"缺少主机", "socks5://:7890"},
		{"带路径", "socks5://127.0.0.1:7890/extra"},
		{"主机名是中文（url.Parse 不校验主机，必须自己拦）", "socks5://这不是地址:7890"},
		{"主机名含百分号编码", "socks5://%E8%BF%99:7890"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got, err := normalizeProxyURL(testCase.input); err == nil {
				t.Fatalf("期望报错，实际得到 %q", got)
			}
		})
	}
	if got, err := normalizeProxyURL("   "); err != nil || got != "" {
		t.Fatalf("空白输入应视为未配置，得到 %q / %v", got, err)
	}
}

func TestMaskProxyURLHidesCredentials(t *testing.T) {
	masked := maskProxyURL("socks5://user:supersecret@127.0.0.1:7890")
	if masked != "socks5://127.0.0.1:7890" {
		t.Fatalf("脱敏结果异常：%q", masked)
	}
	if strings.Contains(masked, "supersecret") || strings.Contains(masked, "user") {
		t.Fatalf("脱敏后仍含凭据：%q", masked)
	}
	for _, input := range []string{"", "   ", "绝不是地址", "socks4://127.0.0.1:1080"} {
		if got := maskProxyURL(input); got != "" {
			t.Fatalf("非法输入 %q 应脱敏为空串，得到 %q", input, got)
		}
	}
}

func TestResolveProxyPrefersSettingsThenEnv(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1081")

	raw, source, err := resolveProxy("socks5://127.0.0.1:7890")
	if err != nil || raw != "socks5://127.0.0.1:7890" || source != "settings" {
		t.Fatalf("设置应优先于环境变量，得到 %q / %q / %v", raw, source, err)
	}

	raw, source, err = resolveProxy("")
	if err != nil || raw != "socks5://127.0.0.1:1081" || source != "env:ALL_PROXY" {
		t.Fatalf("应回落到环境变量，得到 %q / %q / %v", raw, source, err)
	}

	clearProxyEnv(t)
	raw, source, err = resolveProxy("")
	if err != nil || raw != "" || source != "none" {
		t.Fatalf("无任何配置应为 none，得到 %q / %q / %v", raw, source, err)
	}
}

func TestResolveProxyUsesDedicatedEnvKeyFirst(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("FEIGRAM_PROXY_URL", "socks5://127.0.0.1:7891")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1081")

	raw, source, err := resolveProxy("")
	if err != nil || raw != "socks5://127.0.0.1:7891" || source != "env:FEIGRAM_PROXY_URL" {
		t.Fatalf("应优先读应用专用变量，得到 %q / %q / %v", raw, source, err)
	}
}

func TestResolveProxyReportsInvalidSettingsInsteadOfSilentFallback(t *testing.T) {
	raw, source, err := resolveProxy("socks4://127.0.0.1:1080")
	if err == nil {
		t.Fatal("非法设置应报错，而不是静默直连")
	}
	if raw != "" || source != "invalid" {
		t.Fatalf("得到 %q / %q", raw, source)
	}
}

func TestSOCKS5DialerTunnelsThroughProxy(t *testing.T) {
	echoAddr := startEchoServer(t)
	stub := startSOCKS5Stub(t, "", "", false)
	dial, err := newProxyDialer("socks5://" + stub.addr)
	if err != nil {
		t.Fatalf("构造 SOCKS5 dialer 失败：%v", err)
	}
	assertTunnelWorks(t, dial, echoAddr)
	expectTarget(t, stub.targets, echoAddr)
}

func TestSOCKS5DialerSendsCredentials(t *testing.T) {
	echoAddr := startEchoServer(t)
	stub := startSOCKS5Stub(t, "feigram", "s3cret", true)
	dial, err := newProxyDialer("socks5://feigram:s3cret@" + stub.addr)
	if err != nil {
		t.Fatalf("构造带凭据的 SOCKS5 dialer 失败：%v", err)
	}
	assertTunnelWorks(t, dial, echoAddr)
}

func TestSOCKS5DialerWrongCredentialsFail(t *testing.T) {
	echoAddr := startEchoServer(t)
	stub := startSOCKS5Stub(t, "feigram", "s3cret", true)
	dial, err := newProxyDialer("socks5://feigram:wrong@" + stub.addr)
	if err != nil {
		t.Fatalf("构造 dialer 失败：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := dial(ctx, "tcp", echoAddr); err == nil {
		conn.Close()
		t.Fatal("错误凭据应当拨号失败")
	}
}

func TestHTTPConnectDialerTunnelsThroughProxy(t *testing.T) {
	echoAddr := startEchoServer(t)
	stub := startHTTPProxyStub(t, false)
	dial, err := newProxyDialer("http://" + stub.addr)
	if err != nil {
		t.Fatalf("构造 HTTP CONNECT dialer 失败：%v", err)
	}
	assertTunnelWorks(t, dial, echoAddr)
	expectTarget(t, stub.targets, echoAddr)
}

func TestHTTPConnectDialerReportsProxyRejection(t *testing.T) {
	echoAddr := startEchoServer(t)
	stub := startHTTPProxyStub(t, true)
	dial, err := newProxyDialer("http://" + stub.addr)
	if err != nil {
		t.Fatalf("构造 dialer 失败：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", echoAddr)
	if err == nil {
		conn.Close()
		t.Fatal("代理返回 407 时应当报错")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("错误信息应带状态码，实际：%v", err)
	}
}

func TestProxyDialerRefusesUnsupportedNetwork(t *testing.T) {
	stub := startSOCKS5Stub(t, "", "", false)
	dial, err := newProxyDialer("socks5://" + stub.addr)
	if err != nil {
		t.Fatalf("构造 dialer 失败：%v", err)
	}
	if _, err := dial(context.Background(), "udp", "127.0.0.1:1"); err == nil {
		t.Fatal("udp 应被拒绝")
	}
}

func TestReadConnectStatusSkipsInterimResponses(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(
		"HTTP/1.1 100 Continue\r\n\r\n" +
			"HTTP/1.1 200 Connection Established\r\nProxy-Agent: stub\r\n\r\n"))
	code, err := readConnectStatus(reader)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if code != 200 {
		t.Fatalf("得到 %d，期望 200", code)
	}
}

func TestReadConnectStatusRejectsGarbage(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("not-a-status-line\r\n\r\n"))
	if _, err := readConnectStatus(reader); err == nil {
		t.Fatal("异常状态行应报错")
	}
}

func TestProxyRuntimeAppliesAndClearsProxy(t *testing.T) {
	clearProxyEnv(t)
	runtime := newProxyRuntime()

	if runtime.dialer() != nil {
		t.Fatal("初始状态不应有代理")
	}
	runtime.apply("socks5://127.0.0.1:7890")
	if runtime.dialer() == nil {
		t.Fatal("设置合法代理后应生成 dialer")
	}
	status := runtime.status()
	if status["enabled"] != true || status["address"] != "socks5://127.0.0.1:7890" {
		t.Fatalf("状态异常：%v", status)
	}

	runtime.apply("socks4://bad")
	if runtime.dialer() != nil {
		t.Fatal("非法配置应清空 dialer")
	}
	status = runtime.status()
	if status["enabled"] != false || status["source"] != "invalid" {
		t.Fatalf("非法配置状态异常：%v", status)
	}
	if _, hasError := status["error"]; !hasError {
		t.Fatalf("非法配置应带 error 说明：%v", status)
	}

	runtime.apply("")
	if runtime.dialer() != nil {
		t.Fatal("清空配置后不应有代理")
	}
}

func TestNetworkHintCoversProxyAndDirectCases(t *testing.T) {
	dialErr := fmt.Errorf("dial tcp 149.154.167.51:443: i/o timeout")
	if hint := networkHintFor(dialErr, false); !strings.Contains(hint, "网络代理") {
		t.Fatalf("未配代理时应提示去设置代理，实际：%q", hint)
	}
	if hint := networkHintFor(dialErr, true); !strings.Contains(hint, "已配置代理") {
		t.Fatalf("已配代理时应提示检查代理，实际：%q", hint)
	}
	if hint := networkHintFor(fmt.Errorf("AUTH_KEY_UNREGISTERED"), false); hint != "" {
		t.Fatalf("非网络类错误不应追加提示，实际：%q", hint)
	}
	if hint := networkHintFor(nil, false); hint != "" {
		t.Fatalf("nil 错误不应追加提示，实际：%q", hint)
	}
}

// 「等待 Telegram 超时」这类失败本身不含网络关键字，会直接调用 proxyHintFor。
// 两条文案都必须把用户送到「设置 → 网络代理」，否则提示等于没说。
func TestProxyHintForAlwaysPointsToProxySettings(t *testing.T) {
	app := &App{proxy: newProxyRuntime()}

	if hint := app.proxyHintFor(); !strings.Contains(hint, "设置 → 网络代理") {
		t.Fatalf("未配代理时的提示应指向网络代理设置，实际：%q", hint)
	}

	runtime := newProxyRuntime()
	if _, reason := runtime.apply("socks5://127.0.0.1:1080"); reason != "" {
		t.Fatalf("测试用代理地址应合法：%s", reason)
	}
	app = &App{proxy: runtime}
	hint := app.proxyHintFor()
	if !strings.Contains(hint, "设置 → 网络代理") {
		t.Fatalf("已配代理时的提示应指向网络代理设置，实际：%q", hint)
	}
	if !strings.Contains(hint, "已配置代理") {
		t.Fatalf("已配代理时的提示应说明代理已生效，实际：%q", hint)
	}
	// 与 networkHintFor 共用同一份文案，避免两处说法漂移。
	if hint != networkHintFor(fmt.Errorf("dial tcp x: i/o timeout"), true) {
		t.Fatalf("proxyHintFor 与 networkHintFor 的已配代理文案应一致，实际：%q", hint)
	}
}

func TestLoopbackHostDetection(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "LOCALHOST"} {
		if !isLoopbackHost(host) {
			t.Fatalf("%s 应被判为回环", host)
		}
	}
	for _, host := range []string{"149.154.167.51", "example.com", "10.0.0.5"} {
		if isLoopbackHost(host) {
			t.Fatalf("%s 不应被判为回环", host)
		}
	}
}

func TestMediaTransportSkipsProxyForLoopback(t *testing.T) {
	clearProxyEnv(t)
	runtime := newProxyRuntime()
	runtime.apply("socks5://127.0.0.1:7890")
	transport := newMediaTransport(runtime)

	localRequest, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:3090/health", nil)
	proxyURL, err := transport.Proxy(localRequest)
	if err != nil {
		t.Fatalf("回环请求解析代理失败：%v", err)
	}
	if proxyURL != nil {
		t.Fatalf("回环请求不应走代理，得到 %v", proxyURL)
	}

	remoteRequest, _ := http.NewRequest(http.MethodGet, "https://cdn.telegram.org/file", nil)
	proxyURL, err = transport.Proxy(remoteRequest)
	if err != nil {
		t.Fatalf("外部请求解析代理失败：%v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:7890" {
		t.Fatalf("外部请求应走代理，得到 %v", proxyURL)
	}
}

// TestTelegramClientUsesInjectedResolverDialer 是最关键的接线测试：
// 它验证「把 dialer 塞进 telegram.Options.Resolver」之后，gotd 建连时**真的会调用它**。
// 之前的端到端脚本里代理桩一个连接都没收到，问题就落在这条接线上——
// 只断言「dialer 能打通隧道」是不够的，必须证明 gotd 会去用它。
func TestTelegramClientUsesInjectedResolverDialer(t *testing.T) {
	dialed := make(chan string, 16)
	dialer := func(_ context.Context, _, addr string) (net.Conn, error) {
		select {
		case dialed <- addr:
		default:
		}
		return nil, errors.New("桩：拒绝连接（只用于记录拨号目标）")
	}

	client := telegram.NewClient(123456, "0123456789abcdef0123456789abcdef", telegram.Options{
		Resolver:  dcs.Plain(dcs.PlainOptions{Dial: dialer}),
		NoUpdates: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = client.Run(ctx, func(ctx context.Context) error {
		_, err := client.Auth().SendCode(ctx, "+8613800000000", auth.SendCodeOptions{})
		return err
	})

	select {
	case addr := <-dialed:
		t.Logf("gotd 已通过注入的 dialer 拨号：%s", addr)
	case <-time.After(3 * time.Second):
		t.Fatal("gotd 没有调用注入的 dialer —— 代理没接到 MTProto 建连上")
	}
}
