package webapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marvin/light-github/internal/config"
	"github.com/marvin/light-github/internal/logx"
	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
	"github.com/marvin/light-github/internal/selector"
	"github.com/marvin/light-github/internal/source"
)

// ---- 测试替身 ----

type stubResolver struct{ ips []net.IP }

func (s stubResolver) Resolve(context.Context, string) ([]net.IP, error) { return s.ips, nil }

type stubProber struct{}

func (stubProber) Probe(context.Context, net.IP) time.Duration { return 10 * time.Millisecond }

type stubFetcher struct{}

func (stubFetcher) Name() string { return "stub" }
func (stubFetcher) Fetch(context.Context) ([]rule.Rule, error) {
	return []rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.2.3.4"}}, nil
}

// newDeps 构建一套最小可用的依赖
func newDeps(t *testing.T, cfg config.Config) *Deps {
	t.Helper()

	store := metrics.NewStore(time.Second)
	store.RecordConn(metrics.ConnInfo{Domain: "github.com", Via: "fixed-ip", IP: "1.2.3.4", Up: 100, Down: 900, OK: true})

	logs := logx.New(logx.Options{Dir: t.TempDir()})
	t.Cleanup(func() { _ = logs.Close() })
	logs.App().Info("规则加载成功", "count", 2)
	logs.Conn().Info("conn", "domain", "github.com")

	rules := []rule.Rule{
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.2.3.4"},
		{Domain: "*.githubusercontent.com", Kind: rule.KindDynamic},
	}
	sel := selector.New(rule.NewTable(rules), stubResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}, stubProber{}, 5*time.Minute)
	_, _ = sel.Pick(context.Background(), "github.com")

	return &Deps{
		Version:   "test-1",
		StartedAt: time.Now().Add(-time.Minute),
		Addr:      "127.0.0.1:12800",
		Token:     "",
		Metrics:   store,
		Logs:      logs,
		Rules:     func() []rule.Rule { return rules },
		Selector:  sel,
		Source:    source.NewManager(stubFetcher{}),
		Config:    cfg,
	}
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// ---- 端点行为 ----

func TestStatus(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	code, body := get(t, srv.URL+"/api/status")
	if code != 200 || body["success"] != true {
		t.Fatalf("status: %d %v", code, body)
	}
	data := body["data"].(map[string]any)
	for _, k := range []string{"version", "uptime", "addr", "ruleCount", "sources", "lastRefresh"} {
		if _, ok := data[k]; !ok {
			t.Fatalf("status 缺字段 %s: %v", k, data)
		}
	}
	if data["version"] != "test-1" {
		t.Fatalf("version: %v", data["version"])
	}
}

func TestStats(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	code, body := get(t, srv.URL+"/api/stats")
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	data := body["data"].(map[string]any)
	if data["totalConns"].(float64) != 1 || data["totalDown"].(float64) != 900 {
		t.Fatalf("stats 数据不符: %v", data)
	}
}

func TestLogs_KindAllMergesBothChannels(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	_, body := get(t, srv.URL+"/api/logs?kind=all&limit=50")
	entries := body["data"].(map[string]any)["entries"].([]any)
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.(map[string]any)["kind"].(string)]++
	}
	if kinds["app"] == 0 || kinds["conn"] == 0 {
		t.Fatalf("all 应合并两种通道: %v", kinds)
	}
	// 最新在前：首条 At >= 末条
	if len(entries) >= 2 {
		first := entries[0].(map[string]any)["at"].(string)
		last := entries[len(entries)-1].(map[string]any)["at"].(string)
		if first < last {
			t.Fatalf("应按时间降序: %s < %s", first, last)
		}
	}
}

func TestLogs_BadKindReturns400(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	if code, _ := get(t, srv.URL+"/api/logs?kind=bogus"); code != 400 {
		t.Fatalf("未知 kind 应 400: %d", code)
	}
}

func TestRules(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	_, body := get(t, srv.URL+"/api/rules")
	data := body["data"].(map[string]any)
	if len(data["rules"].([]any)) != 2 {
		t.Fatalf("规则数: %v", data["rules"])
	}
	domains := data["domains"].([]any)
	if len(domains) != 1 || domains[0].(map[string]any)["domain"] != "github.com" {
		t.Fatalf("测速数据: %v", domains)
	}
}

func TestRefreshRules_CallsCallback(t *testing.T) {
	deps := newDeps(t, config.Config{})
	called := false
	deps.RefreshRules = func(context.Context) (int, error) { called = true; return 67, nil }

	srv := httptest.NewServer(Handler(deps))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/rules/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 200 || !called || body["success"] != true {
		t.Fatalf("refresh: %d called=%v %v", resp.StatusCode, called, body)
	}
	if body["data"].(map[string]any)["count"].(float64) != 67 {
		t.Fatalf("count: %v", body)
	}
}

