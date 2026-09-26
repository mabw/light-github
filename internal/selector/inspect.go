// Inspect：测速状态观测（Web UI 规则页数据形状）。
package selector

import (
	"context"
	"net"
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

// Reprobe 强制对单域名重新测速（忽略 TTL 缓存；Web UI 手动测速按钮）。
// forceRebuild 旁路 M5-7 重建冷却——防抖面向自动 dirty 循环，用户显式
// 操作必须立即生效（回归修复：冷却合入后 5s 内点按钮无效）。
func (s *Selector) Reprobe(ctx context.Context, domain string) ([]net.IP, error) {
	st := s.stateFor(domain)
	st.mu.Lock()
	st.dirty = true        // 使 Pick 视为过期重建（不清 probedAt，保持 Inspect 可见）
	st.forceRebuild = true // 旁路冷却防抖
	st.mu.Unlock()
	return s.Pick(ctx, domain)
}

// Inspect 返回全部已探测域名的状态快照（按域名升序，UI 稳定展示）。
// 域名与状态必须绑定后一起排序——平行数组各自排序会因 map 迭代序错配
// （review C1：域名 A 挂上 B 的候选集，UI/手动测速返回错误数据）。
func (s *Selector) Inspect() []DomainInfo {
	type kv struct {
		name string
		st   *domainState
	}
	s.mu.Lock()
	pairs := make([]kv, 0, len(s.states))
	for d, st := range s.states {
		pairs = append(pairs, kv{d, st})
	}
	s.mu.Unlock()
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	out := make([]DomainInfo, 0, len(pairs))
	for _, p := range pairs {
		st := p.st
		// TryLock（review M6）：探测在途的分片（持锁最长 ~3s）直接跳过
		// 本轮不展示，而不是让 /api/rules 整体等一个慢域名
		if !st.mu.TryLock() {
			continue
		}
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
		info := DomainInfo{Domain: p.name, ProbedAt: st.probedAt, Candidates: cands}
		st.mu.Unlock()
		out = append(out, info)
	}
	return out
}
