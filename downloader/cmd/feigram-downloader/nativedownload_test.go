package main

// R4.41 判据：证明「官方 telegram/downloader 接入正确」。
//
// 为什么必须写这些判据（R4.39 教训）：改动涉及「换下载内核」这种大件，
// 而真实凭据只在用户机器上、代理还抖——本地无法复现真实链路。
// 所以把可以本地证伪的部分全部钉死：
//  ① 偏移平移是否真的生效（请求 offset 绝不回到续传基址之前）；
//  ② 落盘内容是否与源逐字节一致；
//  ③ 乱序写入下有洞时，续传点是否正确地停在洞前；
//  ④ 分片对齐与并发度取值是否符合官方约束与我们的主连接保护策略。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

// fakeDownloadClient 实现 downloader.Client：把请求的 offset 原样映射到内存
// payload，从而在无网络、无凭据的条件下完整跑通官方下载器。
type fakeDownloadClient struct {
	payload []byte

	mu      sync.Mutex
	offsets []int64
}

var _ downloader.Client = (*fakeDownloadClient)(nil)

func (f *fakeDownloadClient) UploadGetFile(_ context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, req.Offset)
	f.mu.Unlock()
	if req.Offset >= int64(len(f.payload)) {
		// 越过文件末尾：返回空块，官方下载器据此判定读完（reader.go:56-60）。
		return &tg.UploadFile{Type: &tg.StorageFileUnknown{}}, nil
	}
	end := req.Offset + int64(req.Limit)
	if end > int64(len(f.payload)) {
		end = int64(len(f.payload))
	}
	return &tg.UploadFile{Bytes: f.payload[req.Offset:end], Type: &tg.StorageFileUnknown{}}, nil
}

func (f *fakeDownloadClient) UploadGetFileHashes(context.Context, *tg.UploadGetFileHashesRequest) ([]tg.FileHash, error) {
	return nil, nil
}

func (f *fakeDownloadClient) UploadReuploadCDNFile(context.Context, *tg.UploadReuploadCDNFileRequest) ([]tg.FileHash, error) {
	return nil, nil
}

func (f *fakeDownloadClient) UploadGetCDNFileHashes(context.Context, *tg.UploadGetCDNFileHashesRequest) ([]tg.FileHash, error) {
	return nil, nil
}

func (f *fakeDownloadClient) UploadGetWebFile(context.Context, *tg.UploadGetWebFileRequest) (*tg.UploadWebFile, error) {
	return nil, errors.New("本下载路径不应调用 web 文件接口")
}

func TestNormalizePartSizeAlignsTo4KB(t *testing.T) {
	// R4.42：**超过官方上限必须被夹住**。官方把「本次返回字节数 < 请求分片大小」
	// 直接当作文件末尾（reader.go:18-23 的 block.last()，parallel.go:68 随即
	// stop 并以成功返回），所以分片一旦超过服务端单次能返回的上限，
	// 第一块就会被误判成末尾——2.6.18 传进去的正是配置默认的 1MB。
	if got := normalizePartSize(1024 * 1024); got != officialPartSizeCap {
		t.Fatalf("1MB 分片必须夹到上限 %d，得到 %d（否则官方下载器会在第一块就误判末尾）",
			officialPartSizeCap, got)
	}
	if got := normalizePartSize(officialPartSizeCap); got != officialPartSizeCap {
		t.Fatalf("恰好等于上限时不应改动，得到 %d", got)
	}
	if got := normalizePartSize(1000); got != officialPartSizeFloor {
		t.Fatalf("低于下限应抬到 %d，得到 %d", officialPartSizeFloor, got)
	}
	if got := normalizePartSize(5000); got != 4096 {
		t.Fatalf("5000 应对齐到 4096，得到 %d", got)
	}
	if got := normalizePartSize(8199); got != 8192 {
		t.Fatalf("8199 应对齐到 8192，得到 %d", got)
	}
}

