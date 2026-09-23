package main

// R4.1 · 健康检查自动化
//
// 三件事：
//  1. 登录（含 QR）授权成功后自动触发一次健康检查，用户不再需要手动点按钮；
//  2. 无缓存任务时，在已授权会话内自动挑一个低体积媒体（优先缩略图）做抽样读取，
//     不再因「请先缓存一个视频」把刚登录的账号顶回去；
//  3. 每 30 分钟对已授权账号定时巡检；同账号同时只允许一次检查在跑（去重）。
//
// 状态变化的前端推送不在这里做：Go 不持有 socket，由 Node 侧定时拉 /api/state
// 做 diff 后经 socket.io 推送（见 server/src/nativeHealthMonitor.js）。

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const (
	// healthPatrolInterval 定时巡检间隔。
	healthPatrolInterval = 30 * time.Minute
	// autoSampleDialogLimit 自动抽样时扫描的最近会话数。
	autoSampleDialogLimit = 15
	// autoHealthCheckDelay 登录成功后延迟再查，避开登录收尾期的 session 落盘竞争。
	autoHealthCheckDelay = 2 * time.Second
	// accessRecheckCooldown 访问未就绪账号触发的补检冷却期：
	// 聊天页/头像请求可能高频撞到同一个 failed 账号，靠冷却避免反复发起真实 RPC。
	accessRecheckCooldown = 3 * time.Minute
)

// scheduleAutoHealthCheck 在登录成功后异步触发一次健康检查。
// 同账号已有检查在跑时直接跳过（去重），避免 goroutine 堆积。
func (a *App) scheduleAutoHealthCheck(userID, accountID string, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.healthRunning == nil {
		a.healthRunning = map[string]bool{}
	}
	key := nativeAccountKey(userID, accountID)
	if a.healthRunning[key] {
		return
	}
	a.healthRunning[key] = true
	go func() {
		time.Sleep(autoHealthCheckDelay)
		defer func() {
			a.mu.Lock()
			delete(a.healthRunning, key)
			a.mu.Unlock()
		}()
		a.runAutoHealthCheck(userID, accountID, reason)
	}()
}

// runAutoHealthCheck 取账号快照执行一次健康检查；失败只落账号状态，不向上抛
// （自动流程没有调用者接收错误）。
func (a *App) runAutoHealthCheck(userID, accountID, reason string) {
	account, err := a.nativeAccountSnapshot(userID, accountID)
	if err != nil {
		return
	}
	if account.Session == "" {
		return
	}
	if _, err := a.nativeHealthCheck(account); err != nil {
		// nativeHealthCheck 已把失败写进账号状态；这里只留一条服务端日志便于排查。
		fmt.Printf("自动健康检查失败（%s，%s/%s）：%v\n", reason, userID, accountID, err)
	}
}

// markNativeAccountResult 记录账号一次真实可用性结果（下载任务成败共用口径）。
// 只做观测记录：成功清零连续失败并刷新最近成功时间；失败累加计数。
// 不改 Status/Ready——那是健康检查的职责，这里只供 /health 退化预警与账号卡展示。
func (a *App) markNativeAccountResult(userID, accountID string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	account, exists := a.native[nativeAccountKey(userID, accountID)]
	if !exists || account == nil {
		return
	}
	if ok {
		account.LastSuccessAt = now()
		account.ConsecutiveFailures = 0
	} else {
		account.ConsecutiveFailures++
	}
	account.UpdatedAt = now()
	_ = a.saveNativeLocked()
}

// healthLoop 每 healthPatrolInterval 对所有已授权账号巡检一次。
func (a *App) healthLoop() {
	ticker := time.NewTicker(healthPatrolInterval)
	defer ticker.Stop()
	for range ticker.C {
		for _, account := range a.authorizedAccountSnapshots() {
			a.scheduleAutoHealthCheck(account.UserID, account.AccountID, "定时巡检")
		}
	}
}

// scheduleAllHealthChecks 对所有已授权账号各排一次健康检查（复用 per-account 去重）。
// 典型场景：代理配置变更后，此前因网络不通被判 failed 的账号大概率已恢复，
// 不应再等最长 30 分钟的定时巡检。
func (a *App) scheduleAllHealthChecks(reason string) {
	for _, account := range a.authorizedAccountSnapshots() {
		a.scheduleAutoHealthCheck(account.UserID, account.AccountID, reason)
	}
}

