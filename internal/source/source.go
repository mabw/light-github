// Package source 提供可插拔加速规则数据源：steampp（主）/ GitHub520（备）/ 内置清单（兜底），
// 以及多源合并、失败降级与本地缓存（docs/DESIGN.md §3.3）。
package source

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/marvin/light-github/internal/rule"
)

// 与官方客户端一致的请求特征（ApiConstants.GetReferrer 格式），保持克制频率（≥1h）
const (
	steamppUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36"
	steamppReferr = "spp://macOS/3.1.10001.0"
)

// Fetcher 单个数据源。
type Fetcher interface {
	Name() string
	Fetch(ctx context.Context) ([]rule.Rule, error)
}

// ---- steampp 源 ----

// steampp DTO（仅关心字段；顶层为 emoji 键混淆，其中仅一个是分组数组——spike-c 实证）
type steamppProject struct {
	Name               string           `json:"Name"`
	MatchDomainNames   string           `json:"MatchDomainNames"`
	ListenDomainNames  string           `json:"ListenDomainNames"`
	ForwardDomainNames string           `json:"ForwardDomainNames"`
	FakeServerName     string           `json:"FakeServerName"`
	ProxyType          int              `json:"ProxyType"`
	Items              []steamppProject `json:"Items"`
}

type steamppGroup struct {
	Name  string           `json:"Name"`
	Items []steamppProject `json:"Items"`
}

// Steampp steampp 官方加速配置源。
type Steampp struct {
	baseURL string
	hc      *http.Client
}

// NewSteampp 构造 steampp 源。
func NewSteampp(baseURL string) *Steampp {
	return &Steampp{baseURL: baseURL, hc: &http.Client{Timeout: 10 * time.Second}}
}

// Name implements Fetcher.
func (s *Steampp) Name() string { return "steampp" }

// Fetch implements Fetcher. 只提取 Github 分组（本工具的单一场景）。
func (s *Steampp) Fetch(ctx context.Context) ([]rule.Rule, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", steamppUA)
	req.Header.Set("Referer", steamppReferr)

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steampp: status %d", resp.StatusCode)
	}

	raw := map[string]json.RawMessage{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("steampp: decode: %w", err)
	}

	var groups []steamppGroup
	for _, v := range raw {
		if len(v) > 0 && v[0] == '[' { // 只解析数组形态的顶层键（🦓）
			var gs []steamppGroup
			if json.Unmarshal(v, &gs) == nil {
				groups = append(groups, gs...)
			}
		}
	}

	var rules []rule.Rule
	for _, g := range groups {
		if !strings.Contains(strings.ToLower(g.Name), "github") {
			continue
		}
		collectSteampp(g.Items, &rules)
	}
	if len(rules) == 0 {
		return nil, errors.New("steampp: github group not found")
	}
	return rules, nil
}

func collectSteampp(items []steamppProject, out *[]rule.Rule) {
	for _, it := range items {
		domains := splitNonURL(it.MatchDomainNames)
		if len(domains) == 0 {
			domains = splitNonURL(it.ListenDomainNames)
		}
		for _, d := range domains {
			*out = append(*out, rule.Normalize(d, it.ForwardDomainNames, it.FakeServerName))
		}
		collectSteampp(it.Items, out)
	}
}

// splitNonURL 分号分隔；跳过带 scheme 的条目（如 "https://pipelines.actions..."）
func splitNonURL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" && !strings.Contains(p, "://") {
			out = append(out, p)
		}
	}
	return out
}

// ---- GitHub520 源 ----

// GitHub520 hosts.json 源（[["ip","domain"],...]）。
type GitHub520 struct {
	baseURL string
	hc      *http.Client
}

// NewGitHub520 构造 GitHub520 源。
func NewGitHub520(baseURL string) *GitHub520 {
	return &GitHub520{baseURL: baseURL, hc: &http.Client{Timeout: 10 * time.Second}}
}

// Name implements Fetcher.
func (s *GitHub520) Name() string { return "github520" }

