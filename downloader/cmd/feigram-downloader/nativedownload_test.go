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
	if got := normalizePartSize(1024 * 1024); got != 1024*1024 {
		t.Fatalf("已对齐的分片不应被改动，得到 %d", got)
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
	var lastTotal int64
	err = officialDownloadOnce(context.Background(), fake, f,
		&tg.InputDocumentFileLocation{ID: 1}, base, 8*1024, 2, iv,
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
		&tg.InputDocumentFileLocation{ID: 1}, base, 8*1024, 2, iv, nil); err != nil {
		t.Fatalf("已完成文件应直接返回 nil，得到 %v", err)
	}
	if got := iv.ContiguousFrom0(); got != 0 {
		t.Fatalf("无新增写入时连续前缀应为 0，得到 %d", got)
	}
}
