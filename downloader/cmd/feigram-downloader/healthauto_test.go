package main

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestPickAutoSampleMessage(t *testing.T) {
	// 照片优先于文档，即使文档先出现。
	docMessage := &tg.Message{ID: 1}
	docMessage.SetMedia(&tg.MessageMediaDocument{})
	photoMessage := &tg.Message{ID: 2}
	photoMessage.SetMedia(&tg.MessageMediaPhoto{})
	got := pickAutoSampleMessage([]tg.MessageClass{docMessage, photoMessage})
	if got == nil || got.ID != 2 {
		t.Fatalf("照片应优先被选中，got=%v", got)
	}

	// 只有文档时选第一条文档。
	got = pickAutoSampleMessage([]tg.MessageClass{docMessage})
	if got == nil || got.ID != 1 {
		t.Fatalf("无照片时应选文档消息，got=%v", got)
	}

	// 全部无媒体时返回 nil。
	empty := &tg.Message{ID: 3}
	if got := pickAutoSampleMessage([]tg.MessageClass{empty}); got != nil {
		t.Fatalf("无媒体消息应返回 nil，got=%v", got)
	}
}

// 自动健康检查调度的去重只依赖 healthRunning 字段：
// 同账号二次调度应被跳过，不同账号各自登记。
func TestScheduleAutoHealthCheckDedup(t *testing.T) {
	app := &App{
		native: map[string]*NativeAccount{},
		logins: map[string]*NativeLogin{},
	}
	app.scheduleAutoHealthCheck("u", "a", "测试1")
	app.scheduleAutoHealthCheck("u", "a", "重复") // 应被去重
	app.scheduleAutoHealthCheck("u", "b", "测试2")
	app.mu.Lock()
	if !app.healthRunning["u|a"] {
		t.Fatal("首次调度应登记在跑状态")
	}
	if !app.healthRunning["u|b"] {
		t.Fatal("不同账号的调度不应被去重")
	}
	app.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
}

func TestAuthorizedAccountSnapshots(t *testing.T) {
	app := &App{
		native: map[string]*NativeAccount{
			"u/a": {UserID: "u", AccountID: "a", Session: "s1"},
			"u/b": {UserID: "u", AccountID: "b", Session: ""},
		},
	}
	list := app.authorizedAccountSnapshots()
	if len(list) != 1 || list[0].AccountID != "a" {
		t.Fatalf("只应返回已授权账号，got=%v", list)
	}
}

func TestHealthPatrolIntervalBounded(t *testing.T) {
	if healthPatrolInterval <= 0 || healthPatrolInterval > time.Hour {
		t.Fatalf("巡检间隔应在 (0, 1h] 内，got=%v", healthPatrolInterval)
	}
}

// 访问未就绪账号的补检冷却：冷却期内不重复安排，过期后允许再次补检。
func TestShouldRecheckNotReadyCooldown(t *testing.T) {
	nowTs := time.Now()
	if !shouldRecheckNotReady(time.Time{}, nowTs) {
		t.Fatal("从未补检过的账号应允许补检")
	}
	if shouldRecheckNotReady(nowTs.Add(-time.Minute), nowTs) {
		t.Fatal("冷却期内不应重复补检")
	}
	if !shouldRecheckNotReady(nowTs.Add(-accessRecheckCooldown-time.Second), nowTs) {
		t.Fatal("冷却过期后应允许再次补检")
	}
}

// maybeRecheckNotReadyAccount 对「无记录 / 无会话 / 已就绪」的账号应是无害 no-op，
// 不占用冷却名额（这些场景补检无意义或需重新登录）。
func TestMaybeRecheckNotReadyNoopCases(t *testing.T) {
	app := &App{
		proxy:  &proxyRuntime{},
		native: map[string]*NativeAccount{},
	}
	// 账号不存在。
	app.maybeRecheckNotReadyAccount("u", "missing")
	// 无会话（需要重新登录，而非健康检查）。
	app.native["u/a"] = &NativeAccount{UserID: "u", AccountID: "a", Session: "", Ready: false}
	app.maybeRecheckNotReadyAccount("u", "a")
	// 已就绪。
	app.native["u/b"] = &NativeAccount{UserID: "u", AccountID: "b", Session: "s", Ready: true}
	app.maybeRecheckNotReadyAccount("u", "b")

	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.accessRecheck) != 0 {
		t.Fatalf("no-op 场景不应占用冷却名额，got %v", app.accessRecheck)
	}
}

// 有会话但未就绪的账号应被登记冷却并进入补检调度。
func TestMaybeRecheckNotReadySchedules(t *testing.T) {
	app := &App{
		proxy:  &proxyRuntime{},
		native: map[string]*NativeAccount{},
	}
	app.native["u|c"] = &NativeAccount{UserID: "u", AccountID: "c", Session: "s", Ready: false, Status: "failed"}
	app.maybeRecheckNotReadyAccount("u", "c")

	app.mu.Lock()
	_, cooled := app.accessRecheck["u|c"]
	_, running := app.healthRunning["u|c"]
	app.mu.Unlock()
	if !cooled {
		t.Fatal("补检应登记冷却时间")
	}
	if !running {
		t.Fatal("补检应进入在跑调度")
	}
	time.Sleep(60 * time.Millisecond)
}
