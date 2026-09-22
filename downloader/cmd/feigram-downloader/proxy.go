package main

// proxy.go — 应用级网络代理（MTProto 与媒体下载共用同一条出口）。
//
// 为什么需要这个文件：gotd 的 telegram.NewClient 会在 Options.setDefaults() 里把
// Resolver 置为 dcs.DefaultResolver() = dcs.Plain(dcs.PlainOptions{})，其 Dial 字段
// 退化成裸 net.Dialer → MTProto 建连**直连** Telegram DC。
// （gotd 里唯一读环境变量代理的是 OptionsFromEnvironment/ClientFromEnvironment，本项目未使用。）
// 因此即便在系统或 FPK 环境里导出 ALL_PROXY/HTTPS_PROXY，登录、会话、消息、
// 文件引用这些 MTProto 链路也不会走代理。
//
// 本文件把「生效代理」解析成 dialer 并注入 telegram.Options.Resolver，
// 同时把媒体下载用的 http.Transport 也指向同一个代理。
//
// 优先级：应用内设置（settings.json 的 proxyUrl，经 PUT /api/config 下发）> 环境变量 > 直连。

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/dcs"
	xproxy "golang.org/x/net/proxy"
)

// dialFunc 是 dcs.DialFunc 的别名，可直接赋给 dcs.PlainOptions.Dial。
type dialFunc = dcs.DialFunc

const proxyDialTimeout = 15 * time.Second

// proxyEnvKeys 是环境变量兜底的查找顺序（首个有效值生效）。
// FEIGRAM_PROXY_URL 为本应用专用变量，优先级高于通用变量。
var proxyEnvKeys = []string{
	"FEIGRAM_PROXY_URL",
	"ALL_PROXY", "all_proxy",
	"SOCKS5", "socks5",
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
}

