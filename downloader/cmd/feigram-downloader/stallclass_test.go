package main

// R4.40 守卫单测：退避分级 / 断流判定 / DC 断流记忆与候选降级 / 分片自适应。
//
// 这些判据的意义（对应 R4.39 的教训「修复必须自带判据」）：2.6.16 实测里
// 「每轮只推进 2~9MB 却退避 2m40s」本地无法复现（真实凭据只在用户机器上），
// 所以修复的同时必须把判据钉死在单测里——下一轮实测是「一眼可判」，
// 而不是再猜一轮。

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gotd/td/rpc"
)

// TestWatchdogWindowsOrdering 守住两档看门狗窗口的相对关系。
// R4.41 起「已传输」档从 120s 放宽到 5 分钟——官方 downloader 会在内部
// 按 FLOOD_WAIT 秒数静默等待，120s 会把「正在按规矩等限流」误判成挂死。
func TestWatchdogWindowsOrdering(t *testing.T) {
	if nativeFirstByteTimeout >= nativePipelinedStallTimeout {
		t.Fatalf("首字节窗口（%v）必须远小于已传输窗口（%v），否则媒体段不通时无法快速失败",
			nativeFirstByteTimeout, nativePipelinedStallTimeout)
	}
	if nativePipelinedStallTimeout < nativeNoProgressTimeout {
		t.Fatalf("R4.41 后的已传输窗口（%v）不得短于原 120s 档（%v），否则官方下载器的限流等待会被误杀",
			nativePipelinedStallTimeout, nativeNoProgressTimeout)
	}
	if nativePipelinedStallTimeout > 20*time.Minute {
		t.Fatalf("已传输窗口不应超过 20 分钟，否则真挂死的恢复太慢：%v", nativePipelinedStallTimeout)
	}
}

func TestClassifyTransientErrorFloodStallGeneric(t *testing.T) {
	flood := &floodWaitError{Seconds: 24}
	if got := classifyTransientError(flood); got != classFlood {
		t.Fatalf("FLOOD_WAIT 应归为 classFlood，得到 %v", got)
	}

	// 2.6.16 实测的原始包装形态（中间件名 + rpc 错误文案）。
	stall := errors.New("invoke pool: rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts")
	if got := classifyTransientError(stall); got != classStall {
		t.Fatalf("retryUntilAck 断流应归为 classStall，得到 %v", got)
	}

	// 看门狗挂死与空响应由我们自己合成错误，不带 gotd 文本签名，需显式归类。
	if got := classifyTransientError(errDownloadStalled); got != classStall {
		t.Fatalf("无进度挂死应归为 classStall，得到 %v", got)
	}
	if got := classifyTransientError(errEmptyMediaResponse); got != classStall {
		t.Fatalf("空响应应归为 classStall，得到 %v", got)
	}

	// 业务层瞬态（找不到会话）不是断流——它需要的是深翻/等索引，不是换链路。
	generic := errors.New("找不到会话 Channel:2052039292")
	if got := classifyTransientError(generic); got != classGeneric {
		t.Fatalf("找不到会话应归为 classGeneric，得到 %v", got)
	}
	if got := classifyTransientError(nil); got != classGeneric {
		t.Fatalf("nil 应归为 classGeneric，得到 %v", got)
	}
}

func TestTransportStallErrorUsesRealTypeNotJustText(t *testing.T) {
	// 类型断言路径：即使错误文本被包装层改写，也应命中。
	typed := errors.New("wrapped")
	if !transportStallError(&rpc.RetryLimitReachedErr{Retries: 5}) {
		t.Fatal("rpc.RetryLimitReachedErr 必须被判为断流")
	}
	if transportStallError(typed) {
		t.Fatal("普通错误不应被判为断流")
	}
	if !transportStallError(io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF 必须被判为断流")
	}
	// 负例：限流与业务错误都不是断流。
	if transportStallError(&floodWaitError{Seconds: 10}) {
		t.Fatal("FLOOD_WAIT 不是断流（它该按秒数等待）")
	}
	if transportStallError(errors.New("找不到会话 Channel:1")) {
		t.Fatal("找不到会话不是断流")
	}
}

// TestStallRetryDelayMuchShorterThanGeneric 是本轮修复的核心判据：
// 断流退避必须显著短于通用指数退避——2.6.16 实测的痛点正是「断流被罚站 2m40s」。
func TestStallRetryDelayMuchShorterThanGeneric(t *testing.T) {
	// 第 1 次断流：5 秒（不是 retryDelay 的 5 秒巧合相等，关键看后续档位与封顶）。
	if got := stallRetryDelay(1); got != 5*time.Second {
		t.Fatalf("第 1 次断流退避应为 5s，得到 %v", got)
	}
	if got := stallRetryDelay(2); got != 10*time.Second {
		t.Fatalf("第 2 次断流退避应为 10s，得到 %v", got)
	}
	if got := stallRetryDelay(3); got != 20*time.Second {
		t.Fatalf("第 3 次断流退避应为 20s，得到 %v", got)
	}
	// 封顶 60s —— 显著低于 retryDelay 的 5 分钟封顶。
	if got := stallRetryDelay(99); got != stallRetryDelayCap {
		t.Fatalf("断流退避必须封顶在 %v，得到 %v", stallRetryDelayCap, got)
	}
	if stallRetryDelayCap >= retryDelay(99) {
		t.Fatalf("断流封顶（%v）必须短于通用退避封顶（%v），否则等于没修",
			stallRetryDelayCap, retryDelay(99))
	}
	// 对照实测：2.6.16 日志里断流用的是 2m40s（= retryDelay(6)）。
	if stallRetryDelay(6) == retryDelay(6) {
		t.Fatal("第 6 次断流仍与通用指数退避同值，2.6.16 的 2m40s 罚站会原样重现")
	}
}