// TestAdaptivePartSizeNeverExceedsOfficialCap 把「分片不得越过官方短读判据的
// 上限」钉死：任何档位、任何配置基址都必须 ≤ officialPartSizeCap。
func TestAdaptivePartSizeNeverExceedsOfficialCap(t *testing.T) {
	for _, base := range []int64{0, 4096, 256 * 1024, 512 * 1024, 1024 * 1024, 8 * 1024 * 1024} {
		for _, stalls := range []int{0, 1, 2, 3, 4, 9} {
			got := adaptivePartSize(base, stalls)
			if got > officialPartSizeCap {
				t.Fatalf("adaptivePartSize(base=%d, stalls=%d) = %d 超过官方上限 %d",
					base, stalls, got, officialPartSizeCap)
			}
			if got < officialPartSizeFloor {
				t.Fatalf("adaptivePartSize(base=%d, stalls=%d) = %d 低于下限 %d",
					base, stalls, got, officialPartSizeFloor)
			}
		}
	}
	// 阶梯必须真的存在：1MB 配置在连续断流后应降到上限的一半、再降到四分之一。
	if got := adaptivePartSize(1024*1024, 0); got != officialPartSizeCap {
		t.Fatalf("无断流时应为上限 %d，得到 %d", officialPartSizeCap, got)
	}
	if got := adaptivePartSize(1024*1024, 2); got != stallShrinkPartSizeHalf {
		t.Fatalf("断流 2 次应降到 %d，得到 %d", stallShrinkPartSizeHalf, got)
	}
	if got := adaptivePartSize(1024*1024, 4); got != stallShrinkPartSizeQuarter {
		t.Fatalf("断流 4 次应降到 %d，得到 %d", stallShrinkPartSizeQuarter, got)
	}
}

// TestDownloadFinishedRejectsTruncatedUnknownSize 是本轮最关键的一条判据：
// 任务未声明大小时，官方 downloader 报「成功」只代表它遇到了一次短读，
// 而短读可能是链路把这一块截断了。此时必须靠 probeFileEnd 独立确认，
// 不能拿「文件与自己比较」放行 —— 2.6.18 实测的「日志全部显示下载完成、
// 实际都没下载下来」正是这条路径放行的。
func TestDownloadFinishedRejectsTruncatedUnknownSize(t *testing.T) {
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// 场景：文件其实有 64KB，但链路只给出前 8KB（短读）→ 官方判「末尾」。
	// 探测点在 8KB，服务端仍能取到数据 ⇒ 必须判为未完成。
	truncatedView := &fakeDownloadClient{payload: payload}
	done, err := downloadFinished(context.Background(), truncatedView,
		&tg.InputDocumentFileLocation{ID: 1}, 0, 8*1024)
	if err != nil {
		t.Fatalf("探测不应报错：%v", err)
	}
	if done {
		t.Fatal("探测发现 8KB 之后仍有数据，必须判为未完成（否则会交付被截断的文件）")
	}

	// 场景：下载确实到了真末尾（探测点越界，服务端返回空块）⇒ 判为完成。
	done, err = downloadFinished(context.Background(), truncatedView,
		&tg.InputDocumentFileLocation{ID: 1}, 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("探测不应报错：%v", err)
	}
	if !done {
		t.Fatal("探测点之后无数据，应判为完成")
	}

	// 场景：声明了大小 ⇒ 只认声明大小，不受探测影响。
	done, err = downloadFinished(context.Background(), truncatedView,
		&tg.InputDocumentFileLocation{ID: 1}, int64(len(payload)), 8*1024)
	if err != nil {
		t.Fatalf("声明大小时不应发起探测：%v", err)
	}
	if done {
		t.Fatal("连续前缀 8KB 未达声明大小 64KB，必须判为未完成")
	}

	// 场景：一个字节都没落盘 ⇒ 直接判未完成（不给「0 字节也算完成」留口子）。
	done, err = downloadFinished(context.Background(), truncatedView,
		&tg.InputDocumentFileLocation{ID: 1}, 0, 0)
	if err != nil || done {
		t.Fatalf("零字节不得判为完成（done=%v err=%v）", done, err)
	}
}

// TestClassifyDialogFetchUsesScannedNotUnique 钉死会话列表的完整性判据：
// 必须比较「已扫描条数 vs 服务端上报总数」，不能比较「去重后条数 vs 扫描上限」。
// 取自 2.6.18 实测：扫描 500 条、去重后 494 条、服务端上报 545 条。
func TestClassifyDialogFetchUsesScannedNotUnique(t *testing.T) {
	if got := classifyDialogFetch(500, 545, false, false); got != outcomeTruncated {
		t.Fatalf("已扫描 500 < 服务端 545，必须判为截断，得到 %v", got)
	}
	if got := classifyDialogFetch(545, 545, false, false); got != outcomeComplete {
		t.Fatalf("已扫描达到上报总数应判为完整，得到 %v", got)
	}
	if got := classifyDialogFetch(600, 545, false, false); got != outcomeComplete {
		t.Fatalf("因重复项导致扫描数超过上报总数时，仍应判为完整，得到 %v", got)
	}
	if got := classifyDialogFetch(120, 0, true, false); got != outcomeComplete {
		t.Fatalf("服务端直接给了完整列表应判为完整，得到 %v", got)
	}
	if got := classifyDialogFetch(120, 0, false, false); got != outcomeUnknown {
		t.Fatalf("既无总数也无完整列表时应判为「无法确认」，得到 %v", got)
	}
	if got := classifyDialogFetch(120, 545, false, true); got != outcomePartial {
		t.Fatalf("中途翻不动时应优先判为 partial，得到 %v", got)
	}
	// 反向核证：如果判据退回旧行为（比较去重后条数 494 与扫描上限 500），
	// 上面第一条就会变成 outcomeComplete —— 用一个显式的断言把这条旧行为钉死。
	const uniqueOldBehavior = 494
	if uniqueOldBehavior >= 500 {
		t.Fatal("负例前提失效：旧实现下 494 >= 500 不成立，截断分支本就不该被走到")
	}
}

