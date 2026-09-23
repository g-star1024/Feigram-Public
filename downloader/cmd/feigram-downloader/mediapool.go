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
// 绝不获取 mediaMu；acquireMediaConn 的等待窗口（连接建立最多 45s）只持 mediaMu，
// 因此不会阻塞 /api/state。client 内部的 session 存储回调会拿 a.mu——两者无环。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gotd/td/telegram"
)

// mediaConnStartTimeout 等待常驻连接建立（TCP + MTProto 握手 + session 加载）的上限。
const mediaConnStartTimeout = 45 * time.Second

type mediaConn struct {
	key       string
	client    *telegram.Client
	cancel    context.CancelFunc
	ready     chan struct{} // Run 回调已进入（连接 + 握手 + session 加载完成）后关闭
	done      chan error    // 缓冲 1：Run 退出原因
	session   string        // 创建时的 session 原文（内存内比较指纹），换 session 即重建
	createdAt time.Time
}

// acquireMediaConn 取（或建立）账号的常驻 MTProto 连接。
// 多任务并发调用安全：连接内建立后，gotd 客户端的 RPC 可并发复用。
func (a *App) acquireMediaConn(account NativeAccount, apiHash string) (*mediaConn, error) {
	key := nativeAccountKey(account.UserID, account.AccountID)
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()

	if conn := a.mediaConns[key]; conn != nil {
		select {
		case err := <-conn.done:
			log.Printf("media pool: account %s 常驻连接已退出，重建：%v", key, err)
			delete(a.mediaConns, key)
		default:
		}
	}
	if conn := a.mediaConns[key]; conn != nil && conn.session != account.Session {
		// R4.29 自愈：重新登录后 session 变化，旧连接的授权已失效，必须重建。
		log.Printf("media pool: account %s session 已更换（重新登录），重建常驻连接", key)
		a.dropMediaConnLocked(key)
	}
	if conn := a.mediaConns[key]; conn != nil {
		select {
		case <-conn.ready:
			return conn, nil
		case err := <-conn.done:
			delete(a.mediaConns, key)
			return nil, fmt.Errorf("媒体连接启动失败：%w", err)
		case <-time.After(mediaConnStartTimeout):
			a.dropMediaConnLocked(key)
			return nil, errors.New("媒体连接建立超时（45 秒）：请检查代理是否放行 Telegram")
		}
	}

	client, err := a.newTelegramClient(account, apiHash)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &mediaConn{
		key:       key,
		client:    client,
		cancel:    cancel,
		ready:     make(chan struct{}),
		done:      make(chan error, 1),
		session:   account.Session,
		createdAt: time.Now(),
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
		log.Printf("media pool: account %s 常驻连接已建立（耗时 %s）", key, time.Since(conn.createdAt).Round(time.Millisecond))
		return conn, nil
	case err := <-conn.done:
		delete(a.mediaConns, key)
		return nil, fmt.Errorf("媒体连接建立失败：%w", err)
	case <-time.After(mediaConnStartTimeout):
		a.dropMediaConnLocked(key)
		return nil, errors.New("媒体连接建立超时（45 秒）：请检查代理是否放行 Telegram")
	}
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
