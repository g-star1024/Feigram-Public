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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram"
	dcs "github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
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
// R4.32：6s→10s——2.6.9 实测高延迟节点握手就要 4~5s，6s 上限导致随机抖动
// （每轮 4/5、挂的 DC 各不相同），无法区分「节点波动」与「网段黑洞」。
const mediaMTPProbeTimeout = 10 * time.Second

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

// probeTelegramMTProtoSteady 带一次重试的稳态握手探测。
// R4.32：高延迟节点单次握手常贴着超时上限（实测 4~5s），失败重试一次，
// 消除「每轮随机挂掉一个不同 DC」的抖动误报。
func (a *App) probeTelegramMTProtoSteady(dc int, timeout time.Duration) error {
	if err := a.probeTelegramMTProto(dc, timeout); err == nil {
		return nil
	}
	return a.probeTelegramMTProto(dc, timeout)
}

// latestMediaHandshakeMs 返回该账号最近一次媒体探测中指定 DC 的 MTProto 握手
// 耗时（毫秒）；无快照、该 DC 未测过或握手未成功时返回 0（调用方按默认阈值处理）。
// R4.32：供首字节看门狗做自适应放宽（握手都只要 4~5s 的节点，连接+授权+首块
// 的总时长不该按 30s 掐）。
func (a *App) latestMediaHandshakeMs(userID, accountID string, dc int) int64 {
	if dc <= 0 {
		return 0
	}
	a.mu.Lock()
	snap, ok := a.mediaProbes[nativeAccountKey(userID, accountID)]
	a.mu.Unlock()
	if !ok {
		return 0
	}
	for _, r := range snap.Results {
		if r.DC == dc && r.MTPOK && r.MTPDuration > 0 {
			return r.MTPDuration
		}
	}
	return 0
}

// latestMediaHandshakeMaxMs 返回该账号最近一次媒体探测中所有成功握手的最长耗时
// （毫秒）；无快照或全败时返回 0。R4.34：媒体连接建立窗口据此自适应——
// 常驻连接建立在账号主 DC 上，用全 DC 最大握手耗时做保守上界。
func (a *App) latestMediaHandshakeMaxMs(userID, accountID string) int64 {
	a.mu.Lock()
	snap, ok := a.mediaProbes[nativeAccountKey(userID, accountID)]
	a.mu.Unlock()
	if !ok {
		return 0
	}
	var maxMs int64
	for _, r := range snap.Results {
		if r.MTPOK && r.MTPDuration > maxMs {
			maxMs = r.MTPDuration
		}
	}
	return maxMs
}

// latestMediaProbeHint 返回该账号最近一轮媒体探测的可读结论（R4.34，供连接
// 建立超时的错误文案引用；无快照返回空串——调用方自行判空跳过拼接）。
func (a *App) latestMediaProbeHint(userID, accountID string) string {
	a.mu.Lock()
	snap, ok := a.mediaProbes[nativeAccountKey(userID, accountID)]
	a.mu.Unlock()
	if !ok || snap.Summary == "" {
		return ""
	}
	return "最近媒体探测结论：" + snap.Summary
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
			// 追加真实 MTProto 握手探测一锤定音。R4.32：带一次重试去抖。
			mtpStart := time.Now()
			if mtpErr := a.probeTelegramMTProtoSteady(dc, mediaMTPProbeTimeout); mtpErr != nil {
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
	// R4.33：网络自愈闭环——探测确认全部 DC 真实可用时，把该账号「重试上限」
	// 终态任务自动拉起一轮。此前用户在网络修复后还必须逐个手动点重试。
	// R4.34：探测转差时重臂复活标记——复活机会按「断网窗口」计（每窗口一次），
	// 否则复活后一旦撞上网络恶化，任务永久失去自动复活资格（2.6.11 实测：
	// 启动复活→节点恶化→45s 连接超时终态→此后探测全绿也不再拉起）。
	if allMediaDCsHealthy(results) {
		a.reviveNetworkStalledTasks(account.UserID, account.AccountID)
	} else {
		a.rearmNetworkRevive(account.UserID, account.AccountID)
	}
	return snap
}

// allMediaDCsHealthy 判定一轮探测是否全部 DC 真实可用（TCP+MTProto 握手均过）。
// 纯函数便于单测。
func allMediaDCsHealthy(results []mediaDCProbe) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !r.OK || !r.MTPOK {
			return false
		}
	}
	return true
}

// isRetryCapError 判定任务错误是否为「瞬态重试上限」终态（新旧文案均覆盖）。
// 纯函数便于单测。
func isRetryCapError(errText string) bool {
	return strings.Contains(errText, "已自动重试") && strings.Contains(errText, "次后停止")
}

// reviveNetworkStalledTasks 网络自愈自动复活：把指定账号因「重试上限」终态的任务
// 拉起一轮。每个任务在每个「断网窗口」内只自动复活一次（AutoRevived 防抖，
// 网络转差时由 rearmNetworkRevive 重臂）——避免「探测绿→复活→打满→再复活」
// 的无限循环；防抖仍挡不住的持续坏网由重试上限终态 + 用户手动重试兜底。
func (a *App) reviveNetworkStalledTasks(userID, accountID string) {
	a.mu.Lock()
	ids := make([]string, 0, 4)
	for id, t := range a.tasks {
		if t == nil || t.Status != "error" || t.AutoRevived || !isRetryCapError(t.Error) {
			continue
		}
		if t.UserID != userID || t.AccountID != accountID {
			continue
		}
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.updateTask(id, func(t *Task) {
			if t.Status != "error" || t.AutoRevived || !isRetryCapError(t.Error) {
				return // 复活窗口内状态可能已被并发改动，双检防误拉
			}
			t.Status = "queued"
			t.RetryCount = 0
			t.RetryAfter = 0
			t.AutoRevived = true
			t.Error = "网络已恢复（媒体 DC 握手全部正常），自动复活，等待续传"
			t.UpdatedAt = now()
		})
	}
	if len(ids) > 0 {
		log.Printf("network healed: %d 个重试上限终态任务已自动复活，等待续传", len(ids))
	}
}

