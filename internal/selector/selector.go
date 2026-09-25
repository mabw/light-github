// Package selector 出站选择器：按规则产候选 → 测速排序 → 失败统计自动切换。
package selector

import (
	"context"
	"net"
	"time"

	"github.com/marvin/light-github/internal/rule"
)

// Resolver 域名 → IP 候选来源（DoH / CNAME 通道共用）。
type Resolver interface {
	Resolve(ctx context.Context, domain string) ([]net.IP, error)
}

// Prober 单 IP 出口质量探测（TCP 建连中位耗时；失败返回惩罚值）。
type Prober interface {
	Probe(ctx context.Context, ip net.IP) time.Duration
}

// Selector 出站选择器。
type Selector struct{}

// New 构造选择器。TODO: TDD RED 骨架。
func New(tbl *rule.Table, resolver Resolver, prober Prober, cacheTTL time.Duration) *Selector {
	return &Selector{}
}

// Pick 返回 domain 的出口候选 IP（按优先级排序，拨号方逐个尝试）。
func (s *Selector) Pick(ctx context.Context, domain string) ([]net.IP, error) {
	return nil, nil
}

// ReportFailure 上报某 IP 连接失败（连续失败即沉底）。
func (s *Selector) ReportFailure(domain string, ip net.IP) {}

// ReportSuccess 上报连接成功（重置失败计数）。
func (s *Selector) ReportSuccess(domain string, ip net.IP) {}
