package main

// R4.40 · 瞬态错误的「退避分级」
//
// 背景（2.6.16 实测，用户日志）：
//
//	task ... start: offset=335544320/1557338504
//	using Telegram media DC 1 for native upload.getFile
//	media DC 1 通过真实 RPC 探针（候选 1/5），开始传输
//	transient failure, retry in 2m40s: invoke pool: rpcDoRequest: retryUntilAck:
//	    retry limit reached after 5 attempts
//
// 每轮只推进 2~9MB 就被「服务端不 ACK」打断，然后退避 2m40s（封顶 5 分钟）。
// 实测 13:22→13:37 十五分钟只前进 30MB；按此速率 1.5GB 要跑十几小时——
// 用户视角就是「一直卡着不动」。
//
// 根因不是「重试不够」，而是**退避策略与错误性质相反**：
//   - `rpc.RetryLimitReachedErr`（rpc/errors.go:9-16，注释原文「server does not
//     acknowledge request after multiple retries」）是**链路层丢包/黑洞**的签名：
//     请求发出去了、ACK 回不来。这类错误要的是「尽快换一条链路再试」——
//     短退避 + 重建连接。
//   - FLOOD_WAIT 才是「服务端让你慢下来」：必须按它给的秒数等。
//
// 此前两者共用 retryDelay 指数退避，等于把「能跑但会断」当「被限流」罚站。
//
// 本文件把瞬态错误分成三类，各自给出退避与动作：
//
//	classFlood   限流  → 按 Telegram 秒数精确等待（封顶 4h），不动连接
//	classStall   断流  → 短退避（5s→60s 封顶）+ 重建媒体连接 + 记 DC 断流
//	classGeneric 其它  → 沿用指数退避（找不到会话、连接建立超时等）
//
// 配套两项自适应（都是官方 downloader 的既有做法，见
// telegram/downloader/downloader.go:15「defaultPartSize = 512 * 1024」）：
//  1. DC 断流记忆：同一 DC 连续断流达阈值 → 在候选序列里降级它
//     （只降级、不排除——它可能仍是唯一可用 DC，且 FILE_MIGRATE 会把它指回来）；
//  2. 分片自适应：本尝试内连续断流 → 1MB → 512KB → 256KB。大分片在丢包链路上
//     「断一次就整片重来」，缩小分片能把损失摊薄，也更容易在 MTCP ack 窗口内完成。

import (
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/gotd/td/rpc"
)

// retryClass 描述一次瞬态失败应当采用哪种退避/动作。
type retryClass int

const (
	// classGeneric：通用瞬态（找不到会话、连接建立超时、账号未就绪…），
	// 沿用指数退避 retryDelay。
	classGeneric retryClass = iota
	// classFlood：Telegram 显式限流，按秒数精确等待。
	classFlood
	// classStall：链路断流（服务端不 ACK / 连接被掐），短退避 + 重建连接。
	classStall
)

const (
	// stallRetryDelayCap 断流退避封顶：60 秒。断流不是「对方让你等」，
	// 而是「这条链路坏了」——干等 5 分钟只会把 1.5GB 变成十几小时。
	stallRetryDelayCap = 60 * time.Second
	// mediaDCStallThreshold 同一 DC 连续断流达到该次数后，在候选序列里降级它。
	mediaDCStallThreshold = 2
	// stallShrinkPartSizeTo512KB / stallShrinkPartSizeTo256KB 分片自适应的两档，
	// 与官方 downloader 的默认分片（512KB）对齐后继续下探一档。
	stallShrinkPartSize512KB = int64(512 * 1024)
	stallShrinkPartSize256KB = int64(256 * 1024)
)

// stallTextMarkers 是「链路断流」类错误的文本签名（类型断言之外的兜底）。
// 注意 2.6.12 实测的包装形态是
// `invoke pool: rpcDoRequest: retryUntilAck: retry limit reached after 5 attempts`
// ——包了中间件名，因此既匹配 rpc 的错误文案，也匹配中间件名本身。
var stallTextMarkers = []string{
	"retry limit reached", // rpc.RetryLimitReachedErr.Error()
	"retryuntilack",       // 中间件名，出现在包装链中
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"closed network connection",
	"no route to host",
	"network is unreachable",
	"eof",
}

