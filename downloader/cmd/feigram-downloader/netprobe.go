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
	"net"
	"strings"
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

// healthStageDiagnosis 依据分级探测结果解释 client.Run 的失败，返回更精确的错误描述；
// 返回空串表示探测信息不足以细化（调用方沿用原错误与通用提示）。纯函数便于单测。
func healthStageDiagnosis(probeDC int, probeAddr string, probeOK bool, probeDur time.Duration, probeErr, runErr error, proxyActive bool) string {
	if probeAddr == "" {
		return ""
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