// shouldRecheckNotReady 判定是否允许对未就绪账号发起一次访问补检（纯函数便于单测）：
// 冷却期内不重复安排，冷却过期后允许再次补检。
func shouldRecheckNotReady(last time.Time, nowTs time.Time) bool {
	return last.IsZero() || nowTs.Sub(last) >= accessRecheckCooldown
}

// maybeRecheckNotReadyAccount 在业务请求撞到「有会话但未就绪」的账号时顺手补一次健康检查。
// 带冷却去重：同账号在 accessRecheckCooldown 内只安排一次，避免高频请求反复触发真实 RPC。
func (a *App) maybeRecheckNotReadyAccount(userID, accountID string) {
	a.mu.Lock()
	if a.accessRecheck == nil {
		a.accessRecheck = map[string]time.Time{}
	}
	key := nativeAccountKey(userID, accountID)
	last, hasLast := a.accessRecheck[key]
	if hasLast && !shouldRecheckNotReady(last, time.Now()) {
		a.mu.Unlock()
		return
	}
	account, exists := a.native[key]
	if !exists || account == nil || account.Session == "" || account.Ready {
		// 没会话的账号重检无意义（需要重新登录）；已就绪的账号无需补检。
		// 两者都不占冷却名额。
		a.mu.Unlock()
		return
	}
	a.accessRecheck[key] = time.Now()
	a.mu.Unlock()
	a.scheduleAutoHealthCheck(userID, accountID, "访问时未就绪重检")
}

func (a *App) authorizedAccountSnapshots() []NativeAccount {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]NativeAccount, 0, len(a.native))
	for _, account := range a.native {
		if account.Session != "" {
			out = append(out, *account)
		}
	}
	return out
}

// findAutoSampleLocation 在已授权会话内自动挑一个低体积媒体：
// 拉最近若干个会话，从其随带消息中选第一条含照片/文档的消息，取缩略图定位
// （缩略图天然只有几十 KB，抽样读 64KB 即可验证媒体 DC 可达）。
// 返回的 location 可直接交给 UploadGetFile。
func (a *App) findAutoSampleLocation(ctx context.Context, client *telegram.Client) (tg.InputFileLocationClass, int, bool) {
	result, err := client.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      autoSampleDialogLimit,
	})
	if err != nil {
		return nil, 0, false
	}
	_, messages, _, _, ok := flattenNativeDialogs(result)
	if !ok {
		return nil, 0, false
	}
	message := pickAutoSampleMessage(messages)
	if message == nil {
		return nil, 0, false
	}
	source, err := mediaLocationFromMessage(message, true)
	if err != nil || source.Location == nil {
		return nil, 0, false
	}
	return source.Location, source.DCID, true
}

// pickAutoSampleMessage 从会话列表随带消息中选第一条含照片或文档的消息。
// 纯函数，便于单测：照片优先（体积小、必有缩略图），其次文档。
func pickAutoSampleMessage(messages []tg.MessageClass) *tg.Message {
	var documentMessage *tg.Message
	for _, entry := range messages {
		message, ok := entry.(*tg.Message)
		if !ok || message == nil {
			continue
		}
		media, ok := message.GetMedia()
		if !ok || media == nil {
			continue
		}
		switch media.(type) {
		case *tg.MessageMediaPhoto:
			return message
		case *tg.MessageMediaDocument:
			if documentMessage == nil {
				documentMessage = message
			}
		}
	}
	return documentMessage
}

// readLocationSample 按 location 从媒体 DC 读取至多 64KB，验证文件池可达。
func (a *App) readLocationSample(ctx context.Context, client *telegram.Client, location tg.InputFileLocationClass, dc int) (int, int, time.Duration, error) {
	started := time.Now()
	api := client.API()
	var invoker telegram.CloseInvoker
	if dc > 0 {
		var err error
		invoker, err = client.MediaOnly(ctx, dc, 1)
		if err != nil {
			return 0, dc, time.Since(started), fmt.Errorf("connect Telegram media DC %d: %w", dc, err)
		}
		defer invoker.Close()
		api = tg.NewClient(invoker)
	}
	result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Location: location,
		Offset:   0,
		Limit:    64 * 1024,
	})
	if err != nil {
		return 0, dc, time.Since(started), err
	}
	chunk, ok := result.(*tg.UploadFile)
	if !ok {
		return 0, dc, time.Since(started), fmt.Errorf("健康检查收到不支持的 Telegram 文件响应：%T", result)
	}
	if len(chunk.Bytes) == 0 {
		return 0, dc, time.Since(started), fmt.Errorf("Telegram 文件健康检查返回空分片")
	}
	return len(chunk.Bytes), dc, time.Since(started), nil
}
