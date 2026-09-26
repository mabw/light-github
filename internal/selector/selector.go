// Package selector 出站选择器：按规则产候选 → 测速排序 → 失败统计自动切换。
//
// 候选生成策略（docs/DESIGN.md §3.2）：
//   - FixedIP：固定 IP 排首位，DoH 解析原域名作补充候选（spike-a 实训：单 IP 不可靠）
//   - CNAME：DoH 解析 Forward 域名（官方防污染 CNAME 通道）
//   - Dynamic / 未匹配：DoH 解析原域名
//
// 失败统计借鉴 dev-sidecar 的 DynamicChoice：连续失败达阈值的 IP 沉底（不删除，
// 保留为最后候选），连接成功即重置计数。
//
// 并发模型（DEBT-4）：锁按域名分片——states map 仅短锁取 state，测速等网络 IO
// 在 state 级锁内进行，不同域名的 Pick 互不阻塞。
package selector

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mabw/light-github/internal/rule"
	"github.com/mabw/light-github/internal/safego"
)

// sinkThreshold 连续失败达到此次数的候选沉底
const sinkThreshold = 2

// 借段兜底（M5-6）：主候选 ≤ borrowThreshold 个时，从表内其他 FixedIP
// 借最多 maxBorrowed 个补充。实测场景：数据源对 github.com 只给单一官方段
// （steampp/GitHub520/国内 DoH 全给 20.x），段级封锁时主候选全灭，而
// GitHub520 给其他 GitHub 域的 140.82 段完好——GitHub 前端段服务全域，
// TLS 端到端握手保证连错 IP 也不出安全问题（测速即淘汰）。
const (
	borrowThreshold = 2
	maxBorrowed     = 6
)

// Resolver 域名 → IP 候选来源（DoH / CNAME 通道共用）。
type Resolver interface {
	Resolve(ctx context.Context, domain string) ([]net.IP, error)
}

// Prober 单 IP 出口质量探测（TCP 建连中位耗时 + TLS 握手验证；失败返回惩罚值）。
type Prober interface {
	Probe(ctx context.Context, domain string, ip net.IP) time.Duration
}

type candidate struct {
	ip      net.IP
	cost    time.Duration // 测速中位耗时
	failure int           // 连续失败次数
}

type domainState struct {
	mu          sync.Mutex // DEBT-4：域名级分片锁（测速期间仅阻塞同域名）
	probedAt    time.Time
	dirty       bool // 需重建（失败上报/规则换表置位）；不清 probedAt——Inspect 在重建前仍可展示旧数据
	generations int  // 已重建次数：首个缓存用 firstTTL（短），第二个起用 steadyTTL（Watt 10s/100s 语义）
	candidates  []candidate
}

// expired 选路结果是否过期（M5 §2.5 生命周期回收）：dirty 立即过期；
// 否则首轮 firstTTL、之后 steadyTTL——任何「已建连但变坏」的候选
// 上界 steadyTTL 后自然淘汰。
func (st *domainState) expired(now time.Time, firstTTL, steadyTTL time.Duration) bool {
	if st.dirty || st.probedAt.IsZero() {
		return true
	}
	ttl := firstTTL
	if st.generations >= 2 {
		ttl = steadyTTL
	}
	return now.Sub(st.probedAt) >= ttl
}

// Selector 出站选择器。并发安全。
type Selector struct {
	tbl       *rule.Table
	resolver  Resolver
	prober    Prober
	firstTTL  time.Duration
	steadyTTL time.Duration

	tblMu sync.RWMutex // 规则表热更新（数据源刷新时 SetTable 换入）

	mu     sync.Mutex // 仅保护 states map 的读写
	states map[string]*domainState
}

// New 构造选择器。firstTTL 为首轮选路有效期（设计值 10s——启动/预热时
// 网络异常的快速自我纠正），steadyTTL 为稳态周期（设计值 100s——
// 坏候选的淘汰上界，替代「失败摘除状态机」，调研 §3 采纳 #2）。
func New(tbl *rule.Table, resolver Resolver, prober Prober, firstTTL, steadyTTL time.Duration) *Selector {
	return &Selector{
		tbl:       tbl,
		resolver:  resolver,
		prober:    prober,
		firstTTL:  firstTTL,
		steadyTTL: steadyTTL,
		states:    map[string]*domainState{},
	}
}

