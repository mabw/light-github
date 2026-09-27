package selector

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mabw/light-github/internal/rule"
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
	dead  map[string]bool          // ip → 语义判死（指纹/301：确定性不服务该域）
	calls atomic.Int64             // 并发测速下原子计数（buildCandidates 每 IP 一个 goroutine）
}

func (f *fakeProber) Probe(_ context.Context, _ string, ip net.IP) (time.Duration, bool) {
	f.calls.Add(1)
	if f.dead != nil && f.dead[ip.String()] {
		return time.Second, false
	}
	if d, ok := f.costs[ip.String()]; ok {
		return d, true
	}
	return 10 * time.Millisecond, true
}

func ip(s string) net.IP { return net.ParseIP(s) }

// ---- 候选生成：三种规则策略 ----

func TestPick_FixedIPRulePutsFixedFirstWithDoHBackup(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.207.73.82"}})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	ips, _ := s.Pick(context.Background(), "x.com")
	if !(ips[0].Equal(ip("1.1.1.1")) && ips[1].Equal(ip("8.8.8.8")) && ips[2].Equal(ip("9.9.9.9"))) {
		t.Fatalf("候选应按测速耗时升序: %v", ips)
	}
}

func TestPick_CachesProbeWithinTTL(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	prober := &fakeProber{}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	first := prober.calls.Load()
	if _, err := s.Pick(context.Background(), "x.com"); err != nil {
		t.Fatal(err)
	}
	if prober.calls.Load() != first {
		t.Fatalf("TTL 内二次 Pick 不应重复测速: first=%d now=%d", first, prober.calls.Load())
	}
}

// ---- 失败统计自动切换（借鉴 dev-sidecar DynamicChoice） ----

func TestPick_RepeatedFailureSinksIP(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

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
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	// 测速全超时不视为致命：仍返回候选交由拨号方实际尝试
	ips, err := s.Pick(context.Background(), "x.com")
	if err != nil || len(ips) != 1 {
		t.Fatalf("测速超时应保留候选: ips=%v err=%v", ips, err)
	}
}

