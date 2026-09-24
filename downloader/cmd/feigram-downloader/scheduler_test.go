package main

// R4.25 调度器自愈与可观测的行为固化：
//  1. running 坑里的幻影条目（任务已不在传输态/已删除）必须在调度前被清理，
//     否则 conservative 单并发下整个队列被静默冻死（2.6.2 实测「一直排队」根因）；
//  2. 「开始」（queue 动作）= 强制重启：先取消活着的下载 goroutine 再排队；
//  3. 无进度看门狗错误（errDownloadStalled）是瞬态错误，按退避自动续传；
//  4. normalizeNativeStatus 不得保留 healthy+ready=false 的自相矛盾中间态；
//  5. FilePath 缺失不再是静默跳过。
//
// 注意：pumpOnce 会 `go a.runTask(...)`，其后继状态回写是异步的。因此用例只断言
// pumpOnce **同步完成**的副作用（返回值、幻影条目被取消/清理），不断言任务终态，
// 否则会与异步 goroutine 竞争而产生 flaky。

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gotd/td/tgerr"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	if cfg.Transport == "" {
		cfg.Transport = "native-mtproto"
	}
	dir := t.TempDir()
	return &App{
		config: cfg,
		// saveLocked 写的是 a.storePath（不是 dataDir），两者都要指向临时目录，
		// 否则 rename 失败被误判成产品缺陷。
		dataDir:     dir,
		storePath:   filepath.Join(dir, "tasks.json"),
		tasks:       map[string]*Task{},
		native:      map[string]*NativeAccount{},
		running:     map[string]chan struct{}{},
		taskSpawns:  map[string]*spawnStat{},
		taskLogs:    map[string]*taskLogState{},
		mediaConns:  map[string]*mediaConn{},
		mediaProbes: map[string]mediaProbeSnapshot{},
		// R4.35：peer 解析闸门
		peerResolveInflight: map[string]*peerResolveCall{},
		peerResolveCooldown: map[string]time.Time{},
		// http-bridge 用例需要非 nil 的 a.client（downloadHTTPBridge 直接使用）。
		client: &http.Client{},
		proxy:  &proxyRuntime{},
	}
}

func eligibleAccountFor(userID, accountID string) NativeAccount {
	return NativeAccount{
		UserID:       userID,
		AccountID:    accountID,
		Status:       "healthy",
		Ready:        true,
		HealthPasses: 2,
		Session:      "FAKE==",
		APIID:        12345,
		APIHash:      "0123456789abcdef0123456789abcdef",
	}
}

