package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/marvin/light-github/internal/rule"
)

// ---- steampp 源解析（fixture 复刻真实结构：emoji 顶层键 + 混合类型） ----

const steamppFixture = `{
	"🦓": [{"Name":"Github","Items":[
		{"Name":"网站","Port":443,
		 "MatchDomainNames":"github.com;pages.github.com;gist.github.com",
		 "ListenDomainNames":"github.com",
		 "ForwardDomainNames":"20.207.73.82","ProxyType":0},
		{"Name":"Api","Port":443,
		 "MatchDomainNames":"api.github.com",
		 "ForwardDomainNames":"githubapi.rmbgame.net","ProxyType":0},
		{"Name":"动态","Port":443,
		 "MatchDomainNames":"resources.github.com",
		 "ForwardDomainNames":"resources.github.com","ProxyType":0},
		{"Name":"中转","Port":443,
		 "MatchDomainNames":"huggingface.co",
		 "ForwardDomainNames":"http://nl.mossimo.top:41080/","FakeServerName":"nl.mossimo.top","ProxyType":0},
		{"Name":"资产","Port":443,
		 "MatchDomainNames":"githubusercontent.com;raw.github.com",
		 "ForwardDomainNames":"23.235.37.133","FakeServerName":"Github","ProxyType":0}
	]}],
	"🦄": 200,
	"🐴": "ok"
}`

func newSteamppServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("steampp 应为 POST，得到 %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(steamppFixture))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSteampp_NormalizesAllStrategyKinds(t *testing.T) {
	src := NewSteampp(newSteamppServer(t).URL)
	rules, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tbl := rule.NewTable(rules)
	assert := func(domain string, kind rule.Kind) {
		t.Helper()
		r, ok := tbl.Match(domain)
		if !ok || r.Kind != kind {
			t.Fatalf("%s 应为 %s，得到 %+v ok=%v", domain, kind, r, ok)
		}
	}
	assert("github.com", rule.KindFixedIP)
	assert("api.github.com", rule.KindCNAME)
	assert("resources.github.com", rule.KindDynamic)
	// Relay（http:// scheme）与 fakeSNI 规则均降级 Dynamic（spike-c 结论）
	assert("huggingface.co", rule.KindDynamic)
	assert("raw.github.com", rule.KindDynamic)
	// 一个项目的多个 Match 域名应展开为多条规则
	if _, ok := tbl.Match("pages.github.com"); !ok {
		t.Fatal("分号分隔的 MatchDomainNames 应展开")
	}
}

func TestSteampp_OnlyGithubGroup(t *testing.T) {
	src := NewSteampp(newSteamppServer(t).URL)
	rules, _ := src.Fetch(context.Background())
	for _, r := range rules {
		// fixture 只有 Github 分组；若混入其他分组说明分组过滤失效
		if r.Domain == "store.steampowered.com" {
			t.Fatal("不应包含非 Github 分组域名")
		}
	}
}

// ---- GitHub520 源解析 ----

func TestGitHub520_ParsesHostsJSON(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[["140.82.112.25","alive.github.com"],["20.205.243.168","api.github.com"]]`))
	}))
	t.Cleanup(s.Close)

	src := NewGitHub520(s.URL)
	rules, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("应解析出 2 条规则，得到 %d", len(rules))
	}
	if rules[0].Kind != rule.KindFixedIP || rules[0].Forward != "140.82.112.25" {
		t.Fatalf("GitHub520 条目应全部为 FixedIP: %+v", rules[0])
	}
}

func TestGitHub520_SkipsLoopback(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[["127.0.0.1","evil.example.com"],["1.1.1.1","ok.example.com"]]`))
	}))
	t.Cleanup(s.Close)

	src := NewGitHub520(s.URL)
	rules, _ := src.Fetch(context.Background())
	if len(rules) != 1 || rules[0].Domain != "ok.example.com" {
		t.Fatalf("回环条目应被剔除: %+v", rules)
	}
}

// ---- 内置清单 ----

func TestBuiltin_LoadsEmbeddedRules(t *testing.T) {
	src := NewBuiltin()
	rules, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tbl := rule.NewTable(rules)
	// 三大目标域必须覆盖
	for _, d := range []string{"github.com", "hub.docker.com", "huggingface.co", "raw.githubusercontent.com"} {
		if _, ok := tbl.Match(d); !ok {
			t.Fatalf("内置清单缺少 %s", d)
		}
	}
}