// Fetch implements Fetcher. 全部条目归一化为 FixedIP（剔除回环等不可用 IP）。
func (s *GitHub520) Fetch(ctx context.Context) ([]rule.Rule, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github520: status %d", resp.StatusCode)
	}

	var pairs [][2]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pairs); err != nil {
		return nil, fmt.Errorf("github520: decode: %w", err)
	}
	var rules []rule.Rule
	for _, p := range pairs {
		if len(p) != 2 {
			continue
		}
		if ip := net.ParseIP(p[0]); ip == nil || ip.IsLoopback() || ip.IsPrivate() {
			continue
		}
		rules = append(rules, rule.Rule{Domain: strings.ToLower(p[1]), Kind: rule.KindFixedIP, Forward: p[0]})
	}
	if len(rules) == 0 {
		return nil, errors.New("github520: empty result")
	}
	return rules, nil
}

// ---- 内置清单 ----

//go:embed builtin-rules.json
var builtinData []byte

// Builtin 编译期内置清单（兜底）。
type Builtin struct{}

// NewBuiltin 构造内置源。
func NewBuiltin() *Builtin { return &Builtin{} }

// Name implements Fetcher.
func (b *Builtin) Name() string { return "builtin" }

// Fetch implements Fetcher.
func (b *Builtin) Fetch(context.Context) ([]rule.Rule, error) {
	var doc struct {
		Rules []rule.Rule `json:"rules"`
	}
	if err := json.Unmarshal(builtinData, &doc); err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	if len(doc.Rules) == 0 {
		return nil, errors.New("builtin: empty")
	}
	return doc.Rules, nil
}

// ---- Manager ----

// Manager 多源管理：按优先级合并、失败降级、本地缓存。
type Manager struct {
	sources []Fetcher

	// CachePath 规则缓存落盘路径（JSON）；空则不落盘。
	CachePath string
}

// NewManager 构造管理器；sources 按优先级降序排列（靠前的同名规则胜出）。
func NewManager(sources ...Fetcher) *Manager { return &Manager{sources: sources} }

// fetchResult 单源拉取结果（order 为源优先级序号）
type fetchResult struct {
	order int
	rules []rule.Rule
}

// Refresh 并发拉取全部源，合并为单一规则表并写缓存。
// 任一源成功即视为成功（部分失败被容忍），全源失败返回错误。
func (m *Manager) Refresh(ctx context.Context) ([]rule.Rule, error) {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []fetchResult
	)

	for i, src := range m.sources {
		wg.Add(1)
		go func(order int, src Fetcher) {
			defer wg.Done()
			rules, err := src.Fetch(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				return
			}
			results = append(results, fetchResult{order: order, rules: rules})
		}(i, src)
	}
	wg.Wait()

	if len(results) == 0 {
		return nil, errors.New("source: all sources failed")
	}

	// 按源优先级顺序合并：先到者的域名占位，后来者仅补充新域名
	merged := map[string]rule.Rule{}
	var order []string
	sortResults(results)
	for _, res := range results {
		for _, r := range res.rules {
			if _, exists := merged[r.Domain]; !exists {
				merged[r.Domain] = r
				order = append(order, r.Domain)
			}
		}
	}
	rules := make([]rule.Rule, 0, len(order))
	for _, d := range order {
		rules = append(rules, merged[d])
	}

	if m.CachePath != "" {
		if b, err := json.Marshal(rules); err == nil {
			_ = os.WriteFile(m.CachePath, b, 0o600)
		}
	}
	return rules, nil
}

// Load 优先 Refresh；全源失败时回退本地缓存（rules.json，Refresh 成功时写入）。
func (m *Manager) Load(ctx context.Context) ([]rule.Rule, error) {
	rules, err := m.Refresh(ctx)
	if err == nil {
		return rules, nil
	}
	if m.CachePath != "" {
		if b, rerr := os.ReadFile(m.CachePath); rerr == nil {
			var cached []rule.Rule
			if jerr := json.Unmarshal(b, &cached); jerr == nil && len(cached) > 0 {
				return cached, nil
			}
		}
	}
	return nil, err
}

// sortResults 按源序稳定排序（并发完成顺序不确定）
func sortResults(rs []fetchResult) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].order < rs[j-1].order; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}
