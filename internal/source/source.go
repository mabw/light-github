// Package source 提供可插拔加速规则数据源：steampp（主）/ GitHub520（备）/ 内置清单（兜底），
// 以及多源合并、失败降级与本地缓存（docs/DESIGN.md §3.3）。
package source

import (
	"context"

	"github.com/marvin/light-github/internal/rule"
)

// Fetcher 单个数据源。
type Fetcher interface {
	Name() string
	Fetch(ctx context.Context) ([]rule.Rule, error)
}

// Steampp steampp 官方加速配置源。
type Steampp struct{ baseURL string }

// NewSteampp 构造 steampp 源。
func NewSteampp(baseURL string) *Steampp { return &Steampp{baseURL: baseURL} }

// Name implements Fetcher.
func (s *Steampp) Name() string { return "steampp" }

// Fetch implements Fetcher. TODO: TDD RED 骨架.
func (s *Steampp) Fetch(ctx context.Context) ([]rule.Rule, error) { return nil, nil }

// GitHub520 hosts.json 源。
type GitHub520 struct{ baseURL string }

// NewGitHub520 构造 GitHub520 源。
func NewGitHub520(baseURL string) *GitHub520 { return &GitHub520{baseURL: baseURL} }

// Name implements Fetcher.
func (s *GitHub520) Name() string { return "github520" }

// Fetch implements Fetcher. TODO: TDD RED 骨架.
func (s *GitHub520) Fetch(ctx context.Context) ([]rule.Rule, error) { return nil, nil }

// Builtin 编译期内置清单（兜底）。
type Builtin struct{}

// NewBuiltin 构造内置源。
func NewBuiltin() *Builtin { return &Builtin{} }

// Name implements Fetcher.
func (b *Builtin) Name() string { return "builtin" }

// Fetch implements Fetcher. TODO: TDD RED 骨架.
func (b *Builtin) Fetch(ctx context.Context) ([]rule.Rule, error) { return nil, nil }

// Manager 多源管理：按优先级合并、失败降级、本地缓存。
type Manager struct {
	sources []Fetcher

	// CachePath 规则缓存落盘路径（JSON）。
	CachePath string
}

// NewManager 构造管理器；sources 按优先级降序排列。
func NewManager(sources ...Fetcher) *Manager { return &Manager{sources: sources} }

// Refresh 拉取全部可用源并合并写缓存。TODO: TDD RED 骨架.
func (m *Manager) Refresh(ctx context.Context) ([]rule.Rule, error) { return nil, nil }

// Load 优先 Refresh；全源失败时回退本地缓存。TODO: TDD RED 骨架.
func (m *Manager) Load(ctx context.Context) ([]rule.Rule, error) { return nil, nil }
