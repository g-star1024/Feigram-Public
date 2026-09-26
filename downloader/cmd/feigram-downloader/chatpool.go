package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// R4.54：账号级常驻聊天连接池。
//
// 背景（2026-09-25 实测）：chat API 过去每个请求都 newTelegramClient + client.Run，
// 每次都付 TCP+MTProto 连接建立成本；会话页几十个头像/图片并发时形成几十条
// 新连接风暴，拖慢消息历史查询并易触发账号级限流。
// 池化后每账号维持一条常驻连接，dialogs/folders/messages/media/peer/resolve/
// search/send/button/details/avatar 全部复用；blob 长传输仍走每请求独立连接，
// 避免大流量挤占共享连接（单连接 RPC 乱序复用，但带宽共享）。
//
// 锁纪律：chatPoolMu 独立锁，池内不持有 a.mu/mediaMu 做任何 RPC，与既有
// 多锁（R4.29 mediapool / R4.35 peerResolveGate）同模式，无环。

const (
	// chatPoolConnectTimeout 等待常驻连接就绪的上限。实测握手 4~5s（高延迟出口），
	// 按 R4.32 经验放宽到 30s，避免把「慢」误判成「失败」。
	chatPoolConnectTimeout = 30 * time.Second
	// chatPoolIdleTTL 空闲连接保留时长：超过后在下一次池访问时关闭。
	chatPoolIdleTTL = 10 * time.Minute
)

// chatRunner 在「已就绪的连接」上执行一次查询并统一错误分类。
// 两种实现：常驻连接池（fn 直接执行）与每请求独立连接（client.Run 包裹）。
type chatRunner = func(ctx context.Context, fn func(context.Context, *tg.Client) (any, error)) (any, error)

type pooledChatClient struct {
	client *telegram.Client
	api    *tg.Client
	runCtx context.Context
	cancel context.CancelFunc
	// ready 在连接就绪时 close（广播语义）：并发等待的全部请求都能立即通过。
	// 不能用容量 1 的 error channel——多个并发请求只有一个能收到值，其余会
	// 误等超时并错杀健康连接（铁律 8 落盘复核发现）。
	ready chan struct{}
	// fail 在 Run 失败时送入错误（容量 1）；收到者负责丢弃条目，
	// 其余等待者经 done 关闭得到「连接已断开」。
	fail chan error
	// done 在 Run 返回（连接断开/被取消）时关闭。
	done chan struct{}
	// lastUsed 最近一次借用时间（unix nanos），用于空闲清扫。
	lastUsed atomic.Int64
}

func (p *pooledChatClient) idleExpired() bool {
	last := p.lastUsed.Load()
	return last > 0 && time.Since(time.Unix(0, last)) > chatPoolIdleTTL
}

func (p *pooledChatClient) closed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// pooledChatEntry 取出（或建立并等待就绪）账号的常驻连接。
// 连接就绪前的等待、失败重建、空闲清扫都在这里完成。
func (a *App) pooledChatEntry(account NativeAccount, apiHash string) (*pooledChatClient, error) {
	key := nativeAccountKey(account.UserID, account.AccountID)

	a.chatPoolMu.Lock()
	if a.chatPool == nil {
		a.chatPool = map[string]*pooledChatClient{}
	}
	// 顺手清扫：其他账号的空闲/已断开连接（网络 IO 在锁外 cancel）。
	var sweep []context.CancelFunc
	for id, entry := range a.chatPool {
		if id == key {
			continue
		}
		if entry.idleExpired() || entry.closed() {
			delete(a.chatPool, id)
			sweep = append(sweep, entry.cancel)
		}
	}
	entry := a.chatPool[key]
	if entry != nil && entry.closed() {
		// 上一条连接已断开（代理抖动/服务端断开），移除后重建。
		delete(a.chatPool, key)
		sweep = append(sweep, entry.cancel)
		entry = nil
	}
	if entry == nil {
		client, err := a.newTelegramClient(account, apiHash)
		if err != nil {
			a.chatPoolMu.Unlock()
			for _, cancel := range sweep {
				cancel()
			}
			return nil, err
		}
		runCtx, cancel := context.WithCancel(context.Background())
		entry = &pooledChatClient{
			client: client,
			cancel: cancel,
			ready:  make(chan struct{}),
			fail:   make(chan error, 1),
			done:   make(chan struct{}),
			runCtx: runCtx,
		}
		a.chatPool[key] = entry
		go func() {
			defer close(entry.done)
			runErr := client.Run(runCtx, func(ctx context.Context) error {
				entry.api = client.API()
				entry.runCtx = ctx
				close(entry.ready)
				<-ctx.Done()
				return nil
			})
			if runErr != nil {
				select {
				case entry.fail <- runErr:
				default:
				}
			}
		}()
	}
	entry.lastUsed.Store(time.Now().UnixNano())
	a.chatPoolMu.Unlock()
	for _, cancel := range sweep {
		cancel()
	}

	// 等连接就绪（首次建立时）。ready 关闭后 api/client/runCtx 的读取有
	// channel close 的 happens-before 保证。
	select {
	case err := <-entry.fail:
		a.dropChatPoolEntry(key, entry)
		return nil, errors.New(a.withNetworkHint(err))
	case <-entry.ready:
	case <-entry.done:
		return nil, errors.New("聊天连接已断开，请重试")
	case <-time.After(chatPoolConnectTimeout):
		a.dropChatPoolEntry(key, entry)
		return nil, fmt.Errorf("聊天连接建立超时（%s），请重试", chatPoolConnectTimeout)
	}
	return entry, nil
}