// ---- Manager：多源合并/降级/缓存 ----

type stubSource struct {
	name  string
	rules []rule.Rule
	err   error
}

func (s *stubSource) Name() string { return s.name }
func (s *stubSource) Fetch(context.Context) ([]rule.Rule, error) {
	return s.rules, s.err
}

func TestManager_MergesWithPriority(t *testing.T) {
	// 高优先级源的规则胜出；低优先级源补充新域名
	m := NewManager(
		&stubSource{name: "a", rules: []rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.1.1.1"}}},
		&stubSource{name: "b", rules: []rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "2.2.2.2"}, {Domain: "gitee.com", Kind: rule.KindDynamic}}},
	)
	rules, err := m.Refresh(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tbl := rule.NewTable(rules)
	r, _ := tbl.Match("github.com")
	if r.Forward != "1.1.1.1" {
		t.Fatalf("高优先级源应胜出，得到 Forward=%s", r.Forward)
	}
	if _, ok := tbl.Match("gitee.com"); !ok {
		t.Fatal("低优先级源的新域名应被合并")
	}
}

func TestManager_FallsBackWhenEarlierSourceFails(t *testing.T) {
	m := NewManager(
		&stubSource{name: "bad", err: os.ErrDeadlineExceeded},
		&stubSource{name: "good", rules: []rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}}},
	)
	rules, err := m.Refresh(context.Background())
	if err != nil {
		t.Fatalf("前序源失败应降级到后继源: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("应得到后继源的规则，得到 %d 条", len(rules))
	}
}

func TestManager_AllSourcesFailUsesLocalCache(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "rules.json")

	// 第一次：正常源成功并写缓存
	m := NewManager(&stubSource{name: "ok", rules: []rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}}})
	m.CachePath = cache
	if _, err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第二次：全部源失败，应读缓存
	m2 := NewManager(&stubSource{name: "bad", err: os.ErrDeadlineExceeded})
	m2.CachePath = cache
	rules, err := m2.Load(context.Background())
	if err != nil {
		t.Fatalf("全源失败应回退本地缓存: %v", err)
	}
	if len(rules) != 1 || rules[0].Domain != "github.com" {
		t.Fatalf("缓存内容不符: %+v", rules)
	}
}

func TestManager_AllFailNoCacheReturnsError(t *testing.T) {
	m := NewManager(&stubSource{name: "bad", err: os.ErrDeadlineExceeded})
	m.CachePath = filepath.Join(t.TempDir(), "nonexistent.json")
	if _, err := m.Load(context.Background()); err == nil {
		t.Fatal("全源失败且无缓存应返回错误")
	}
}

// ---- Manager.Status：刷新时间与各源成败（Web UI 状态页数据） ----

func TestManager_StatusReportsPerSource(t *testing.T) {
	m := NewManager(
		&stubSource{name: "ok-src", rules: []rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}}},
		&stubSource{name: "bad-src", err: errors.New("boom")},
	)

	// 初始：从未刷新
	if st := m.Status(); !st.LastRefreshAt.IsZero() {
		t.Fatalf("未刷新时 LastRefreshAt 应为零值: %+v", st)
	}

	if _, err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("任一源成功即成功: %v", err)
	}

	st := m.Status()
	if st.LastRefreshAt.IsZero() || !st.LastOK {
		t.Fatalf("刷新成功应记录时间与状态: %+v", st)
	}
	byName := map[string]SourceState{}
	for _, s := range st.Sources {
		byName[s.Name] = s
	}
	if !byName["ok-src"].OK {
		t.Fatalf("成功源应标记 OK: %+v", byName["ok-src"])
	}
	if byName["bad-src"].OK || byName["bad-src"].Err != "boom" {
		t.Fatalf("失败源应携带错误: %+v", byName["bad-src"])
	}
}

func TestManager_StatusAllFailed(t *testing.T) {
	m := NewManager(&stubSource{name: "only", err: errors.New("down")})

	if _, err := m.Refresh(context.Background()); err == nil {
		t.Fatal("全源失败应报错")
	}
	if st := m.Status(); st.LastOK {
		t.Fatalf("全失败时 LastOK 应为 false: %+v", st)
	}
}
