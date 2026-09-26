package doh

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer 构造一个返回固定 DoH JSON 的测试端点
func newTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "" {
			t.Errorf("请求缺少 name 参数: %s", r.URL)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

const bodyMixed = `{"Status":0,"Answer":[
	{"name":"api.github.com.","type":5,"data":"githubapi.rmbgame.net."},
	{"name":"api.github.com.","type":1,"data":"20.205.243.168"},
	{"name":"api.github.com.","type":28,"data":"2606:50c0:8003::153"}
]}`

func TestResolve_ParsesARecordsOnly(t *testing.T) {
	s := newTestServer(t, 200, bodyMixed)
	c := New(s.URL)

	ips, err := c.Resolve(context.Background(), "api.github.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("20.205.243.168")) {
		t.Fatalf("应只取 IPv4 A 记录（跳过 CNAME/AAAA），得到 %v", ips)
	}
}

func TestResolve_FiltersUnusableIPs(t *testing.T) {
	s := newTestServer(t, 200, `{"Answer":[
		{"type":1,"data":"127.0.0.1"},
		{"type":1,"data":"192.168.1.1"},
		{"type":1,"data":"8.8.8.8"}
	]}`)
	c := New(s.URL)

	ips, err := c.Resolve(context.Background(), "x.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("8.8.8.8")) {
		t.Fatalf("回环/私有 IP 应被过滤，得到 %v", ips)
	}
}

// review M3：按信任序采纳首个成功端点的答案——次端点即使返回
// 额外/异常 IP 也不进候选池（并集会让被污染端点污染整个候选集）
func TestResolve_TrustOrderPrefersFirstSuccessfulEndpoint(t *testing.T) {
	a := newTestServer(t, 200, `{"Answer":[{"type":1,"data":"1.1.1.1"},{"type":1,"data":"2.2.2.2"}]}`)
	b := newTestServer(t, 200, `{"Answer":[{"type":1,"data":"2.2.2.2"},{"type":1,"data":"3.3.3.3"}]}`)
	c := New(a.URL, b.URL)

	ips, err := c.Resolve(context.Background(), "x.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ips) != 2 || !ips[0].Equal(net.ParseIP("1.1.1.1")) || !ips[1].Equal(net.ParseIP("2.2.2.2")) {
		t.Fatalf("应只采纳信任序第一端点的答案，得到 %v", ips)
	}
}

func TestResolve_ToleratesPartialEndpointFailure(t *testing.T) {
	bad := newTestServer(t, 500, `{"error":"boom"}`)
	good := newTestServer(t, 200, `{"Answer":[{"type":1,"data":"8.8.8.8"}]}`)
	c := New(bad.URL, good.URL)

	ips, err := c.Resolve(context.Background(), "x.com")
	if err != nil {
		t.Fatalf("单端点失败不应导致整体失败: %v", err)
	}
	if len(ips) != 1 {
		t.Fatalf("应得到可用端点的结果，得到 %v", ips)
	}
}

func TestResolve_AllEndpointsFailReturnsError(t *testing.T) {
	bad := newTestServer(t, 500, `{}`)
	c := New(bad.URL, bad.URL)

	if _, err := c.Resolve(context.Background(), "x.com"); err == nil {
		t.Fatal("全部端点失败应返回错误")
	}
}

func TestResolve_ContextCancelAborts(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	t.Cleanup(s.Close)
	c := New(s.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Resolve(ctx, "x.com"); err == nil {
		t.Fatal("ctx 取消应返回错误")
	}
	if time.Since(start) > time.Second {
		t.Fatal("应随 ctx 及时中止，而不是等满服务端延迟")
	}
}

// ---- M5 §2.4：结果缓存——同域名 TTL 内二次 Resolve 不再发 HTTP ----

func newCountingServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"Answer":[{"type":1,"data":"8.8.8.8"}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestResolve_CachesWithinTTL(t *testing.T) {
	var calls atomic.Int64
	c := New(newCountingServer(t, &calls).URL)

	if _, err := c.Resolve(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("TTL 内应命中缓存，实际发请求 %d 次", n)
	}

	// 不同域名不共享缓存条目
	if _, err := c.Resolve(context.Background(), "y.com"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("不同域名应独立查询: %d", n)
	}
}

func TestResolve_CacheExpires(t *testing.T) {
	var calls atomic.Int64
	c := New(newCountingServer(t, &calls).URL)
	c.CacheTTL = 40 * time.Millisecond // 注入短 TTL（含 ±10% 抖动，60ms 必过期）

	_, _ = c.Resolve(context.Background(), "x.com")
	_, _ = c.Resolve(context.Background(), "x.com")
	if n := calls.Load(); n != 1 {
		t.Fatalf("TTL 内应命中缓存: %d", n)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := c.Resolve(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("过期后应重新查询: %d", n)
	}
}

// 失败不缓存（端点故障恢复后立即可用）
func TestResolve_FailureNotCached(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`{"Answer":[{"type":1,"data":"8.8.8.8"}]}`))
	}))
	t.Cleanup(s.Close)
	c := New(s.URL)

	if _, err := c.Resolve(context.Background(), "x.com"); err == nil {
		t.Fatal("首次应失败")
	}
	if _, err := c.Resolve(context.Background(), "x.com"); err != nil {
		t.Fatalf("失败不应被缓存，二次应成功: %v", err)
	}
}