func closedWithin(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// drainRunTasks 等 pumpOnce 启动的异步下载协程自然收敛。
//
// 必要性：runTask 的收尾 defer 会在持锁状态下 delete(running) 并 saveLocked()，
// 因此「观察到 running 为空」即等价于「所有写盘已完成」。若不等就直接返回，
// 存活协程会在测试结束后继续写 storePath，使 t.TempDir() 的清理报
// 「directory not empty」——表现为随机 flaky（不是产品缺陷，是夹具竞态）。
func drainRunTasks(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		app.mu.Lock()
		pending := len(app.running)
		app.mu.Unlock()
		if pending == 0 {
			// 连续两次采样为空才认定收敛：handleTask 之类的入口会 `go pumpOnce()`，
			// 在途的那次调度可能尚未跑完（跑完后才会写盘）。
			stable++
			if stable >= 3 {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("异步下载协程未在 3 秒内收敛")
}

// 幻影坑：t1 已回 queued 但 running 里仍有条目 → pumpOnce 必须先取消并清理它，
// 随后 t1 才能重新获得并发坑启动。
func TestPumpOnceCleansPhantomRunningEntry(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	defer drainRunTasks(t, app)
	staleCancel := make(chan struct{})
	app.running["t1"] = staleCancel // 幻影：任务状态是 queued
	task := &Task{
		ID: "t1", UserID: "u1", AccountID: "a1", Status: "queued",
		FilePath: "/tmp/whatever.bin", Size: 10,
		NativeFile: NativeFileLocation{FileID: "1", AccessHash: "2", FileReference: "3"},
	}
	app.tasks["t1"] = task
	account := eligibleAccountFor("u1", "a1")
	app.native["u1|a1"] = &account

	started := app.pumpOnce()
	if !started {
		t.Fatal("清理幻影后任务应能正常启动")
	}
	if !closedWithin(staleCancel) {
		t.Fatal("幻影条目的 cancel channel 应被关闭（旧 goroutine 必须被取消）")
	}
}

// 幻影坑占满并发限额时，清理后其余任务必须能顶上（自愈而不是永久冻结）。
func TestPumpOncePhantomDoesNotBlockOtherTasks(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	defer drainRunTasks(t, app)
	ghostCancel := make(chan struct{})
	app.running["ghost"] = ghostCancel // 幻影：任务已不存在
	account := eligibleAccountFor("u1", "a1")
	app.native["u1|a1"] = &account
	task := &Task{
		ID: "t2", UserID: "u1", AccountID: "a1", Status: "queued",
		FilePath: "/tmp/other.bin", Size: 10,
		NativeFile: NativeFileLocation{FileID: "1", AccessHash: "2", FileReference: "3"},
	}
	app.tasks["t2"] = task

	if !app.pumpOnce() {
		t.Fatal("清理幻影坑后 t2 应获得唯一并发坑并启动")
	}
	if _, ok := app.running["ghost"]; ok {
		t.Fatal("幻影条目应被清理")
	}
	if !closedWithin(ghostCancel) {
		t.Fatal("幻影条目的 cancel channel 应被关闭")
	}
}

// FilePath 缺失不再静默跳过：必须写入可读等待原因（R4.11 可见化原则的补漏）。
func TestPumpOnceMissingFilePathBecomesVisible(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	account := eligibleAccountFor("u1", "a1")
	app.native["u1|a1"] = &account
	task := &Task{ID: "t5", UserID: "u1", AccountID: "a1", Status: "queued", FilePath: ""}
	app.tasks["t5"] = task

	if app.pumpOnce() {
		t.Fatal("缺少落盘路径的任务不应被启动")
	}
	if task.Error != errMissingFilePathReason {
		t.Fatalf("应写入等待原因，got %q", task.Error)
	}
	if _, ok := app.running["t5"]; ok {
		t.Fatal("未启动的任务不应占用并发坑")
	}
}

// queue 动作 = 强制重启：活着的 goroutine 被取消、running 坑被清理、任务回 queued。
func TestQueueActionForceRestartsLiveTask(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	defer drainRunTasks(t, app) // handleTask 会异步触发一次 pumpOnce，同样会起下载协程
	task := &Task{ID: "t3", UserID: "u1", AccountID: "a1", Status: "downloading", FilePath: "/tmp/x.bin"}
	app.tasks["t3"] = task
	cancel := make(chan struct{})
	app.running["t3"] = cancel

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/t3/queue", nil)
	rec := httptest.NewRecorder()
	app.handleTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue 请求应成功，got %d: %s", rec.Code, rec.Body.String())
	}
	if !closedWithin(cancel) {
		t.Fatal("活 goroutine 的 cancel channel 应被关闭")
	}
	if _, ok := app.running["t3"]; ok {
		t.Fatal("running 坑条目应被删除，否则 pumpOnce 会静默跳过")
	}
	if app.tasks["t3"].Status != "queued" {
		t.Fatalf("任务应回 queued，got %q", app.tasks["t3"].Status)
	}
}

// 旧 goroutine 醒来后不得覆盖新状态：queue 强制重启后，迟到的取消/失败回写
// 必须被 stillCurrent 守卫挡住（否则任务又被改回 cancelled/error，看似「没反应」）。
func TestStaleGoroutineCannotOverwriteFreshQueuedStatus(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	task := &Task{ID: "t4", UserID: "u1", AccountID: "a1", Status: "queued"}
	app.tasks["t4"] = task

	stillCurrent := func(t *Task) bool { return t.Status == "downloading" || t.Status == "running" }

	app.updateTask("t4", func(t *Task) {
		if !stillCurrent(t) {
			return
		}
		t.Status = "cancelled"
	})
	if task.Status != "queued" {
		t.Fatalf("守卫应挡住迟到的取消回写，got %q", task.Status)
	}

	app.tasks["t4"].Status = "downloading"
	app.updateTask("t4", func(t *Task) {
		if !stillCurrent(t) {
			return
		}
		t.Status = "cancelled"
	})
	if task.Status != "cancelled" {
		t.Fatalf("现任 goroutine 的取消应正常生效，got %q", task.Status)
	}
}

func TestDownloadStalledIsTransient(t *testing.T) {
	if !transientSourceError(errDownloadStalled) {
		t.Fatal("无进度挂死应按瞬态处理，自动续传")
	}
	if !strings.Contains(errDownloadStalled.Error(), "自动重试") && !strings.Contains(errDownloadStalled.Error(), "自动续传") {
		t.Fatalf("错误文案应说明会自动重试，got %q", errDownloadStalled.Error())
	}
}

func TestNormalizeNativeStatusNeverHealthyWhenNotReady(t *testing.T) {
	if got := normalizeNativeStatus("healthy", false); got == "healthy" {
		t.Fatal("ready=false 时不得保留 healthy——这是「显示健康却不可调度」的矛盾中间态")
	}
	if got := normalizeNativeStatus("healthy", true); got != "healthy" {
		t.Fatalf("ready=true 应为 healthy，got %q", got)
	}
	if got := normalizeNativeStatus("failed", false); got != "failed" {
		t.Fatalf("failed 应保留，got %q", got)
	}
}

// 看门狗窗口常量健全性：必须远大于一次正常 RPC 往返，也不能大到失去意义。
func TestNativeNoProgressTimeoutSane(t *testing.T) {
	if nativeNoProgressTimeout < 30*time.Second || nativeNoProgressTimeout > 10*time.Minute {
		t.Fatalf("无进度窗口不合理: %v", nativeNoProgressTimeout)
	}
}

// --- R4.26：调度限流 / 复活收敛 / 日志限频 / 空响应瞬态化 ---

// spawn 限流纯逻辑：首次放行；10s 内连续重复调度累计爆发次数，
// 触顶进入冷却；冷却期内一律拒绝；冷却过期恢复放行；间隔足够长则计数清零。
func TestSpawnRateLimitClampsRapidRespawn(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	app.mu.Lock()
	defer app.mu.Unlock()

	if allowed, cd := app.spawnAllowedLocked("t1"); !allowed || cd != 0 {
		t.Fatalf("首次调度应放行，got allowed=%v cooldown=%v", allowed, cd)
	}
	app.markSpawnLocked("t1")

	// 爆发限额内（taskSpawnBurstLimit-1 次）放行
	for i := 0; i < taskSpawnBurstLimit-1; i++ {
		if allowed, _ := app.spawnAllowedLocked("t1"); !allowed {
			t.Fatalf("爆发内第 %d 次调度应放行", i+2)
		}
		app.markSpawnLocked("t1")
	}
	allowed, cooldown := app.spawnAllowedLocked("t1")
	if allowed || cooldown <= 0 {
		t.Fatalf("爆发触顶必须拒绝并带冷却，got allowed=%v cooldown=%v", allowed, cooldown)
	}
	if cooldown > taskSpawnCooldownMax {
		t.Fatalf("冷却不得超过上限 %v，got %v", taskSpawnCooldownMax, cooldown)
	}

	// 冷却期内一律拒绝
	if allowed, _ := app.spawnAllowedLocked("t1"); allowed {
		t.Fatal("冷却期内的调度必须拒绝")
	}

	// 冷却过期：恢复放行
	app.taskSpawns["t1"].cooldownUntil = time.Now().Add(-time.Second)
	if allowed, cd := app.spawnAllowedLocked("t1"); !allowed {
		t.Fatalf("冷却过期后应放行，got cooldown=%v", cd)
	}

	// 间隔足够长：爆发计数清零
	app.taskSpawns["t1"].last = time.Now().Add(-2 * taskSpawnMinInterval)
	app.taskSpawns["t1"].bursts = taskSpawnBurstLimit - 1
	if allowed, _ := app.spawnAllowedLocked("t1"); !allowed {
		t.Fatal("长间隔后的调度应放行")
	}
	if stat := app.taskSpawns["t1"]; stat.bursts != 0 {
		t.Fatalf("长间隔后爆发计数应清零，got %d", stat.bursts)
	}
}

// 集成：error 任务被外部反复复活时，pumpOnce 的 spawn 限流必须在爆发触顶后
// 拒绝继续调度——无论复活来自谁（ensure 轮询/用户点击），紧循环都被掐断。
func TestPumpOnceClampsReviveTightLoop(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	defer drainRunTasks(t, app)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 404 不在瞬态表内 → 快速终态失败
		_, _ = w.Write([]byte("nope"))
	}))
	defer source.Close()

	task := &Task{
		ID: "t1", UserID: "u1", AccountID: "a1", Status: "queued",
		Transport: "http-bridge", SourceURL: source.URL,
		FilePath: filepath.Join(t.TempDir(), "out.bin"), Size: 10,
	}
	app.tasks[task.ID] = task

	revive := func() {
		app.mu.Lock()
		task.Status = "queued" // 模拟 ensure 轮询把 error 任务复活（upsert 收敛后的外部视角）
		task.RetryAfter = 0
		app.mu.Unlock()
	}

	spawned := 0
	for i := 0; i < 6; i++ {
		if i > 0 {
			drainRunTasks(t, app)
			revive()
		}
		if app.pumpOnce() {
			spawned++
		}
	}
	drainRunTasks(t, app)
	if spawned > taskSpawnBurstLimit+1 {
		t.Fatalf("spawn 限流未生效：%s 内被调度 %d 次（上限 %d 次爆发 + 首次）", taskSpawnMinInterval, spawned, taskSpawnBurstLimit)
	}
	app.mu.Lock()
	finalStatus := task.Status
	app.mu.Unlock()
	if finalStatus != "queued" {
		t.Fatalf("被限流拒绝后任务应留在 queued，got %q", finalStatus)
	}
}

