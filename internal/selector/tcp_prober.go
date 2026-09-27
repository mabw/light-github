package selector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
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

// checkResult 应用层验证三态（M5-13）：
//   - checkAlive：服务该域，健康
//   - checkNetFail：网络层失败（超时/握手失败/TCP 断）——时序抖动，候选
//     保留交由拨号方翻盘（测速窗口死 ≠ 拨号窗口死）
//   - checkSemanticDead：确定性不服务该域（200 裸 text/plain 健康节点/
//     3xx 重定向/400 跨域路由）——彻底剔除出拨号列表
type checkResult int

const (
	checkNetFail checkResult = iota
	checkAlive
	checkSemanticDead
)

// Probe implements Prober.
func (p *MedianProber) Probe(ctx context.Context, domain string, ip net.IP) (time.Duration, bool) {
	count, timeout, port := p.defaults()

	costs := make([]time.Duration, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return timeout, true
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

	// 应用层验证：真实 HEAD 请求（握手 + 证书域身份 + 响应形态）
	switch p.httpCheck(ctx, domain, ip, port, timeout) {
	case checkAlive:
		sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
		return costs[len(costs)/2], true
	case checkSemanticDead:
		return timeout, false // 确定性错：剔除（M5-13）
	default: // checkNetFail：时序抖动，惩罚排序但保留
		return timeout, true
	}
}

// httpCheck 经指定 IP 发一次真实 HEAD https://domain/ 请求：完整走 TLS
// 握手（SNI=域名）+ 证书域身份校验 + 应用层响应形态，返回三态判定。
// 仅 TLS 层验证不够——实测同段 IP 证书全部匹配但应用层混杂 200/400
// （GitHub 边缘按 IP 分工路由，400 即 "Whoa there!" 页，浏览器故障来源）。
// 响应码判定：<500 且非 400——400 是跨域路由拒绝；404/405 是服务该域
// 仅无此资源（资产类域 GET / 常 404）。CA 链校验留给端到端客户端完成
// ——测速是初筛而非安全边界。
func (p *MedianProber) httpCheck(ctx context.Context, domain string, ip net.IP, port string, timeout time.Duration) checkResult {
	addr := net.JoinHostPort(ip.String(), port)
	certMismatch := false // 证书域不匹配是确定性身份错（区别于 RST/超时的网络抖动）
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			raw, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(raw, &tls.Config{
				ServerName:         domain,
				InsecureSkipVerify: true, //nolint:gosec // 链校验由端到端客户端完成；此处验证书域名身份
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					if len(rawCerts) == 0 {
						return errors.New("无证书")
					}
					c, err := x509.ParseCertificate(rawCerts[0])
					if err != nil {
						return err
					}
					if err := c.VerifyHostname(domain); err != nil {
						certMismatch = true
						return err
					}
					return nil
				},
			})
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return tc, nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+domain+"/", nil)
	if err != nil {
		return checkNetFail
	}
	// 禁用重定向跟随（M5-11）：拿原始响应判定——默认跟随会把「301 到别域」
	// 洗成「最终 200」（实测 api.github.com 被 web 前端 IP 301 到 github.com/，
	// octotree 的 API 请求全拿到 HTML）。3xx = 此 IP 不直接服务该域。
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if certMismatch {
			return checkSemanticDead // 证书域不匹配：服务器不属于该域，确定性身份错
		}
		return checkNetFail
	}
	defer resp.Body.Close()

	// 重定向：此 IP 把该域指到别处 = 不服务该域（跨域路由形态）
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return checkSemanticDead
	}

	// 健康节点指纹（M5-10）：GitHub 边缘存在独立健康服务（实测 140.82.114.22，
	// 2026-09-26「页面只返回一个 ok」的元凶）——*.github.com 泛证书握手合法、
	// 任意路径秒回 200 text/plain "OK"、无路由层头（x-github-request-id）。
	// 只看状态码会把它判为最优候选（响应最快、cost 最低），流量全打到它。
	// 加速域的正常服务没有 200 裸 text/plain 形态（GitHub 系 html/json、
	// registry json、404 资产域不在此列），故该指纹判死。
	if resp.StatusCode == http.StatusOK &&
		strings.HasPrefix(strings.TrimSpace(resp.Header.Get("Content-Type")), "text/plain") {
		return checkSemanticDead
	}
	if resp.StatusCode < 500 && resp.StatusCode != http.StatusBadRequest {
		return checkAlive
	}
	return checkSemanticDead // 400/5xx：跨域路由拒绝或服务端拒绝，确定性形态
}