func TestConfig_GetAndPost(t *testing.T) {
	deps := newDeps(t, config.Config{Addr: "127.0.0.1:12800", Refresh: time.Hour, LogLevel: "info"})
	deps.ConfigPath = t.TempDir() + "/config.json"
	var applied config.Config
	deps.OnConfigChange = func(c config.Config) { applied = c }

	srv := httptest.NewServer(Handler(deps))
	defer srv.Close()

	// GET
	_, body := get(t, srv.URL+"/api/config")
	if body["data"].(map[string]any)["addr"] != "127.0.0.1:12800" {
		t.Fatalf("GET config: %v", body)
	}

	// POST 修改热生效项
	resp, err := http.Post(srv.URL+"/api/config", "application/json",
		stringReader(`{"logLevel":"debug","refresh":"30m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body2 map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body2)
	if resp.StatusCode != 200 || body2["success"] != true {
		t.Fatalf("POST config: %d %v", resp.StatusCode, body2)
	}
	if applied.LogLevel != "debug" || applied.Refresh != 30*time.Minute {
		t.Fatalf("热应用回调应收到新配置: %+v", applied)
	}

	// 落盘校验
	saved, err := config.Load(deps.ConfigPath)
	if err != nil || saved.LogLevel != "debug" {
		t.Fatalf("应写回文件: %+v err=%v", saved, err)
	}

	// 非法 body → 400
	resp2, _ := http.Post(srv.URL+"/api/config", "application/json", stringReader(`{bad`))
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("坏 body 应 400: %d", resp2.StatusCode)
	}
}

// Token 非空（非 loopback 场景）时 /api/* 必须带凭据
func TestAPI_RequiresTokenWhenConfigured(t *testing.T) {
	deps := newDeps(t, config.Config{})
	deps.Token = "s3cret"

	srv := httptest.NewServer(Handler(deps))
	defer srv.Close()

	if code, _ := get(t, srv.URL+"/api/status"); code != 401 {
		t.Fatalf("无凭据应 401: %d", code)
	}
	if code, _ := get(t, srv.URL+"/api/status?token=s3cret"); code != 200 {
		t.Fatalf("正确 token 应放行: %d", code)
	}
	if code, _ := get(t, srv.URL+"/api/status?token=wrong"); code != 401 {
		t.Fatalf("错误 token 应 401: %d", code)
	}
}

func TestPAC(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	resp, err := http.Get(srv.URL+"/pac")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	pac := string(buf[:n])
	if !contains(pac, "github.com") || !contains(pac, "127.0.0.1:12800") || !contains(pac, "DIRECT") {
		t.Fatalf("PAC 应含白名单/代理地址/DIRECT: %s", pac)
	}
	// 仅 https 走代理（http 明文站点直连，代理不提供 absolute-form 转发）
	if !contains(pac, `!== "https:"`) {
		t.Fatalf("PAC 应含 scheme 判断: %s", pac)
	}
	if resp.Header.Get("Content-Type") == "" {
		t.Fatal("应声明 Content-Type")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func stringReader(s string) io.Reader { return strings.NewReader(s) }

// GET / 返回控制台静态页（webapi 整体作为 proxy.Web）
func TestIndexPage(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	code, _, body := getRaw(t, srv.URL+"/")
	if code != 200 || !strings.Contains(body, "light-github 控制台") {
		t.Fatalf("首页: %d %.80s", code, body)
	}
}

func getRaw(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, "", string(b[:n])
}

// 单域名手动测速：强制重测并返回该域名最新状态
func TestProbeDomain(t *testing.T) {
	srv := httptest.NewServer(Handler(newDeps(t, config.Config{})))
	defer srv.Close()

	// 缺 domain → 400
	resp, _ := http.Post(srv.URL+"/api/rules/probe", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("缺 domain 应 400: %d", resp.StatusCode)
	}

	// 正常重测 → 返回该域名候选
	resp2, err := http.Post(srv.URL+"/api/rules/probe?domain=github.com", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&body)
	if resp2.StatusCode != 200 || body["success"] != true {
		t.Fatalf("probe: %d %v", resp2.StatusCode, body)
	}
	d := body["data"].(map[string]any)
	if d["domain"] != "github.com" {
		t.Fatalf("data.domain: %v", d)
	}
	if cands := d["candidates"].([]any); len(cands) == 0 {
		t.Fatal("应返回候选列表")
	}
}
