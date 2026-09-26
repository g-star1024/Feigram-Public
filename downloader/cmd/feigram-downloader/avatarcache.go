package main

// R4.64：头像字节缓存 —— 此前每次 /api/avatar 都真实走 MTProto
// （photos.getUserPhotos + upload.getFile 256KB 分块）过代理，群消息列表一屏
// 十几个发送者头像全走网络：代理差时慢/失败，消息页头像大面积裂图
// （2026-09-26 实测）。头像低频变更，这里加「账号+peer」级字节缓存：
// 命中直接回内存（微秒级），未命中 single-flight 只拉一次网络，失败不缓存
// （保持 Node 侧 SVG 占位兜底链路，账号/代理恢复后下一次自然回到真实头像）。

import (
	"context"
	"sync"
	"time"
)

const (
	avatarCacheTTL        = 6 * time.Hour
	avatarCacheMaxEntries = 512
)

type avatarCacheEntry struct {
	data      []byte
	fetchedAt time.Time
}

// avatarInflightCall 是同 key 并发请求的 single-flight 载体：
// 首个请求真实拉取，其余等待者共享结果（成功或失败）。
type avatarInflightCall struct {
	wg   sync.WaitGroup
	data []byte
	err  error
}

// cachedAvatar 返回缓存字节；过期视为未命中并惰性清除。
func (a *App) cachedAvatar(key string) ([]byte, bool) {
	a.avatarCacheMu.Lock()
	defer a.avatarCacheMu.Unlock()
	entry, ok := a.avatarCache[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.fetchedAt) > avatarCacheTTL {
		delete(a.avatarCache, key)
		return nil, false
	}
	return entry.data, true
}

func (a *App) storeAvatar(key string, data []byte) {
	a.avatarCacheMu.Lock()
	defer a.avatarCacheMu.Unlock()
	// 惰性初始化（沿用 dialogcache 模式）：不在 main() 字面量里铺字段。
	if a.avatarCache == nil {
		a.avatarCache = map[string]*avatarCacheEntry{}
	}
	// 简易容量控制：头像条目等价无序，超限随机淘汰即可，不值得维护 LRU。
	for len(a.avatarCache) >= avatarCacheMaxEntries {
		for k := range a.avatarCache {
			delete(a.avatarCache, k)
			break
		}
	}
	a.avatarCache[key] = &avatarCacheEntry{data: data, fetchedAt: time.Now()}
}

// avatarWithCache single-flight 包装：同 key 并发只发一次网络拉取，
// 等待者共享结果；成功结果落缓存，失败不落（下次请求重试）。
func (a *App) avatarWithCache(ctx context.Context, key string, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	if data, ok := a.cachedAvatar(key); ok {
		return data, nil
	}
	a.avatarCacheMu.Lock()
	if a.avatarInflight == nil {
		a.avatarInflight = map[string]*avatarInflightCall{}
	}
	if call, ok := a.avatarInflight[key]; ok {
		a.avatarCacheMu.Unlock()
		call.wg.Wait()
		return call.data, call.err
	}
	call := &avatarInflightCall{}
	call.wg.Add(1)
	a.avatarInflight[key] = call
	a.avatarCacheMu.Unlock()

	// 参照 dialogcache（R4.55）：共享拉取脱离单个请求的取消域，
	// 防止首个请求方（浏览器 abort/超时）把同 key 等待者一起带崩。
	fetchCtx := context.WithoutCancel(ctx)
	call.data, call.err = fetch(fetchCtx)
	call.wg.Done()

	a.avatarCacheMu.Lock()
	delete(a.avatarInflight, key)
	a.avatarCacheMu.Unlock()

	if call.err == nil && len(call.data) > 0 {
		a.storeAvatar(key, call.data)
	}
	return call.data, call.err
}
