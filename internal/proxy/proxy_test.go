package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
)

// ---- 测试替身 ----

type fakeDialer struct {
	ips      []net.IP
	err      error
	picks    int
	success  []string
	failures []string
}

func (f *fakeDialer) Pick(_ context.Context, _ string) ([]net.IP, error) {
	f.picks++
	return f.ips, f.err
}
func (f *fakeDialer) ReportFailure(_ string, ip net.IP) {
	f.failures = append(f.failures, ip.String())
}
func (f *fakeDialer) ReportSuccess(_ string, ip net.IP) {
	f.success = append(f.success, ip.String())
}

// newStack 启动代理 + fake 上游，返回代理 URL 与清理函数
func newStack(t *testing.T, dialer Dialer) (*url.URL, *Server) {
	t.Helper()

	proxySrv := &Server{
		Addr:       "127.0.0.1:0",
		Table:      rule.NewTable([]rule.Rule{{Domain: "accel.test", Kind: rule.KindDynamic}}),
		Dialer:     dialer,
		Metrics:    metrics.NewStore(time.Second),
		DialTimeout: 500 * time.Millisecond,
	}
	addr, err := proxySrv.ListenAndServe(context.Background())
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = proxySrv.Close() })
	return &url.URL{Scheme: "http", Host: addr.String()}, proxySrv
}

func proxiedClient(t *testing.T, proxyURL *url.URL) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 测试自签证书
		},
		Timeout: 5 * time.Second,
	}
}

// ---- 行为验证 ----

func TestProxy_AcceleratedDomainTunnelsViaPickedIP(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-accel"))
	}))
	t.Cleanup(upstream.Close)
	upIP, _, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	dialer := &fakeDialer{ips: []net.IP{net.ParseIP(upIP)}}
	proxyURL, _ := newStack(t, dialer)

	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if dialer.picks == 0 {
		t.Fatal("白名单域名必须经 Dialer 选路")
	}
	if len(dialer.success) != 1 {
		t.Fatalf("连接成功应上报 ReportSuccess: %v", dialer.success)
	}
}

func TestProxy_UnmatchedDomainPassesThroughDirectly(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	t.Cleanup(upstream.Close)

	dialer := &fakeDialer{} // 未命中白名单不应触发选路
	proxyURL, _ := newStack(t, dialer)

	// 目标是 127.0.0.1（不在规则表）→ 直通
	resp, err := proxiedClient(t, proxyURL).Get(upstream.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || dialer.picks != 0 {
		t.Fatalf("白名单外域名应直通且不经选路: status=%d picks=%d", resp.StatusCode, dialer.picks)
	}
}

func TestProxy_FallsBackToNextCandidateOnDialFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	upIP, _, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	// 第一个候选为拒连端口（127.0.0.1:443 本测试环境无监听），第二个为可用上游
	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(upIP)}}
	proxyURL, _ := newStack(t, dialer)

	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应回退到第二候选成功: %d", resp.StatusCode)
	}
	if len(dialer.failures) == 0 {
		t.Fatal("失败候选应上报 ReportFailure")
	}
}

func TestProxy_AllCandidatesFailReturns502(t *testing.T) {
	// 240.0.0.1：保留地址，必然不可达（受 DialTimeout 限制）
	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("240.0.0.1")}}
	proxyURL, _ := newStack(t, dialer)

	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("全部候选失败应返回 502，得到 %d", resp.StatusCode)
	}
	if len(dialer.failures) != 1 {
		t.Fatalf("失败应上报: %v", dialer.failures)
	}
}

func TestProxy_RecordsMetrics(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("metrics"))
	}))
	t.Cleanup(upstream.Close)
	upIP, _, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	dialer := &fakeDialer{ips: []net.IP{net.ParseIP(upIP)}}
	proxyURL, srv := newStack(t, dialer)

	if _, err := proxiedClient(t, proxyURL).Get("https://accel.test/"); err != nil {
		t.Fatal(err)
	}

	conns := srv.Metrics.Conns(10)
	if len(conns) == 0 {
		t.Fatal("应记录连接日志")
	}
	c := conns[0]
	if c.Domain != "accel.test" || !c.OK || c.Down == 0 {
		t.Fatalf("连接日志字段不完整: %+v", c)
	}
}