// pooledChatRunner 返回在账号常驻连接上执行查询的 runner（供 JSON 查询类动作使用）。
func (a *App) pooledChatRunner(account NativeAccount, apiHash string) (chatRunner, error) {
	entry, err := a.pooledChatEntry(account, apiHash)
	if err != nil {
		return nil, err
	}
	key := nativeAccountKey(account.UserID, account.AccountID)
	api := entry.api
	return func(ctx context.Context, fn func(context.Context, *tg.Client) (any, error)) (any, error) {
		entry.lastUsed.Store(time.Now().UnixNano())
		out, err := fn(ctx, api)
		entry.lastUsed.Store(time.Now().UnixNano())
		if err != nil {
			// 连接已死/授权失效类错误：丢弃池条目，下个请求重建新连接。
			if chatPoolFatal(err) {
				a.dropChatPoolEntry(key, entry)
			}
			return nil, classifyNativeReadError(err)
		}
		return out, nil
	}, nil
}

// pooledChatAvatar 在常驻连接上拉取头像字节（保留 MediaOnly 迁移回退）。
// R4.64：外层套头像字节缓存 + single-flight——群消息列表一屏十几个发送者
// 头像此前每次请求都真实走 MTProto 过代理（零缓存），代理差时消息页头像
// 大面积裂图；命中缓存微秒级返回，未命中同 key 并发只拉一次。
func (a *App) pooledChatAvatar(ctx context.Context, account NativeAccount, apiHash, peerID string) ([]byte, error) {
	return a.avatarWithCache(ctx, nativeAccountKey(account.UserID, account.AccountID)+"|"+peerID, func(fetchCtx context.Context) ([]byte, error) {
		entry, err := a.pooledChatEntry(account, apiHash)
		if err != nil {
			return nil, err
		}
		entry.lastUsed.Store(time.Now().UnixNano())
		data, err := a.fetchNativeAvatarOnAPI(fetchCtx, entry.api, entry.client, account, peerID)
		entry.lastUsed.Store(time.Now().UnixNano())
		if err != nil && chatPoolFatal(err) {
			a.dropChatPoolEntry(nativeAccountKey(account.UserID, account.AccountID), entry)
		}
		return data, err
	})
}

// chatPoolFatal 判断错误是否意味着这条常驻连接不可再用（需要丢弃重建），
// 而不是单次 RPC 失败（连接仍健康）。
func chatPoolFatal(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{
		"AUTH_KEY", "SESSION_EXPIRED", "SESSION_REVOKED", "Unauthorized",
		"use of closed network connection", "connection dead",
		"engine forcibly closed", "授权已失效", "会话已失效",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (a *App) dropChatPoolEntry(key string, entry *pooledChatClient) {
	a.chatPoolMu.Lock()
	if a.chatPool[key] == entry {
		delete(a.chatPool, key)
	}
	a.chatPoolMu.Unlock()
	entry.cancel()
}

// dropChatPool 对外清理入口：账号退出/被删除时立即关掉它的常驻连接。
func (a *App) dropChatPool(userID, accountID string) {
	key := nativeAccountKey(userID, accountID)
	a.chatPoolMu.Lock()
	entry := a.chatPool[key]
	delete(a.chatPool, key)
	a.chatPoolMu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}
