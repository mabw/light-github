package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
		Addr:        "127.0.0.1:0",
		Table:       rule.NewTable([]rule.Rule{{Domain: "accel.test", Kind: rule.KindDynamic}}),
		Dialer:      dialer,
		Metrics:     metrics.NewStore(time.Second),
		DialTimeout: 500 * time.Millisecond,
		IdleTimeout: 150 * time.Millisecond, // DEBT-2：测试用短空闲超时
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
	_, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	proxyURL, _ := newStack(t, dialer)

	// 请求带上游端口：CONNECT accel.test:<port> → 拨 127.0.0.1:<port>
	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test:" + upPort + "/")
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
	_, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	// 第一候选 240.0.0.1 保留地址必失败，第二候选 127.0.0.1 可用（端口=上游端口）
	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("240.0.0.1"), net.ParseIP("127.0.0.1")}}
	proxyURL, _ := newStack(t, dialer)

	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test:" + upPort + "/")
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

	// Go http client 收到非 200 的 CONNECT 响应表现为 error（"Bad Gateway"）
	_, err := proxiedClient(t, proxyURL).Get("https://accel.test/")
	if err == nil {
		t.Fatal("全部候选失败应表现为 Bad Gateway 错误")
	}
	if len(dialer.failures) != 1 {
		t.Fatalf("失败应上报: %v", dialer.failures)
	}
}

func TestProxy_RecordsMetrics(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close") // 促使隧道双向结束（record 异步发生）
		_, _ = w.Write([]byte("metrics"))
	}))
	t.Cleanup(upstream.Close)
	_, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	proxyURL, srv := newStack(t, dialer)

	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test:" + upPort + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close() // 促使隧道双向结束，record 异步发生

	var conns []metrics.ConnInfo
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if conns = srv.Metrics.Conns(10); len(conns) > 0 {
			break
		}
	}
	if len(conns) == 0 {
		t.Fatal("应记录连接日志")
	}
	c := conns[0]
	if c.Domain != "accel.test" || !c.OK || c.Down == 0 {
		t.Fatalf("连接日志字段不完整: %+v", c)
	}
}

func TestProxy_SetTableHotSwapsRules(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("swapped"))
	}))
	t.Cleanup(upstream.Close)
	_, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	proxyURL, srv := newStack(t, dialer)

	// 初始规则表含 accel.test → 走选路
	resp, err := proxiedClient(t, proxyURL).Get("https://accel.test:" + upPort + "/")
	if err != nil {
		t.Fatalf("err: %v (picks=%d failures=%v success=%v)", err, dialer.picks, dialer.failures, dialer.success)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if dialer.picks == 0 {
		t.Fatal("前置条件：初始规则应命中选路")
	}

	// 热更新为空表 → 同域名转为直通（accel.test 为假域名，直通拨号必失败；
	// 本用例断言点是"不再触发选路"，直通成败不在关注范围）
	srv.SetTable(rule.NewTable(nil))
	_, _ = proxiedClient(t, proxyURL).Get("https://accel.test:" + upPort + "/")
	if dialer.picks != 1 {
		t.Fatalf("换表后不应再走选路: picks=%d", dialer.picks)
	}
}

// DEBT-2：空闲隧道必须在 IdleTimeout 附近被双向关闭（防 goroutine 泄漏）
func TestProxy_IdleTimeoutClosesTunnel(t *testing.T) {
	// 挂起上游：accept 后保持连接但不读不写
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		var held []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // 持有不断开，制造永久挂起
		}
	}()
	_, upPort, _ := net.SplitHostPort(ln.Addr().String())

	dialer := &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1")}}
	proxyURL, _ := newStack(t, dialer)

	// 原始 TCP 客户端：发 CONNECT → 收 200 → 之后静默等待
	c, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "CONNECT accel.test:%s HTTP/1.1\r\nHost: accel.test:%s\r\n\r\n", upPort, upPort); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT 应成功: resp=%v err=%v", resp, err)
	}

	// 双向无字节的隧道应在 IdleTimeout(150ms) 附近被服务器关闭
	start := time.Now()
	_, err = br.ReadByte()
	if err == nil {
		t.Fatal("挂起上游不应返回数据")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("空闲隧道应在 IdleTimeout 附近关闭，实际挂了 %v", elapsed)
	}
}

// DEBT-6：配置 Token 后无凭据 CONNECT 返回 407，正确 Bearer 凭据放行
func TestProxy_TokenAuth(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte("secured"))
	}))
	t.Cleanup(upstream.Close)
	_, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	srv := &Server{
		Addr:        "127.0.0.1:0",
		Table:       rule.NewTable([]rule.Rule{{Domain: "accel.test", Kind: rule.KindDynamic}}),
		Dialer:      &fakeDialer{ips: []net.IP{net.ParseIP("127.0.0.1")}},
		Metrics:     metrics.NewStore(time.Second),
		DialTimeout: 500 * time.Millisecond,
		Token:       "s3cret-token",
	}
	addr, err := srv.ListenAndServe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// 无凭据 → 407
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT accel.test:%s HTTP/1.1\r\nHost: accel.test:%s\r\n\r\n", upPort, upPort)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("无凭据应 407: resp=%v err=%v", resp, err)
	}
	c.Close()

	// 正确凭据 → 200 + 隧道可用
	authed := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr.String()}),
			ProxyConnectHeader: http.Header{
				"Proxy-Authorization": []string{"Bearer s3cret-token"},
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 测试
		},
		Timeout: 5 * time.Second,
	}
	resp2, err := authed.Get("https://accel.test:" + upPort + "/")
	if err != nil {
		t.Fatalf("正确凭据应放行: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp2.StatusCode)
	}
}

// ---- M2-5：端口复用（非 CONNECT 的 origin-form 请求交给 Web 处理器） ----

// GET /api/x 经代理端口应到达 Web handler（含响应头透传）
func TestProxy_WebRoutesOriginForm(t *testing.T) {
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := &Server{
		Addr:  "127.0.0.1:0",
		Table: rule.NewTable(nil),
		Web:   web,
	}
	addr, err := srv.ListenAndServe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + addr.String() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("web 路由失败: %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("响应头应透传: %q", ct)
	}
}

// absolute-form 的普通代理请求（GET http://example.com/）仍拒绝，不进 Web
func TestProxy_WebDoesNotServeAbsoluteForm(t *testing.T) {
	srv := &Server{
		Addr:  "127.0.0.1:0",
		Table: rule.NewTable(nil),
		Web:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("web")) }),
	}
	addr, err := srv.ListenAndServe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// 原始 TCP 模拟正向代理形态请求（absolute-form）
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("absolute-form 应 400: %d", resp.StatusCode)
	}
}

// Web 为 nil 时保持旧行为（400）
func TestProxy_NilWebStill400(t *testing.T) {
	srv := &Server{Addr: "127.0.0.1:0", Table: rule.NewTable(nil)}
	addr, err := srv.ListenAndServe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + addr.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("nil Web 应保持 400: %d", resp.StatusCode)
	}
}