func TestPick_NoCandidatesReturnsError(t *testing.T) {
	res := &fakeResolver{ips: nil, err: context.DeadlineExceeded}
	tbl := rule.NewTable([]rule.Rule{{Domain: "x.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

	if _, err := s.Pick(context.Background(), "x.com"); err == nil {
		t.Fatal("无任何候选应返回错误")
	}
}

// 按域名映射的 resolver（DEBT-4 测试需要）
type domainResolver struct {
	byDomain map[string][]net.IP
}

func (d *domainResolver) Resolve(_ context.Context, domain string) ([]net.IP, error) {
	return d.byDomain[domain], nil
}

// blockingProber 真实阻塞的探针（fakeProber 即时返回耗时值，无法验证锁行为）
type blockingProber struct {
	delay map[string]time.Duration
	calls atomic.Int64
}

func (b *blockingProber) Probe(_ context.Context, _ string, ip net.IP) (time.Duration, bool) {
	b.calls.Add(1)
	if d, ok := b.delay[ip.String()]; ok {
		time.Sleep(d)
		return d, true
	}
	return time.Millisecond, true
}

// DEBT-4：域名 A 测速期间（真阻塞 prober），域名 B 的 Pick 不应被全局锁阻塞
func TestPick_DifferentDomainsDoNotBlockEachOther(t *testing.T) {
	res := &domainResolver{byDomain: map[string][]net.IP{
		"slow.test": {ip("1.1.1.1")},
		"fast.test": {ip("2.2.2.2")},
	}}
	prober := &blockingProber{delay: map[string]time.Duration{
		"1.1.1.1": 300 * time.Millisecond, // slow.test 候选：真实阻塞 300ms
	}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "slow.test", Kind: rule.KindDynamic}, {Domain: "fast.test", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	doneSlow := make(chan time.Time, 1)
	doneFast := make(chan time.Time, 1)
	errCh := make(chan error, 2)
	start := time.Now()
	go func() { _, err := s.Pick(context.Background(), "slow.test"); errCh <- err; doneSlow <- time.Now() }()
	time.Sleep(30 * time.Millisecond) // 确保 slow 已进入测速（持锁）
	go func() { _, err := s.Pick(context.Background(), "fast.test"); errCh <- err; doneFast <- time.Now() }()

	fastAt := <-doneFast
	slowAt := <-doneSlow
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("Pick 不应出错: %v", err)
		}
	}
	if fastAt.Sub(start) >= slowAt.Sub(start) {
		t.Fatalf("fast 域名不应被 slow 域名的测速阻塞: fast=%v slow=%v proberCalls=%d",
			fastAt.Sub(start), slowAt.Sub(start), prober.calls.Load())
	}
}

// DEBT-3：Preload 后续 Pick 应直接命中缓存不再测速
func TestSelector_PreloadFillsCache(t *testing.T) {
	res := &domainResolver{byDomain: map[string][]net.IP{
		"a.test": {ip("1.1.1.1")},
	}}
	prober := &fakeProber{}
	tbl := rule.NewTable([]rule.Rule{{Domain: "a.test", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	s.Preload(context.Background(), []string{"a.test"}, 2)
	afterPreload := prober.calls.Load()

	if _, err := s.Pick(context.Background(), "a.test"); err != nil {
		t.Fatal(err)
	}
	if prober.calls.Load() != afterPreload {
		t.Fatalf("预热后 Pick 不应再次测速: preload=%d now=%d", afterPreload, prober.calls.Load())
	}
	if afterPreload == 0 {
		t.Fatal("预热本身应触发测速")
	}
}

// SetTable 清缓存时不得持全局锁等分片锁（review H1）：
// 分片锁内是网络 IO（DoH+测速），持全局锁等待会卡住所有域名的 stateFor/Pick。
// 场景：a.com 在途慢探测（2s）→ SetTable → b.com 的 Pick 仍应快速完成。
func TestSetTable_DoesNotBlockOtherDomainsOnInflightProbe(t *testing.T) {
	res := &mapResolver{m: map[string][]net.IP{
		"a.com": {ip("1.1.1.1")},
		"b.com": {ip("2.2.2.2")},
	}}
	// 真阻塞探针：fakeProber 只返回耗时值不阻塞，复现不了持锁跨 IO
	prober := &blockingProber{delay: map[string]time.Duration{
		"1.1.1.1": 2 * time.Second, // a.com 慢探测（持其分片锁）
		"2.2.2.2": 10 * time.Millisecond,
	}}
	tbl := rule.NewTable([]rule.Rule{
		{Domain: "a.com", Kind: rule.KindDynamic},
		{Domain: "b.com", Kind: rule.KindDynamic},
	})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	go func() { _, _ = s.Pick(context.Background(), "a.com") }()
	time.Sleep(100 * time.Millisecond) // 等 a.com 进入慢探测

	setDone := make(chan struct{})
	go func() { s.SetTable(rule.NewTable(nil)); close(setDone) }() // 换表清缓存
	time.Sleep(150 * time.Millisecond)                             // 让 SetTable 进入等分片锁状态

	// 关键断言：SetTable 等待期间，其他域名的 Pick 不被全局锁拖住
	start := time.Now()
	if _, err := s.Pick(context.Background(), "b.com"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("SetTable 等待在途探测期间其他域名应仍可 Pick，实际 %v（全局锁被拖住）", elapsed)
	}
	<-setDone
	_ = setDone
}

// ---- M5 Phase 2：失败即时反馈 + 选路生命周期回收 ----

// 拨号失败上报应失效测速缓存（冷却过后的下次 Pick 重建重测，不等 TTL；
// M5-7 起重建有冷却防抖，注入短冷却验证失效语义本身）
func TestReportFailure_InvalidatesCache(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, time.Minute, time.Minute)
	s.RebuildCooldown = time.Millisecond

	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 1 {
		t.Fatalf("首次 Pick 应解析一次: %d", res.calls)
	}
	s.ReportFailure("github.com", ip("1.1.1.1"))
	time.Sleep(2 * time.Millisecond) // 越过冷却窗口（防抖：冷却内沿用旧候选）
	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 2 {
		t.Fatalf("失败上报且冷却过后应重建候选（重新解析）: %d", res.calls)
	}
}

// 首轮 TTL 短（10s 语义的注入版）：启动/预热时网络坏的自我纠正窗口
func TestPick_FirstRoundShorterTTL(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 50*time.Millisecond, time.Hour)

	_, _ = s.Pick(context.Background(), "github.com")
	time.Sleep(60 * time.Millisecond) // 超过首轮 TTL
	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 2 {
		t.Fatalf("首轮 TTL 过期后应重建: %d", res.calls)
	}
	_, _ = s.Pick(context.Background(), "github.com") // 稳态 TTL 内
	if res.calls != 2 {
		t.Fatalf("进入稳态后不应频繁重建: %d", res.calls)
	}
}

// 稳态节奏（Watt 10s/100s 语义）：首个缓存用 firstTTL，第二个起用 steadyTTL
func TestPick_SteadyRoundTTL(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, 30*time.Millisecond, 2*time.Second)

	_, _ = s.Pick(context.Background(), "github.com") // 首个缓存（firstTTL 起算）
	time.Sleep(40 * time.Millisecond)                 // > firstTTL(30ms)
	_, _ = s.Pick(context.Background(), "github.com") // 重建 → 第二个缓存（steadyTTL 起算）
	if res.calls != 2 {
		t.Fatalf("首个缓存应按 firstTTL 过期重建: %d", res.calls)
	}
	time.Sleep(40 * time.Millisecond) // > firstTTL 但 < steadyTTL(2s)
	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 2 {
		t.Fatalf("稳态缓存应按 steadyTTL 存活（不受 firstTTL 影响）: %d", res.calls)
	}
}

// 同名多源 FixedIP 全部进入候选（M5-4：单一源全灭时其他源同域 IP 仍可用）
func TestPick_AllFixedIPsFromDuplicateRules(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("3.3.3.3")}}
	tbl := rule.NewTable([]rule.Rule{
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "1.1.1.1"},
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "2.2.2.2"},
	})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	has := func(want string) bool {
		for _, x := range ips {
			if x.String() == want {
				return true
			}
		}
		return false
	}
	if !has("1.1.1.1") || !has("2.2.2.2") || !has("3.3.3.3") {
		t.Fatalf("候选应含两条固定 IP 与 DoH 补充: %v", ips)
	}
}

