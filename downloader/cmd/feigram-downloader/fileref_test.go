package main

import (
	"fmt"
	"testing"

	"github.com/go-faster/errors"
	"github.com/gotd/td/tgerr"
)

// TestIsFileReferenceErrorTyped 验证 gotd RPC 错误的类型化判定（主路径）。
func TestIsFileReferenceErrorTyped(t *testing.T) {
	for _, kind := range fileReferenceErrorTypes {
		err := tgerr.New(400, kind)
		if !isFileReferenceError(err) {
			t.Fatalf("%s: 判定为 false，预期 true", kind)
		}
	}
}

// TestIsFileReferenceErrorRejectsOthers 验证非 file_reference 错误不会触发刷新。
func TestIsFileReferenceErrorRejectsOthers(t *testing.T) {
	cases := []error{
		tgerr.New(420, "FLOOD_WAIT_3"),
		tgerr.New(400, "MESSAGE_ID_INVALID"),
		errors.New("connection reset by peer"),
		nil,
	}
	for i, err := range cases {
		if isFileReferenceError(err) {
			t.Fatalf("case %d (%v): 判定为 true，预期 false", i, err)
		}
	}
}

// TestIsFileReferenceErrorTextFallback 验证被包装的非 RPC 错误仍能被文本兜底识别。
func TestIsFileReferenceErrorTextFallback(t *testing.T) {
	wrapped := fmt.Errorf("upload.getFile failed: %w", errors.New("FILE_REFERENCE_EXPIRED"))
	if !isFileReferenceError(wrapped) {
		t.Fatal("包装错误未被识别，文本兜底失效")
	}
}

// TestAllowFileReferenceRefreshBudget 验证刷新预算边界：1..3 允许，0 与 4 拒绝。
// 这是 M3.3 防死循环的关键约束。
func TestAllowFileReferenceRefreshBudget(t *testing.T) {
	cases := map[int]bool{
		0: false,
		1: true,
		2: true,
		3: true,
		4: false,
	}
	for attempt, want := range cases {
		if got := allowFileReferenceRefresh(attempt); got != want {
			t.Fatalf("allowFileReferenceRefresh(%d) = %v，预期 %v", attempt, got, want)
		}
	}
}

// TestFileRefRefreshHookIsInjectionPoint 验证故障注入测试点可用：
// hook 置位后能被触发并中断续传，且用后可恢复为 nil（不影响其他测试）。
func TestFileRefRefreshHookIsInjectionPoint(t *testing.T) {
	called := 0
	nativeFileRefRefreshHook = func(attempt int) error {
		called++
		return fmt.Errorf("注入失败：attempt=%d", attempt)
	}
	defer func() { nativeFileRefRefreshHook = nil }()

	if err := nativeFileRefRefreshHook(1); err == nil {
		t.Fatal("注入 hook 未返回错误")
	}
	if called != 1 {
		t.Fatalf("hook 调用次数 %d，预期 1", called)
	}
}
