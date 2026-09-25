package main

import (
	"context"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

// R4.55①：会话列表 single-flight + 短 TTL 缓存（借鉴 tdesktop DialogsLoadState
// 单一加载器与 TDLib「按需 loadChats + 本地库优先」的思路）。
//
// 问题（2.6.32 实测日志）：前端打开会话页会并发发出 /api/chats 与 /api/folders，
// 而 fetchNativeFolders 内部又全量重拉一遍 dialogs——同账号 30s 内出现 2~3 遍
// 全量分页拉取（每遍 6 页 getDialogs），在代理质量差时直接把会话列表拖到超时，
// 还叠加自造限流（R4.39 的 FLOOD_WAIT 教训）。
//
// 解法：
//   - 同账号 + 同查询词的拉取全局单飞：并发请求共享同一次结果；
//   - 成功结果缓存 dialogCacheTTL，期间后续请求零 RPC 直接返回；
//   - 失败不缓存（下个请求立即重试），避免把一次网络抖动钉死 30s；
//   - 拉取固定包含归档（includeArchived=true 的超集），请求方按需过滤——
//     这样 loadChats（可能不含归档）与 folders（必含归档）能共享同一次拉取。
//
// 锁纪律：dialogCacheMu 独立锁，锁内不做任何网络 IO；共享拉取用
// context.WithoutCancel 脱离首个请求的 handler 超时，避免首个请求被
// 掐断时把所有等待者一起带崩（常驻池连接在池里活着，api 仍有效）。

const dialogCacheTTL = 30 * time.Second

type dialogCacheEntry struct {
	done  chan struct{}
	items []map[string]any
	err   error
	at    time.Time
}

func dialogCacheKey(account NativeAccount, query string) string {
	return account.UserID + "|" + account.AccountID + "|" + strings.ToLower(strings.TrimSpace(query))
}

// cachedNativeDialogs 是 fetchNativeDialogs 的缓存外壳。
// limit 仅用于最终截断（两处调用方实际都等于 maxChatDialogLimit）。
func (a *App) cachedNativeDialogs(ctx context.Context, api *tg.Client, account NativeAccount, limit int, query string, includeArchived bool) ([]map[string]any, error) {
	key := dialogCacheKey(account, query)

	a.dialogCacheMu.Lock()
	entry := a.dialogCache[key]
	if entry != nil {
		select {
		case <-entry.done:
			// 已完成的拉取：TTL 内直接复用；过期则作废重拉。
			if time.Since(entry.at) < dialogCacheTTL {
				a.dialogCacheMu.Unlock()
				return filterCachedDialogs(entry.items, entry.err, limit, includeArchived)
			}
			entry = nil
		default:
			// 拉取进行中：落到下方等待分支。
		}
	}
	if entry == nil {
		entry = &dialogCacheEntry{done: make(chan struct{})}
		if a.dialogCache == nil {
			a.dialogCache = map[string]*dialogCacheEntry{}
		}
		a.dialogCache[key] = entry
		a.dialogCacheMu.Unlock()

		// 拉取固定含归档（超集），请求方按需过滤——loadChats 与 folders 共享同一次拉取。
		pullCtx := context.WithoutCancel(ctx)
		items, err := a.fetchNativeDialogs(pullCtx, api, account, maxChatDialogLimit, query, true)

		a.dialogCacheMu.Lock()
		if err != nil {
			// 失败不缓存：移除条目，下一个请求立即重试。
			if a.dialogCache[key] == entry {
				delete(a.dialogCache, key)
			}
			entry.err = err
		} else {
			entry.items = items
			entry.at = time.Now()
		}
		a.dialogCacheMu.Unlock()
		close(entry.done)
		return filterCachedDialogs(items, err, limit, includeArchived)
	}
	a.dialogCacheMu.Unlock()

	// 等待他人正在进行的拉取（ respects 调用方 ctx，超时/断开即返回）。
	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, classifyNativeReadError(ctx.Err())
	}
	a.dialogCacheMu.Lock()
	defer a.dialogCacheMu.Unlock()
	if entry.err != nil {
		return nil, entry.err
	}
	return filterCachedDialogs(entry.items, nil, limit, includeArchived)
}

// filterCachedDialogs 按调用方的 includeArchived 语义过滤并截断缓存的列表。
func filterCachedDialogs(items []map[string]any, err error, limit int, includeArchived bool) ([]map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxChatDialogLimit {
		limit = maxChatDialogLimit
	}
	if includeArchived {
		if len(items) > limit {
			return items[:limit], nil
		}
		return items, nil
	}
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if archived, _ := item["archived"].(bool); archived {
			continue
		}
		filtered = append(filtered, item)
		if len(filtered) >= limit {
			break
		}
	}
	return filtered, nil
}