// M5-6 借段兜底：数据源对某域只给单一官方段（github.com 实测只给 20.x），
// 段级封锁时主候选全灭。主候选稀缺（≤2）时从表内其他 FixedIP 借段补充，
// TLS 测速淘汰死段——端到端握手保证连错 IP 也不出安全问题。
func TestPick_BorrowSiblingsWhenPrimaryScarce(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("20.205.1.1")}} // DoH 也是同一死段
	tbl := rule.NewTable([]rule.Rule{
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.207.1.1"},
		{Domain: "collector.github.com", Kind: rule.KindFixedIP, Forward: "140.82.1.1"},
		{Domain: "avatars.githubusercontent.com", Kind: rule.KindFixedIP, Forward: "185.199.1.1"},
	})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	has := func(want string) bool {
		for _, x := range ips {
			if x.String() == want {
				return true
			}
		}
		return false
	}
	if !has("20.207.1.1") || !has("20.205.1.1") {
		t.Fatalf("主候选应保留: %v", ips)
	}
	if !has("140.82.1.1") { // collector.github.com：同根域家族（github.com），可借
		t.Fatalf("主候选稀缺时应借同家族 FixedIP: %v", ips)
	}
	if has("185.199.1.1") { // avatars.githubusercontent.com：跨根域，不借
		t.Fatalf("跨根域 IP 不应借段（应用层不服务目标域）: %v", ips)
	}

	// 主候选充足（≥3）的域不借段——避免无谓扩大候选
	res2 := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	tbl2 := rule.NewTable([]rule.Rule{
		{Domain: "a.com", Kind: rule.KindFixedIP, Forward: "3.3.3.3"},
		{Domain: "b.com", Kind: rule.KindFixedIP, Forward: "9.9.9.9"},
	})
	s2 := New(tbl2, res2, &fakeProber{}, 10*time.Second, 5*time.Minute)
	ips2, _ := s2.Pick(context.Background(), "a.com")
	for _, x := range ips2 {
		if x.String() == "9.9.9.9" {
			t.Fatalf("主候选充足（3 个）不应借段: %v", ips2)
		}
	}
}