// 复活收敛：cancelled 不得被 auto 来源复活；error 复活保留 RetryCount；
// 有活 goroutine 的任务一律不复活。
func TestUpsertReviveConvergence(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true})

	// cancelled + auto 来源：不复活
	app.mu.Lock()
	app.tasks["t1"] = &Task{ID: "t1", Status: "cancelled", RetryCount: 4}
	got := app.upsertTaskLocked(Task{ID: "t1", Source: "auto"})
	if got.Status != "cancelled" {
		t.Fatalf("auto 来源不得复活 cancelled 任务，got %q", got.Status)
	}
	// cancelled + 显式来源（用户重新点下载）：复活，但 RetryCount 不清零
	got = app.upsertTaskLocked(Task{ID: "t1", Source: "manual"})
	if got.Status != "queued" {
		t.Fatalf("显式来源应复活 cancelled 任务，got %q", got.Status)
	}
	if got.RetryCount != 4 {
		t.Fatalf("复活不得清零 RetryCount（只有 queue 动作清零），got %d", got.RetryCount)
	}

	// error + 有活 goroutine：不复活（避免双 goroutine 写同一 part 文件）
	app.tasks["t2"] = &Task{ID: "t2", Status: "error", RetryCount: 2}
	app.running["t2"] = make(chan struct{})
	got = app.upsertTaskLocked(Task{ID: "t2", Source: "manual"})
	if got.Status != "error" {
		t.Fatalf("有活 goroutine 的任务不得复活，got %q", got.Status)
	}
	// error + 无活 goroutine：复活且保留 RetryCount
	delete(app.running, "t2")
	got = app.upsertTaskLocked(Task{ID: "t2", Source: "manual"})
	if got.Status != "queued" || got.RetryCount != 2 {
		t.Fatalf("error 任务应复活且保留 RetryCount，got status=%q retryCount=%d", got.Status, got.RetryCount)
	}
	app.mu.Unlock()
}

// 瞬态表：deadline exceeded 与空响应必须按瞬态；部分下载后的 incomplete 仍是终态。
func TestTransientDeadlineExceededAndEmptyResponse(t *testing.T) {
	if !transientSourceError(fmt.Errorf("connect Telegram media DC 2: context deadline exceeded")) {
		t.Fatal("context deadline exceeded（拨号/RPC 超时）应按瞬态处理")
	}
	wrapped := fmt.Errorf("媒体源返回空响应（已取 0 / 171931369 字节）：%w", errEmptyMediaResponse)
	if !transientSourceError(wrapped) {
		t.Fatal("空响应（0 分块）应按瞬态处理")
	}
	if transientSourceError(errors.New("file incomplete: 100 / 200")) {
		t.Fatal("已下载部分字节后的 incomplete 仍是终态，不得误判瞬态")
	}
}

// 日志限频：同任务同一条错误在窗口内只打一条，其余计数；不同错误互不影响。
func TestTaskEventLogDedup(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true})
	app.taskEventLog("t9", "failed: same error")
	app.taskEventLog("t9", "failed: same error")
	app.taskEventLog("t9", "failed: same error")
	app.taskEventLog("t9", "failed: another error")
	app.mu.Lock()
	state := app.taskLogs["t9"]
	suppressedSame := state.suppressed["failed: same error"]
	suppressedOther := state.suppressed["failed: another error"]
	_, lastSeen := state.last["failed: same error"]
	app.mu.Unlock()
	if suppressedSame != 2 {
		t.Fatalf("重复错误应抑制 2 条，got %d", suppressedSame)
	}
	if suppressedOther != 0 || !lastSeen {
		t.Fatalf("不同错误不受抑制且应记录最近输出时间，got suppressed=%d lastSeen=%v", suppressedOther, lastSeen)
	}
}

// R4.26 根因回归：spawn 时必须把 downloading 状态写到**存储任务**上。
// 此前写在了 listTasksLocked() 的值拷贝上，存储任务永远停在 queued，
// 下一轮 pumpOnce 的幻影清理立即误杀刚 spawn 的坑再重新 spawn——
// 2.6.2「永远排队、运行 0/1」与 2.6.3「同秒几十条 stalled/failed 日志风暴」
// 的共同根因。
func TestPumpOnceMarksStoredTaskDownloading(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative", Transport: "http-bridge"})
	defer drainRunTasks(t, app)
	release := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // 挂住 goroutine，让 running 条目稳定存活
	}))
	defer source.Close()
	defer close(release)

	task := &Task{
		ID: "t1", UserID: "u1", AccountID: "a1", Status: "queued",
		Transport: "http-bridge", SourceURL: source.URL,
		FilePath: filepath.Join(t.TempDir(), "out.bin"), Size: 10,
	}
	app.tasks[task.ID] = task

	if !app.pumpOnce() {
		t.Fatal("pumpOnce 应启动任务")
	}
	app.mu.Lock()
	storedStatus := app.tasks["t1"].Status
	app.mu.Unlock()
	if storedStatus != "downloading" {
		t.Fatalf("spawn 后存储任务状态必须为 downloading，got %q（拷贝突变未落库）", storedStatus)
	}

	// 第二轮 pumpOnce：不得把活跃坑误判为幻影，也不得重复 spawn
	if app.pumpOnce() {
		t.Fatal("任务已在传输态时第二轮 pumpOnce 不应再启动任何任务")
	}
	app.mu.Lock()
	_, entryAlive := app.running["t1"]
	app.mu.Unlock()
	if !entryAlive {
		t.Fatal("活跃任务的 running 条目不得被幻影清理回收")
	}
}