// transportStallError 判定一次失败是否为「链路断流」（而非限流或业务错误）。
// 优先用类型断言（rpc.RetryLimitReachedErr / io.EOF / net.ErrClosed），
// 文本匹配只做兜底——gotd 会用 go-faster/errors 包装，错误链里带上下文前缀。
func transportStallError(err error) bool {
	if err == nil {
		return false
	}
	var retryLimit *rpc.RetryLimitReachedErr
	if errors.As(err, &retryLimit) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range stallTextMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// classifyTransientError 把瞬态错误分成三类，决定退避策略与是否重建连接。
// 调用前应已确认 transientSourceError(err) 为真。
func classifyTransientError(err error) retryClass {
	if err == nil {
		return classGeneric
	}
	// 限流优先：FLOOD_WAIT 是服务端显式指令，等待秒数必须照办。
	if floodWaitFromError(err) > 0 {
		return classFlood
	}
	// 看门狗判定的「无进度挂死」与「0 分块空响应」本质也是链路坏了，
	// 只是它们由我们自己合成错误（不带 gotd 的文本签名），需显式归类。
	if errors.Is(err, errDownloadStalled) || errors.Is(err, errEmptyMediaResponse) {
		return classStall
	}
	if transportStallError(err) {
		return classStall
	}
	return classGeneric
}

// stallRetryDelay 断流重试的短退避：5s / 10s / 20s / 40s / 60s（封顶）。
// 与 retryDelay 的差别是「起步就短、封顶也短」——断流要的是尽快换链路再试。
func stallRetryDelay(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	delay := time.Duration(5*(1<<minInt(count-1, 4))) * time.Second
	if delay > stallRetryDelayCap {
		return stallRetryDelayCap
	}
	return delay
}

// adaptivePartSize 依据「本次尝试内的连续断流次数」缩小分片。
// 纯函数便于单测；base <= 0 时回退默认分片。
func adaptivePartSize(base int64, stallCount int) int64 {
	if base <= 0 {
		base = defaultPartSize
	}
	switch {
	case stallCount >= 4:
		return min64(base, stallShrinkPartSize256KB)
	case stallCount >= 2:
		return min64(base, stallShrinkPartSize512KB)
	default:
		return base
	}
}

// stallDegradeHint 给出「该 DC 是否已进入降级档」的用户可见提示（纯函数便于单测）。
func stallDegradeHint(count int) string {
	if count >= mediaDCStallThreshold {
		return "，该 DC 已在候选序列中降级"
	}
	return ""
}

// noteMediaDCStall 记一次媒体 DC 断流，返回该 DC 的连续断流累计次数。
// 只持有 a.mu，不触碰 mediaMu（锁纪律见 mediapool.go）。
func (a *App) noteMediaDCStall(userID, accountID string, dc int) int {
	if dc <= 0 {
		return 0
	}
	key := nativeAccountKey(userID, accountID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mediaDCStalls == nil {
		a.mediaDCStalls = map[string]map[int]int{}
	}
	counts, ok := a.mediaDCStalls[key]
	if !ok {
		counts = map[int]int{}
		a.mediaDCStalls[key] = counts
	}
	counts[dc]++
	return counts[dc]
}

// clearMediaDCStall 清零某 DC 的断流计数：一旦它重新跑出流量就说明链路恢复，
// 继续降级它会让我们自断可用路径。
func (a *App) clearMediaDCStall(userID, accountID string, dc int) {
	if dc <= 0 {
		return
	}
	key := nativeAccountKey(userID, accountID)
	a.mu.Lock()
	defer a.mu.Unlock()
	counts, ok := a.mediaDCStalls[key]
	if !ok {
		return
	}
	delete(counts, dc)
	if len(counts) == 0 {
		delete(a.mediaDCStalls, key)
	}
}

// mediaDCStallCounts 返回某账号的 DC 断流计数快照（拷贝，供候选排序使用）。
func (a *App) mediaDCStallCounts(userID, accountID string) map[int]int {
	key := nativeAccountKey(userID, accountID)
	a.mu.Lock()
	defer a.mu.Unlock()
	counts, ok := a.mediaDCStalls[key]
	if !ok || len(counts) == 0 {
		return nil
	}
	out := make(map[int]int, len(counts))
	for dc, n := range counts {
		out[dc] = n
	}
	return out
}
