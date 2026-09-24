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
	// R4.42：配置默认分片是 1MB，而官方 downloader 的末尾判据是
	// 「返回字节数 < 请求分片大小」（reader.go:18-23）——分片超过服务端单次
	// 返回上限时第一块就会被误判成末尾。所以自适应必须先把基址夹到上限，
	// 否则「缩到 512KB」既越界又是空操作。
	if got := adaptivePartSize(base, 0); got != officialPartSizeCap {
		t.Fatalf("1MB 配置在无断流时应被夹到上限 %d，得到 %d", officialPartSizeCap, got)
	}
	if got := adaptivePartSize(base, 1); got != officialPartSizeCap {
		t.Fatalf("首次断流尚不缩小（避免抖动），应仍为上限 %d，得到 %d", officialPartSizeCap, got)
	}
	if got := adaptivePartSize(base, 2); got != stallShrinkPartSizeHalf {
		t.Fatalf("第 2 次断流应缩到上限的一半（%d），得到 %d", stallShrinkPartSizeHalf, got)
	}
	if got := adaptivePartSize(base, 5); got != stallShrinkPartSizeQuarter {
		t.Fatalf("第 5 次断流应缩到上限的四分之一（%d），得到 %d", stallShrinkPartSizeQuarter, got)
	}
	// 只允许调小：即便配置分片小于最小档，也不得被放大。
	small := int64(64 * 1024)
	if got := adaptivePartSize(small, 9); got != small {
		t.Fatalf("自适应不得放大配置分片，期望 %d，得到 %d", small, got)
	}
	// base<=0 回退默认分片；默认分片 1MB 同样要被夹到上限。
	if got := adaptivePartSize(0, 0); got != officialPartSizeCap {
		t.Fatalf("base<=0 应回退默认分片再夹到上限 %d，得到 %d", officialPartSizeCap, got)
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

// --- R4.43：FLOOD_PREMIUM_WAIT（免费账号下载带宽限流）---

// TestPremiumWaitFromErrorParsesRealForms 覆盖 2.6.19 实测的原始错误形态
// 「invoke pool: rpcDoRequest: rpc error code 420: FLOOD_PREMIUM_WAIT (7)」
// 与 tgerr 风格的下划线形态；并守住「普通 FLOOD_WAIT 不被误当 premium」。
func TestPremiumWaitFromErrorParsesRealForms(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"实测包装形态", errors.New("invoke pool: rpcDoRequest: rpc error code 420: FLOOD_PREMIUM_WAIT (7)"), 7},
		{"括号形态大写", errors.New("rpc error code 420: FLOOD_PREMIUM_WAIT (13)"), 13},
		{"下划线形态", errors.New("FLOOD_PREMIUM_WAIT_3"), 3},
		{"类型化上抛", &floodWaitError{Seconds: 12, Err: errors.New("x")}, 12},
		{"普通 FLOOD_WAIT 不是 premium", errors.New("rpc error code 420: FLOOD_WAIT (1464)"), 0},
		{"无数字", errors.New("FLOOD_PREMIUM_WAIT"), 0},
		{"无关错误", errors.New("connection reset"), 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		if got := premiumWaitFromError(tc.err); got != tc.want {
			t.Errorf("%s: premiumWaitFromError=%d want %d", tc.name, got, tc.want)
		}
	}
}

// TestClassifyTransientErrorPremiumIsFlood 钉死分类：premium 限流必须进
// classFlood（按 Telegram 秒数等待），不得落进 classStall/classGeneric——
// 2.6.19 实测它落进「非瞬态」直接终态失败，用户看到的是任务躺死。
func TestClassifyTransientErrorPremiumIsFlood(t *testing.T) {
	raw := errors.New("invoke pool: rpcDoRequest: rpc error code 420: FLOOD_PREMIUM_WAIT (7)")
	if !transientSourceError(raw) {
		t.Fatal("FLOOD_PREMIUM_WAIT 必须是瞬态（transientSourceError），否则任务直接终态失败——这正是 2.6.19 的缺陷")
	}
	if got := classifyTransientError(raw); got != classFlood {
		t.Fatalf("premium 限流应归为 classFlood，得到 %v", got)
	}
	// 类型化上抛形态（长等待）也要进 classFlood。
	var typed error = &floodWaitError{Seconds: 300, Err: raw}
	if !transientSourceError(typed) {
		t.Fatal("类型化上抛形态也必须瞬态")
	}
	if got := classifyTransientError(typed); got != classFlood {
		t.Fatalf("类型化上抛形态应归为 classFlood，得到 %v", got)
	}
}

// TestAdaptivePremiumThreadsDowngrades 钉死并发降档阶梯：
// 0~2 次保持原档；≥3 次减半；≥6 次单线程；base<=0 兜底 1。
func TestAdaptivePremiumThreadsDowngrades(t *testing.T) {
	if got := adaptivePremiumThreads(4, 0); got != 4 {
		t.Fatalf("无限流应保持 4 路，得到 %d", got)
	}
	if got := adaptivePremiumThreads(4, 2); got != 4 {
		t.Fatalf("偶发限流（2 次）不应降档，得到 %d", got)
	}
	if got := adaptivePremiumThreads(4, 3); got != 2 {
		t.Fatalf("累计 3 次应减半到 2 路，得到 %d", got)
	}
	if got := adaptivePremiumThreads(4, 6); got != 1 {
		t.Fatalf("累计 6 次应降到单线程，得到 %d", got)
	}
	if got := adaptivePremiumThreads(0, 0); got != 1 {
		t.Fatalf("base<=0 应回退 1，得到 %d", got)
	}
	// 复用主连接的 1 路档不得被进一步放大或变动。
	if got := adaptivePremiumThreads(1, 9); got != 1 {
		t.Fatalf("已是单线程时保持 1，得到 %d", got)
	}
}

// TestPremiumStallCounters 钉死账号级计数：累计、隔离、清零。
func TestPremiumStallCounters(t *testing.T) {
	app := &App{}
	if n := app.notePremiumStall("u1", "a1"); n != 1 {
		t.Fatalf("首次计数应为 1，得到 %d", n)
	}
	if n := app.notePremiumStall("u1", "a1"); n != 2 {
		t.Fatalf("二次计数应为 2，得到 %d", n)
	}
	if n := app.premiumStallCount("u2", "a1"); n != 0 {
		t.Fatalf("不同用户应隔离，得到 %d", n)
	}
	app.clearPremiumStalls("u1", "a1")
	if n := app.premiumStallCount("u1", "a1"); n != 0 {
		t.Fatalf("清零后应为 0，得到 %d", n)
	}
}