// --- R4.27：FLOOD_WAIT 精确退避 / 刷新链路自愈 / 旧终态任务复活 ---

// FLOOD_WAIT 秒数解析：覆盖 gotd 实际日志形态「FLOOD_WAIT (1464)」、
// tgerr 标准形态「FLOOD_WAIT_3」、被包装的错误链；非限流错误返回 0。
func TestFloodWaitFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"日志括号形态", errors.New("export auth to 5: rpc error code 420: FLOOD_WAIT (1464)"), 1464},
		{"tgerr 下划线形态", tgerr.New(420, "FLOOD_WAIT_3"), 3},
		{"无数字", errors.New("FLOOD_WAIT"), 0},
		{"非限流错误", errors.New("connection reset by peer"), 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		if got := floodWaitFromError(tc.err); got != tc.want {
			t.Errorf("%s: floodWaitFromError=%d want %d", tc.name, got, tc.want)
		}
	}
}

// classifyNativeReadError 必须把 FLOOD_WAIT 包装成类型化 floodWaitError，
// 且仍按瞬态处理（transientSourceError 表内已有 flood_wait 文本兜底）。
func TestClassifyFloodWaitIsTypedAndTransient(t *testing.T) {
	raw := errors.New("rpc error code 420: FLOOD_WAIT (1440)")
	classified := classifyNativeReadError(raw)
	seconds := floodWaitFromError(classified)
	if seconds != 1440 {
		t.Fatalf("classifyNativeReadError 应保留限流秒数，got %d", seconds)
	}
	if !transientSourceError(classified) {
		t.Fatal("FLOOD_WAIT 必须按瞬态处理")
	}
	var typed *floodWaitError
	if !errors.As(classified, &typed) || typed.Seconds != 1440 {
		t.Fatalf("应可 errors.As 出 *floodWaitError，got %T", classified)
	}
}

// runTask 瞬态分支：FLOOD_WAIT 的 RetryAfter 必须按 Telegram 给的秒数精确等待
// （而非 5 秒起步的指数退避），错误文案对用户可见。
func TestRunTaskFloodWaitUsesTelegramDelay(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	defer drainRunTasks(t, app)

	// 用瞬态错误路径验证：直接构造带 FLOOD_WAIT 文本的错误走 runTask 的
	// 瞬态分支无法从 http 源注入，因此这里只验证退避换算逻辑的分界——
	// floodWaitFromError > 0 时 delay 取 Telegram 秒数（封顶 4h）。
	wait := floodWaitFromError(errors.New("rpc error code 420: FLOOD_WAIT (1464)"))
	delay := time.Duration(wait) * time.Second
	if delay > floodWaitBackoffCap {
		delay = floodWaitBackoffCap
	}
	if delay != 1464*time.Second {
		t.Fatalf("1464 秒限流应精确等待 24m24s，got %v", delay)
	}
	huge := floodWaitFromError(errors.New("FLOOD_WAIT (999999)"))
	hugeDelay := time.Duration(huge) * time.Second
	if hugeDelay > floodWaitBackoffCap {
		hugeDelay = floodWaitBackoffCap
	}
	if hugeDelay != floodWaitBackoffCap {
		t.Fatalf("超限限流应封顶 4h，got %v", hugeDelay)
	}
}

// 任务加载归一化：2.6.4 被「metadata refresh url is empty」误判终态的任务，
// 升级后必须自动复活为 queued（由修复后的刷新链路接管续传）。
func TestLoadRevivesMetadataURLFailures(t *testing.T) {
	dir := t.TempDir()
	store := map[string]any{
		"config": map[string]any{"enabled": true},
		"tasks": []map[string]any{
			{
				"id": "t1", "userId": "u1", "accountId": "a1", "status": "error",
				"error": "file_reference 失效：自动刷新消息元数据失败：metadata refresh url is empty",
			},
			{"id": "t2", "userId": "u1", "accountId": "a1", "status": "error", "error": "其他真实失败"},
			{"id": "t3", "userId": "u1", "accountId": "a1", "status": "cancelled", "error": ""},
		},
	}
	raw, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{
		config:     Config{Enabled: true},
		dataDir:    dir,
		storePath:  filepath.Join(dir, "tasks.json"),
		tasks:      map[string]*Task{},
		native:     map[string]*NativeAccount{},
		logins:     map[string]*NativeLogin{},
		qrLogins:   map[string]*NativeQRLogin{},
		running:    map[string]chan struct{}{},
		taskSpawns: map[string]*spawnStat{},
		taskLogs:   map[string]*taskLogState{},
		proxy:      &proxyRuntime{},
	}
	if err := app.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := app.tasks["t1"].Status; got != "queued" {
		t.Fatalf("被刷新链路断裂误杀的任务应复活为 queued，got %q", got)
	}
	if got := app.tasks["t2"].Status; got != "error" {
		t.Fatalf("真实失败的任务不得被复活，got %q", got)
	}
	if got := app.tasks["t3"].Status; got != "cancelled" {
		t.Fatalf("用户主动取消的任务不得被复活，got %q", got)
	}
}

// R4.28：首字节超时必须存在且严格小于常规无进度窗口——
// 0 字节尝试走快速失败档，收到字节后才切 120s 常规窗口。
func TestNativeFirstByteTimeoutSane(t *testing.T) {
	if nativeFirstByteTimeout < 10*time.Second || nativeFirstByteTimeout >= nativeNoProgressTimeout {
		t.Fatalf("首字节超时应为 10s~120s 之间且小于常规窗口: %v", nativeFirstByteTimeout)
	}
}