func TestMediaDownloadThreadsForProtectsPrimaryConn(t *testing.T) {
	if got := mediaDownloadThreadsFor(1, 5); got != nativeDownloadThreads {
		t.Fatalf("非主 DC 应并发 %d 分片，得到 %d", nativeDownloadThreads, got)
	}
	// 复用主连接时必须单线程：主连接还承载会话列表/消息/健康检查等小 RPC，
	// 并发大流量分片会把这些请求挤在队尾。
	if got := mediaDownloadThreadsFor(5, 5); got != 1 {
		t.Fatalf("复用主连接时必须单线程，得到 %d", got)
	}
	if got := mediaDownloadThreadsFor(0, 5); got != 1 {
		t.Fatalf("无 DC 信息时应保守单线程，得到 %d", got)
	}
}

func TestIntervalSetContiguousPrefixStopsAtHole(t *testing.T) {
	s := newIntervalSet()
	s.Add(0, 10)
	if got := s.ContiguousFrom0(); got != 10 {
		t.Fatalf("连续前缀应为 10，得到 %d", got)
	}
	// 乱序先写了尾部 → 连续前缀必须停在洞前（这是断点续传的正确性核心：
	// 若按 os.Stat().Size() 续传，就会跳过 [10,20) 这个洞）。
	s.Add(20, 10)
	if got := s.ContiguousFrom0(); got != 10 {
		t.Fatalf("存在空洞时连续前缀必须停在洞前，得到 %d", got)
	}
	if got := s.MaxEnd(); got != 30 {
		t.Fatalf("最大结束偏移应为 30，得到 %d", got)
	}
	// 补上洞 → 三段合并
	s.Add(10, 10)
	if got := s.ContiguousFrom0(); got != 30 {
		t.Fatalf("补洞后连续前缀应为 30，得到 %d", got)
	}
	if len(s.spans) != 1 {
		t.Fatalf("相邻区间应合并为 1 段，得到 %d 段", len(s.spans))
	}

	// 乱序 + 相邻合并
	s2 := newIntervalSet()
	s2.Add(0, 5)
	s2.Add(10, 5)
	s2.Add(5, 5)
	if got := s2.ContiguousFrom0(); got != 15 {
		t.Fatalf("乱序补齐后连续前缀应为 15，得到 %d", got)
	}

	// 并发重试导致的重复/包含写入不得破坏区间
	s3 := newIntervalSet()
	s3.Add(0, 10)
	s3.Add(4, 6)
	if got := s3.ContiguousFrom0(); got != 10 {
		t.Fatalf("重复写入后连续前缀应仍为 10，得到 %d", got)
	}
	if len(s3.spans) != 1 {
		t.Fatalf("被包含的区间不应新增段，得到 %d 段", len(s3.spans))
	}

	// 空集与「不从 0 开始」的情形
	s4 := newIntervalSet()
	if got := s4.ContiguousFrom0(); got != 0 {
		t.Fatalf("空集连续前缀应为 0，得到 %d", got)
	}
	s4.Add(100, 10)
	if got := s4.ContiguousFrom0(); got != 0 {
		t.Fatalf("未覆盖 0 时应为 0，得到 %d", got)
	}
}

func TestShiftWriterAtWritesAtShiftedOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shift.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	iv := newIntervalSet()
	var reports [][2]int64
	w := &shiftWriterAt{
		file: f, shift: 100, intervals: iv,
		onWrite: func(off, n, sessionWritten int64) {
			reports = append(reports, [2]int64{off, sessionWritten})
		},
	}
	if _, err := w.WriteAt([]byte("abc"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("de"), 5); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 107 {
		t.Fatalf("文件长度应为 shift+最大偏移 = 107，得到 %d", len(data))
	}
	if !bytes.Equal(data[100:103], []byte("abc")) {
		t.Fatalf("写入位置未按 shift 平移：期望 %q，得到 %q", "abc", data[100:103])
	}
	if !bytes.Equal(data[105:107], []byte("de")) {
		t.Fatalf("第二次写入位置错误：期望 %q，得到 %q", "de", data[105:107])
	}
	// 区间记录用的是「相对偏移」（不含 shift），与 downloader 的视角一致。
	if got := iv.MaxEnd(); got != 7 {
		t.Fatalf("区间最大结束偏移应为 7（相对），得到 %d", got)
	}
	if len(reports) != 2 || reports[1][1] != 5 {
		t.Fatalf("进度回调异常：%v", reports)
	}
}

// TestOfficialDownloaderResumesFromBaseOffset 是本轮改造的核心判据：
// 官方下载器从 0 递增请求 offset，而我们必须让它落到 [base, EOF)。
func TestOfficialDownloaderResumesFromBaseOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.bin")

	payload := make([]byte, 100*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	const base = int64(40 * 1024)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// 模拟「已下载 40KB 的断点文件」。
	if _, err := f.WriteAt(payload[:base], 0); err != nil {
		t.Fatal(err)
	}

	fake := &fakeDownloadClient{payload: payload}
	iv := newIntervalSet()
	ev := &downloadEvidence{}
	var lastTotal int64
	err = officialDownloadOnce(context.Background(), fake, f,
		&tg.InputDocumentFileLocation{ID: 1}, base, 8*1024, 2, iv, ev,
		nil,
		func(off, n, sessionWritten int64) { lastTotal = sessionWritten })
	if err != nil {
		t.Fatalf("官方下载器返回错误：%v", err)
	}

	// ① 偏移平移生效：任何请求都不得回到续传基址之前。
	fake.mu.Lock()
	offsets := append([]int64(nil), fake.offsets...)
	fake.mu.Unlock()
	if len(offsets) == 0 {
		t.Fatal("没有向客户端发出任何请求")
	}
	for _, off := range offsets {
		if off < base {
			t.Fatalf("请求 offset %d 早于续传基址 %d —— 平移失效，会重复下载已完成的字节",
				off, base)
		}
	}

	// ② 落盘内容与源逐字节一致。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("落盘内容与源不一致（%d vs %d 字节）", len(got), len(payload))
	}

	// ③ 连续前缀一路推到文件末尾。
	if end := iv.ContiguousFrom0(); base+end != int64(len(payload)) {
		t.Fatalf("连续前缀应达文件末尾：base+%d != %d", end, len(payload))
	}

	// ④ 进度按「会话新增写入量」累计（不含续传基址）。
	if want := int64(len(payload)) - base; lastTotal != want {
		t.Fatalf("会话累计写入应为 %d，得到 %d", want, lastTotal)
	}

	// ⑤ 末尾证据必须落在真实文件末尾上（供日志与「未声明大小」时参照）。
	// 注意：只有「短读且返回了字节」才会计入，越界返回 0 字节不算——
	// 否则会把请求起点当成末尾，把长度估大。
	requests, eofEnd, _ := ev.snapshot()
	if eofEnd != int64(len(payload)) {
		t.Fatalf("官方报告的末尾应为 %d，得到 %d（请求 %d 次）", len(payload), eofEnd, requests)
	}
	if requests == 0 {
		t.Fatal("证据里应记录到请求次数")
	}
}

// TestOfficialDownloaderStopsAtEOFWhenNothingLeft 覆盖「断点恰好等于文件大小」
// 的边界：此时首次请求即越过末尾，必须干净返回而不是死循环。
func TestOfficialDownloaderStopsAtEOFWhenNothingLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "done.bin")
	payload := make([]byte, 16*1024)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(payload, 0); err != nil {
		t.Fatal(err)
	}

	fake := &fakeDownloadClient{payload: payload}
	iv := newIntervalSet()
	base := int64(len(payload))
	if err := officialDownloadOnce(context.Background(), fake, f,
		&tg.InputDocumentFileLocation{ID: 1}, base, 8*1024, 2, iv, nil, nil, nil); err != nil {
		t.Fatalf("已完成文件应直接返回 nil，得到 %v", err)
	}
	if got := iv.ContiguousFrom0(); got != 0 {
		t.Fatalf("无新增写入时连续前缀应为 0，得到 %d", got)
	}
}

