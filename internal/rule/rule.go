// Package rule 提供加速规则模型、域名匹配与 steampp 字段归一化。
package rule

import (
	"net"
	"strings"
)

// Kind 出站策略类型
type Kind string

const (
	// KindFixedIP 固定出口 IP（连接时 SNI 保持原域名）
	KindFixedIP Kind = "FixedIP"
	// KindCNAME DNS 解析通道：解析时改查 Forward 域名（官方防污染 CNAME 链）
	KindCNAME Kind = "CNAME"
	// KindDynamic DoH 动态解析 + 测速选优
	KindDynamic Kind = "Dynamic"
)

// Rule 单条加速规则
type Rule struct {
	Domain  string // 精确域名或 *.suffix 通配
	Kind    Kind
	Forward string // FixedIP: IP；CNAME: 查询域名；Dynamic: 空
}

// Normalize 把 steampp 的 (domain, forward, fakeSNI) 三元组归一化为安全策略。
//
// 降级规则（spike 实证，见 docs/SPIKE-RESULT.md）：
//   - forward 为 http(s):// scheme：官方带宽真中转（mossimo.top 类），不使用 → Dynamic
//   - fakeSNI 非空：纯隧道无法改写客户端 SNI → Dynamic
//   - forward 为回环/私有 IP：hosts 可能被其他工具劫持 → Dynamic
func Normalize(domain, forward, fakeSNI string) Rule {
	r := Rule{Domain: normalizeDomain(domain), Kind: KindDynamic}
	forward = strings.TrimSpace(forward)

	if fakeSNI != "" || forward == "" || strings.Contains(forward, "://") {
		return r // Dynamic
	}

	if ip := net.ParseIP(forward); ip != nil {
		if isUsableIP(ip) {
			r.Kind, r.Forward = KindFixedIP, forward
		}
		return r // 不可用 IP → Dynamic
	}

	if strings.EqualFold(strings.TrimSuffix(forward, "."), strings.TrimSuffix(r.Domain, ".")) {
		return r // 自身 → Dynamic
	}
	r.Kind, r.Forward = KindCNAME, forward
	return r
}

// Table 规则表，支持精确与 *.suffix 通配匹配。
type Table struct {
	exact map[string][]Rule
	wild  map[string][]Rule
}

// NewTable 构建规则表。重复域名以靠后的规则覆盖靠前的。
func NewTable(rules []Rule) *Table {
	t := &Table{exact: map[string][]Rule{}, wild: map[string][]Rule{}}
	for _, r := range rules {
		d := normalizeDomain(r.Domain)
		r.Domain = d
		if suffix, ok := strings.CutPrefix(d, "*."); ok {
			t.wild[suffix] = append(t.wild[suffix], r)
		} else {
			t.exact[d] = append(t.exact[d], r)
		}
	}
	return t
}

// Match 返回域名命中的规则；精确规则优先于通配规则。
// 输入兼容 host:port 形态并大小写不敏感。
func (t *Table) Match(domain string) (Rule, bool) {
	d := normalizeDomain(domain)
	if host, _, err := net.SplitHostPort(d); err == nil {
		d = normalizeDomain(host)
	}
	if rs, ok := t.exact[d]; ok && len(rs) > 0 {
		return rs[0], true // 首条 = 高优先级源（构建序）
	}
	// 自右向左逐级取后缀查通配表（a.b.suffix → b.suffix → suffix）
	for rest := d; ; {
		idx := strings.IndexByte(rest, '.')
		if idx < 0 {
			break
		}
		rest = rest[idx+1:]
		if rs, ok := t.wild[rest]; ok && len(rs) > 0 {
			return rs[0], true
		}
	}
	return Rule{}, false
}

func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
}

// isUsableCandidate 同款过滤：回环/私有/链路本地/未指定地址不可作出口
func isUsableIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

// MatchAll 返回域名命中的全部规则（同名多源并存，按构建序=源优先级）。
// Match 返回首条维持「高优先级胜出」；候选收集用本方法取全部 FixedIP
// （M5-4：同名 FixedIP 多源并集——单一源全灭时其他源的同域 IP 仍可用）。
func (t *Table) MatchAll(domain string) []Rule {
	d := normalizeDomain(domain)
	if host, _, err := net.SplitHostPort(d); err == nil {
		d = normalizeDomain(host)
	}
	if rs, ok := t.exact[d]; ok {
		return rs
	}
	for rest := d; ; {
		idx := strings.IndexByte(rest, '.')
		if idx < 0 {
			break
		}
		rest = rest[idx+1:]
		if rs, ok := t.wild[rest]; ok {
			return rs
		}
	}
	return nil
}
