package main

// R4.41 · 下载内核改用官方 telegram/downloader
//
// 背景（2.6.16 实测）：此前是自己手写的 upload.getFile 循环——单线程、顺序、
// 1MB 分片，唯一的重试发生在整片失败之后由外层按错误分类退避。实测表现为
// 「每轮只推进 2~9MB 就被服务端不 ACK 打断，然后退避 2m40s」，1.5GB 的文件
// 要跑十几小时。
//
// 而 gotd 早就随库提供了生产级实现 telegram/downloader（1197 行含单测）：
//   - 并发分片：WithThreads(n) 起 n 个 goroutine 同时拉不同 offset（parallel.go）；
//   - 分片级重试：reader.next（reader.go:90-106）内建
//     `tgerr.FloodWait`（按 Telegram 秒数等待）与 `tg.ErrTimeout`（立即重试），
//     这些此前要靠外层 2m40s 退避兜；
//   - 512KB 默认分片（downloader.go:15 `defaultPartSize = 512 * 1024`，官方推荐值）；
//   - Download() 内部固定 allowCDN=false（downloader.go:33-40），
//     因此不会收到 CDN redirect —— 恰好消除我们原先
//     「暂不支持 CDN redirect 响应」的报错路径。
//
// 我们此前不仅重复实现了它，还漏掉了并发与分片级重试，这正是「能跑但会断」。
//
// 接入只需两处「偏移平移」适配（官方 reader 的 offset 从 0 递增，
// 而我们要从断点续传），其余逻辑（DC 选择/迁移、file_reference 刷新、
// 看门狗、错误分类退避）全部保留：
//
//	① offsetShiftClient：在 Client 层把请求 offset 加上 resumeBase
//	   —— 断点续传对 downloader 透明，它以为在下整个文件；
//	② shiftWriterAt：同步平移写入偏移，并回调进度/喂狗，
//	   同时把「已写区间」记进 intervalSet。
//
// 为什么必须记区间：并发分片是**乱序**写入，中途失败时文件尾部可能已有数据
// 但中间有洞。断点续传只能从「从 0 起的连续前缀」之后继续，直接拿
// os.Stat().Size() 当续传点会把洞永久留在文件里。

