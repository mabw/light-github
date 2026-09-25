package selector

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marvin/light-github/internal/rule"
)

// ---- 测试替身 ----

type fakeResolver struct {
	ips        []net.IP
	err        error
	calls      int
	lastDomain string
}

func (f *fakeResolver) Resolve(_ context.Context, domain string) ([]net.IP, error) {
	f.calls++
	f.lastDomain = domain
	return f.ips, f.err
}

type fakeProber struct {
	costs map[string]time.Duration // ip → 耗时；未配置的返回 10ms
	calls atomic.Int64             // 并发测速下原子计数（buildCandidates 每 IP 一个 goroutine）
}

func (f *fakeProber) Probe(_ context.Context, ip net.IP) time.Duration {
	f.calls.Add(1)
	if d, ok := f.costs[ip.String()]; ok {
		return d
	}
	return 10 * time.Millisecond
}

func ip(s string) net.IP { return net.ParseIP(s) }

func newSel(r rule.Rule) *Selector {
	tbl := rule.NewTable([]rule.Rule{r})
	return New(tbl, &fakeResolver{}, &fakeProber{}, 5*time.Minute)
}

// ---- 候选生成：三种规则策略 ----

func TestPick_FixedIPRulePutsFixedFirstWithDoHBackup(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.207.73.82"}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// spike-a 实训：单固定 IP 不可靠，固定 IP 必须排第一但 DoH 结果作补充候选
	if len(ips) < 3 || !ips[0].Equal(ip("20.207.73.82")) {
		t.Fatalf("固定 IP 应排第一且包含 DoH 补充候选，得到 %v", ips)
	}
}

func TestPick_CNAMERuleResolvesForwardDomain(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("20.205.243.168")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "api.github.com", Kind: rule.KindCNAME, Forward: "githubapi.rmbgame.net"}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "api.github.com"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.lastDomain != "githubapi.rmbgame.net" {
		t.Fatalf("CNAME 规则应解析 Forward 域名，实际解析了 %s", res.lastDomain)
	}
}

func TestPick_DynamicRuleResolvesSelf(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.2.3.4")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "resources.github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "resources.github.com"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.lastDomain != "resources.github.com" {
		t.Fatalf("Dynamic 规则应解析自身域名，实际解析了 %s", res.lastDomain)
	}
}

func TestPick_UnmatchedDomainResolvesSelf(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.2.3.4")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	// 白名单外域名若被调用（兜底路径），解析自身即可
	if _, err := s.Pick(context.Background(), "example.com"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.lastDomain != "example.com" {
		t.Fatalf("未匹配域名应解析自身，实际 %s", res.lastDomain)
	}
}

// ---- 测速排序与缓存 ----

func TestPick_SortsByProbeCost(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("9.9.9.9"), ip("1.1.1.1"), ip("8.8.8.8")}}
	prober := &fakeProber{costs: map[string]time.Duration{
		"9.9.9.9": 300 * time.Millisecond,
		"1.1.1.1": 50 * time.Millisecond,
		"8.8.8.8": 120 * time.Millisecond,
	}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 5*time.Minute)

	ips, _ := s.Pick(context.Background(), "x.com")
	if !(ips[0].Equal(ip("1.1.1.1")) && ips[1].Equal(ip("8.8.8.8")) && ips[2].Equal(ip("9.9.9.9"))) {
		t.Fatalf("候选应按测速耗时升序: %v", ips)
	}
}

func TestPick_CachesProbeWithinTTL(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	prober := &fakeProber{}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	first := prober.calls.Load()
	if _, err := s.Pick(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	if prober.calls.Load() != first {
		t.Fatalf("TTL 内二次 Pick 不应重复测速: first=%d now=%d", first, prober.calls)
	}
}

// ---- 失败统计自动切换（借鉴 dev-sidecar DynamicChoice） ----

func TestPick_RepeatedFailureSinksIP(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	ips, _ := s.Pick(context.Background(), "x.com")
	if !ips[0].Equal(ip("1.1.1.1")) {
		t.Fatalf("前置条件失败: %v", ips)
	}

	// 连续两次失败 → 该 IP 沉底
	s.ReportFailure("x.com", ip("1.1.1.1"))
	s.ReportFailure("x.com", ip("1.1.1.1"))
	ips, _ = s.Pick(context.Background(), "x.com")
	if ips[0].Equal(ip("1.1.1.1")) {
		t.Fatalf("连续失败的 IP 应沉底: %v", ips)
	}
}

func TestPick_SuccessResetsFailureCount(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	s.Pick(context.Background(), "x.com")
	s.ReportFailure("x.com", ip("1.1.1.1"))
	s.ReportSuccess("x.com", ip("1.1.1.1")) // 成功重置计数
	s.ReportFailure("x.com", ip("1.1.1.1")) // 重新累计第一次

	ips, _ := s.Pick(context.Background(), "x.com")
	if !ips[0].Equal(ip("1.1.1.1")) {
		t.Fatalf("仅一次失败（成功已重置）不应沉底: %v", ips)
	}
}

func TestPick_AllProbeTimeoutStillReturnsCandidates(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	prober := &fakeProber{costs: map[string]time.Duration{"1.1.1.1": time.Second}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 5*time.Minute)

	// 测速全超时不视为致命：仍返回候选交由拨号方实际尝试
	ips, err := s.Pick(context.Background(), "x.com")
	if err != nil || len(ips) != 1 {
		t.Fatalf("测速超时应保留候选: ips=%v err=%v", ips, err)
	}
}

func TestPick_NoCandidatesReturnsError(t *testing.T) {
	res := &fakeResolver{ips: nil, err: context.DeadlineExceeded}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "x.com"); err == nil {
		t.Fatal("无任何候选应返回错误")
	}
}
