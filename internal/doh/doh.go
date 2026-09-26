// Package doh 提供 RFC 8484 JSON 变体的 DoH 多端点解析客户端。
package doh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// 默认端点：国内可达优先（清单对照 Watt Toolkit 内置 DoH 常量取舍：
// 两家主力各双 IP + 360 备胎；Google/Cloudflare 国内可达性差不采用）。
// 全部 IP 直连形态——DoH 端点自身不依赖系统 DNS，防鸡生蛋。
var defaultEndpoints = []string{
	"https://223.5.5.5/resolve",    // 阿里
	"https://223.6.6.6/resolve",    // 阿里（备用 IP）
	"https://120.53.53.53/resolve", // DNSPod
	"https://1.12.12.12/resolve",   // DNSPod（备用 IP）
	"https://doh.pub/resolve",      // DNSPod（域名形态）
	"https://doh.360.cn/resolve",   // 360
}

// Client 多端点 DoH 解析器。
type Client struct {
	endpoints []string
	hc        *http.Client
}

// New 构造客户端；endpoints 为空时使用内置默认端点。
func New(endpoints ...string) *Client {
	if len(endpoints) == 0 {
		endpoints = defaultEndpoints
	}
	return &Client{
		endpoints: endpoints,
		hc:        &http.Client{Timeout: 3 * time.Second},
	}
}

type dohAnswer struct {
	Type int    `json:"type"` // 1=A, 5=CNAME, 28=AAAA
	Data string `json:"data"`
}

type dohResp struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// Resolve 并发查询所有端点，返回去重后的公网 IPv4 并集；部分端点失败被容忍，
// 全部失败返回错误。结果顺序为各端点响应到达顺序（先到先入）。
func (c *Client) Resolve(ctx context.Context, domain string) ([]net.IP, error) {
	var (
		mu    sync.Mutex
		seen  = map[string]bool{}
		union []net.IP
		wg    sync.WaitGroup
	)

	for _, ep := range c.endpoints {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			ips, err := c.queryOne(ctx, ep, domain)
			if err != nil {
				return // 单端点失败被容忍
			}
			mu.Lock()
			defer mu.Unlock()
			for _, ip := range ips {
				if k := ip.String(); !seen[k] {
					seen[k] = true
					union = append(union, ip)
				}
			}
		}(ep)
	}
	wg.Wait()

	if len(union) == 0 {
		return nil, fmt.Errorf("doh: all endpoints failed for %s", domain)
	}
	return union, nil
}

// queryOne 查询单端点，只取公网 IPv4 A 记录
func (c *Client) queryOne(ctx context.Context, endpoint, domain string) ([]net.IP, error) {
	q := url.Values{"name": {domain}, "type": {"A"}}
	u := endpoint + "?" + q.Encode() // review N3：domain 含 &/# 等字符时防查询串损坏
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh %s: status %d", endpoint, resp.StatusCode)
	}

	var r dohResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range r.Answer {
		if a.Type != 1 {
			continue
		}
		if ip := net.ParseIP(a.Data); ip != nil && ip.To4() != nil && isUsable(ip) {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// isUsable 回环/私有/链路本地/未指定地址不可作出口
// （spike-b 实训：hosts 被其他加速工具劫持时 DoH 前的系统解析会出现 127.0.0.1）
func isUsable(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}