import (
	"context"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

// nativeDownloadThreads 是单个媒体 DC 的并发分片数，同时也是该 DC 连接池的
// 连接数上限（client.DC 的 max 参数）。官方 builder 默认单线程
// （builder.go:28 `threads: 1`），我们按多连接下载策略取 4：既提升吞吐，
// 也让「某个分片丢包」不再拖住整条流。
const nativeDownloadThreads = 4

// officialPartSizeFloor 是官方 downloader 对分片的下限要求
// （downloader.go:22-25「Must be divisible by 4KB」）。
const officialPartSizeFloor = 4096

// officialPartSizeCap 是**必须遵守**的分片上限（R4.42）。
//
// 原因：官方 downloader 把「本次返回的字节数 < 请求的分片大小」直接当作
// 「已到文件末尾」——见 reader.go:18-23 的 block.last()，parallel.go:68
// 一旦判定 last 就立刻 stop()，整个 Parallel 以**成功**返回。
//
// 于是分片只要超过服务端单次 upload.getFile 实际能返回的上限，第一块
// 就会被判成末尾：go 侧看到的是「官方下载器成功返回」，实际只落了一小块。
// 官方自己的默认值就是 512KB（downloader.go:15 defaultPartSize），
// 这里对齐它——**不允许超过**。2.6.18 的接入漏了这条，直接把配置里的
// 1MB 传了进去（main.go 的 defaultPartSize = 1024*1024），
// 实测表现就是「任务秒完成 / 反复报不完整」。
const officialPartSizeCap = 512 * 1024

// mediaDownloadThreadsFor 给出某个媒体 DC 应使用的并发分片数。
// 复用主连接时降为 1：主连接同时承载会话列表、消息、健康检查等小 RPC，
// 大流量并发分片会把这些请求挤在队尾（这也是 Telegram 官方客户端
// 对大文件另开专用连接的原因）。
func mediaDownloadThreadsFor(dc, primaryDC int) int {
	if dc <= 0 || dc == primaryDC {
		return 1
	}
	return nativeDownloadThreads
}

// normalizePartSize 把分片对齐到 4KB 的整数倍（官方硬要求），
// 并夹在 [officialPartSizeFloor, officialPartSizeCap] 之间。
// 上限是硬约束而非建议：超过它就会撞上官方「短读即末尾」的误判（见上）。
// 纯函数便于单测。
func normalizePartSize(partSize int) int {
	if partSize < officialPartSizeFloor {
		return officialPartSizeFloor
	}
	if partSize > officialPartSizeCap {
		partSize = officialPartSizeCap
	}
	return partSize - partSize%officialPartSizeFloor
}

// downloadEvidence 记录官方 downloader 自己的「末尾证据」与请求统计（R4.42）。
//
// 官方判定末尾的唯一依据是「返回字节数 < 请求分片大小」（reader.go:20-22），
// 这里把那次短读的绝对结束位置记下来，用于：
//
//	① 日志：让用户一眼看到「官方报告到达末尾于第 N 字节」；
//	② 任务未声明大小时的独立参照（绝不拿文件与自己比较）。
//
// 注意：短读**既可能是真末尾，也可能是链路把这一块截断了**——后者在丢包
// 链路上很常见。因此这个值只作为线索，最终是否算下载完成由
// probeFileEnd 再发一次小请求独立确认（见 main.go 的 downloadFinished）。
type downloadEvidence struct {
	mu       sync.Mutex
	requests int   // 已发出的 upload.getFile 次数
	lastEnd  int64 // 绝对偏移：最近一次响应覆盖到的位置
	eofEnd   int64 // 绝对偏移：出现「短读」时该响应覆盖到的位置（>0 表示官方报告过末尾）
}

func (e *downloadEvidence) note(absOffset int64, requested, returned int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests++
	end := absOffset + int64(returned)
	if end > e.lastEnd {
		e.lastEnd = end
	}
	// 只有「短读且返回了字节」才能推算出文件末尾：返回 0 字节只说明
	// 请求起点已经越过末尾，反过来会把末尾估大（例如续传基址正确、
	// 首次请求即越界的情形）。
	if returned > 0 && returned < requested && end > e.eofEnd {
		e.eofEnd = end
	}
}

func (e *downloadEvidence) snapshot() (requests int, eofEnd, lastEnd int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests, e.eofEnd, e.lastEnd
}

// offsetShiftClient 把官方 downloader「从 0 开始」的请求平移到真实文件的
// resumeBase 处，使断点续传对 downloader 完全透明。
// 只平移带 offset 的两个方法；CDN/web 相关方法本路径不会调用，
// 直通以保证接口完整。
// 字段类型用 downloader.Client（接口）而非 *tg.Client，便于单测注入替身。
//
// R4.43：FLOOD_PREMIUM_WAIT（免费账号下载带宽限流）gotd 全链路都不认识，
// 会原样上抛。这里在适配器内做两层处置：
//  1. 短等待（≤ premiumInlineWaitCap，默认 3 分钟 < 看门狗 5 分钟）——
//     内联睡够 Telegram 给的秒数后重试同一请求，不打断官方 downloader
//     的分片状态机，4 路并发各自独立等待，代价最小；
//  2. 长等待——包装成 *floodWaitError 上抛，由任务层 classFlood 精确等待
//     （封顶 4h），避免内联空等撞上看门狗被误杀。
type offsetShiftClient struct {
	base     downloader.Client
	shift    int64
	evidence *downloadEvidence // 可为 nil（单测中不需要统计时）
	// premiumInlineWaitCap 内联等待上限；0 取默认 nativePremiumInlineWaitCap。
	premiumInlineWaitCap time.Duration
	// onPremiumWait 每次撞上 FLOOD_PREMIUM_WAIT 时回调（秒数），可为 nil。
	// 用于任务层计数与日志；在等待发生前调用。
	onPremiumWait func(secs int)
}

func (c offsetShiftClient) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	req.Offset += c.shift
	for {
		resp, err := c.base.UploadGetFile(ctx, req)
		if err == nil {
			if c.evidence != nil {
				if f, ok := resp.(*tg.UploadFile); ok {
					c.evidence.note(req.Offset, req.Limit, len(f.Bytes))
				}
			}
			return resp, nil
		}
		// R4.43：FLOOD_PREMIUM_WAIT 按 Telegram 秒数等待后重试同一请求。
		// 官方 master.go:36 每个分片都新建请求对象，req 里的 offset 不会被
		// 复用污染；等待期间响应 ctx 取消（用户暂停/看门狗）立即退出。
		if secs := premiumWaitFromError(err); secs > 0 {
			if c.onPremiumWait != nil {
				c.onPremiumWait(secs)
			}
			wait := time.Duration(secs) * time.Second
			if cap := c.premiumInlineWaitCap; cap > 0 && wait > cap {
				return nil, &floodWaitError{Seconds: secs, Err: err}
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		}
		return nil, err
	}
}

func (c offsetShiftClient) UploadGetFileHashes(ctx context.Context, req *tg.UploadGetFileHashesRequest) ([]tg.FileHash, error) {
	req.Offset += c.shift
	return c.base.UploadGetFileHashes(ctx, req)
}

func (c offsetShiftClient) UploadReuploadCDNFile(ctx context.Context, req *tg.UploadReuploadCDNFileRequest) ([]tg.FileHash, error) {
	return c.base.UploadReuploadCDNFile(ctx, req)
}

func (c offsetShiftClient) UploadGetCDNFileHashes(ctx context.Context, req *tg.UploadGetCDNFileHashesRequest) ([]tg.FileHash, error) {
	return c.base.UploadGetCDNFileHashes(ctx, req)
}

func (c offsetShiftClient) UploadGetWebFile(ctx context.Context, req *tg.UploadGetWebFileRequest) (*tg.UploadWebFile, error) {
	return c.base.UploadGetWebFile(ctx, req)
}

// intervalSet 维护「已写入的字节区间」（相对本次会话基址的偏移），
// 用于在并发乱序写入后求出「从 0 起的连续前缀」。
// 区间数受并发度限制（官方 writeAtLoop 按请求顺序串行写，乱序窗口 = 线程数），
// 因此线性合并足够快，无需树结构。
type intervalSet struct {
	spans [][2]int64 // 按 start 升序、两两不相交且不相邻（已合并）
}

// newIntervalSet 建一个空的区间集合。
func newIntervalSet() *intervalSet {
	return &intervalSet{}
}

// Reset 清空（每轮 downloader 调用前重置，因为基址会变）。
func (s *intervalSet) Reset() {
	s.spans = s.spans[:0]
}

// Add 记入区间 [start, start+length)，与既有区间合并。
func (s *intervalSet) Add(start, length int64) {
	if length <= 0 {
		return
	}
	end := start + length
	// 找到第一个「结束点 >= start」的区间作为合并起点。
	i := sort.Search(len(s.spans), func(i int) bool { return s.spans[i][1] >= start })
	j := i
	for j < len(s.spans) && s.spans[j][0] <= end {
		if s.spans[j][0] < start {
			start = s.spans[j][0]
		}
		if s.spans[j][1] > end {
			end = s.spans[j][1]
		}
		j++
	}
	merged := [2]int64{start, end}
	// 先拷出被覆盖区间之后的尾巴，避免 append 覆盖污染。
	rest := append([][2]int64(nil), s.spans[j:]...)
	s.spans = append(s.spans[:i], merged)
	s.spans = append(s.spans, rest...)
}

// ContiguousFrom0 返回「从 0 起连续已写」的长度——断点续传的唯一合法起点。
func (s *intervalSet) ContiguousFrom0() int64 {
	if len(s.spans) == 0 || s.spans[0][0] > 0 {
		return 0
	}
	// Add 会把「相交或相邻」的区间合并，因此 spans[0] 的结束点即连续前缀。
	return s.spans[0][1]
}

// MaxEnd 返回已写入的最大结束偏移（用于完成时的收尾统计）。
func (s *intervalSet) MaxEnd() int64 {
	if len(s.spans) == 0 {
		return 0
	}
	return s.spans[len(s.spans)-1][1]
}

// shiftWriterAt 是给官方 downloader 的 io.WriterAt：
//   - 把 downloader 的偏移平移到真实文件位置（shift + off）；
//   - 每次写入后回调进度（喂看门狗、更新 UI、限速反压）；
//   - 记录已写区间，供续传点计算。
//
// 并发安全说明：官方 writeAtLoop（sink.go:10-28）是**单 goroutine 顺序调用**
// WriteAt，因此这里无需加锁。
type shiftWriterAt struct {
	file      *os.File
	shift     int64
	intervals *intervalSet
	written   int64 // 本次会话累计写入字节
	onWrite   func(off, n, sessionWritten int64)
}

func (w *shiftWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := w.file.WriteAt(p, w.shift+off)
	if n > 0 {
		if w.intervals != nil {
			w.intervals.Add(off, int64(n))
		}
		w.written += int64(n)
		if w.onWrite != nil {
			w.onWrite(off, int64(n), w.written)
		}
	}
	return n, err
}

// officialDownloadOnce 走官方 downloader 从 base 处下载到文件末尾。
// 返回 nil 表示官方下载器遇到了「短读 / 空块」而收工——**这不等于文件完整**，
// 是否完整由调用方用权威长度或 probeFileEnd 确认（见 main.go）。
func officialDownloadOnce(
	ctx context.Context,
	api downloader.Client,
	file *os.File,
	location tg.InputFileLocationClass,
	base int64,
	partSize int,
	threads int,
	intervals *intervalSet,
	evidence *downloadEvidence,
	onPremiumWait func(secs int),
	onWrite func(off, n, sessionWritten int64),
) error {
	writer := &shiftWriterAt{file: file, shift: base, intervals: intervals, onWrite: onWrite}
	builder := downloader.NewDownloader().
		WithPartSize(normalizePartSize(partSize)).
		Download(offsetShiftClient{
			base:     api,
			shift:    base,
			evidence: evidence,
			// R4.43：内联等待上限默认 3 分钟（< 看门狗已传输档 5 分钟），
			// 超过的 FLOOD_PREMIUM_WAIT 上抛任务层精确等待。
			premiumInlineWaitCap: nativePremiumInlineWaitCap,
			onPremiumWait:        onPremiumWait,
		}, location)
	if threads > 0 {
		builder = builder.WithThreads(threads)
	}
	_, err := builder.Parallel(ctx, writer)
	return err
}

// probeFileEndProbeSize 是末尾探测的请求大小——一个 4KB 块，代价可忽略。
const probeFileEndProbeSize = 4096

// downloadFinished 判定「本轮官方 downloader 报成功之后」文件是否真的完整（R4.42）。
//
//   - 任务声明了大小：连续前缀达到声明值即完成。这是权威判据——链路短读
//     导致的假末尾在这里会被直接挡掉（官方说成功，但前缀没到，就不算完）。
//   - 任务未声明大小：**不能**拿文件与自己比较（那恒为真），改为向连续前缀处
//     再请求一个 4KB 块做独立确认：真末尾返回空块；仍能取到字节说明这里不是
//     末尾，必须继续续传。
//
// 这条判据存在的理由就是 2.6.18 的实测：官方「短读即末尾」被判成功 →
// 外层把「有洞/被截断的文件」当完整文件改名交付，日志显示全部完成、
// 实际文件不可用。
func downloadFinished(
	ctx context.Context,
	api downloader.Client,
	location tg.InputFileLocationClass,
	declared, downloaded int64,
) (bool, error) {
	if declared > 0 {
		return downloaded >= declared, nil
	}
	if downloaded <= 0 {
		return false, nil
	}
	return probeFileEnd(ctx, api, location, downloaded)
}

// probeFileEnd 在 offset 处请求 4KB，用「服务端是否还有数据」独立判定
// 该偏移是否就是文件末尾（R4.42）。
//
// 它解决的是官方 downloader「短读即末尾」判据的固有歧义：短读可能是真末尾，
// 也可能是链路把这一块截断了。真末尾返回空块；若仍取到字节，
// 说明文件在这里还没结束——任务就必须继续续传，而不是当作下完了交付。
//
// 这是「判据要运行产物本身」的一个实例：不信任上游给的结论，
// 用一次独立的最小请求去验证它。
func probeFileEnd(ctx context.Context, api downloader.Client, location tg.InputFileLocationClass, offset int64) (bool, error) {
	if offset < 0 {
		return false, nil
	}
	req := &tg.UploadGetFileRequest{
		Offset:   offset,
		Limit:    probeFileEndProbeSize,
		Location: location,
	}
	req.SetPrecise(true)
	resp, err := api.UploadGetFile(ctx, req)
	if err != nil {
		return false, err
	}
	f, ok := resp.(*tg.UploadFile)
	if !ok {
		// CDN redirect 等非预期形态：本路径 allowCDN=false，不应出现；
		// 无法确认就按「未到末尾」处理，交由调用方续传/报错。
		return false, nil
	}
	return len(f.Bytes) == 0, nil
}
