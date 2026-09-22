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

	// 坏账号但带 SourceURL：与 download() 运行时回退一致，应可启动（运行时回 HTTP）。
	badWithHTTP := &Task{
		ID: "t-bad2", UserID: "u", AccountID: "bad",
		Transport: "native-mtproto", SourceURL: "https://example.test/v",
	}
	if !app.taskCanStartLocked(badWithHTTP, "native-mtproto") {
		t.Fatal("坏账号但带 HTTP 回退源的任务应可启动")
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