// SetTable 原子替换规则表并清空测速缓存时间戳（策略已变，
// 各域名下次 Pick 重建候选；failure 记忆与旧候选保留供兜底）。
//
// 锁序（review H1）：先在全局锁内收集分片快照并立即释放，再逐分片清——
// 分片锁内是网络 IO（DoH+测速，秒级），若持全局锁逐域等分片锁，
// 等待期间所有域名的 stateFor/Pick 都会被拖住。
func (s *Selector) SetTable(t *rule.Table) {
	s.tblMu.Lock()
	s.tbl = t
	s.tblMu.Unlock()

	s.mu.Lock()
	states := make([]*domainState, 0, len(s.states))
	for _, st := range s.states {
		states = append(states, st)
	}
	s.mu.Unlock()

	for _, st := range states {
		st.mu.Lock()
		st.dirty = true
		st.mu.Unlock()
	}
}

// currentTable 返回当前规则表
func (s *Selector) currentTable() *rule.Table {
	s.tblMu.RLock()
	defer s.tblMu.RUnlock()
	return s.tbl
}

// stateFor 取或建域名的分片状态（map 短锁）
func (s *Selector) stateFor(domain string) *domainState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[domain]
	if !ok {
		st = &domainState{}
		s.states[domain] = st
	}
	return st
}

// Pick 返回 domain 的出口候选 IP（按优先级排序，拨号方逐个尝试）。
// 测速结果缓存至 TTL 过期；连续失败的 IP 沉底。
func (s *Selector) Pick(ctx context.Context, domain string) ([]net.IP, error) {
	st := s.stateFor(domain)
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.expired(time.Now(), s.firstTTL, s.steadyTTL) {
		cands, err := s.buildCandidates(ctx, domain, st)
		if err != nil && len(cands) == 0 {
			return nil, err
		}
		st.candidates, st.probedAt, st.dirty = cands, time.Now(), false
		st.generations++
	}

	ordered := make([]candidate, len(st.candidates))
	copy(ordered, st.candidates)
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

// Preload 并发预热域名测速缓存（DEBT-3：让首请求命中缓存而非同步测速）。
// concurrency 限制对 DoH 端点与目标网络的并发压力，默认 4。
func (s *Selector) Preload(ctx context.Context, domains []string, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, d := range domains {
		wg.Add(1)
		d := d
		safego.Go("preload-domain", nil, func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			_, _ = s.Pick(ctx, d)
		})
	}
	wg.Wait()
}

// ReportFailure 上报某 IP 连接失败：连续失败达阈值后沉底；
// 并立即失效该域名的选路缓存（M5：下次 Pick 重建重测——封锁期
// 死 IP 不再等到 TTL 自然过期才出局，两次断网实测的 150s 拨号主因）。
func (s *Selector) ReportFailure(domain string, ip net.IP) {
	st := s.stateFor(domain)
	st.mu.Lock()
	defer st.mu.Unlock()
	if c := findCandidate(st.candidates, ip); c != nil {
		c.failure++
	}
	st.dirty = true // 下次 Pick 重建重测；不清 probedAt（Inspect 重建前仍展示旧候选）
}

// ReportSuccess 上报连接成功（重置失败计数）。
func (s *Selector) ReportSuccess(domain string, ip net.IP) {
	st := s.stateFor(domain)
	st.mu.Lock()
	defer st.mu.Unlock()
	if c := findCandidate(st.candidates, ip); c != nil {
		c.failure = 0
	}
}

func findCandidate(cands []candidate, ip net.IP) *candidate {
	for i := range cands {
		if cands[i].ip.Equal(ip) {
			return &cands[i]
		}
	}
	return nil
}

