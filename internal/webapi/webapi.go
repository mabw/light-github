// Package webapi 管理控制台 HTTP 端点：状态/统计/日志/规则/配置/PAC
// （docs/DESIGN.md M2；与代理共用监听端口，由 proxy 层路由进来）。
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marvin/light-github/internal/config"
	"github.com/marvin/light-github/internal/logx"
	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
	"github.com/marvin/light-github/internal/selector"
	"github.com/marvin/light-github/internal/source"
)

// Deps 端点依赖（cmd 组装注入；函数字段为 nil 时对应操作返回 503）。
type Deps struct {
	Version   string
	StartedAt time.Time
	Addr      string // 代理监听地址（PAC/指引展示用）
	Token     string // 非空时 /api/* 需要凭据（?token= 或 Bearer）

	Metrics *metrics.Store
	Logs    *logx.Manager
	Source  *source.Manager
	Selector *selector.Selector

	Rules func() []rule.Rule // 当前规则快照

	Config     config.Config
	ConfigPath string

	// OnConfigChange 配置热应用（logLevel/refresh）；addr/token 需重启，由 UI 提示
	OnConfigChange func(config.Config)
	// RefreshRules 手动触发规则刷新，返回新规则数
	RefreshRules func(ctx context.Context) (int, error)
}

// envelope 统一响应信封（全局 API 规范：success/data/error）。
type envelope struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Handler 返回管理端点 mux（挂 /api/* 与 /pac）。
func Handler(d *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/stats", d.handleStats)
	mux.HandleFunc("/api/logs", d.handleLogs)
	mux.HandleFunc("/api/rules", d.handleRules)
	mux.HandleFunc("/api/rules/refresh", d.handleRefresh)
	mux.HandleFunc("/api/config", d.handleConfig)
	mux.HandleFunc("/pac", d.handlePAC)
	return d.auth(mux)
}

// ---- 鉴权 ----

// auth Token 非空时保护 /api/*（静态页与 PAC 放开：不含敏感数据，
// 页面 JS 会把 ?token= 透传给 API 请求）。
func (d Deps) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Token != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			token := r.URL.Query().Get("token")
			if token == "" {
				if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
					token = strings.TrimPrefix(ah, "Bearer ")
				}
			}
			if token != d.Token {
				writeJSON(w, http.StatusUnauthorized, envelope{Success: false, Error: "需要 token"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 端点 ----

func (d Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	src := d.Source.Status()
	ruleCount := 0
	if d.Rules != nil {
		ruleCount = len(d.Rules())
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{
		"version":     d.Version,
		"uptime":      time.Since(d.StartedAt).String(),
		"addr":        d.Addr,
		"ruleCount":   ruleCount,
		"lastRefresh": src.LastRefreshAt.Format(time.RFC3339),
		"lastOk":      src.LastOK,
		"sources":     src.Sources,
	}})
}

func (d Deps) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: d.Metrics.Snapshot()})
}

// logItem 带通道标记的日志条目（all 合并视图用）。
type logItem struct {
	Kind string `json:"kind"` // app | conn
	logx.Entry
}

func (d Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "all"
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	switch kind {
	case logx.KindApp, logx.KindConn:
		entries := d.Logs.Recent(kind, limit)
		items := make([]logItem, len(entries))
		for i, e := range entries {
			items[i] = logItem{Kind: kind, Entry: e}
		}
		writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"entries": items}})
	case "all":
		app := d.Logs.Recent(logx.KindApp, limit)
		conn := d.Logs.Recent(logx.KindConn, limit)
		items := make([]logItem, 0, len(app)+len(conn))
		for _, e := range app {
			items = append(items, logItem{Kind: logx.KindApp, Entry: e})
		}
		for _, e := range conn {
			items = append(items, logItem{Kind: logx.KindConn, Entry: e})
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
		if len(items) > limit {
			items = items[:limit]
		}
		writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"entries": items}})
	default:
		writeJSON(w, http.StatusBadRequest, envelope{Error: "kind 须为 all/app/conn"})
	}
}

func (d Deps) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	var rules []rule.Rule
	if d.Rules != nil {
		rules = d.Rules()
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{
		"rules":   rules,
		"domains": d.Selector.Inspect(),
	}})
}

func (d Deps) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	if d.RefreshRules == nil {
		writeJSON(w, http.StatusServiceUnavailable, envelope{Error: "刷新回调未配置"})
		return
	}
	count, err := d.RefreshRules(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, envelope{Success: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"count": count}})
}

func (d Deps) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, envelope{Success: true, Data: d.Config})
	case http.MethodPost:
		d.postConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
	}
}

// postConfig 部分更新：请求中出现的字段覆盖现值，其余保留；合法即落盘并回调热应用。
func (d Deps) postConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr     *string `json:"addr"`
		Token    *string `json:"token"`
		Refresh  *string `json:"refresh"`
		LogLevel *string `json:"logLevel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, envelope{Error: "body 解析失败"})
		return
	}

	next := d.Config
	if req.Addr != nil {
		if _, _, err := net.SplitHostPort(*req.Addr); err != nil {
			writeJSON(w, http.StatusBadRequest, envelope{Error: "addr 格式应为 host:port"})
			return
		}
		next.Addr = *req.Addr
	}
	if req.Token != nil {
		next.Token = *req.Token
	}
	if req.Refresh != nil {
		dur, err := time.ParseDuration(*req.Refresh)
		if err != nil || dur < time.Minute {
			writeJSON(w, http.StatusBadRequest, envelope{Error: "refresh 须为 ≥1m 的时长"})
			return
		}
		next.Refresh = dur
	}
	if req.LogLevel != nil {
		switch *req.LogLevel {
		case "debug", "info", "warn", "error":
			next.LogLevel = *req.LogLevel
		default:
			writeJSON(w, http.StatusBadRequest, envelope{Error: "logLevel 须为 debug/info/warn/error"})
			return
		}
	}

	if d.ConfigPath != "" {
		if err := config.Save(d.ConfigPath, next); err != nil {
			writeJSON(w, http.StatusInternalServerError, envelope{Error: "写盘失败: " + err.Error()})
			return
		}
	}
	if d.OnConfigChange != nil {
		d.OnConfigChange(next)
	}
	needRestart := next.Addr != d.Config.Addr || next.Token != d.Config.Token
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"needRestart": needRestart}})
}

// handlePAC 生成 PAC（白名单经代理，其余 DIRECT；接入指引页展示）。
func (d Deps) handlePAC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	var patterns []string
	if d.Rules != nil {
		for _, rl := range d.Rules() {
			patterns = append(patterns, rl.Domain)
		}
	}

	var b strings.Builder
	b.WriteString("// light-github PAC——白名单走本地代理，其余直连\n")
	b.WriteString("function FindProxyForURL(url, host) {\n")
	for _, p := range patterns {
		fmt.Fprintf(&b, "  if (shExpMatch(host, %q)) return %q;\n", p, "PROXY "+d.Addr)
	}
	b.WriteString("  return \"DIRECT\";\n}\n")

	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	_, _ = w.Write([]byte(b.String()))
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, e envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(e)
}