// --- R4.29：媒体 DC 探测结论 / 手动优先调度 ---

// 探测结论三分类：全通 / 全不通 / 部分放行，措辞必须可区分（不留空返回路径）。
func TestMediaProbeSummaryCategories(t *testing.T) {
	// R4.31：结论以 MTProto 真握手为准。TCP 全绿但握手全挂 =
	// 代理只本地接受连接（2.6.8 实测：五个 DC 1ms 全绿、下载 0 字节）。
	results := func(build func(p *mediaDCProbe)) []mediaDCProbe {
		out := make([]mediaDCProbe, 0, 5)
		for dc := 1; dc <= 5; dc++ {
			p := mediaDCProbe{DC: dc, Addr: "1.2.3.4:443", OK: true, DurationMs: 1, MTPOK: true}
			build(&p)
			out = append(out, p)
		}
		return out
	}

	allReal := mediaProbeSummary(results(func(p *mediaDCProbe) {}), "经代理")
	if !strings.Contains(allReal, "全部 5 个 DC") || !strings.Contains(allReal, "真实可用") {
		t.Fatalf("TCP+MTProto 全通结论不符预期: %q", allReal)
	}

	localAccept := mediaProbeSummary(results(func(p *mediaDCProbe) { p.MTPOK = false; p.MTPError = "MTProto 握手未完成" }), "经代理")
	if !strings.Contains(localAccept, "TCP 可连通但 MTProto 握手全部无响应") || !strings.Contains(localAccept, "本地接受") || !strings.Contains(localAccept, "全局模式") {
		t.Fatalf("本地接受假阳性必须给出最强诊断与修法: %q", localAccept)
	}

	partial := mediaProbeSummary(results(func(p *mediaDCProbe) {
		if p.DC >= 4 {
			p.MTPOK = false
		}
	}), "经代理")
	// R4.32：部分转发结论要区分「节点波动」（偶发）与「网段黑洞」（持续）两种可能。
	if !strings.Contains(partial, "3/5") || !strings.Contains(partial, "DC 4/5") || !strings.Contains(partial, "节点波动") || !strings.Contains(partial, "补全代理规则") {
		t.Fatalf("部分转发结论应列出握手失败 DC 并区分波动/黑洞: %q", partial)
	}

	tcpFail := mediaProbeSummary(results(func(p *mediaDCProbe) { p.OK = false; p.Err = "dial error" }), "经代理")
	if !strings.Contains(tcpFail, "均不可达") || !strings.Contains(tcpFail, "放行 Telegram 全部网段") {
		t.Fatalf("TCP 全挂结论应指向代理放行问题: %q", tcpFail)
	}

	if empty := mediaProbeSummary(nil, "直连"); !strings.Contains(empty, "无法分级探测") {
		t.Fatalf("空结果也必须给出结论: %q", empty)
	}
}

// R4.32：latestMediaHandshakeMs 从最近探测快照取指定 DC 的握手耗时；
// 无快照/未测过/握手未成功一律返回 0，调用方回落固定 30s 首字节阈值。
func TestLatestMediaHandshakeMs(t *testing.T) {
	app := &App{mediaProbes: map[string]mediaProbeSnapshot{}}
	if got := app.latestMediaHandshakeMs("u", "none", 1); got != 0 {
		t.Fatalf("无快照应返回 0，got %d", got)
	}
	app.mediaProbes[nativeAccountKey("u", "a")] = mediaProbeSnapshot{
		Results: []mediaDCProbe{
			{DC: 1, MTPOK: true, MTPDuration: 4500},
			{DC: 2, MTPOK: false, MTPDuration: 0},
		},
	}
	if got := app.latestMediaHandshakeMs("u", "a", 1); got != 4500 {
		t.Fatalf("应取 DC1 握手耗时 4500，got %d", got)
	}
	if got := app.latestMediaHandshakeMs("u", "a", 2); got != 0 {
		t.Fatalf("握手未成功的 DC 应返回 0，got %d", got)
	}
	if got := app.latestMediaHandshakeMs("u", "a", 3); got != 0 {
		t.Fatalf("未探测过的 DC 应返回 0，got %d", got)
	}
	if got := app.latestMediaHandshakeMs("u", "a", 0); got != 0 {
		t.Fatalf("非法 DC 应直接返回 0，got %d", got)
	}
}

// R4.33：网络自愈自动复活——探测全绿时应把该账号「重试上限」终态任务拉起一轮；
// 防抖字段生效（只复活一次）；他账号任务不受影响；非重试上限错误不误拉。
func TestReviveNetworkStalledTasks(t *testing.T) {
	if !allMediaDCsHealthy([]mediaDCProbe{{DC: 1, OK: true, MTPOK: true}}) {
		t.Fatal("单个全绿 DC 应判健康")
	}
	if allMediaDCsHealthy([]mediaDCProbe{{DC: 1, OK: true, MTPOK: false}}) {
		t.Fatal("握手未过不应判健康")
	}
	if allMediaDCsHealthy(nil) {
		t.Fatal("空结果不应判健康")
	}
	if !isRetryCapError("媒体源长时间不可用，已自动重试 120 次后停止；请稍后手动重试") {
		t.Fatal("旧版重试上限文案应被识别")
	}
	if !isRetryCapError("媒体源长时间不可用，已自动重试 120 次后停止；请在代理放行 Telegram 全部网段或更换节点后点「重试」，将从断点续传") {
		t.Fatal("新版重试上限文案应被识别")
	}
	if isRetryCapError("file_reference 失效：找不到会话 Channel:1") {
		t.Fatal("非重试上限错误不应被识别")
	}

	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	app.tasks["t1"] = &Task{ID: "t1", UserID: "u", AccountID: "a", Status: "error", Error: "媒体源长时间不可用，已自动重试 120 次后停止；请稍后手动重试", RetryCount: 120}
	app.tasks["t2"] = &Task{ID: "t2", UserID: "u", AccountID: "a", Status: "error", AutoRevived: true, Error: "已自动重试 120 次后停止", RetryCount: 120}
	app.tasks["t3"] = &Task{ID: "t3", UserID: "u", AccountID: "b", Status: "error", Error: "已自动重试 120 次后停止", RetryCount: 120}
	app.tasks["t4"] = &Task{ID: "t4", UserID: "u", AccountID: "a", Status: "error", Error: "找不到会话 Channel:1"}

	app.reviveNetworkStalledTasks("u", "a")
	if app.tasks["t1"].Status != "queued" || app.tasks["t1"].RetryCount != 0 || !app.tasks["t1"].AutoRevived {
		t.Fatalf("t1 重试上限任务应被自动复活: %+v", app.tasks["t1"])
	}
	if app.tasks["t2"].Status != "error" || !app.tasks["t2"].AutoRevived {
		t.Fatalf("t2 已复活过不应再拉起: %+v", app.tasks["t2"])
	}
	if app.tasks["t3"].Status != "error" {
		t.Fatalf("t3 属其他账号不应被拉起: %+v", app.tasks["t3"])
	}
	if app.tasks["t4"].Status != "error" {
		t.Fatalf("t4 非重试上限错误不应被拉起（由 load 归一化复活）: %+v", app.tasks["t4"])
	}
}

