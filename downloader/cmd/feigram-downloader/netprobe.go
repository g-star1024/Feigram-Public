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
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

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
type mediaDCProbe struct {
	DC         int    `json:"dc"`
	Addr       string `json:"addr"`
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs"`
	Err        string `json:"error,omitempty"`
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
			} else {
				p.OK = true
			}
			results[i] = p
		}(i, j.dc, j.addr)
	}
	wg.Wait()
	snap.Results = results
	okCount := 0
	bad := make([]string, 0)
	for _, r := range results {
		if r.OK {
			okCount++
		} else {
			bad = append(bad, strconv.Itoa(r.DC))
		}
	}
	layer := "直连"
	if snap.Proxy {
		layer = "经代理"
	}
	snap.Summary = mediaProbeSummary(okCount, len(results), bad, layer)
	a.mu.Lock()
	a.mediaProbes[key] = snap
	a.mu.Unlock()
	log.Printf("media probe: account %s %s", key, snap.Summary)
	return snap
}

// mediaProbeSummary 依据探测结果生成可区分的三类结论（R4.22 铁律：不留「空返回」路径）。
// 纯函数便于单测。
func mediaProbeSummary(okCount, total int, bad []string, layer string) string {
	switch {
	case total == 0:
		return "未能查到任何 DC 的生产地址，无法分级探测"
	case okCount == total:
		return fmt.Sprintf("全部 %d 个 DC 的 TCP 均可连通（%s）——链路层正常", okCount, layer)
	case okCount == 0:
		return fmt.Sprintf("全部 %d 个 DC 的 TCP 均不可达（%s）——代理/出口链路故障：请在代理规则放行 Telegram 全部网段", total, layer)
	default:
		return fmt.Sprintf("%d/%d 个 DC 可达（%s），不可达：DC %s——代理仅放行了部分 Telegram 网段，下载会持续失败，请放行全部媒体 DC",
			okCount, total, layer, strings.Join(bad, "/"))
	}
}

// diagnoseMediaDC 对单个媒体 DC 快速探测并给出一句话结论（拼进下载错误文案）。
func (a *App) diagnoseMediaDC(dc int) string {
	if dc <= 0 {
		return "未能确定媒体 DC，无法分级探测"
	}
	addr := primaryDCAddr(dc)
	if addr == "" {
		return fmt.Sprintf("未能查到媒体 DC %d 的生产地址，无法分级探测", dc)
	}
	start := time.Now()
	err := a.probeTelegramTCP(addr, mediaProbeTimeout)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		return fmt.Sprintf("分级探测：TCP 拨号媒体 DC %d（%s）即失败（%d ms）——问题在出口链路（代理未放行媒体网段或节点故障），MTProto 层尚未开始", dc, addr, dur)
	}
	return fmt.Sprintf("分级探测：媒体 DC %d（%s）TCP 可连通（%d ms）——链路层正常，问题多在节点转发质量（丢包/限速），建议更换节点", dc, addr, dur)
}
