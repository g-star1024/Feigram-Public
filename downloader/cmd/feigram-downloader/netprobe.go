package main

// R4.20 · 健康检查分级探测
//
// 背景（2.5.3/2.5.4 实测）：账号健康检查报「context deadline exceeded」，但 45s 的
// client.Run 把「代理 TCP 拨号失败」「MTProto 握手卡死」「授权 RPC 卡死」全部混成一个
// 超时，用户无从判断该修代理放行规则还是换节点。
//
// 本文件在 client.Run 之前，先对账号主 DC 做一次裸 TCP 拨号探测（走与 MTProto 相同的
// 代理拨号器），把失败拆成两层：
//   - 探测失败 → 问题在代理/出口链路（代理未放行该 DC 的 IP 直通、节点故障、端口配错）；
//   - 探测成功但 Run 超时 → 代理端口与 TCP 层正常，问题在节点转发质量（丢包/限速），
//     建议换节点或稍后重试。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram"
	dcs "github.com/gotd/td/telegram/dcs"
)

// healthProbeTimeout 分级探测的拨号超时；远小于健康检查整体 45s，避免叠加等待。
const healthProbeTimeout = 10 * time.Second

// primaryDCAddr 查 gotd 生产 DC 表返回主 DC 的 IPv4 地址（ip:port）；未知 DC 返回空串。
// 只挑 IPv4：代理场景下 IPv4 最有代表性，且 v2rayA 类出口对 IPv6 的放行往往与 IPv4 不一致。
func primaryDCAddr(dcID int) string {
	if dcID <= 0 {
		return ""
	}
	for _, option := range dcs.FindPrimaryDCs(dcs.Prod().Options, dcID, false) {
		addr := strings.TrimSpace(option.IPAddress)
		if addr == "" || strings.Contains(addr, ":") {
			continue // 跳过空地址与 IPv6
		}
		return fmt.Sprintf("%s:%d", addr, option.Port)
	}
	return ""
}

