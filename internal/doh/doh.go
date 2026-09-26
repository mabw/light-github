// Package doh 提供 RFC 8484 JSON 变体的 DoH 多端点解析客户端。
package doh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
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

// defaultCacheTTL 结果缓存默认有效期。取 9.5min 而非整 10min，且每条目
// 独立 ±10% 抖动（调研 §2.4，Watt 同思路 9.9min）：避免大量域名同时过期
// 造成对 DoH 端点的解析风暴。
const defaultCacheTTL = 9*time.Minute + 30*time.Second

type cacheEntry struct {
	ips      []net.IP
	expireAt time.Time
}

// Client 多端点 DoH 解析器。
type Client struct {
	endpoints []string
	hc        *http.Client

	// CacheTTL 结果缓存有效期；零值用默认 9.5min。仅缓存成功结果，
	// 失败不缓存（端点故障恢复后立即可用）。
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New 构造客户端；endpoints 为空时使用内置默认端点。
func New(endpoints ...string) *Client {
	if len(endpoints) == 0 {
		endpoints = defaultEndpoints
	}
	return &Client{
		endpoints: endpoints,
		hc:        &http.Client{Timeout: 3 * time.Second},
		cache:     map[string]cacheEntry{},
	}
}

func (c *Client) cacheTTL() time.Duration {
	if c.CacheTTL > 0 {
		return c.CacheTTL
	}
	return defaultCacheTTL
}

func (c *Client) cached(domain string) ([]net.IP, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[domain]
	if !ok || time.Now().After(e.expireAt) {
		return nil, false
	}
	return e.ips, true
}

func (c *Client) store(domain string, ips []net.IP) {
	ttl := c.cacheTTL()
	jitter := time.Duration(float64(ttl) * (0.9 + 0.2*rand.Float64())) // ±10%
	c.mu.Lock()
	c.cache[domain] = cacheEntry{ips: ips, expireAt: time.Now().Add(jitter)}
	c.mu.Unlock()
}

type dohAnswer struct {
	Type int    `json:"type"` // 1=A, 5=CNAME, 28=AAAA
	Data string `json:"data"`
}

type dohResp struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// Resolve 并发查询所有端点（保延迟），但按端点信任序采纳**首个成功**者
// 的答案（review M3：原取全端点并集，任一被污染端点的假 IP 都会进入
// 候选池——TCP 测速只校验握手耗时无法识别身份）。失败降级到下一家，
// 全部失败返回错误。端点顺序即信任序（cmd 构造时阿里/DNSPod 在前）。
// 成功结果按 CacheTTL 缓存（M5 §2.4）。
func (c *Client) Resolve(ctx context.Context, domain string) ([]net.IP, error) {
	if ips, ok := c.cached(domain); ok {
		return ips, nil
	}

	results := make([][]net.IP, len(c.endpoints))
	var wg sync.WaitGroup
	for i, ep := range c.endpoints {
		wg.Add(1)
		go func(i int, ep string) {
			defer wg.Done()
			if ips, err := c.queryOne(ctx, ep, domain); err == nil {
				results[i] = ips
			}
		}(i, ep)
	}
	wg.Wait()

	for _, ips := range results { // 信任序采纳
		if len(ips) > 0 {
			c.store(domain, ips)
			return ips, nil
		}
	}
	return nil, fmt.Errorf("doh: all endpoints failed for %s", domain)
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
