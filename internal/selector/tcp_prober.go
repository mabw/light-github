package selector

import (
	"context"
	"crypto/tls"
	"net"
	"sort"
	"time"
)

// MedianProber TCP 建连中位数测速器（GitHub520 的中位数法，spike-b 实证）：
// 对候选 IP 的 443 端口建连 N 次取中位耗时；单次失败按超时值参与排序惩罚。
//
// M5 起增加 TLS 握手验证（调研 §2.1「拨号即验证」）：末次建连上叠一次
// tls.Client 握手——TCP 通但 TLS 被 RST/黑洞的「假好候选」直接按超时值
// 惩罚（2026-09-26 两次断网实测的主因）。只验证握手连通，不校验证书身份：
// 身份校验属于端到端路径上客户端的职责（本代理不解密 TLS）。
type MedianProber struct {
	Count   int           // 测速次数，默认 3
	Timeout time.Duration // 单次超时，默认 1s
	Port    string        // 目标端口，默认 443
}

func (p *MedianProber) defaults() (count int, timeout time.Duration, port string) {
	count, timeout, port = p.Count, p.Timeout, p.Port
	if count <= 0 {
		count = 3
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	if port == "" {
		port = "443"
	}
	return count, timeout, port
}

// Probe implements Prober.
func (p *MedianProber) Probe(ctx context.Context, domain string, ip net.IP) time.Duration {
	count, timeout, port := p.defaults()

	costs := make([]time.Duration, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return timeout
		}
		start := time.Now()
		// DialContext 而非 DialTimeout（review N4）：ctx 取消时单次拨号立即中止，
		// 不必等满超时窗口
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if conn != nil {
			_ = conn.Close()
		}
		cost := time.Since(start)
		if err != nil {
			cost = timeout
		}
		costs = append(costs, cost)
	}

	// TLS 层验证：失败按超时值惩罚（排序自然沉底）
	if !tlsHandshakeOK(ctx, domain, ip, port, timeout) {
		return timeout
	}

	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	return costs[len(costs)/2]
}

// tlsHandshakeOK 在独立建连上完成一次 TLS 握手（SNI=域名）。
func tlsHandshakeOK(ctx context.Context, domain string, ip net.IP, port string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return false
	}
	defer conn.Close()

	// InsecureSkipVerify 只验「TLS 层能握手」（排除 RST/黑洞）；证书身份
	// 校验在端到端路径由客户端完成，此处不越权
	tc := tls.Client(conn, &tls.Config{ServerName: domain, InsecureSkipVerify: true}) //nolint:gosec
	hsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return tc.HandshakeContext(hsCtx) == nil
}
