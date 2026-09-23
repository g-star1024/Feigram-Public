package main

// R4.22 简化重构的行为固化：
//  1. 账号不可用时错误必须直出账号记录里的真实原因（健康检查诊断），不再经 HTTP 桥二次包装；
//  2. 该错误按瞬态处理，账号恢复后任务能自动续传；
//  3. 调度层的等待原因同样带上真实原因，用户不必翻服务端日志。

import (
	"errors"
	"strings"
	"testing"
)

func TestAccountNotReadyErrorCarriesRealReason(t *testing.T) {
	task := &Task{ID: "t1", UserID: "u", AccountID: "a"}
	account := NativeAccount{
		Status: "failed",
		Error:  "分级探测：TCP 拨号 Telegram DC 5（91.108.56.173:443）即失败——问题在经代理出口链路",
	}
	err := accountNotReadyError(task, account, nil)
	if !errors.Is(err, errAccountNotReady) {
		t.Fatal("应可被 errors.Is 识别为「账号未就绪」，供调度层判定自动续传")
	}
	if !strings.Contains(err.Error(), "分级探测") {
		t.Fatalf("必须直出账号记录的真实原因，got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "自动续传") {
		t.Fatalf("应带等待语义，got %q", err.Error())
	}
	if !transientSourceError(err) {
		t.Fatal("账号未就绪应按瞬态处理，账号恢复后自动续传")
	}
	if transientSourceError(nil) {
		t.Fatal("nil 不应判为瞬态")
	}
}

func TestAccountNotReadyErrorWithoutReason(t *testing.T) {
	task := &Task{ID: "t2", UserID: "u", AccountID: "a"}

	// 账号存在但尚未写过任何原因。
	blank := accountNotReadyError(task, NativeAccount{Status: "failed"}, nil)
	if !strings.Contains(blank.Error(), "等待健康检查通过") {
		t.Fatalf("无原因时应给出通用等待说明，got %q", blank.Error())
	}

	// 账号查不到（nativeAccountSnapshot 直接报错）：透出原始错误。
	lookupErr := accountNotReadyError(task, NativeAccount{}, errors.New("Go 原生 MTProto 账号未准备好"))
	if !strings.Contains(lookupErr.Error(), "账号未准备好") {
		t.Fatalf("查号失败应透出原始错误，got %q", lookupErr.Error())
	}
	if !errors.Is(lookupErr, errAccountNotReady) {
		t.Fatal("查号失败同样应可识别为账号未就绪")
	}
}

// 调度层的等待原因要把账号自身诊断带出来（R4.22：用户不必翻日志）。
func TestTaskWaitReasonIncludesAccountDiagnosis(t *testing.T) {
	bad := NativeAccount{
		UserID: "u", AccountID: "bad", Status: "failed",
		Error: "分级探测：出口已能 TCP 连通 Telegram DC 5，失败在其后的 MTProto 握手/授权阶段——建议更换节点",
	}
	app := &App{native: map[string]*NativeAccount{nativeAccountKey("u", "bad"): &bad}}

	reason := app.taskWaitReasonLocked(&Task{ID: "t", UserID: "u", AccountID: "bad"}, "native-mtproto")
	if !strings.Contains(reason, "尚未就绪") || !strings.Contains(reason, "自动继续") {
		t.Fatalf("应给出等待语义，got %q", reason)
	}
	if !strings.Contains(reason, "更换节点") {
		t.Fatalf("应把账号真实诊断带出，got %q", reason)
	}

	// 账号记录不存在 → 提示重新登录，而不是空串（否则任务会静默 queued）。
	missing := app.taskWaitReasonLocked(&Task{ID: "t2", UserID: "u", AccountID: "ghost"}, "native-mtproto")
	if !strings.Contains(missing, "重新登录") {
		t.Fatalf("缺账号记录应提示重新登录，got %q", missing)
	}
}
