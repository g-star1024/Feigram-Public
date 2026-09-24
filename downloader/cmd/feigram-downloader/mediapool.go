package main

// R4.29 · 账号级媒体连接池
//
// 背景（2.6.4/2.6.5 实测）：此前每个下载任务都 newTelegramClient 新建一个 MTProto
// 客户端，媒体 DC 的 exportAuth（授权导出）随任务反复发起——同一账号短时间几十次
// export 后 Telegram 直接 FLOOD_WAIT 1400+ 秒（≈24 分钟），下载全面停摆。
// 登录/会话侧从不出现该问题，因为它只连一次主 DC。
//
// 本文件为每个账号维持一条**常驻** MTProto 连接（gotd client）：
//   - 连接内的 DC 池（MediaOnly）随连接常驻——每个媒体 DC 只 exportAuth 一次，
//     后续任务直接复用授权与连接，根治反复 export 导致的 FLOOD_WAIT；
//   - 任务退出不拆连接（下载走完只关媒体 invoker，主连接保活）；
//   - 重新登录（session 变化）或账号删除时自动重建/回收（session 指纹自愈）。
//
// 锁纪律：mediaMu 与 a.mu 是两把独立的锁。持 a.mu 的代码路径（如 stateLocked）
// 绝不获取 mediaMu；acquireMediaConn 的等待窗口（连接建立最多 45~120s，R4.34 起
// 按握手耗时自适应）只持 mediaMu，因此不会阻塞 /api/state。client 内部的 session
// 存储回调会拿 a.mu——两者无环。

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/gotd/td/telegram"
)

// 媒体连接建立窗口（R4.34 自适应）：基础上限 45s；该账号最近媒体探测的握手
// 耗时偏高（高延迟节点）时按 8×握手ms 放宽，封顶 120s——与 R4.32 首字节
// 看门狗的自适应思路对齐，别把「慢」误判成「挂死」。
const (
	mediaConnStartBaseTimeout = 45 * time.Second
	mediaConnStartMaxTimeout  = 120 * time.Second
)

// mediaConnStartTimeoutFor 依据最近握手耗时计算连接建立等待上限。纯函数便于单测。
func mediaConnStartTimeoutFor(handshakeMs int64) time.Duration {
	timeout := mediaConnStartBaseTimeout
	if handshakeMs > 0 {
		if scaled := time.Duration(handshakeMs) * 8 * time.Millisecond; scaled > timeout {
			timeout = scaled
		}
	}
	if timeout > mediaConnStartMaxTimeout {
		timeout = mediaConnStartMaxTimeout
	}
	return timeout
}

type mediaConn struct {
	key         string
	client      *telegram.Client
	cancel      context.CancelFunc
	ready       chan struct{} // Run 回调已进入（连接 + 握手 + session 加载完成）后关闭
	done        chan error    // 缓冲 1：Run 退出原因
	session     string        // 创建时的 session 原文（内存内比较指纹），换 session 即重建
	fingerprint string        // R4.30：AuthKeyID 认证指纹——区分「回写」与「重新登录」
	createdAt   time.Time
}

// refreshConnIdentity 在连接就绪后用「当前落库 session」刷新连接的指纹与快照。
// 背景（R4.30）：常驻连接与主连接/健康检查共用同一 nativeSessionStorage，连接
// 握手成功时 gotd 会把新盐值的 session 回写进账号——若一直用创建时的快照比对，
// 连接会被自己的回写「打失效」，下次 acquire 误判重新登录而重建
// （2.6.7 实测：常驻连接存活 3 秒即被重建）。就绪后以最新落库值为准：
// AuthKeyID 没变（只是盐值/心跳回写）就不重建。
func (a *App) refreshConnIdentity(conn *mediaConn) {
	a.mu.Lock()
	account := a.native[conn.key]
	latest := ""
	if account != nil {
		latest = account.Session
	}
	a.mu.Unlock()
	if latest == "" {
		return
	}
	conn.session = latest
	if fp := a.sessionFingerprint(latest); fp != "" {
		conn.fingerprint = fp
	}
}

// mediaSessionChanged 判断账号 session 是否已更换到需要重建连接的程度。
// R4.30：优先比 AuthKeyID 指纹（两侧都能解析时）；任一侧解析失败退回逐字节
// 比较，保持旧行为不弱化安全性。
func (a *App) mediaSessionChanged(conn *mediaConn, accountSession string) bool {
	connFP := conn.fingerprint
	accountFP := a.sessionFingerprint(accountSession)
	if connFP != "" && accountFP != "" {
		return connFP != accountFP
	}
	return conn.session != accountSession
}

