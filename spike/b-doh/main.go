// Spike B：DoH 多源解析 + TCP-443 中位数测速
//
// 验证目标（DESIGN.md §3.2 出站选择器）：
//  1. 阿里/DNSPod DoH JSON API 查询 A 记录（防污染，替代系统 DNS）
//  2. 多端点结果取并集去重
//  3. 对每个候选 IP TCP 建连 :443 计时，3 次取中位数（GitHub520 的中位数法）
//  4. 与系统 DNS 结果对照，验证 DoH 是否带来更多/更优候选
//
// 验收：对 github.com / api.github.com / codeload.github.com / raw.githubusercontent.com
// 输出按耗时排序的候选 IP 表。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// dohEndpoints 国内可达的 DoH JSON 端点（RFC 8484 的 JSON 变体，GET 即用）
var dohEndpoints = []string{
	"https://223.5.5.5/resolve",    // 阿里公共 DNS
	"https://120.53.53.53/resolve", // DNSPod
	"https://doh.pub/resolve",      // DNSPod（域名形态）
}

var testDomains = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"raw.githubusercontent.com",
}

const (
	probeCount   = 3               // 每 IP 测速次数
	probeTimeout = 1 * time.Second // 单次超时，失败按 1000ms 记（GitHub520 同款）
)

type dohResp struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"` // 1=A, 5=CNAME
		Data string `json:"data"`
	} `json:"Answer"`
}

func queryDoH(ctx context.Context, endpoint, domain string) ([]net.IP, error) {
	url := fmt.Sprintf("%s?name=%s&type=A", endpoint, domain)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r dohResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range r.Answer {
		if a.Type != 1 { // 跳过 CNAME 链，只要最终 A 记录
			continue
		}
		if ip := net.ParseIP(a.Data); ip != nil && ip.To4() != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// isUsableCandidate 剔除不可用作出口的 IP：回环/私有/链路本地。
// 真实教训：本机 /etc/hosts 有 Watt Toolkit 残留的 域名→127.0.0.1 条目，
// 系统 DNS 会返回 127.0.0.1 且建连 0ms（443 上恰有 Watt 反代监听），会污染排序。
func isUsableCandidate(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

// resolveUnion 多 DoH 端点并发查询，结果并集去重（保序）
func resolveUnion(domain string) []net.IP {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type result struct {
		ips []net.IP
	}
	ch := make(chan result, len(dohEndpoints))
	var wg sync.WaitGroup
	for _, ep := range dohEndpoints {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			ips, err := queryDoH(ctx, ep, domain)
			if err != nil {
				fmt.Printf("    [doh-fail] %s %s: %v\n", ep, domain, err)
				return
			}
			ch <- result{ips}
		}(ep)
	}
	wg.Wait()
	close(ch)

	seen := make(map[string]bool)
	var union []net.IP
	for r := range ch {
		for _, ip := range r.ips {
			k := ip.String()
			if !seen[k] && isUsableCandidate(ip) {
				seen[k] = true
				union = append(union, ip)
			}
		}
	}
	return union
}

// medianProbe 对单 IP 做 N 次 TCP-443 建连计时，返回中位数
func medianProbe(ip net.IP) time.Duration {
	var costs []time.Duration
	for i := 0; i < probeCount; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "443"), probeTimeout)
		cost := time.Since(start)
		if conn != nil {
			conn.Close()
		}
		if err != nil {
			cost = probeTimeout // 失败按超时值记，参与排序惩罚
		}
		costs = append(costs, cost)
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	return costs[len(costs)/2]
}

func main() {
	for _, domain := range testDomains {
		fmt.Printf("=== %s ===\n", domain)

		// 系统解析对照
		sysStart := time.Now()
		sysIPs, sysErr := net.LookupIP(domain)
		fmt.Printf("  系统 DNS: %v (%s, err=%v)\n", ipsString(sysIPs), time.Since(sysStart).Round(time.Millisecond), sysErr)

		// DoH 并集
		dohIPs := resolveUnion(domain)
		fmt.Printf("  DoH 并集: %d 个 IP\n", len(dohIPs))

		// 全部候选（DoH + 系统解析）测速排序
		all := append([]net.IP{}, dohIPs...)
		for _, ip := range sysIPs {
			if ip.To4() == nil || !isUsableCandidate(ip) {
				continue
			}
			dup := false
			for _, e := range all {
				if e.Equal(ip) {
					dup = true
					break
				}
			}
			if !dup {
				all = append(all, ip)
			}
		}

		type scored struct {
			ip   net.IP
			cost time.Duration
		}
		var results []scored
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, ip := range all {
			wg.Add(1)
			go func(ip net.IP) {
				defer wg.Done()
				cost := medianProbe(ip)
				mu.Lock()
				results = append(results, scored{ip, cost})
				mu.Unlock()
			}(ip)
		}
		wg.Wait()
		sort.Slice(results, func(i, j int) bool { return results[i].cost < results[j].cost })

		for i, r := range results {
			mark := " "
			if i == 0 {
				mark = "★"
			}
			fmt.Printf("  %s %s  中位建连 %v\n", mark, r.ip.String(), r.cost.Round(time.Millisecond))
		}
		fmt.Println()
	}
	os.Exit(0)
}

func ipsString(ips []net.IP) string {
	out := ""
	for i, ip := range ips {
		if ip.To4() == nil {
			continue
		}
		if i > 0 {
			out += ", "
		}
		out += ip.String()
	}
	if out == "" {
		return "(无 IPv4)"
	}
	return out
}