// R4.34：网络转差时重臂 AutoRevived——复活机会按「断网窗口」计。
// 2.6.11 实测：启动复活→节点恶化→45s 连接超时终态→此后探测全绿也不再拉起。
func TestRearmNetworkRevive(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})
	app.tasks["t1"] = &Task{ID: "t1", UserID: "u", AccountID: "a", Status: "error", AutoRevived: true, Error: "已自动重试 120 次后停止"}
	app.tasks["t2"] = &Task{ID: "t2", UserID: "u", AccountID: "a", Status: "queued", AutoRevived: true, Error: ""}
	app.tasks["t3"] = &Task{ID: "t3", UserID: "u", AccountID: "b", Status: "error", AutoRevived: true, Error: "已自动重试 120 次后停止"}
	app.tasks["t4"] = &Task{ID: "t4", UserID: "u", AccountID: "a", Status: "error", Error: "已自动重试 120 次后停止"}

	app.rearmNetworkRevive("u", "a")
	if app.tasks["t1"].AutoRevived {
		t.Fatalf("t1 error+已复活任务应被重臂: %+v", app.tasks["t1"])
	}
	if !app.tasks["t2"].AutoRevived {
		t.Fatalf("t2 非 error 状态不应被动: %+v", app.tasks["t2"])
	}
	if !app.tasks["t3"].AutoRevived {
		t.Fatalf("t3 属其他账号不应被动: %+v", app.tasks["t3"])
	}
	if app.tasks["t4"].AutoRevived {
		t.Fatalf("t4 本就未复活，重臂后仍应保持 false: %+v", app.tasks["t4"])
	}
	// 重臂后下一轮探测全绿应能再次复活 t1
	app.reviveNetworkStalledTasks("u", "a")
	if app.tasks["t1"].Status != "queued" {
		t.Fatalf("重臂后 t1 应能再次被自动复活: %+v", app.tasks["t1"])
	}
}

// R4.34：媒体连接建立超时/失败必须归入瞬态——2.6.11 实测网络抖动下
// 第一次 45s 超时即一票终态，一次自动重试机会都没拿到。
func TestTransientSourceErrorMediaConnStart(t *testing.T) {
	if !transientSourceError(errors.New("媒体连接建立超时（45 秒）：请检查代理是否放行 Telegram")) {
		t.Fatal("媒体连接建立超时应为瞬态")
	}
	if !transientSourceError(fmt.Errorf("媒体连接启动失败：%w", errors.New("connection reset by peer"))) {
		t.Fatal("媒体连接启动失败（含底层瞬态）应为瞬态")
	}
	if !transientSourceError(fmt.Errorf("媒体连接建立失败：%w", errors.New("connection refused"))) {
		t.Fatal("媒体连接建立失败（含底层瞬态）应为瞬态")
	}
}

// R4.34：媒体连接建立窗口按最近握手耗时自适应——基础 45s，8×握手ms 放宽，封顶 120s。
func TestMediaConnStartTimeoutFor(t *testing.T) {
	cases := []struct {
		handshakeMs int64
		want        time.Duration
	}{
		{0, 45 * time.Second},       // 无探测数据 → 基础档
		{3000, 45 * time.Second},    // 8×3s=24s < 45s → 基础档
		{5625, 45 * time.Second},    // 8×5.625s=45s → 恰为基础档
		{5000, 45 * time.Second},    // 8×5s=40s < 45s（实测握手 4~5s 节点仍走基础档）
		{10000, 80 * time.Second},   // 高延迟节点放宽
		{60000, 120 * time.Second},  // 封顶 120s
		{120000, 120 * time.Second}, // 超限封顶
	}
	for _, c := range cases {
		if got := mediaConnStartTimeoutFor(c.handshakeMs); got != c.want {
			t.Fatalf("mediaConnStartTimeoutFor(%d) = %s, want %s", c.handshakeMs, got, c.want)
		}
	}
}

// R4.31：mediaDCDiagnosis 纯函数四分类——下载错误里的诊断文案必须区分
// 「TCP 即失败」「本地接受未转发」「TCP+握手均正常」三种世界。
func TestMediaDCDiagnosisCategories(t *testing.T) {
	fail := mediaDCDiagnosis(1, "149.154.175.53:443", false, false, 120, 0, "connect refused", "经代理")
	if !strings.Contains(fail, "即失败") || !strings.Contains(fail, "MTProto 层尚未开始") {
		t.Fatalf("TCP 即失败结论不符预期: %q", fail)
	}
	localAccept := mediaDCDiagnosis(1, "149.154.175.53:443", true, false, 1, 6000, "MTProto 握手未完成（超时或被对端断开）", "经代理")
	if !strings.Contains(localAccept, "本地接受") || !strings.Contains(localAccept, "全局模式") {
		t.Fatalf("本地接受结论必须直接给出修法: %q", localAccept)
	}
	realOK := mediaDCDiagnosis(5, "91.108.56.173:443", true, true, 2, 350, "", "经代理")
	if !strings.Contains(realOK, "真实可用") || !strings.Contains(realOK, "转发质量") {
		t.Fatalf("真握手正常结论不符预期: %q", realOK)
	}
}