// probeTelegramTCP 用当前出口（代理拨号器；未配置代理则直连）对 addr 做一次裸 TCP
// 拨号，验证「代理 → Telegram DC」这一层是否可达。探测成功即关连接，不做任何握手。
//
// R4.31 警示：这个「成功」经代理时是假阳性——本地代理（127.0.0.1）接受
// TCP 连接只需 1ms 级，并不代表它真的把流量转发到了 Telegram。2.6.8 实测
// 五个 DC 全部「1ms 可连通」但下载 0 字节，就是代理只放行了主 DC 网段。
// 因此 TCP 探测只能作为第一层，关键判定必须用 probeTelegramMTProto。
func (a *App) probeTelegramTCP(addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dial := a.proxy.dialer()
	if dial == nil {
		var dialer net.Dialer
		dial = dialer.DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// mediaMTPProbeTimeout 单个媒体 DC 的 MTProto 握手探测超时。
const mediaMTPProbeTimeout = 6 * time.Second

// probeTelegramMTProto 对 dc 做一次真实 MTProto 密钥交换探测（走与下载完全相同的
// 代理拨号器与生产 DC 地址表）。
//
// 为什么必须这一层：MTProto 握手是真实的 DH 密钥交换，需要 Telegram 服务端逐包
// 应答——代理「本地接受」糊弄不过去。握手完成 = 该网段被真正转发。
// 探测是匿名的：不携带任何账号会话、不做授权 RPC、不落会话存储，不会触发限流
// （Telegram 对未授权握手不计数）。
func (a *App) probeTelegramMTProto(dc int, timeout time.Duration) error {
	if dc <= 0 {
		return errors.New("MTProto 探测需要有效 DC 编号")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	opts := telegram.Options{
		DC: dc,
		Resolver: dcs.Plain(dcs.PlainOptions{
			// dialer 为 nil 时 Plain 回落 net 直连，与 TCP 探测行为一致。
			Dial: a.proxy.dialer(),
		}),
		NoUpdates: true,
	}
	// api id/hash 不参与 MTProto 握手，匿名探测无需真实凭据。
	client := telegram.NewClient(1, "", opts)
	var reached atomic.Bool
	_ = client.Run(ctx, func(ctx context.Context) error {
		// 回调进入即代表 TCP + MTProto 密钥交换全部完成；立即收工，不发起任何 RPC。
		reached.Store(true)
		cancel()
		return nil
	})
	if reached.Load() {
		return nil
	}
	return errors.New("MTProto 握手未完成（超时或被对端断开）")
}

// healthStageDiagnosis 依据分级探测结果解释 client.Run 的失败，返回更精确的错误描述。
// 纯函数便于单测。
//
// R4.22：已不存在「返回空串 = 不给结论」的路径——探测被跳过（拿不到主 DC）也必须有
// 明确措辞。此前 probeAddr 为空即返回空串，错误里因此没有「分级探测：」前缀，
// 与「诊断功能根本没生效」无法区分（2.5.6 实测困惑点）。
func healthStageDiagnosis(probeDC int, probeAddr string, probeOK bool, probeDur time.Duration, probeErr, runErr error, proxyActive bool) string {
	if probeAddr == "" {
		return fmt.Sprintf(
			"分级探测：未能确定账号主 DC（session 解析结果 %d，无法查 gotd 生产地址表），TCP 分级探测已跳过——无法据此判定问题在出口链路还是 MTProto 层，请连同原始错误与代理配置一并反馈",
			probeDC)
	}
	if !probeOK {
		layer := "直连"
		if proxyActive {
			layer = "经代理"
		}
		return fmt.Sprintf(
			"分级探测：TCP 拨号 Telegram DC %d（%s）即失败（%v，耗时 %d ms）——问题在%s出口链路（代理未放行该 DC 直通、节点故障或端口配错），MTProto 握手尚未开始",
			probeDC, probeAddr, probeErr, probeDur.Milliseconds(), layer)
	}
	if runErr != nil && strings.Contains(strings.ToLower(runErr.Error()), "context deadline exceeded") {
		return fmt.Sprintf(
			"分级探测：出口已能 TCP 连通 Telegram DC %d（%s，探测 %d ms），失败发生在其后的 MTProto 握手/授权阶段——代理端口与 TCP 层正常，多为节点转发质量差（丢包/限速），建议更换节点或稍后重试",
			probeDC, probeAddr, probeDur.Milliseconds())
	}
	return ""
}

// --- R4.29 · 媒体 DC 分级探测 ---
//
// 背景（2.6.4~2.6.6 实测）：登录/会话走主 DC 单连接小 RPC，下载/缓存要走媒体 DC
// 网段 + exportAuth + 大流量长连接。代理只放行主 DC 时「登录正常、下载 0 字节」，
// 而错误里只有笼统的超时/空响应，用户无法区分「代理没放行媒体段」还是「代码坏了」。
// 本节把登录侧已有的分级探测思路推广到媒体 DC：对 DC1–DC5 全量 TCP 探测并给出
// 「放行了哪些、没放行哪些」的可读结论，写进健康检查、诊断页（/api/state）与下载错误。

// mediaProbeTimeout 单个媒体 DC 的 TCP 探测超时。
const mediaProbeTimeout = 5 * time.Second

// mediaProbeMaxDC 覆盖 Telegram 生产环境全部 5 个 DC。
const mediaProbeMaxDC = 5

// mediaDCProbe 单个 DC 的探测结果。
// R4.31：TCP 之外追加 MTProto 真握手——TCP 经代理可连通可能只是本地接受。
type mediaDCProbe struct {
	DC         int    `json:"dc"`
	Addr       string `json:"addr"`
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs"`
	Err        string `json:"error,omitempty"`
	// MTProto 真握手结果：仅在 TCP 可连通时探测（TCP 都不通的 DC 必然不可达）。
	MTPOK       bool   `json:"mtpOk"`
	MTPDuration int64  `json:"mtpMs"`
	MTPError    string `json:"mtpError,omitempty"`
}

// mediaProbeSnapshot 一次全量媒体 DC 探测的快照（进 /api/state 诊断页）。
type mediaProbeSnapshot struct {
	At      time.Time      `json:"at"`
	Proxy   bool           `json:"proxy"`
	Results []mediaDCProbe `json:"results"`
	Summary string         `json:"summary"`
}

// probeMediaDCs 并发探测 DC1–DC5 的 TCP 可达性，落快照 + 留日志。
// 结论必须可区分（R4.22 铁律）：全通 / 全不通 / 部分放行三种措辞各不相同。
func (a *App) probeMediaDCs(account NativeAccount) mediaProbeSnapshot {
	key := nativeAccountKey(account.UserID, account.AccountID)
	snap := mediaProbeSnapshot{At: time.Now(), Proxy: a.proxy.dialer() != nil}
	type job struct {
		dc   int
		addr string
	}
	jobs := make([]job, 0, mediaProbeMaxDC)
	for dc := 1; dc <= mediaProbeMaxDC; dc++ {
		if addr := primaryDCAddr(dc); addr != "" {
			jobs = append(jobs, job{dc: dc, addr: addr})
		}
	}
	results := make([]mediaDCProbe, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, dc int, addr string) {
			defer wg.Done()
			start := time.Now()
			err := a.probeTelegramTCP(addr, mediaProbeTimeout)
			p := mediaDCProbe{DC: dc, Addr: addr, DurationMs: time.Since(start).Milliseconds()}
			if err != nil {
				p.Err = err.Error()
				results[i] = p
				return
			}
			p.OK = true
			// R4.31：TCP 可连通不代表转发正常（代理本地接受假阳性），
			// 追加真实 MTProto 握手探测一锤定音。
			mtpStart := time.Now()
			if mtpErr := a.probeTelegramMTProto(dc, mediaMTPProbeTimeout); mtpErr != nil {
				p.MTPError = mtpErr.Error()
			} else {
				p.MTPOK = true
			}
			p.MTPDuration = time.Since(mtpStart).Milliseconds()
			results[i] = p
		}(i, j.dc, j.addr)
	}
	wg.Wait()
	snap.Results = results
	layer := "直连"
	if snap.Proxy {
		layer = "经代理"
	}
	snap.Summary = mediaProbeSummary(results, layer)
	a.mu.Lock()
	a.mediaProbes[key] = snap
	a.mu.Unlock()
	log.Printf("media probe: account %s %s", key, snap.Summary)
	return snap
}

// mediaProbeSummary 依据 TCP+MTProto 两层探测结果生成可区分结论（R4.22 铁律：
// 不留「空返回」路径）。纯函数便于单测。
//
// R4.31 重构：入参改为完整结果切片——TCP「可连通」经代理是假阳性（本地接受），
// 结论必须以 MTProto 真握手为准，四类措辞互斥可区分：
//  1. TCP 全挂 → 出口链路故障（代理没放行）；
//  2. TCP 通且 MTProto 全过 → 链路层真实可用；
//  3. TCP 通但 MTProto 全挂 → 代理只本地接受、未真正转发（最强诊断，直接给修法）；
//  4. 部分过 → 列出未转发网段。
func mediaProbeSummary(results []mediaDCProbe, layer string) string {
	total := len(results)
	if total == 0 {
		return "未能查到任何 DC 的生产地址，无法分级探测"
	}
	tcpOK, mtpOK := 0, 0
	tcpBad, mtpBad := make([]string, 0), make([]string, 0)
	for _, r := range results {
		if !r.OK {
			tcpBad = append(tcpBad, strconv.Itoa(r.DC))
			continue
		}
		tcpOK++
		if !r.MTPOK {
			mtpBad = append(mtpBad, strconv.Itoa(r.DC))
			continue
		}
		mtpOK++
	}
	switch {
	case tcpOK == 0:
		return fmt.Sprintf("全部 %d 个 DC 的 TCP 均不可达（%s）——代理/出口链路故障：请在代理规则放行 Telegram 全部网段", total, layer)
	case mtpOK == tcpOK:
		return fmt.Sprintf("全部 %d 个 DC 的 TCP 与 MTProto 真握手均正常（%s）——链路层真实可用", total, layer)
	case mtpOK == 0:
		return fmt.Sprintf("%d/%d 个 DC TCP 可连通但 MTProto 握手全部无响应（%s）——代理只是本地接受了连接，并未真正转发 Telegram 网段（规则未覆盖或节点黑洞）：请放行 Telegram 全部网段、改全局模式或更换节点", tcpOK, total, layer)
	default:
		return fmt.Sprintf("%d/%d 个 DC 的 MTProto 握手正常（%s）；TCP 可连通但握手无响应：DC %s——这些网段未被真正转发，下载会持续失败，请补全代理规则或更换节点", mtpOK, total, layer, strings.Join(mtpBad, "/"))
	}
}

// diagnoseMediaDC 对单个媒体 DC 做两层探测（TCP + MTProto 真握手）并给出一句
// 结论（拼进下载错误文案）。R4.31：TCP 可连通不再直接判「链路层正常」——
// 经代理时可能只是本地接受，必须以真实握手为准。
func (a *App) diagnoseMediaDC(dc int) string {
	if dc <= 0 {
		return "未能确定媒体 DC，无法分级探测"
	}
	addr := primaryDCAddr(dc)
	if addr == "" {
		return fmt.Sprintf("未能查到媒体 DC %d 的生产地址，无法分级探测", dc)
	}
	layer := "直连"
	if a.proxy.dialer() != nil {
		layer = "经代理"
	}
	start := time.Now()
	tcpErr := a.probeTelegramTCP(addr, mediaProbeTimeout)
	tcpMs := time.Since(start).Milliseconds()
	if tcpErr != nil {
		return mediaDCDiagnosis(dc, addr, false, false, tcpMs, 0, tcpErr.Error(), layer)
	}
	mtpStart := time.Now()
	mtpErr := a.probeTelegramMTProto(dc, mediaMTPProbeTimeout)
	mtpMs := time.Since(mtpStart).Milliseconds()
	mtpErrMsg := ""
	if mtpErr != nil {
		mtpErrMsg = mtpErr.Error()
	}
	return mediaDCDiagnosis(dc, addr, true, mtpErr == nil, tcpMs, mtpMs, mtpErrMsg, layer)
}

// mediaDCDiagnosis 依据两层探测结果生成下载错误里的诊断文案。纯函数便于单测。
func mediaDCDiagnosis(dc int, addr string, tcpOK, mtpOK bool, tcpMs, mtpMs int64, errText, layer string) string {
	if addr == "" {
		return fmt.Sprintf("未能查到媒体 DC %d 的生产地址，无法分级探测", dc)
	}
	if !tcpOK {
		return fmt.Sprintf(
			"分级探测：TCP 拨号媒体 DC %d（%s）即失败（%s，%d ms）——问题在%s出口链路（代理未放行媒体网段或节点故障），MTProto 层尚未开始",
			dc, addr, errText, tcpMs, layer)
	}
	if !mtpOK {
		return fmt.Sprintf(
			"分级探测：媒体 DC %d（%s）TCP 可连通（%d ms）但 MTProto 握手无响应（%s，%d ms）——经代理时 TCP 连通可能只是代理本地接受连接，实际并未转发到 Telegram（规则未覆盖或节点黑洞）：请在代理放行 Telegram 全部网段、改全局模式或更换节点",
			dc, addr, tcpMs, errText, mtpMs)
	}
	return fmt.Sprintf(
		"分级探测：媒体 DC %d（%s）TCP + MTProto 真握手均正常（TCP %d ms / 握手 %d ms，%s）——链路层真实可用，问题多在节点转发质量（丢包/限速），建议更换节点",
		dc, addr, tcpMs, mtpMs, layer)
}
