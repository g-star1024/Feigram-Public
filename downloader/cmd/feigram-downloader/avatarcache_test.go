package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R4.64：头像字节缓存行为测试——命中/未命中、single-flight、失败不落缓存。
func TestAvatarCacheHitAndSingleFlight(t *testing.T) {
	app := &App{}
	var fetches atomic.Int64
	fetch := func(ctx context.Context) ([]byte, error) {
		fetches.Add(1)
		time.Sleep(20 * time.Millisecond) // 放大并发窗口，验证 single-flight
		return []byte("avatar-bytes"), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := app.avatarWithCache(context.Background(), "u1|a1|peer1", fetch)
			if err != nil {
				t.Errorf("avatarWithCache 并发请求报错: %v", err)
				return
			}
			if string(data) != "avatar-bytes" {
				t.Errorf("avatarWithCache 返回数据不符: %q", data)
			}
		}()
	}
	wg.Wait()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("single-flight 失效：8 个并发请求实际拉取 %d 次", got)
	}
	// 缓存命中：第二次调用不再走网络。
	if _, err := app.avatarWithCache(context.Background(), "u1|a1|peer1", fetch); err != nil {
		t.Fatalf("缓存命中请求报错: %v", err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("缓存未生效：命中请求仍触发网络拉取（共 %d 次）", got)
	}
}

func TestAvatarCacheFailureNotCached(t *testing.T) {
	app := &App{}
	calls := 0
	failing := func(ctx context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("proxy dead")
	}
	if _, err := app.avatarWithCache(context.Background(), "u1|a1|peer2", failing); err == nil {
		t.Fatal("失败请求应返回错误")
	}
	if _, err := app.avatarWithCache(context.Background(), "u1|a1|peer2", failing); err == nil {
		t.Fatal("失败不应落缓存，第二次也应再次真实拉取并报错")
	}
	if calls != 2 {
		t.Fatalf("失败被错误缓存：期望 2 次真实拉取，实际 %d 次", calls)
	}
	// 恢复后成功结果落缓存。
	data, err := app.avatarWithCache(context.Background(), "u1|a1|peer2", func(ctx context.Context) ([]byte, error) {
		return []byte("ok"), nil
	})
	if err != nil || string(data) != "ok" {
		t.Fatalf("恢复后的拉取应成功: %v", err)
	}
	if _, ok := app.cachedAvatar("u1|a1|peer2"); !ok {
		t.Fatal("成功结果应落缓存")
	}
}

func TestAvatarCacheTTLExpiry(t *testing.T) {
	app := &App{}
	app.storeAvatar("u1|a1|peer3", []byte("stale"))
	// 直接改 fetchedAt 模拟过期（不走 6h 真实等待）。
	app.avatarCacheMu.Lock()
	app.avatarCache["u1|a1|peer3"].fetchedAt = time.Now().Add(-avatarCacheTTL - time.Minute)
	app.avatarCacheMu.Unlock()
	if _, ok := app.cachedAvatar("u1|a1|peer3"); ok {
		t.Fatal("过期条目应视为未命中并被清除")
	}
	if _, ok := app.cachedAvatar("u1|a1|peer3"); ok {
		t.Fatal("过期条目清除后不应再次命中")
	}
}
