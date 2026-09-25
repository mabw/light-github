// Spike C：steampp 数据源归一化 + CNAME 通道端到端验证
//
// 验证目标（DESIGN.md §3.3 数据源层）：
//  1. 拉取 POST https://api.steampp.net/accelerator/projectgroups（无认证，已实测 200）
//  2. 归一化 Github 分组为规则表：
//     Forward 为 IP     → FixedIP 策略
//     Forward 为其他域名 → CNAME 通道策略（解析时改查该域名，连接 SNI 保持原域名）
//     Forward 为自身     → Dynamic 策略（DoH 解析）
//  3. 端到端验证 CNAME 通道：api.github.com → 解析 githubapi.rmbgame.net →
//     连其 IP + TLS SNI=api.github.com → 真实 HTTPS GET 期待 200
//  4. 请求头带与官方一致的 Referer（spp://mac/{version}）与浏览器 UA（克制白嫖）
//
// 验收：打印归一化规则表；CNAME 通道 GET https://api.github.com/zen 返回 200。
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiURL = "https://api.steampp.net/accelerator/projectgroups"

// 官方客户端同款请求特征（ApiConstants.GetReferrer 格式）
const (
	spoofUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36"
	spoofReferr = "spp://macOS/3.1.10001.0"
)

// ---- steampp DTO（仅取关心的字段） ----

type projectGroup struct {
	Name  string    `json:"Name"`
	Items []project `json:"Items"`
}

type project struct {
	Name               string    `json:"Name"`
	Port               int       `json:"Port"`
	MatchDomainNames   string    `json:"MatchDomainNames"`
	ListenDomainNames  string    `json:"ListenDomainNames"`
	ForwardDomainNames string    `json:"ForwardDomainNames"`
	FakeServerName     string    `json:"FakeServerName"`
	ProxyType          int       `json:"ProxyType"`
	Items              []project `json:"Items"`
}

// ---- 归一化后的规则 ----

type strategyKind string

const (
	strategyFixedIP strategyKind = "FixedIP"
	strategyCNAME   strategyKind = "CNAME"
	strategyDynamic strategyKind = "Dynamic"
)

type rule struct {
	Domain      string
	Kind        strategyKind
	Forward     string // FixedIP: IP；CNAME: 查询域名；Dynamic: 空
	FakeSNI     string
}

func (r rule) String() string {
	extra := ""
	if r.FakeSNI != "" {
		extra = " fakeSNI=" + r.FakeSNI
	}
	return fmt.Sprintf("  %-42s %-8s → %s%s", r.Domain, r.Kind, r.Forward, extra)
}

// normalize 把 ForwardDomainNames 归一化为策略
func normalize(domain, forward string) strategyKind {
	if ip := net.ParseIP(forward); ip != nil {
		return strategyFixedIP
	}
	if strings.EqualFold(strings.TrimSuffix(forward, "."), strings.TrimSuffix(domain, ".")) {
		return strategyDynamic
	}
	return strategyCNAME
}

// splitDomains 分号分隔且去空
func splitDomains(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "http") {
			out = append(out, p)
		}
	}
	return out
}

func fetchGroups(ctx context.Context) ([]projectGroup, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", spoofUA)
	req.Header.Set("Referer", spoofReferr)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 顶层是若干 emoji 键（平台混淆），其中只有一个是分组数组（实测为 🦓），其余是数字
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var groups []projectGroup
	for _, v := range raw {
		if len(v) > 0 && v[0] == '[' { // 只解析数组形态的键
			var gs []projectGroup
			if err := json.Unmarshal(v, &gs); err == nil {
				groups = append(groups, gs...)
			}
		}
	}
	return groups, nil
}

// collectRules 从 Github 分组递归提取规则（Match 域名 × Forward 策略）
func collectRules(items []project, out *[]rule) {
	for _, it := range items {
		domains := splitDomains(it.MatchDomainNames)
		if len(domains) == 0 {
			domains = splitDomains(it.ListenDomainNames)
		}
		for _, d := range domains {
			*out = append(*out, rule{
				Domain:  d,
				Kind:    normalize(d, it.ForwardDomainNames),
				Forward: it.ForwardDomainNames,
				FakeSNI: it.FakeServerName,
			})
		}
		collectRules(it.Items, out)
	}
}

// ---- CNAME 通道端到端验证 ----

// resolveViaDoH 用阿里 DoH 解析（本机 hosts 已被 Watt 污染，系统解析不可信）
func resolveViaDoH(ctx context.Context, domain string) ([]net.IP, error) {
	u := "https://223.5.5.5/resolve?name=" + url.QueryEscape(domain) + "&type=A"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range r.Answer {
		if a.Type == 1 {
			if ip := net.ParseIP(a.Data); ip != nil && !ip.IsLoopback() && !ip.IsPrivate() {
				ips = append(ips, ip)
			}
		}
	}
	return ips, nil
}

// cnameChannelGet 端到端验证：解析 forward 域名 → 连其 IP → TLS(SNI=原域名) → GET path
func cnameChannelGet(ctx context.Context, originDomain, forwardDomain, path string) error {
	ips, err := resolveViaDoH(ctx, forwardDomain)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("解析 %s 失败: %v (ips=%v)", forwardDomain, err, ips)
	}
	ip := ips[0]
	fmt.Printf("  [cname] %s → %s → %s\n", originDomain, forwardDomain, ip)

	raw, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "443"), 3*time.Second)
	if err != nil {
		return fmt.Errorf("TCP %s: %w", ip, err)
	}
	defer raw.Close()

	// 关键验证点：SNI 用原域名（不是 forward 域名）
	conn := tls.Client(raw, &tls.Config{ServerName: originDomain})
	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS(SNI=%s) 握手: %w", originDomain, err)
	}
	fmt.Printf("  [tls]   SNI=%s 证书 CN=%v\n", originDomain, conn.ConnectionState().PeerCertificates[0].Subject.CommonName)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nAccept: */*\r\nConnection: close\r\n\r\n", path, originDomain, spoofUA)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	fmt.Printf("  [http]  GET https://%s%s → %s  body: %q\n", originDomain, path, resp.Status, string(body[:n]))
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("=== 1. 拉取 steampp 数据源 ===")
	groups, err := fetchGroups(ctx)
	if err != nil {
		fmt.Printf("FATAL: %v\n", err)
		return
	}
	fmt.Printf("拉取成功，共 %d 个分组\n", len(groups))

	var githubGroups []projectGroup
	for _, g := range groups {
		if strings.Contains(strings.ToLower(g.Name), "github") {
			githubGroups = append(githubGroups, g)
		}
	}
	if len(githubGroups) == 0 {
		fmt.Println("FATAL: 未找到 Github 分组")
		return
	}

	fmt.Println("\n=== 2. 归一化为规则表 ===")
	var rules []rule
	collectRules(githubGroups[0].Items, &rules)
	for _, r := range rules {
		fmt.Println(r)
	}

	// 统计
	byKind := map[strategyKind]int{}
	for _, r := range rules {
		byKind[r.Kind]++
	}
	fmt.Printf("\n规则统计: %s\n", byKind)

	fmt.Println("\n=== 3. CNAME 通道端到端验证（api.github.com via githubapi.rmbgame.net）===")
	if err := cnameChannelGet(ctx, "api.github.com", "githubapi.rmbgame.net", "/zen"); err != nil {
		fmt.Printf("FAIL: %v\n", err)
		return
	}
	fmt.Println("\n✅ Spike C 全部验证通过")
}
