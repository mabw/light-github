package selector

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/marvin/light-github/internal/rule"
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
