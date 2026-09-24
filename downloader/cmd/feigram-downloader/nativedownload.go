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
// 并保证不低于下限。纯函数便于单测。
func normalizePartSize(partSize int) int {
	if partSize < officialPartSizeFloor {
		return officialPartSizeFloor
	}
	return partSize - partSize%officialPartSizeFloor
}

// offsetShiftClient 把官方 downloader「从 0 开始」的请求平移到真实文件的
// resumeBase 处，使断点续传对 downloader 完全透明。
// 只平移带 offset 的两个方法；CDN/web 相关方法本路径不会调用，
// 直通以保证接口完整。
// 字段类型用 downloader.Client（接口）而非 *tg.Client，便于单测注入替身。
type offsetShiftClient struct {
	base  downloader.Client
	shift int64
}

func (c offsetShiftClient) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	req.Offset += c.shift
	return c.base.UploadGetFile(ctx, req)
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
// 返回 nil 表示官方下载器已读到文件末尾（block.last()/空块），即本次续传完成。
func officialDownloadOnce(
	ctx context.Context,
	api downloader.Client,
	file *os.File,
	location tg.InputFileLocationClass,
	base int64,
	partSize int,
	threads int,
	intervals *intervalSet,
	onWrite func(off, n, sessionWritten int64),
) error {
	writer := &shiftWriterAt{file: file, shift: base, intervals: intervals, onWrite: onWrite}
	builder := downloader.NewDownloader().
		WithPartSize(normalizePartSize(partSize)).
		Download(offsetShiftClient{base: api, shift: base}, location)
	if threads > 0 {
		builder = builder.WithThreads(threads)
	}
	_, err := builder.Parallel(ctx, writer)
	return err
}
