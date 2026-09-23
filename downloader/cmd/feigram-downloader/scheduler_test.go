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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		dataDir:   dir,
		storePath: filepath.Join(dir, "tasks.json"),
		tasks:     map[string]*Task{},
		native:    map[string]*NativeAccount{},
		running:   map[string]chan struct{}{},
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
