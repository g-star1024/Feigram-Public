package main

// R4.1 · 健康检查自动化
//
// 三件事：
//  1. 登录（含 QR）授权成功后自动触发一次健康检查，用户不再需要手动点按钮；
//  2. 每 30 分钟对已授权账号定时巡检；同账号同时只允许一次检查在跑（去重）；
//  3. 启动时对未就绪账号立即补检（R4.21），让升级后的诊断结论秒级可见。
//
// R4.22 简化（用户明确要求「改简单些」）：删除了「媒体抽样」层——原先没有缓存任务时
// 会自动找一个缩略图、读 64KB 来验证文件池可达。该层自 R4.17 起已不参与健康判定，
// 却是 DC_ID_INVALID / 媒体池问题的最大来源。健康检查就此收敛为
// 「Auth().Status（真实授权 RPC）成功 = 账号健康」，媒体层问题交给下载时的真实错误暴露。
//
// 状态变化的前端推送不在这里做：Go 不持有 socket，由 Node 侧定时拉 /api/state
// 做 diff 后经 socket.io 推送（见 server/src/nativeHealthMonitor.js）。

import (
	"fmt"
	"time"
)

const (
	// healthPatrolInterval 定时巡检间隔。
	healthPatrolInterval = 30 * time.Minute
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

// bootstrapHealthChecks 启动时对「有会话但未就绪」的账号立即各排一次健康检查。
// 背景（R4.21）：升级/重启后，failed 账号原本要等 30 分钟巡检或 3 分钟访问冷却才有补检，
// 用户装新版后短时间看到的仍是升级前落盘的旧错误（如 2.5.4 的 context deadline exceeded），
// 新版诊断（R4.20 分级探测）要再等几分钟才可见——启动即补检，让诊断秒级可见。
func (a *App) bootstrapHealthChecks() {
	time.Sleep(autoHealthCheckDelay)
	for _, account := range a.authorizedAccountSnapshots() {
		if account.Ready && account.Status == "healthy" {
			continue
		}
		a.scheduleAutoHealthCheck(account.UserID, account.AccountID, "启动补检")
	}
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
