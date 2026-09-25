// Package rule 提供加速规则模型、域名匹配与 steampp 字段归一化。
package rule

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
func Normalize(domain, forward, fakeSNI string) Rule {
	return Rule{} // TODO: TDD RED 骨架
}

// Table 规则表，支持精确与 *.suffix 通配匹配。
type Table struct{}

// NewTable 构建规则表。
func NewTable(rules []Rule) *Table { return &Table{} }

// Match 返回域名命中的规则；精确规则优先于通配规则。
func (t *Table) Match(domain string) (Rule, bool) { return Rule{}, false }
