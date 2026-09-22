package main

import (
	"context"
	"testing"
	"time"
)

// newLoginGuardApp 只装配 cancelLoginsLocked 需要的最小状态：
// 该方法只碰 a.mu 与 a.logins，不需要磁盘目录或代理运行时。
func newLoginGuardApp() *App {
	return &App{logins: map[string]*NativeLogin{}}
}

func addLogin(t *testing.T, app *App, id, userID, accountID string) *int {
	t.Helper()
	cancelled := 0
	ctx, cancel := context.WithCancel(context.Background())
	app.logins[id] = &NativeLogin{
		ID:        id,
		UserID:    userID,
		AccountID: accountID,
		Cancel: func() {
			cancelled++
			cancel()
		},
	}
	t.Cleanup(func() {
		// 避免 vet/静态检查报「ctx 未使用」；同时让 context 一定被释放。
		if ctx.Err() == nil {
			cancel()
		}
	})
	return &cancelled
}

// 同一账号重复点「登录」时，旧流程必须被取消并摘除登记，
// 否则旧流程会一边用旧代理拨号、一边在收尾时覆盖账号状态。
func TestCancelLoginsLockedReplacesSameAccountLogin(t *testing.T) {
	app := newLoginGuardApp()
	oldCount := addLogin(t, app, "login-old", "u1", "a1")
	otherCount := addLogin(t, app, "login-other", "u2", "a9")

	if n := app.cancelLoginsLocked("u1", "a1"); n != 1 {
		t.Fatalf("应只取消 1 个同账号登录，实际 %d", n)
	}
	if *oldCount != 1 {
		t.Fatalf("旧登录的 Cancel 应被调用 1 次，实际 %d", *oldCount)
	}
	if *otherCount != 0 {
		t.Fatalf("其它账号的登录不应被取消，实际取消 %d 次", *otherCount)
	}
	if _, ok := app.logins["login-old"]; ok {
		t.Fatal("被取消的登录应从 a.logins 中摘除")
	}
	if _, ok := app.logins["login-other"]; !ok {
		t.Fatal("其它账号的登录应保留")
	}
}

// 代理变更时应当取消全部在途登录：它们把旧 dialer 固定在 gotd client 里。
func TestCancelLoginsLockedAllClearsEveryLogin(t *testing.T) {
	app := newLoginGuardApp()
	first := addLogin(t, app, "login-1", "u1", "a1")
	second := addLogin(t, app, "login-2", "u2", "a2")

	if n := app.cancelLoginsLocked("", ""); n != 2 {
		t.Fatalf("应取消 2 个登录，实际 %d", n)
	}
	if *first != 1 || *second != 1 {
		t.Fatalf("两个登录都应被取消，实际 %d / %d", *first, *second)
	}
	if len(app.logins) != 0 {
		t.Fatalf("取消后 a.logins 应为空，实际剩 %d 条", len(app.logins))
	}
}

// 空集合下不应 panic，也不应报错，便于在热路径上无条件调用。
func TestCancelLoginsLockedOnEmptyApp(t *testing.T) {
	app := newLoginGuardApp()
	if n := app.cancelLoginsLocked("u1", "a1"); n != 0 {
		t.Fatalf("空集合应返回 0，实际 %d", n)
	}
	if n := app.cancelLoginsLocked("", ""); n != 0 {
		t.Fatalf("空集合应返回 0，实际 %d", n)
	}
}

// 登录生命周期必须有上限：前端超时放弃后 goroutine 不能永久重试 Telegram DC。
func TestLoginLifetimeIsBounded(t *testing.T) {
	if loginLifetime <= 0 {
		t.Fatalf("loginLifetime 必须为正，实际 %s", loginLifetime)
	}
	if loginLifetime > 30*time.Minute {
		t.Fatalf("loginLifetime 过长（%s）：用户等待 Telegram 验证码通常只有几分钟", loginLifetime)
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginLifetime)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("以 loginLifetime 创建的 context 必须有截止时间")
	}
}

// 发起登录的等待上限必须短于流程总生命周期，否则「发起超时」这条兜底永远轮不到。
func TestLoginStartTimeoutShorterThanLifetime(t *testing.T) {
	if loginStartTimeout <= 0 {
		t.Fatalf("loginStartTimeout 必须为正，实际 %s", loginStartTimeout)
	}
	if loginStartTimeout >= loginLifetime {
		t.Fatalf("发起超时（%s）必须短于流程生命周期（%s）", loginStartTimeout, loginLifetime)
	}
	if loginStartTimeout > 2*time.Minute {
		t.Fatalf("发起登录等 %s 太久：用户在前端会先超时并拿不到 loginID", loginStartTimeout)
	}
}