func TestAdaptivePartSizeShrinksOnRepeatedStalls(t *testing.T) {
	base := int64(1024 * 1024)
	if got := adaptivePartSize(base, 0); got != base {
		t.Fatalf("无断流时应保持原分片 %d，得到 %d", base, got)
	}
	if got := adaptivePartSize(base, 1); got != base {
		t.Fatalf("首次断流尚不缩小（避免抖动），得到 %d", got)
	}
	if got := adaptivePartSize(base, 2); got != stallShrinkPartSize512KB {
		t.Fatalf("第 2 次断流应缩到 512KB，得到 %d", got)
	}
	if got := adaptivePartSize(base, 5); got != stallShrinkPartSize256KB {
		t.Fatalf("第 5 次断流应缩到 256KB，得到 %d", got)
	}
	// 只允许调小：即便配置分片小于 256KB，也不得被放大。
	small := int64(128 * 1024)
	if got := adaptivePartSize(small, 9); got != small {
		t.Fatalf("自适应不得放大配置分片，期望 %d，得到 %d", small, got)
	}
	// base<=0 回退默认。
	if got := adaptivePartSize(0, 0); got != defaultPartSize {
		t.Fatalf("base<=0 应回退默认分片 %d，得到 %d", defaultPartSize, got)
	}
}

func TestMediaDCStallMemoryDemotesCandidateOnlyWhenRepeated(t *testing.T) {
	app := &App{
		mediaProbes:   map[string]mediaProbeSnapshot{},
		mediaDCStalls: map[string]map[int]int{},
	}

	// 无断流记录：候选顺序保持「任务元数据 DC 优先」。
	candidates := app.mediaDCCandidates("u1", "a1", 1, 5)
	if len(candidates) < 2 || candidates[0] != 1 || candidates[len(candidates)-1] != 5 {
		t.Fatalf("无断流时候选顺序应保持 preferred 优先、primary 兜底，得到 %v", candidates)
	}

	// 记 1 次断流：未达阈值，不降级（避免单次抖动就换路）。
	if n := app.noteMediaDCStall("u1", "a1", 1); n != 1 {
		t.Fatalf("首次断流计数应为 1，得到 %d", n)
	}
	candidates = app.mediaDCCandidates("u1", "a1", 1, 5)
	if candidates[0] != 1 {
		t.Fatalf("仅 1 次断流不应降级该 DC，得到 %v", candidates)
	}
	if hint := stallDegradeHint(1); hint != "" {
		t.Fatalf("未达阈值不应给出降级提示，得到 %q", hint)
	}

	// 达阈值：该 DC 被降到候选末尾，但仍保留在序列中（不得被排除）。
	if n := app.noteMediaDCStall("u1", "a1", 1); n != mediaDCStallThreshold {
		t.Fatalf("第二次断流计数应为 %d，得到 %d", mediaDCStallThreshold, n)
	}
	candidates = app.mediaDCCandidates("u1", "a1", 1, 5)
	if len(candidates) < 2 {
		t.Fatalf("降级后候选不应减少，得到 %v", candidates)
	}
	if candidates[0] == 1 {
		t.Fatalf("连续断流达阈值的 DC 应被降级，得到 %v", candidates)
	}
	if candidates[len(candidates)-1] != 1 {
		t.Fatalf("被降级的 DC 必须仍在候选内（兜底），得到 %v", candidates)
	}
	if hint := stallDegradeHint(mediaDCStallThreshold); hint == "" {
		t.Fatal("达阈值时应给出「已在候选序列中降级」提示")
	}

	// 跑出流量后清零：已恢复的 DC 不得继续被降级。
	app.clearMediaDCStall("u1", "a1", 1)
	if counts := app.mediaDCStallCounts("u1", "a1"); len(counts) != 0 {
		t.Fatalf("清零后不应残留计数，得到 %v", counts)
	}
	if candidates = app.mediaDCCandidates("u1", "a1", 1, 5); candidates[0] != 1 {
		t.Fatalf("清零后该 DC 应恢复首选，得到 %v", candidates)
	}

	// 账号隔离：另一账号的断流不得影响本账号候选。
	if n := app.noteMediaDCStall("u2", "a2", 1); n != 1 {
		t.Fatalf("不同账号计数应独立，得到 %d", n)
	}
}