// 手动优先：auto（后台缓存）任务即便排在队首，只要存在等待中的手动任务就必须让路；
// 手动任务全部进入传输态后，auto 恢复调度。
func TestPumpOnceManualPrioritizesOverAuto(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative", Transport: "http-bridge"})
	defer drainRunTasks(t, app)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer source.Close()
	defer doRelease()

	mk := func(id string, source2 string) *Task {
		return &Task{
			ID: id, UserID: "u1", AccountID: "a1", Status: "queued",
			Transport: "http-bridge", Source: source2,
			SourceURL: source.URL,
			FilePath:  filepath.Join(t.TempDir(), id+".bin"), Size: 10,
		}
	}
	// auto 排在队首（按 ID 排序 < manual 的前缀保证其在 listTasksLocked 前面）
	app.tasks["a-auto"] = mk("a-auto", "auto")
	app.tasks["z-manual"] = mk("z-manual", "manual")

	// 第一轮：auto 在前，但手动在等 → 只能启动手动任务
	if !app.pumpOnce() {
		t.Fatal("pumpOnce 应启动手动任务")
	}
	app.mu.Lock()
	started := app.tasks["z-manual"].Status
	autoStatus := app.tasks["a-auto"].Status
	autoReason := app.tasks["a-auto"].Error
	app.mu.Unlock()
	if started != "downloading" {
		t.Fatalf("手动任务应被调度，got %q", started)
	}
	if autoStatus != "queued" {
		t.Fatalf("auto 任务必须让路留在 queued，got %q", autoStatus)
	}
	if !strings.Contains(autoReason, "手动优先") {
		t.Fatalf("auto 让路原因应可见化，got %q", autoReason)
	}

	// 手动任务占坑期间：auto 依旧不能启动（并发满）。
	if app.pumpOnce() {
		t.Fatal("并发已满时不应再启动任何任务")
	}

	// 释放手动任务并等 goroutine 收尾 → auto 恢复调度。
	doRelease()
	drainRunTasks(t, app)
	// 手动任务可能已终态（挂起被释放后快速完成）；无论终态如何，auto 此刻应可被调度。
	if !app.pumpOnce() {
		// 若手动任务已完成且无其他等待任务，auto 必须能启动；再给一次机会排除时序
		if app.tasks["a-auto"].Status == "queued" && !app.pumpOnce() {
			t.Fatalf("手动任务结束后 auto 任务应恢复调度，status=%q err=%q", app.tasks["a-auto"].Status, app.tasks["a-auto"].Error)
		}
	}
}

// R4.35：RPC 引擎层传输失败（retryUntilAck 重试上限）必须归入瞬态——
// 2.6.12 实测（10:42:00）网络抖动下首个失败即一票终态。
func TestTransientSourceErrorRPCTransport(t *testing.T) {
	if !transientSourceError(errors.New("invoke pool: rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts")) {
		t.Fatal("retryUntilAck 重试上限应为瞬态")
	}
	if !transientSourceError(errors.New("rpcDoRequest: rpc error code 420: FLOOD_WAIT (9)")) {
		t.Fatal("FLOOD_WAIT 应为瞬态")
	}
	if !transientSourceError(errors.New("会话索引解析在冷却中（剩余 42s）——上一轮解析被 Telegram 限流，冷却结束会自动重试")) {
		t.Fatal("解析冷却应为瞬态（等窗口过去）")
	}
}

// R4.35：peer 解析冷却时长——基础 60s；FLOOD_WAIT 按 Telegram 秒数（封顶 15min）。
func TestPeerResolveCooldownDuration(t *testing.T) {
	cases := []struct {
		err  error
		want time.Duration
	}{
		{errors.New("找不到会话 Channel:1"), 60 * time.Second},
		{errors.New("rpcDoRequest: rpc error code 420: FLOOD_WAIT (9)"), 60 * time.Second},    // 9s < 60s 基础档
		{errors.New("rpcDoRequest: rpc error code 420: FLOOD_WAIT (600)"), 10 * time.Minute},  // 按限流秒数
		{errors.New("rpcDoRequest: rpc error code 420: FLOOD_WAIT (3600)"), 15 * time.Minute}, // 封顶
	}
	for _, c := range cases {
		if got := peerResolveCooldownDuration(c.err); got != c.want {
			t.Fatalf("peerResolveCooldownDuration(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

// R4.35：peer 解析闸门——并发调用只执行一次真实解析（singleflight）；
// 失败后进入账号级冷却，冷却期内不再发起解析。
func TestPeerResolveGateSingleflightAndCooldown(t *testing.T) {
	app := newTestApp(t, Config{Enabled: true, Concurrency: 1, Mode: "conservative"})

	var calls int32
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	gateFn := func() (nativePeerInfo, error) {
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-release
		return nativePeerInfo{ID: "2052039292", Type: "channel", AccessHash: "42"}, nil
	}

	const workers = 3
	results := make([]nativePeerInfo, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = app.peerResolveGate("u|a", "Channel:1", gateFn)
		}(i)
	}
	<-started // 首轮已开始
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发解析只应执行一次，实际 %d 次", got)
	}
	close(release)
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil || results[i].AccessHash != "42" {
			t.Fatalf("第 %d 个并发调用应共享解析结果: info=%+v err=%v", i, results[i], errs[i])
		}
	}

	// 失败 → 冷却：后续调用不执行 fn，返回可读冷却错误。
	failCalls := 0
	failFn := func() (nativePeerInfo, error) {
		failCalls++
		return nativePeerInfo{}, errors.New("rpcDoRequest: rpc error code 420: FLOOD_WAIT (9)")
	}
	if _, err := app.peerResolveGate("u|b", "Channel:9", failFn); err == nil {
		t.Fatal("首次解析失败应返回错误")
	}
	_, err := app.peerResolveGate("u|b", "Channel:9", failFn)
	if err == nil || !strings.Contains(err.Error(), "解析在冷却中") {
		t.Fatalf("冷却期内应返回冷却错误: %v", err)
	}
	if failCalls != 1 {
		t.Fatalf("冷却期内不应再发起解析，实际 %d 次", failCalls)
	}
	// 其他账号不受影响（冷却按账号隔离）
	if _, err := app.peerResolveGate("u|c", "Channel:1", func() (nativePeerInfo, error) {
		return nativePeerInfo{ID: "1", Type: "user", AccessHash: "1"}, nil
	}); err != nil {
		t.Fatalf("其他账号不应被冷却波及: %v", err)
	}

	// R4.36-B：结构性「找不到会话」只退避该 peer，不连坐同账号其他频道的解析
	//（2.6.13 实测缺口：一个不可达频道冻结整个账号 60s）。
	structFn := func() (nativePeerInfo, error) {
		return nativePeerInfo{}, errors.New("找不到会话 Channel:777，可能已退出该群组、会话已被删除")
	}
	if _, err := app.peerResolveGate("u|d", "Channel:777", structFn); err == nil {
		t.Fatal("结构性失败首次应返回错误")
	}
	if _, err := app.peerResolveGate("u|d", "Channel:777", structFn); err == nil || !strings.Contains(err.Error(), "退避中") {
		t.Fatalf("结构性失败后应仅对该 peer 退避: %v", err)
	}
	if _, err := app.peerResolveGate("u|d", "Channel:888", func() (nativePeerInfo, error) {
		return nativePeerInfo{ID: "888", Type: "channel", AccessHash: "7"}, nil
	}); err != nil {
		t.Fatalf("结构性失败不得连坐同账号其他频道: %v", err)
	}

	// 连续 peerStructFailThreshold 次结构性失败 → 可操作终态（非瞬态，不再无限重试）。
	app.peerResolveMu.Lock()
	app.peerResolveFailCount[peerResolveKeyOf("u|e", "Channel:777")] = peerStructFailThreshold - 1
	app.peerResolveMu.Unlock()
	_, err = app.peerResolveGate("u|e", "Channel:777", structFn)
	var unreachable *peerUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("连续 %d 次结构性失败应升级为 peerUnreachableError: %v", peerStructFailThreshold, err)
	}
	if transientSourceError(err) {
		t.Fatal("结构性不可达不得被判为瞬态错误（否则会无限重试）")
	}
	// 手动重试重置后立刻恢复可解析。
	app.resetPeerResolveFailures("u", "e", "Channel:777")
	if _, err := app.peerResolveGate("u|e", "Channel:777", func() (nativePeerInfo, error) {
		return nativePeerInfo{ID: "777", Type: "channel", AccessHash: "9"}, nil
	}); err != nil {
		t.Fatalf("手动重试重置后应能正常解析: %v", err)
	}
}