// acquireMediaConn 取（或建立）账号的常驻 MTProto 连接。
// 多任务并发调用安全：连接内建立后，gotd 客户端的 RPC 可并发复用。
func (a *App) acquireMediaConn(account NativeAccount, apiHash string) (*mediaConn, error) {
	key := nativeAccountKey(account.UserID, account.AccountID)
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	// R4.34：等待窗口按最近探测握手耗时自适应（无探测数据保持 45s 基础档）。
	// 此处持 mediaMu 后取 a.mu——锁序合法（持 a.mu 的路径不取 mediaMu）。
	startTimeout := mediaConnStartTimeoutFor(a.latestMediaHandshakeMaxMs(account.UserID, account.AccountID))

	if conn := a.mediaConns[key]; conn != nil {
		select {
		case err := <-conn.done:
			log.Printf("media pool: account %s 常驻连接已退出，重建：%v", key, err)
			delete(a.mediaConns, key)
		default:
		}
	}
	if conn := a.mediaConns[key]; conn != nil && a.mediaSessionChanged(conn, account.Session) {
		// R4.29 自愈：重新登录后 session 变化，旧连接的授权已失效，必须重建。
		// R4.30：判定改走 AuthKeyID 指纹——盐值级回写不再触发重建。
		log.Printf("media pool: account %s session 已更换（重新登录），重建常驻连接", key)
		a.dropMediaConnLocked(key)
	}
	if conn := a.mediaConns[key]; conn != nil {
		select {
		case <-conn.ready:
			a.refreshConnIdentity(conn)
			return conn, nil
		case err := <-conn.done:
			delete(a.mediaConns, key)
			return nil, fmt.Errorf("媒体连接启动失败：%w", err)
		case <-time.After(startTimeout):
			a.dropMediaConnLocked(key)
			return nil, fmt.Errorf("媒体连接建立超时（%s）：请检查代理是否放行 Telegram。%s", startTimeout.Round(time.Second), a.latestMediaProbeHint(account.UserID, account.AccountID))
		}
	}

	client, err := a.newTelegramClient(account, apiHash)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &mediaConn{
		key:         key,
		client:      client,
		cancel:      cancel,
		ready:       make(chan struct{}),
		done:        make(chan error, 1),
		session:     account.Session,
		fingerprint: a.sessionFingerprint(account.Session),
		createdAt:   time.Now(),
	}
	a.mediaConns[key] = conn
	go func() {
		err := client.Run(ctx, func(ctx context.Context) error {
			close(conn.ready)
			<-ctx.Done()
			return context.Cause(ctx)
		})
		conn.done <- err
		log.Printf("media pool: account %s 常驻连接退出（存活 %s）：%v", key, time.Since(conn.createdAt).Round(time.Second), err)
	}()
	select {
	case <-conn.ready:
		a.refreshConnIdentity(conn)
		log.Printf("media pool: account %s 常驻连接已建立（耗时 %s，会话指纹 %s）", key, time.Since(conn.createdAt).Round(time.Millisecond), fingerprintPreview(conn.fingerprint))
		return conn, nil
	case err := <-conn.done:
		delete(a.mediaConns, key)
		return nil, fmt.Errorf("媒体连接建立失败：%w", err)
	case <-time.After(startTimeout):
		a.dropMediaConnLocked(key)
		return nil, fmt.Errorf("媒体连接建立超时（%s）：请检查代理是否放行 Telegram。%s", startTimeout.Round(time.Second), a.latestMediaProbeHint(account.UserID, account.AccountID))
	}
}

// fingerprintPreview 指纹日志预览：只取前 8 字节十六进制，避免整段 AuthKeyID 进日志。
func fingerprintPreview(fp string) string {
	if fp == "" {
		return "(空)"
	}
	return hex.EncodeToString([]byte(fp))[:16]
}

// dropMediaConnLocked 回收常驻连接（调用方须持 a.mediaMu）。不等待 goroutine 退出：
// Run 会因 ctx 取消自行收尾并写 done（缓冲 1，不会泄漏）。
func (a *App) dropMediaConnLocked(key string) {
	if conn := a.mediaConns[key]; conn != nil {
		conn.cancel()
		delete(a.mediaConns, key)
		log.Printf("media pool: account %s 常驻连接已回收", key)
	}
}

// dropMediaConn 回收账号常驻连接（账号删除/登出时调用）。
func (a *App) dropMediaConn(key string) {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	a.dropMediaConnLocked(key)
}
