// Package selector 出站选择器：按规则产候选 → 测速排序 → 失败统计自动切换。
//
// 候选生成策略（docs/DESIGN.md §3.2）：
//   - FixedIP：固定 IP 排首位，DoH 解析原域名作补充候选（spike-a 实训：单 IP 不可靠）
//   - CNAME：DoH 解析 Forward 域名（官方防污染 CNAME 通道）
//   - Dynamic / 未匹配：DoH 解析原域名
//
// 失败统计借鉴 dev-sidecar 的 DynamicChoice：连续失败达阈值的 IP 沉底（不删除，
// 保留为最后候选），连接成功即重置计数。
package selector

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/marvin/light-github/internal/rule"
)

// sinkThreshold 连续失败达到此次数的候选沉底
const sinkThreshold = 2

// Resolver 域名 → IP 候选来源（DoH / CNAME 通道共用）。
type Resolver interface {
	Resolve(ctx context.Context, domain string) ([]net.IP, error)
}

// Prober 单 IP 出口质量探测（TCP 建连中位耗时；失败返回惩罚值）。
type Prober interface {
	Probe(ctx context.Context, ip net.IP) time.Duration
}

type candidate struct {
	ip      net.IP
	cost    time.Duration // 测速中位耗时
	failure int           // 连续失败次数（内存态，不随缓存过期重置语义）
}

type domainState struct {
	probedAt   time.Time
	candidates []candidate
}

// Selector 出站选择器。并发安全。
type Selector struct {
	tbl      *rule.Table
	resolver Resolver
	prober   Prober
	ttl      time.Duration

	mu     sync.Mutex
	states map[string]*domainState
}

// New 构造选择器；cacheTTL 为测速结果缓存有效期（设计值 5min）。
func New(tbl *rule.Table, resolver Resolver, prober Prober, cacheTTL time.Duration) *Selector {
	return &Selector{
		tbl:      tbl,
		resolver: resolver,
		prober:   prober,
		ttl:      cacheTTL,
		states:   map[string]*domainState{},
	}
}

// Pick 返回 domain 的出口候选 IP（按优先级排序，拨号方逐个尝试）。
// 测速结果缓存至 TTL 过期；连续失败的 IP 沉底。
func (s *Selector) Pick(ctx context.Context, domain string) ([]net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.states[domain]
	if !ok || time.Since(state.probedAt) >= s.ttl {
		cands, err := s.buildCandidates(ctx, domain, state)
		if err != nil && len(cands) == 0 {
			return nil, err
		}
		if state == nil {
			state = &domainState{}
			s.states[domain] = state
		}
		state.candidates, state.probedAt = cands, time.Now()
	}

	ordered := make([]candidate, len(state.candidates))
	copy(ordered, state.candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := sinkRank(ordered[i]), sinkRank(ordered[j])
		if si != sj {
			return si < sj
		}
		return ordered[i].cost < ordered[j].cost
	})

	ips := make([]net.IP, len(ordered))
	for i, c := range ordered {
		ips[i] = c.ip
	}
	return ips, nil
}

// ReportFailure 上报某 IP 连接失败（连续失败达阈值后沉底）。
func (s *Selector) ReportFailure(domain string, ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.findCandidate(domain, ip); ok {
		c.failure++
	}
}

// ReportSuccess 上报连接成功（重置失败计数）。
func (s *Selector) ReportSuccess(domain string, ip net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.findCandidate(domain, ip); ok {
		c.failure = 0
	}
}

func (s *Selector) findCandidate(domain string, ip net.IP) (*candidate, bool) {
	state, ok := s.states[domain]
	if !ok {
		return nil, false
	}
	for i := range state.candidates {
		if state.candidates[i].ip.Equal(ip) {
			return &state.candidates[i], true
		}
	}
	return nil, false
}

// buildCandidates 生成候选集并测速；resolver 失败但存在历史候选时沿用旧候选。
func (s *Selector) buildCandidates(ctx context.Context, domain string, prev *domainState) ([]candidate, error) {
	ips, resolveErr := s.candidateIPs(ctx, domain)

	// resolve 失败：有旧候选则降级沿用（DoH 短暂抖动不应击穿缓存）
	if resolveErr != nil {
		if prev != nil && len(prev.candidates) > 0 {
			return prev.candidates, nil
		}
		return nil, resolveErr
	}

	// 去重合并（保持来源顺序：固定 IP → DoH 结果）
	var merged []net.IP
	seen := map[string]bool{}
	for _, ip := range ips {
		if k := ip.String(); !seen[k] {
			seen[k] = true
			merged = append(merged, ip)
		}
	}

	// 并发测速
	cands := make([]candidate, len(merged))
	var wg sync.WaitGroup
	for i, ip := range merged {
		wg.Add(1)
		go func(i int, ip net.IP) {
			defer wg.Done()
			cands[i] = candidate{ip: ip, cost: s.prober.Probe(ctx, ip)}
		}(i, ip)
	}
	wg.Wait()

	// 沿用历史失败计数（测速刷新不应清零运行期失败记忆）
	if prev != nil {
		for i := range cands {
			if old, ok := s.findIn(prev.candidates, cands[i].ip); ok {
				cands[i].failure = old.failure
			}
		}
	}
	return cands, nil
}

// candidateIPs 按规则策略产出原始候选（可能为空 + err）。
func (s *Selector) candidateIPs(ctx context.Context, domain string) ([]net.IP, error) {
	r, matched := s.tbl.Match(domain)

	if matched && r.Kind == rule.KindFixedIP {
		if fixed := net.ParseIP(r.Forward); fixed != nil {
			// 固定 IP 优先，DoH 补充
			supplement, _ := s.resolver.Resolve(ctx, domain)
			return append([]net.IP{fixed}, supplement...), nil
		}
	}
	query := domain
	if matched && r.Kind == rule.KindCNAME {
		query = r.Forward
	}
	ips, err := s.resolver.Resolve(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("selector: resolve %s: %w", query, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("selector: no usable candidates for %s (via %s)", domain, query)
	}
	return ips, nil
}

func (s *Selector) findIn(cands []candidate, ip net.IP) (candidate, bool) {
	for _, c := range cands {
		if c.ip.Equal(ip) {
			return c, true
		}
	}
	return candidate{}, false
}

func sinkRank(c candidate) int {
	if c.failure >= sinkThreshold {
		return 1
	}
	return 0
}
