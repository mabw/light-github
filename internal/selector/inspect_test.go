package selector

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mabw/light-github/internal/rule"
)

// Pick 后 Inspect 暴露各域名的候选（IP/耗时/失败计数/沉底状态）
func TestInspect_ExposesCandidatesAndSinkState(t *testing.T) {
	sel := New(
		rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.1.1.1"}}),
		&fakeResolver{ips: []net.IP{ip("2.2.2.2")}},
		&fakeProber{costs: map[string]time.Duration{"1.1.1.1": 50 * time.Millisecond}},
		5*time.Minute,
	)

	if _, err := sel.Pick(context.Background(), "github.com"); err != nil {
		t.Fatal(err)
	}

	infos := sel.Inspect()
	if len(infos) != 1 {
		t.Fatalf("应含 1 个已探测域名: %+v", infos)
	}
	d := infos[0]
	if d.Domain != "github.com" || d.ProbedAt.IsZero() {
		t.Fatalf("域名与探测时间: %+v", d)
	}
	if len(d.Candidates) != 2 { // fixed 1.1.1.1 + DoH 2.2.2.2
		t.Fatalf("应有 2 个候选: %+v", d.Candidates)
	}
	if d.Candidates[0].IP != "1.1.1.1" || d.Candidates[0].CostMS != 50 {
		t.Fatalf("候选耗时应来自测速: %+v", d.Candidates[0])
	}

	// 连续失败 2 次 → 沉底标记（UI 高亮）
	sel.ReportFailure("github.com", ip("1.1.1.1"))
	sel.ReportFailure("github.com", ip("1.1.1.1"))
	for _, c := range sel.Inspect()[0].Candidates {
		if c.IP == "1.1.1.1" {
			if c.Failure != 2 || !c.Sunk {
				t.Fatalf("连续失败 2 次应沉底: %+v", c)
			}
		}
	}
}

// 未探测过的域名不出现在 Inspect；多域名按名称排序（UI 稳定展示）
func TestInspect_SkipsUnprobedAndSorts(t *testing.T) {
	sel := New(
		rule.NewTable([]rule.Rule{
			{Domain: "b.com", Kind: rule.KindDynamic},
			{Domain: "a.com", Kind: rule.KindDynamic},
		}),
		&fakeResolver{ips: []net.IP{ip("3.3.3.3")}},
		&fakeProber{},
		5*time.Minute,
	)

	if _, err := sel.Pick(context.Background(), "b.com"); err != nil {
		t.Fatal(err)
	}

	infos := sel.Inspect()
	if len(infos) != 1 || infos[0].Domain != "b.com" {
		t.Fatalf("未探测域名不应出现: %+v", infos)
	}

	if _, err := sel.Pick(context.Background(), "a.com"); err != nil {
		t.Fatal(err)
	}
	infos = sel.Inspect()
	if len(infos) != 2 || infos[0].Domain != "a.com" || infos[1].Domain != "b.com" {
		t.Fatalf("应按域名升序: %+v", infos)
	}
}

// SetTable：规则热刷新后测速缓存失效、策略判断跟随新表（FixedIP 换为 Dynamic）
func TestSetTable_HotSwapsStrategy(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("9.9.9.9")}}
	fixedTable := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.1.1.1"}})
	sel := New(fixedTable, res, &fakeProber{}, 5*time.Minute)

	ips, err := sel.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if ips[0].String() != "1.1.1.1" {
		t.Fatalf("固定 IP 应排首位: %v", ips)
	}

	// 热更新：同域名改为 Dynamic → 缓存清空重建，不再注入固定 IP
	sel.SetTable(rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}}))
	ips2, err := sel.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips2) != 1 || ips2[0].String() != "9.9.9.9" {
		t.Fatalf("Dynamic 应只含 DoH 候选: %v", ips2)
	}
}

// Reprobe：TTL 缓存命中时不重测，Reprobe 强制重测（Web UI 手动测速按钮）
func TestReprobe_ForcesRetestIgnoringCache(t *testing.T) {
	prober := &fakeProber{}
	sel := New(
		rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.1.1.1"}}),
		&fakeResolver{ips: []net.IP{ip("9.9.9.9")}},
		prober, 5*time.Minute,
	)

	if _, err := sel.Pick(context.Background(), "github.com"); err != nil {
		t.Fatal(err)
	}
	afterFirst := prober.calls.Load()

	// TTL 内再次 Pick：命中缓存，不重测
	if _, err := sel.Pick(context.Background(), "github.com"); err != nil {
		t.Fatal(err)
	}
	if got := prober.calls.Load(); got != afterFirst {
		t.Fatalf("缓存命中不应重测: %d → %d", afterFirst, got)
	}

	// Reprobe：强制重测
	if _, err := sel.Reprobe(context.Background(), "github.com"); err != nil {
		t.Fatal(err)
	}
	if got := prober.calls.Load(); got <= afterFirst {
		t.Fatalf("Reprobe 应强制重测: %d → %d", afterFirst, got)
	}
}

// Inspect 的域名与候选集必须对齐：map 迭代序随机，平行数组排序失配会
// 把 A 域名的 IP 挂到 B 域名下（review C1 回归测试；50 轮覆盖随机序）。
type mapResolver struct{ m map[string][]net.IP }

func (r *mapResolver) Resolve(_ context.Context, d string) ([]net.IP, error) {
	return r.m[d], nil
}

func TestInspect_DomainCandidatesStayAligned(t *testing.T) {
	fixed := map[string]string{
		"a.com": "1.1.1.1",
		"b.com": "2.2.2.2",
		"c.com": "3.3.3.3",
	}
	rules := make([]rule.Rule, 0, len(fixed))
	res := &mapResolver{m: map[string][]net.IP{}}
	for d, ipStr := range fixed {
		rules = append(rules, rule.Rule{Domain: d, Kind: rule.KindFixedIP, Forward: ipStr})
		res.m[d] = []net.IP{net.ParseIP(ipStr)}
	}
	s := New(rule.NewTable(rules), res, &fakeProber{}, 5*time.Minute)

	for d := range fixed {
		if _, err := s.Pick(context.Background(), d); err != nil {
			t.Fatalf("Pick %s: %v", d, err)
		}
	}

	for round := 0; round < 50; round++ {
		infos := s.Inspect()
		if len(infos) != len(fixed) {
			t.Fatalf("应返回 %d 个域名，得到 %d", len(fixed), len(infos))
		}
		for _, info := range infos {
			want := fixed[info.Domain]
			if len(info.Candidates) == 0 || info.Candidates[0].IP != want {
				t.Fatalf("round %d: 域名 %s 的候选应为 [%s]，实际 %v（域名-候选错配）",
					round, info.Domain, want, info.Candidates)
			}
		}
	}
}
