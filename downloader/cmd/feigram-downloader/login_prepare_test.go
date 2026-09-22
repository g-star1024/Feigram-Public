package main

import (
	"strings"
	"testing"
)

// ensureNativeAccountLocked 的最小装配：只碰 a.mu 与 a.native，不需要磁盘目录。
func newLoginPrepareApp() *App {
	return &App{native: map[string]*NativeAccount{}}
}

// 首次登录（auth/start）的 accountId 由 Node 新生成，Go 侧必然无记录：
// 必须用请求自带的手机号 + API 凭据自动建档，而不是报 "native account is not prepared"。
// （2026-09-22 用户实测回归：升级 2.1.2 后首次登录 100% 复现该错误。）
func TestEnsureNativeAccountCreatesForFirstLogin(t *testing.T) {
	app := newLoginPrepareApp()

	account, err := app.ensureNativeAccountLocked("u1", "fresh-1", "+8613800000000", 123456, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("首次登录应自动建档，实际报错：%v", err)
	}
	if account.Phone != "+8613800000000" || account.APIID != 123456 {
		t.Fatalf("建档应带上请求里的手机号与 apiId，实际 phone=%q apiId=%d", account.Phone, account.APIID)
	}
	if account.Status != "needs-relogin" || account.CreatedAt == "" {
		t.Fatalf("新档状态应为 needs-relogin 且有 CreatedAt，实际 status=%q createdAt=%q", account.Status, account.CreatedAt)
	}
	stored, ok := app.native[nativeAccountKey("u1", "fresh-1")]
	if !ok || stored != account {
		t.Fatal("建档后必须写入 a.native，后续 continueNativeLogin 才能找到")
	}

	// 第二次调用（同账号）应返回同一条记录，不重复建。
	again, err := app.ensureNativeAccountLocked("u1", "fresh-1", "", 0, "")
	if err != nil || again != account {
		t.Fatalf("已有记录时应原样返回，实际 err=%v same=%v", err, again == account)
	}
}

// 凭据不全时给明确错误（apiId/apiHash is required），不能回到含糊的 not prepared。
func TestEnsureNativeAccountRequiresCredentialsForNewRecord(t *testing.T) {
	app := newLoginPrepareApp()

	_, err := app.ensureNativeAccountLocked("u1", "fresh-2", "+8613800000000", 0, "")
	if err == nil || !strings.Contains(err.Error(), "apiId/apiHash is required") {
		t.Fatalf("缺凭据应报 apiId/apiHash is required，实际：%v", err)
	}
	_, err = app.ensureNativeAccountLocked("", "", "", 1, "hash")
	if err == nil || !strings.Contains(err.Error(), "userId and accountId are required") {
		t.Fatalf("缺 id 应报 userId and accountId are required，实际：%v", err)
	}
	if len(app.native) != 0 {
		t.Fatalf("失败路径不应留下半成品记录，实际剩 %d 条", len(app.native))
	}
}

// admin 重登链路（Node 已 upsert 过）凭据可能留空：已有记录时不能因凭据为空而报错。
func TestEnsureNativeAccountKeepsExistingWithoutCredentials(t *testing.T) {
	app := newLoginPrepareApp()
	app.native[nativeAccountKey("u1", "a1")] = &NativeAccount{
		UserID:    "u1",
		AccountID: "a1",
		Phone:     "+8613800000000",
		APIID:     123456,
		Status:    "ready",
	}

	account, err := app.ensureNativeAccountLocked("u1", "a1", "", 0, "")
	if err != nil {
		t.Fatalf("已有记录不应要求凭据，实际报错：%v", err)
	}
	if account.Status != "ready" || account.Phone != "+8613800000000" {
		t.Fatalf("已有记录字段不应被清掉，实际 status=%q phone=%q", account.Status, account.Phone)
	}
}