// proxyHostPattern 限定主机名只能由 ASCII 主机字符组成（域名/IPv4/IPv6 字面量）。
// 需要这道检查的原因：url.Parse 不做主机校验，`socks5://这不是地址` 会被当成合法主机，
// 于是非法输入会伪装成可用配置去建连，报错也变成难以理解的形式。
var proxyHostPattern = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]+$`)

func isValidProxyHost(host string) bool {
	if host == "" || strings.Contains(host, "%") {
		return false
	}
	return proxyHostPattern.MatchString(host)
}

// normalizeProxyURL 规范化用户输入的代理地址：
// 省略 scheme 时按 socks5 处理；仅接受 socks5 / socks5h / http / https 四种协议。
// 返回空串表示「未配置」。
func normalizeProxyURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "socks5://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("代理地址无法解析：%v", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return "", fmt.Errorf("不支持的代理协议 %q（仅支持 socks5 / socks5h / http / https）", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", errors.New("代理地址缺少主机名")
	}
	if !isValidProxyHost(parsed.Hostname()) {
		return "", errors.New("代理地址的主机名无效，请填写 IPv4/IPv6/域名")
	}
	if parsed.Port() == "" {
		return "", errors.New("代理地址缺少端口，请写完整（如 socks5://127.0.0.1:7890）")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("代理地址不应包含路径")
	}
	parsed.Scheme = scheme
	parsed.Path = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// maskProxyURL 脱敏：只保留 scheme://host:port，去掉账号密码，可安全对外暴露。
// 先过一遍 normalizeProxyURL，避免把「非法输入」脱敏成看似正常的地址写进日志与界面。
func maskProxyURL(raw string) string {
	normalized, err := normalizeProxyURL(raw)
	if err != nil || normalized == "" {
		return ""
	}
	parsed, perr := url.Parse(normalized)
	if perr != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// resolveProxy 按优先级求出生效代理。
// source 取值：settings / env:<KEY> / none / invalid。
// 显式配置非法时返回错误而不静默回落到直连，避免用户以为配好了其实没生效。
func resolveProxy(configured string) (raw string, source string, err error) {
	if strings.TrimSpace(configured) != "" {
		normalized, nerr := normalizeProxyURL(configured)
		if nerr != nil {
			return "", "invalid", fmt.Errorf("设置中的网络代理无效：%v", nerr)
		}
		return normalized, "settings", nil
	}
	for _, key := range proxyEnvKeys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		normalized, nerr := normalizeProxyURL(value)
		if nerr != nil {
			// 环境变量非法只跳过该变量，不阻断其它来源的匹配。
			continue
		}
		return normalized, "env:" + key, nil
	}
	return "", "none", nil
}

// newProxyDialer 依据规范化后的代理 URL 构造 dialer。
func newProxyDialer(rawURL string) (dialFunc, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "socks5", "socks5h":
		return newSOCKS5Dialer(parsed)
	case "http", "https":
		return newHTTPConnectDialer(parsed)
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q", parsed.Scheme)
	}
}

// newSOCKS5Dialer 走 golang.org/x/net/proxy 的 SOCKS5 实现。
// gotd 传入的是 DC 的 IP:port，因此 socks5 与 socks5h 的「远端/本地解析域名」
// 差异在本场景下没有实际影响，两者按同一实现处理。
func newSOCKS5Dialer(parsed *url.URL) (dialFunc, error) {
	var auth *xproxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &xproxy.Auth{User: parsed.User.Username(), Password: password}
	}
	dialer, err := xproxy.SOCKS5("tcp", parsed.Host, auth, xproxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 代理初始化失败：%v", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 代理不支持带 context 的拨号")
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		debugProxyDial("socks5", network, addr)
		if !isTCPNetwork(network) {
			return nil, fmt.Errorf("SOCKS5 代理仅支持 tcp，收到 %q", network)
		}
		// 底层 dialer 未设超时，未带 deadline 时补一个，避免建连无限挂起。
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, proxyDialTimeout)
			defer cancel()
		}
		conn, derr := contextDialer.DialContext(ctx, "tcp", addr)
		if derr != nil {
			return nil, fmt.Errorf("经 SOCKS5 代理 %s 连接 %s 失败：%w", parsed.Host, addr, derr)
		}
		return conn, nil
	}, nil
}

// newHTTPConnectDialer 自己实现 HTTP CONNECT 隧道拨号。
// 不依赖 net/http 的 Transport 代理逻辑，是因为这里要的是「拿到裸 TCP 隧道」，
// 交给 MTProto 自己跑 obfuscated 握手。
func newHTTPConnectDialer(parsed *url.URL) (dialFunc, error) {
	proxyAddr := parsed.Host
	authHeader := ""
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		credentials := parsed.User.Username() + ":" + password
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(credentials))
	}
	useTLS := parsed.Scheme == "https"
	serverName := parsed.Hostname()

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		debugProxyDial("http-connect", network, addr)
		if !isTCPNetwork(network) {
			return nil, fmt.Errorf("HTTP 代理仅支持 tcp，收到 %q", network)
		}
		conn, err := (&net.Dialer{Timeout: proxyDialTimeout}).DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("连接 HTTP 代理 %s 失败：%w", proxyAddr, err)
		}
		if useTLS {
			tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
			if herr := tlsConn.HandshakeContext(ctx); herr != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("与 HTTPS 代理 %s 握手失败：%w", proxyAddr, herr)
			}
			conn = tlsConn
		}

		deadline := time.Now().Add(proxyDialTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		_ = conn.SetDeadline(deadline)

		request := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Opaque: addr},
			Host:   addr,
			Header: http.Header{},
		}
		if authHeader != "" {
			request.Header.Set("Proxy-Authorization", authHeader)
		}
		if werr := request.Write(conn); werr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("向代理发送 CONNECT 失败：%w", werr)
		}

		reader := bufio.NewReader(conn)
		statusCode, rerr := readConnectStatus(reader)
		if rerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("读取代理 CONNECT 响应失败：%w", rerr)
		}
		if statusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("代理拒绝 CONNECT %s：HTTP %d（请确认代理允许访问该端口）", addr, statusCode)
		}

		_ = conn.SetDeadline(time.Time{})
		if reader.Buffered() > 0 {
			// 代理可能在响应头之后多读进了隧道数据，必须把它们接回连接，
			// 否则首个 MTProto 字节会被 bufio 吞掉，表现为「建连后静默失败」。
			return &bufferedConn{Conn: conn, reader: reader}, nil
		}
		return conn, nil
	}, nil
}

// readConnectStatus 手工解析 CONNECT 响应：跳过 1xx 中间响应，返回首个终态状态码。
// 刻意不用 http.ReadResponse —— 它会按 body 语义处理响应，可能把隧道当成待读 body。
func readConnectStatus(reader *bufio.Reader) (int, error) {
	textReader := textproto.NewReader(reader)
	for attempt := 0; attempt < 5; attempt++ {
		statusLine, err := textReader.ReadLine()
		if err != nil {
			return 0, err
		}
		parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
		if len(parts) < 2 {
			return 0, fmt.Errorf("代理返回了异常状态行：%q", statusLine)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, fmt.Errorf("代理返回了异常状态行：%q", statusLine)
		}
		// 消费掉本次响应的头部，读到空行为止。
		if _, err := textReader.ReadMIMEHeader(); err != nil {
			return code, nil
		}
		if code < 100 || code >= 200 {
			return code, nil
		}
	}
	return 0, errors.New("代理返回了过多中间响应")
}

// bufferedConn 让 CONNECT 之后的隧道数据先经 bufio 缓冲读出。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func isTCPNetwork(network string) bool {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return true
	default:
		return false
	}
}

// debugProxyDial 记录每次走代理的拨号目标。
// 默认静默，LOG_LEVEL=debug 时开启：排「配了代理却连不上」时，
// 第一件要确认的事就是「代理 dialer 到底有没有被调用到」。
func debugProxyDial(label, network, addr string) {
	if strings.ToLower(os.Getenv("LOG_LEVEL")) != "debug" {
		return
	}
	log.Printf("[proxy] %s dial network=%s target=%s", label, network, addr)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// proxyRuntime 保存当前生效代理，供 MTProto dialer 与媒体 transport 并发读取。
// 用独立的锁而不复用 App.mu：newTelegramClient 的调用方多数已持有 App.mu，
// 若在此处再取 App.mu 会自锁。
type proxyRuntime struct {
	mu     sync.RWMutex
	raw    string
	source string
	reason string
	parsed *url.URL
	dial   dialFunc
}

func newProxyRuntime() *proxyRuntime {
	return &proxyRuntime{source: "none"}
}

// apply 重新解析并原子替换生效代理；解析失败时清空代理并保留原因供诊断。
func (p *proxyRuntime) apply(configured string) (string, string) {
	raw, source, err := resolveProxy(configured)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.raw, p.source, p.parsed, p.dial, p.reason = "", source, nil, nil, ""
	if err != nil {
		p.source = "invalid"
		p.reason = err.Error()
		return p.source, p.reason
	}
	if raw == "" {
		return p.source, ""
	}
	dialer, derr := newProxyDialer(raw)
	if derr != nil {
		p.source = "invalid"
		p.reason = derr.Error()
		return p.source, p.reason
	}
	p.raw = raw
	p.dial = dialer
	if parsed, perr := url.Parse(raw); perr == nil {
		p.parsed = parsed
	}
	return p.source, ""
}

func (p *proxyRuntime) dialer() dialFunc {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dial
}

// proxyFuncForRequest 供 http.Transport.Proxy 使用；返回 nil 表示本次直连。
func (p *proxyRuntime) proxyFuncForRequest(req *http.Request) (*url.URL, error) {
	p.mu.RLock()
	target := p.parsed
	p.mu.RUnlock()
	if target == nil {
		// 未显式配置代理时保持既有行为（读环境变量 + 尊重 NO_PROXY）。
		return http.ProxyFromEnvironment(req)
	}
	if isLoopbackHost(req.URL.Hostname()) {
		// 本机回环流量（Node 网关 / sidecar / 本地缓存）永远直连，
		// 不因用户配了远端代理就把本机调用绕出去。
		return nil, nil
	}
	return target, nil
}

// status 返回可安全对外暴露的代理状态（凭据已脱敏）。
func (p *proxyRuntime) status() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	status := map[string]any{
		"source":  p.source,
		"enabled": p.dial != nil,
		"address": maskProxyURL(p.raw),
	}
	if p.reason != "" {
		status["error"] = p.reason
	}
	return status
}

// describeProxyConfig 生成一行可写入日志的代理摘要。
func (p *proxyRuntime) describeProxyConfig() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch {
	case p.reason != "":
		return fmt.Sprintf("代理配置无效（source=%s）：%s", p.source, p.reason)
	case p.dial == nil:
		return "未配置网络代理，MTProto 与媒体下载均直连"
	default:
		return fmt.Sprintf("网络代理已启用：%s（source=%s）", maskProxyURL(p.raw), p.source)
	}
}

// 出口不可达时给用户看的两句话。抽成常量，供 networkHintFor 与
// 「等待 Telegram 超时」这类本身不含网络关键字的失败复用。
const (
	hintProxyUnreachable = "已配置代理但连接 Telegram DC 仍失败：请确认代理进程在运行，并在「设置 → 网络代理」核对地址与端口，且该代理允许 TCP 直通 Telegram。"
	hintDirectBlocked    = "无法直连 Telegram DC。若本机网络不能直接访问 Telegram，请在「设置 → 网络代理」填写可用的 SOCKS5/HTTP 代理。"
)

// proxyHintFor 按当前是否启用了代理，返回对应的出口提示。
func (a *App) proxyHintFor() string {
	if a.proxy.dialer() != nil {
		return hintProxyUnreachable
	}
	return hintDirectBlocked
}

// networkHintFor 判断错误是否属于「网络不可达 / 拨号失败」类，并给出可操作提示。
// 返回空串表示该错误与网络出口无关，不需要追加提示。
func networkHintFor(err error, proxyActive bool) string {
	if err == nil {
		return ""
	}
	lower := strings.ToLower(err.Error())
	markers := []string{
		"i/o timeout",
		"connection refused",
		"connection reset by peer",
		"no route to host",
		"network is unreachable",
		"context deadline exceeded",
		"dial tcp",
		"proxyconnect",
		"socks5",
		"connect tunnel",
	}
	matched := false
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return ""
	}
	if proxyActive {
		return hintProxyUnreachable
	}
	return hintDirectBlocked
}

// withNetworkHint 在错误消息后追加网络出口提示（若适用）。
func (a *App) withNetworkHint(err error) string {
	if err == nil {
		return ""
	}
	base := err.Error()
	hint := networkHintFor(err, a.proxy.dialer() != nil)
	if hint == "" {
		return base
	}
	if strings.Contains(base, hint) {
		return base
	}
	return base + " —— " + hint
}

// newMediaTransport 构造媒体下载用的 transport；代理随运行时状态即时生效。
func newMediaTransport(proxy *proxyRuntime) *http.Transport {
	return &http.Transport{
		Proxy:               proxy.proxyFuncForRequest,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
}
