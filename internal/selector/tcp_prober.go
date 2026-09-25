package selector

import (
	"context"
	"net"
	"sort"
	"time"
)

// MedianProber TCP 建连中位数测速器（GitHub520 的中位数法，spike-b 实证）：
// 对候选 IP 的 443 端口建连 N 次取中位耗时；单次失败按超时值参与排序惩罚。
type MedianProber struct {
	Count   int           // 测速次数，默认 3
	Timeout time.Duration // 单次超时，默认 1s
	Port    string        // 目标端口，默认 443
}

// Probe implements Prober.
func (p *MedianProber) Probe(ctx context.Context, ip net.IP) time.Duration {
	count, timeout, port := p.Count, p.Timeout, p.Port
	if count <= 0 {
		count = 3
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	if port == "" {
		port = "443"
	}

	costs := make([]time.Duration, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return timeout
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), timeout)
		if conn != nil {
			_ = conn.Close()
		}
		cost := time.Since(start)
		if err != nil {
			cost = timeout
		}
		costs = append(costs, cost)
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	return costs[len(costs)/2]
}
