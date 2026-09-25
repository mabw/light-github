// Package doh 提供 RFC 8484 JSON 变体的 DoH 多端点解析客户端。
package doh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// 默认端点：国内可达优先（借鉴 GitHub520 的灾备顺序），可按需覆盖
var defaultEndpoints = []string{
	"https://223.5.5.5/resolve",    // 阿里
	"https://120.53.53.53/resolve", // DNSPod
	"https://doh.pub/resolve",      // DNSPod（域名形态）
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
	url := endpoint + "?name=" + domain + "&type=A"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
