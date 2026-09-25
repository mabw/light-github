// Inspect：测速状态观测（Web UI 规则页数据形状）。
package selector

import (
	"sort"
	"time"
)

// CandidateInfo 单个候选 IP 的状态。
type CandidateInfo struct {
	IP      string `json:"ip"`
	CostMS  int64  `json:"costMs"`  // 测速中位耗时
	Failure int    `json:"failure"` // 连续失败次数
	Sunk    bool   `json:"sunk"`    // 已沉底（连续失败 ≥ 阈值）
}

// DomainInfo 单域名的探测状态。
type DomainInfo struct {
	Domain     string          `json:"domain"`
	ProbedAt   time.Time       `json:"probedAt"`
	Candidates []CandidateInfo `json:"candidates"`
}

// Inspect 返回全部已探测域名的状态快照（按域名升序，UI 稳定展示）。
func (s *Selector) Inspect() []DomainInfo {
	s.mu.Lock()
	domains := make([]string, 0, len(s.states))
	for d := range s.states {
		domains = append(domains, d)
	}
	states := make([]*domainState, len(domains))
	for i, d := range domains {
		states[i] = s.states[d]
	}
	s.mu.Unlock()
	sort.Strings(domains)

	out := make([]DomainInfo, 0, len(domains))
	for i, st := range states {
		st.mu.Lock()
		if st.probedAt.IsZero() { // Preload 进行中尚未完成的分片
			st.mu.Unlock()
			continue
		}
		cands := make([]CandidateInfo, len(st.candidates))
		for j, c := range st.candidates {
			cands[j] = CandidateInfo{
				IP:      c.ip.String(),
				CostMS:  c.cost.Milliseconds(),
				Failure: c.failure,
				Sunk:    sinkRank(c) == 1,
			}
		}
		info := DomainInfo{Domain: domains[i], ProbedAt: st.probedAt, Candidates: cands}
		st.mu.Unlock()
		out = append(out, info)
	}
	return out
}