// rearmNetworkRevive 网络转差时重置该账号 error 任务的 AutoRevived 标记（R4.34）：
// 自动复活机会按「断网窗口」计——每个窗口结束时（下一轮探测全绿）仍能获得一次
// 自动拉起，而单个窗口内不会形成复活循环。
func (a *App) rearmNetworkRevive(userID, accountID string) {
	a.mu.Lock()
	ids := make([]string, 0, 4)
	for id, t := range a.tasks {
		if t == nil || t.Status != "error" || !t.AutoRevived {
			continue
		}
		if t.UserID != userID || t.AccountID != accountID {
			continue
		}
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.updateTask(id, func(t *Task) {
			if t.Status != "error" || !t.AutoRevived {
				return // 窗口内状态可能已被并发改动，双检防误清
			}
			t.AutoRevived = false
			t.UpdatedAt = now()
		})
	}
	if len(ids) > 0 {
		log.Printf("network degraded: %d 个终态任务的自动复活标记已重臂（网络恢复后可再拉起一次）", len(ids))
	}
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
		// R4.32：措辞区分「节点波动」（偶发、每轮 DC 不同）与「网段黑洞」（同 DC 持续失败）。
		return fmt.Sprintf("%d/%d 个 DC 的 MTProto 握手正常（%s）；握手无响应：DC %s——单个 DC 偶发超时多为节点波动（本轮探测重试后仍失败），若同一 DC 每轮都失败则该网段未被真正转发、下载会持续失败：请补全代理规则或更换节点", mtpOK, total, layer, strings.Join(mtpBad, "/"))
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
	mtpErr := a.probeTelegramMTProtoSteady(dc, mediaMTPProbeTimeout)
	mtpMs := time.Since(mtpStart).Milliseconds()
	mtpErrMsg := ""
	if mtpErr != nil {
		mtpErrMsg = mtpErr.Error()
	}
	return mediaDCDiagnosis(dc, addr, true, mtpErr == nil, tcpMs, mtpMs, mtpErrMsg, layer)
}

// mediaDCCandidates 给出本次下载的候选 media DC 顺序（R4.36-E）：
// 任务元数据里的 DC 优先；其余按最近一轮媒体探测结论排序（MTProto 握手成功
// 且耗时最短的在前，握手失败的直接排除）；主 DC 兜底。
// 依据：2.6.13 实测 DC 4/1 的 MTProto 握手全绿，但 upload.getFile 的 invoke
// 反复 retryUntilAck 5 次失败——握手正常不等于能跑 RPC 流，候选需优选。
func (a *App) mediaDCCandidates(userID, accountID string, preferred, primaryDC int) []int {
	out := []int{}
	seen := map[int]bool{}
	push := func(dc int) {
		if dc <= 0 || seen[dc] {
			return
		}
		seen[dc] = true
		out = append(out, dc)
	}
	push(preferred)
	a.mu.Lock()
	snap, ok := a.mediaProbes[nativeAccountKey(userID, accountID)]
	a.mu.Unlock()
	if ok {
		type cand struct {
			dc int
			ms int64
		}
		items := []cand{}
		for _, r := range snap.Results {
			if r.DC <= 0 || !r.MTPOK {
				continue
			}
			items = append(items, cand{dc: r.DC, ms: r.MTPDuration})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].ms < items[j].ms })
		for _, it := range items {
			push(it.dc)
		}
	}
	push(primaryDC)
	// R4.40：连续断流的 DC 降级到候选末尾。只降级、不排除——它可能仍是唯一
	// 可用 DC（且服务端的 FILE_MIGRATE 会把请求指回来），排除它等于自断路径。
	// 该 DC 重新跑出字节流时计数清零（见 download 循环的 progressSeen 分支）。
	stalls := a.mediaDCStallCounts(userID, accountID)
	if len(stalls) == 0 {
		return out
	}
	healthy := make([]int, 0, len(out))
	degraded := make([]int, 0, len(stalls))
	for _, dc := range out {
		if stalls[dc] >= mediaDCStallThreshold {
			degraded = append(degraded, dc)
			continue
		}
		healthy = append(healthy, dc)
	}
	return append(healthy, degraded...)
}

// probeMediaDCRealRPC 在已切换到的 media DC 上发一次轻量真实 RPC（help.getConfig），
// 验证「不只是握手成功，而是能跑真实加密 RPC 往返」（R4.36-E）。
// 2.6.13 实测：DC 1/4 的 MTProto 真握手全绿，upload.getFile 却反复
// retryUntilAck 5 次失败——握手与真实 RPC 是两回事。
// 探针只做「优选」：调用方在全部候选探针失败时仍会兜底使用能连上的 DC。
func (a *App) probeMediaDCRealRPC(ctx context.Context, api *tg.Client, dc int) error {
	if api == nil {
		return errors.New("media DC client 不可用")
	}
	probeCtx, cancel := context.WithTimeout(ctx, mediaDCRealRPCProbeTimeout)
	defer cancel()
	if _, err := api.HelpGetConfig(probeCtx); err != nil {
		return fmt.Errorf("DC %d help.getConfig: %w", dc, err)
	}
	return nil
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
