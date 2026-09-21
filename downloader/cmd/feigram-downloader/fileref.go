package main

import (
	"strings"

	"github.com/gotd/td/tgerr"
)

// M3.3：强化 native file_reference 过期的自动刷新续传。
//
// 改动三点：
//  1. 判定方式由 strings.Contains(err.Error(), "FILE_REFERENCE_EXPIRED")
//     改为 gotd tgerr 的类型化判定，并覆盖等价变体（EMPTY / INVALID）。
//     原写法依赖错误文本，既会漏判变体，也可能因消息文本恰好含该串而误判。
//  2. 刷新重试加入预算上限。原实现在刷新成功后直接 continue，
//     若服务端持续返回过期引用会形成无限循环，任务永不失败也不前进。
//  3. 提供 nativeFileRefRefreshHook 故障注入测试点，
//     使「刷新后仍然失败」这条极端路径可以被单元测试覆盖。

const (
	// maxFileReferenceRefreshes 是单次下载允许的 file_reference 刷新次数上限。
	maxFileReferenceRefreshes = 3
)

// fileReferenceErrorTypes 覆盖需要重新拉取消息元数据以续期的错误类型。
var fileReferenceErrorTypes = []string{
	"FILE_REFERENCE_EXPIRED",
	"FILE_REFERENCE_EMPTY",
	"FILE_REFERENCE_INVALID",
}

// nativeFileRefRefreshHook 是故障注入测试点：非 nil 时在每次刷新成功后调用，
// 返回错误即中断续传。生产环境恒为 nil，仅测试改写。
var nativeFileRefRefreshHook func(attempt int) error

// isFileReferenceError 判定错误是否属于 file_reference 失效类。
func isFileReferenceError(err error) bool {
	if err == nil {
		return false
	}
	if tgerr.Is(err, fileReferenceErrorTypes...) {
		return true
	}
	// 兜底：被包装的传输层错误不是 *tgerr.Error，保留文本判定。
	message := strings.ToUpper(err.Error())
	for _, kind := range fileReferenceErrorTypes {
		if strings.Contains(message, kind) {
			return true
		}
	}
	return false
}

// allowFileReferenceRefresh 判断第 attempt 次刷新（1-based）是否仍在预算内。
func allowFileReferenceRefresh(attempt int) bool {
	return attempt >= 1 && attempt <= maxFileReferenceRefreshes
}