// buildCandidates 生成候选集并测速；resolver 失败但存在历史候选时沿用旧候选。
func (s *Selector) buildCandidates(ctx context.Context, domain string, st *domainState) ([]candidate, error) {
	ips, resolveErr := s.candidateIPs(ctx, domain)

	// resolve 失败：有旧候选则降级沿用（DoH 短暂抖动不应击穿缓存）
	if resolveErr != nil {
		if len(st.candidates) > 0 {
			return st.candidates, nil
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
		i, ip := i, ip
		safego.Go("probe", nil, func() {
			defer wg.Done()
			cands[i] = candidate{ip: ip, cost: s.prober.Probe(ctx, domain, ip)}
		})
	}
	wg.Wait()

	// 沿用历史失败计数（测速刷新不应清零运行期失败记忆）
	for i := range cands {
		if old := findCandidate(st.candidates, cands[i].ip); old != nil {
			cands[i].failure = old.failure
		}
	}
	return cands, nil
}

// candidateIPs 按规则策略产出原始候选（可能为空 + err）。
func (s *Selector) candidateIPs(ctx context.Context, domain string) ([]net.IP, error) {
	matched := s.currentTable().MatchAll(domain)

	// 全部同名 FixedIP 依源优先级入候选（M5-4：多源并集），
	// 后接 DoH 解析补充
	var primary []net.IP
	if len(matched) > 0 && matched[0].Kind == rule.KindFixedIP {
		for _, r := range matched {
			if ip := net.ParseIP(r.Forward); ip != nil {
				primary = append(primary, ip)
			}
		}
		if len(primary) > 0 {
			supplement, _ := s.resolver.Resolve(ctx, domain)
			primary = append(primary, supplement...)
		}
	}
	if primary == nil { // 非 FixedIP：CNAME 通道或 Dynamic 均解析得候选
		query := domain
		if len(matched) > 0 && matched[0].Kind == rule.KindCNAME {
			query = matched[0].Forward
		}
		ips, err := s.resolver.Resolve(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("selector: resolve %s: %w", query, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("selector: no usable candidates for %s (via %s)", domain, query)
		}
		primary = ips
	}

	// 去重（DoH 补充常与数据源 FixedIP 同 IP——含重复计数会让借段判定虚高，
	// 实测：github.com 的 DoH 与 GitHub520 同给 20.205.243.166，len=3 误判充足）
	seen := make(map[string]bool, len(primary))
	deduped := make([]net.IP, 0, len(primary))
	for _, ip := range primary {
		if !seen[ip.String()] {
			seen[ip.String()] = true
			deduped = append(deduped, ip)
		}
	}

	// 借段兜底：主候选稀缺（去重后）时补充同根域家族的 FixedIP
	if len(deduped) <= borrowThreshold {
		deduped = append(deduped, s.borrowedIPs(domain, deduped)...)
	}
	return deduped, nil
}

// borrowedIPs 表内**同根域家族**其他 FixedIP（原始规则序，去重已有，≤maxBorrowed 个）。
// 家族限定（registrable domain 后两段相等）：github.com 只借 *.github.com 的 IP
// （alive/central/collector 等 140.82 段，同前端服务主站）；跨服务域不借——
// 实测 github.dev(20.43)/hub.docker(54.208)/github.io(185.199) 的 IP 虽然
// TLS 可握手甚至证书匹配（*.github.com 泛证书），应用层回 400/403
// （GitHub 边缘按 IP 分工路由，"Whoa there!" 页面即此）。
func (s *Selector) borrowedIPs(domain string, have []net.IP) []net.IP {
	family := rootDomain(domain)
	seen := make(map[string]bool, len(have))
	for _, ip := range have {
		seen[ip.String()] = true
	}
	var out []net.IP
	for _, r := range s.currentTable().AllRules() {
		if r.Kind != rule.KindFixedIP || rootDomain(r.Domain) != family {
			continue
		}
		ip := net.ParseIP(r.Forward)
		if ip == nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		out = append(out, ip)
		if len(out) >= maxBorrowed {
			break
		}
	}
	return out
}

// rootDomain 根域（后两段）：alive.github.com → github.com。
// 简化判定对本工具的域集合足够（github.com/githubusercontent.com 等
// 均两段根域）；公后缀（com.cn 类）不存在于加速清单。
func rootDomain(d string) string {
	parts := strings.Split(d, ".")
	if len(parts) <= 2 {
		return d
	}
	return parts[len(parts)-2] + "." + parts[len(parts)-1]
}

func sinkRank(c candidate) int {
	if c.failure >= sinkThreshold {
		return 1
	}
	return 0
}