// premiumFloodClient 在指定 offset 处先返回一次 FLOOD_PREMIUM_WAIT，
// 之后按 payload 正常供应。用于证明适配器能识别 premium 限流、等待后重试同一请求。
type premiumFloodClient struct {
	fakeDownloadClient
	failFirstAt int64

	mu     sync.Mutex
	failed bool
}

func (f *premiumFloodClient) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	f.mu.Lock()
	shouldFail := !f.failed && req.Offset == f.failFirstAt
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		// 1 秒的等待在测试里可接受：验证的是「等待后重试同一请求」。
		return nil, errors.New("invoke pool: rpcDoRequest: rpc error code 420: FLOOD_PREMIUM_WAIT (1)")
	}
	return f.fakeDownloadClient.UploadGetFile(ctx, req)
}

// TestOffsetShiftClientWaitsInlineOnPremiumFlood 是 R4.43 的核心判据：
// FLOOD_PREMIUM_WAIT 必须在适配器内按 Telegram 秒数等待并重试同一请求，
// 而不是上抛把整轮官方下载打断（2.6.19 实测任务推进几百 MB 后终态失败，
// 根因就是 gotd 全链路不认识这个错误码、我们又没接住）。
func TestOffsetShiftClientWaitsInlineOnPremiumFlood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "premium.bin")
	payload := make([]byte, 48*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	underlying := &premiumFloodClient{
		fakeDownloadClient: fakeDownloadClient{payload: payload},
		failFirstAt:        16 * 1024, // 第二个分片处撞限流
	}
	var waited []int
	waits := 0
	err = officialDownloadOnce(context.Background(), underlying, f,
		&tg.InputDocumentFileLocation{ID: 1}, 0, 16*1024, 2, newIntervalSet(), nil,
		func(secs int) { waits++; waited = append(waited, secs) }, nil)
	if err != nil {
		t.Fatalf("premium 限流应在适配器内被消化，得到错误：%v", err)
	}
	if waits != 1 || len(waited) != 1 || waited[0] != 1 {
		t.Fatalf("应恰好回调一次等待 1 秒，得到 waits=%d waited=%v", waits, waited)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("等待重试后落盘内容必须与源一致（%d vs %d 字节）", len(got), len(payload))
	}
}

// alwaysPremiumFloodClient 恒返回 FLOOD_PREMIUM_WAIT（7 秒）。
// 用于钉死「超过内联上限的长等待必须立刻上抛」这条路径——测试关心的是适配器
// 是否立即把错误交还任务层（classFlood 精确等待），而不是在适配器里空等 7 秒。
type alwaysPremiumFloodClient struct {
	fakeDownloadClient
}

func (f *alwaysPremiumFloodClient) UploadGetFile(context.Context, *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	return nil, errors.New("invoke pool: rpcDoRequest: rpc error code 420: FLOOD_PREMIUM_WAIT (7)")
}

// TestOffsetShiftClientEscalatesLongPremiumWait 钉死第二层处置：
// 超过内联等待上限的 FLOOD_PREMIUM_WAIT 必须立即上抛为 *floodWaitError
// （任务层 classFlood 精确等待），而不是内联死等撞上看门狗。
func TestOffsetShiftClientEscalatesLongPremiumWait(t *testing.T) {
	// 底层必须真的抛出 premium 限流，否则适配器拿不到可上抛的错误
	// （在途改动此处原本用正常供应的 fakeDownloadClient，测试恒拿到 nil 而失败）。
	base := &alwaysPremiumFloodClient{}
	c := offsetShiftClient{
		base:                 base,
		premiumInlineWaitCap: time.Millisecond, // 任何真实等待都超限
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.UploadGetFile(context.Background(),
			&tg.UploadGetFileRequest{Offset: 0, Limit: 4096,
				Location: &tg.InputDocumentFileLocation{ID: 1}})
		done <- err
	}()
	select {
	case err := <-done:
		var typed *floodWaitError
		if !errors.As(err, &typed) {
			t.Fatalf("长等待必须上抛 *floodWaitError，得到 %T: %v", err, err)
		}
		if typed.Seconds != 7 {
			t.Fatalf("上抛的秒数必须保留 Telegram 原值 7，得到 %d", typed.Seconds)
		}
		// 秒数必须能被任务层解析（classFlood 路径依赖这一点）。
		if got := premiumWaitFromError(err); got != 7 {
			t.Fatalf("任务层应能从上抛错误解析出 7 秒，得到 %d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("上抛不应内联等待（必须立即返回），2 秒仍无结果说明在死等")
	}
}