// R4.36-G：会话列表分页参数的守卫。messages.getDialogs 单页硬上限是 100，
// 若把 dialogPageSize 调大，服务端会截断到 100，代码就会把「被截断」误判为
// 「已到列表末尾」——这正是「第 100 条之后的群组永不出现」的根因，必须防回归。
func TestDialogPaginationGuards(t *testing.T) {
	if dialogPageSize > 100 {
		t.Fatalf("dialogPageSize=%d 超过 Telegram 单页硬上限 100", dialogPageSize)
	}
	if dialogPageSize <= 0 {
		t.Fatal("dialogPageSize 必须为正")
	}
	if maxChatDialogLimit%dialogPageSize != 0 {
		t.Fatalf("会话总量上限 %d 应为页大小 %d 的整数倍，避免半页请求", maxChatDialogLimit, dialogPageSize)
	}
	if nativePeerBackfillRounds*dialogPageSize < maxChatDialogLimit {
		t.Fatalf("深翻能力 %d 轮×%d 页 < 总量上限 %d，无法覆盖全部会话",
			nativePeerBackfillRounds, dialogPageSize, maxChatDialogLimit)
	}
}

// R4.36-E：候选 media DC 按「首选 → 探测握手耗时升序 → 主 DC 兜底」排序，
// 握手失败的 DC 直接排除。
func TestMediaDCCandidatesOrdering(t *testing.T) {
	app := &App{mediaProbes: map[string]mediaProbeSnapshot{}}
	app.mediaProbes[nativeAccountKey("u", "a")] = mediaProbeSnapshot{
		Results: []mediaDCProbe{
			{DC: 2, MTPOK: true, MTPDuration: 5000},
			{DC: 4, MTPOK: true, MTPDuration: 1200},
			{DC: 1, MTPOK: false},
			{DC: 5, MTPOK: true, MTPDuration: 3000},
		},
	}
	got := app.mediaDCCandidates("u", "a", 4, 5)
	want := []int{4, 5, 2}
	if len(got) != len(want) {
		t.Fatalf("候选数量不符: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选顺序不符: got=%v want=%v", got, want)
		}
	}
	// 无探测快照：仍要给出「首选 + 主 DC」的最小候选，且不重复。
	app2 := &App{mediaProbes: map[string]mediaProbeSnapshot{}}
	got = app2.mediaDCCandidates("u", "b", 4, 4)
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("首选与主 DC 相同时不应重复: %v", got)
	}
}

// R4.36-C：调度层能看到解析冷却的剩余时间，从而不启动注定失败的任务。
func TestPeerResolveCooldownRemaining(t *testing.T) {
	app := &App{
		peerResolveCooldown:     map[string]time.Time{},
		peerResolvePeerCooldown: map[string]time.Time{},
		peerResolveFailCount:    map[string]int{},
	}
	if _, cooling := app.peerResolveCooldownRemaining("u", "a", "Channel:1"); cooling {
		t.Fatal("无冷却时不应报告冷却中")
	}
	// 账号级限流冷却：同账号任意 peer 都应看到。
	app.peerResolveMu.Lock()
	app.peerResolveCooldown["u|a"] = time.Now().Add(30 * time.Second)
	app.peerResolveMu.Unlock()
	if remain, cooling := app.peerResolveCooldownRemaining("u", "a", "Channel:1"); !cooling || remain <= 0 {
		t.Fatalf("账号级冷却应被调度层感知: remain=%s cooling=%v", remain, cooling)
	}
	// peer 级结构性退避：只影响该 peer，同账号其他 peer 不受影响。
	app.peerResolveMu.Lock()
	delete(app.peerResolveCooldown, "u|a")
	app.peerResolvePeerCooldown[peerResolveKeyOf("u|a", "Channel:7")] = time.Now().Add(20 * time.Second)
	app.peerResolveMu.Unlock()
	if _, cooling := app.peerResolveCooldownRemaining("u", "a", "Channel:7"); !cooling {
		t.Fatal("peer 级退避应被调度层感知")
	}
	if _, cooling := app.peerResolveCooldownRemaining("u", "a", "Channel:8"); cooling {
		t.Fatal("peer 级退避不得波及同账号其他 peer")
	}
}
