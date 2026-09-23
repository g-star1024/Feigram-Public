package main

import (
	"testing"
)

func healthyAccount() NativeAccount {
	return NativeAccount{
		UserID:       "u",
		AccountID:    "a",
		Status:       "healthy",
		Ready:        true,
		HealthPasses: 2,
		Session:      "session-blob",
		APIID:        123456,
		APIHash:      "hash",
	}
}

func TestNativeAccountEligibleAcceptsHealthy(t *testing.T) {
	if !nativeAccountEligible(healthyAccount()) {
		t.Fatal("healthy 账号应 eligible")
	}
}

// R4.7：坏账号即使残留 Ready=true，也必须被显式排除。
func TestNativeAccountEligibleRejectsBadStatus(t *testing.T) {
	for _, bad := range []string{"needs-relogin", "failed"} {
		acc := healthyAccount()
		acc.Status = bad // Ready/HealthPasses 等仍为"好"，模拟坏状态残留
		if nativeAccountEligible(acc) {
			t.Fatalf("Status=%s 的账号不应 eligible", bad)
		}
	}
}

// 缺关键条件（session/API/健康通过次数）时不 eligible，保持原有门槛。
func TestNativeAccountEligibleRequiresPrerequisites(t *testing.T) {
	cases := []NativeAccount{}
	base := healthyAccount()

	noSession := base
	noSession.Session = ""
	cases = append(cases, noSession)

	noPasses := base
	noPasses.HealthPasses = 1
	cases = append(cases, noPasses)

	noAPI := base
	noAPI.APIID = 0
	cases = append(cases, noAPI)

	for i, acc := range cases {
		if nativeAccountEligible(acc) {
			t.Fatalf("用例 %d 不应 eligible", i)
		}
	}
}

// R4.7：调度前判定必须阻止绑定坏账号、且无 HTTP 回退源的任务启动，
// 否则它会起 goroutine 占满并发坑、空跑到失败，拖垮整批。
func TestTaskCanStartLockedFiltersByAccountHealth(t *testing.T) {
	goodKey := nativeAccountKey("u", "good")
	badKey := nativeAccountKey("u", "bad")

	good := healthyAccount()
	good.AccountID = "good"
	bad := healthyAccount()
	bad.AccountID = "bad"
	bad.Status = "needs-relogin"
	bad.Ready = false

	app := &App{
		native: map[string]*NativeAccount{
			goodKey: &good,
			badKey:  &bad,
		},
	}

	goodTask := &Task{ID: "t-good", UserID: "u", AccountID: "good", Transport: "native-mtproto"}
	badTask := &Task{ID: "t-bad", UserID: "u", AccountID: "bad", Transport: "native-mtproto"}

	if !app.taskCanStartLocked(goodTask, "native-mtproto") {
		t.Fatal("绑定 healthy 账号的任务应可启动")
	}
	if app.taskCanStartLocked(badTask, "native-mtproto") {
		t.Fatal("绑定坏账号、无 HTTP 源的任务不应启动")
	}

	// R4.22：坏账号即使带 SourceURL 也不得启动——HTTP 回退已移除（回退源指向
	// Go 自身的 blob 端点，只会绕回同一个坏账号），账号可用性是一票否决。
	badWithHTTP := &Task{
		ID: "t-bad2", UserID: "u", AccountID: "bad",
		Transport: "native-mtproto", SourceURL: "https://example.test/v",
	}
	if app.taskCanStartLocked(badWithHTTP, "native-mtproto") {
		t.Fatal("坏账号任务不应启动（回退已移除，应等账号恢复）")
	}

	// http-bridge 无源不启动、有源可启动。
	httpNoSrc := &Task{ID: "t-http", UserID: "u", AccountID: "good", Transport: "http-bridge"}
	if app.taskCanStartLocked(httpNoSrc, "http-bridge") {
		t.Fatal("http-bridge 无源不应启动")
	}
	httpWithSrc := &Task{
		ID: "t-http2", UserID: "u", AccountID: "good",
		Transport: "http-bridge", SourceURL: "https://example.test/v",
	}
	if !app.taskCanStartLocked(httpWithSrc, "http-bridge") {
		t.Fatal("http-bridge 有源应可启动")
	}
}

// R4.8：/health 的账号汇总要给出 ready/failed，且 ready 必须与调度口径（eligible）一致。
// 反例：坏账号残留 Ready=true 时，normalizeNativeStatus 仍判 healthy，但 ready 必须为 0。
func TestAccountsSummaryCountsReadyAndFailed(t *testing.T) {
	good := healthyAccount()
	good.AccountID = "good"

	failed := healthyAccount()
	failed.AccountID = "failed"
	failed.Status = "failed" // Ready 仍为 true，模拟失败后残留

	needsRelogin := healthyAccount()
	needsRelogin.AccountID = "relogin"
	needsRelogin.Status = "needs-relogin"

	app := &App{
		native: map[string]*NativeAccount{
			nativeAccountKey("u", "good"):    &good,
			nativeAccountKey("u", "failed"):  &failed,
			nativeAccountKey("u", "relogin"): &needsRelogin,
		},
	}

	summary := app.accountsSummaryLocked()
	if got := summary["total"]; got != 3 {
		t.Fatalf("total 应为 3，实际 %v", got)
	}
	if got := summary["ready"]; got != 1 {
		t.Fatalf("ready 应为 1（只有 healthy 账号 eligible），实际 %v", got)
	}
	if got := summary["failed"]; got != 1 {
		t.Fatalf("failed 应为 1，实际 %v", got)
	}
	// healthy 保持原口径（normalizeNativeStatus，不看 Status 字符串），供既有前端兼容：
	// failed/needs-relogin 两个坏账号因残留 Ready=true 仍会被算作 healthy，这正是 ready 要纠正的偏差。
	if got := summary["healthy"]; got != 3 {
		t.Fatalf("healthy 保持原口径应为 3，实际 %v", got)
	}
	if summary["ready"].(int) > summary["healthy"].(int) {
		t.Fatal("ready 不应大于 healthy")
	}
}

// R4.8：/health 顶层要带传输层当前值，供外部监控直读。
func TestStateLockedExposesTransportAndAccounts(t *testing.T) {
	good := healthyAccount()
	// proxy 是指针类型，stateLocked 末尾会读 a.proxy.status()，零值 App 会 nil panic。
	app := &App{
		native: map[string]*NativeAccount{
			nativeAccountKey("u", "a"): &good,
		},
		config: Config{Transport: "native-mtproto"},
		proxy:  &proxyRuntime{},
	}
	state := app.stateLocked()
	if got := state["transport"]; got != "native-mtproto" {
		t.Fatalf("transport 应为 native-mtproto，实际 %v", got)
	}
	accounts, ok := state["accounts"].(map[string]any)
	if !ok {
		t.Fatal("state.accounts 缺失或类型不符")
	}
	for _, key := range []string{"total", "ready", "failed", "healthy", "byStatus"} {
		if _, ok := accounts[key]; !ok {
			t.Fatalf("accounts 缺字段 %s", key)
		}
	}
}