// DoH 补充与 FixedIP 同 IP（真实场景：github.com 的 DoH 与 GitHub520
// 同给 20.205.243.166）时，借段判定按去重后计数——重复不得虚高
func TestPick_DuplicateSupplementDoesNotBlockBorrow(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("20.205.1.1")}} // 与第二条 FixedIP 重复
	tbl := rule.NewTable([]rule.Rule{
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.207.1.1"},
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.205.1.1"},
		{Domain: "collector.github.com", Kind: rule.KindFixedIP, Forward: "140.82.1.1"},
	})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ips {
		if x.String() == "140.82.1.1" {
			return // 借到了
		}
	}
	t.Fatalf("重复补充不应阻断借段: %v", ips)
}

// M5-7：深封锁期 dirty 循环防抖——冷却期内重复 Pick 不再全量重测
// （每请求在锁内同步测 8 IP×3TCP+8HEAD 会阻塞所有并发请求）
func TestPick_DirtyRebuildCooldown(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, &fakeProber{}, time.Hour, time.Hour) // TTL 长期不过期，仅靠 dirty 触发
	s.RebuildCooldown = 500 * time.Millisecond

	_, _ = s.Pick(context.Background(), "github.com") // 首建（lastBuildAt 记时）
	s.ReportFailure("github.com", ip("1.1.1.1"))      // dirty
	_, _ = s.Pick(context.Background(), "github.com")
	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 1 { // 冷却挡住首建后立即的 dirty，不重建
		t.Fatalf("冷却期内不应重复重建: %d", res.calls)
	}
	time.Sleep(550 * time.Millisecond)
	_, _ = s.Pick(context.Background(), "github.com")
	if res.calls != 2 {
		t.Fatalf("冷却过后应重建: %d", res.calls)
	}
}

// M5-12：借段只对根域开放——子域服务（api.github.com 有专属服务 IP）借
// 根域/web 前端的 IP 只会得到 301 跨域路由（2026-09-26 实测：github.com 的
// 20.207.73.82 被借给 api.github.com，拨号"成功"但隧道里全是 301 HTML，
// 零解密架构无感知；GitHub 边缘路由漂移 + TTL 窗口内测速快照错配）。
func TestPick_SubdomainDoesNotBorrow(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("20.205.1.1")}} // 单一主候选（稀缺）
	tbl := rule.NewTable([]rule.Rule{
		{Domain: "api.github.com", Kind: rule.KindFixedIP, Forward: "20.205.1.1"},
		{Domain: "github.com", Kind: rule.KindFixedIP, Forward: "20.207.9.9"},
		{Domain: "collector.github.com", Kind: rule.KindFixedIP, Forward: "140.82.9.9"},
	})
	s := New(tbl, res, &fakeProber{}, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ips {
		if x.String() == "20.207.9.9" || x.String() == "140.82.9.9" {
			t.Fatalf("子域不应借段（借来的 IP 不服务该域，301 隐性故障）: %v", ips)
		}
	}
}

// M5-13：语义判死（指纹/301）的候选彻底剔除出拨号列表——只排序沉底不够：
// 波动期前序候选全失败时拨号轮换仍会撞上健康节点（TCP+TLS 通，拨号
// "成功"，隧道内裸 "OK"——2026-09-27 用户实测「还是偶尔出现 ok」，
// 连接日志 10:02:18 github.com→140.82.114.22 复现）。
func TestPick_SemanticDeadCandidateExcluded(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1"), ip("2.2.2.2")}}
	prober := &fakeProber{dead: map[string]bool{"2.2.2.2": true}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ips {
		if x.String() == "2.2.2.2" {
			t.Fatalf("语义判死候选不应出现在拨号列表: %v", ips)
		}
	}
	if len(ips) == 0 {
		t.Fatal("活候选应保留")
	}
}

// 全部语义判死 → 返回空列表（proxy 侧 ips 空 → 直连兜底接管；
// 确定性错 IP 不值得拨号尝试）
func TestPick_AllSemanticDeadReturnsEmpty(t *testing.T) {
	res := &fakeResolver{ips: []net.IP{ip("1.1.1.1")}}
	prober := &fakeProber{dead: map[string]bool{"1.1.1.1": true}}
	tbl := rule.NewTable([]rule.Rule{{Domain: "github.com", Kind: rule.KindDynamic}})
	s := New(tbl, res, prober, 10*time.Second, 5*time.Minute)

	ips, err := s.Pick(context.Background(), "github.com")
	if err != nil {
		t.Fatalf("全 dead 是确定性结论，不是错误: %v", err)
	}
	if len(ips) != 0 {
		t.Fatalf("全 dead 应返回空（拨号侧走兜底）: %v", ips)
	}
}
