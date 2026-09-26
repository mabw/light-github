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
	"sync"
	"time"

	"github.com/marvin/light-github/internal/config"
	"github.com/marvin/light-github/internal/logx"
	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
	"github.com/marvin/light-github/internal/selector"
	"github.com/marvin/light-github/internal/source"
	"github.com/marvin/light-github/internal/webui"
)

// Deps 端点依赖（cmd 组装注入；函数字段为 nil 时对应操作返回 503）。
type Deps struct {
	Version   string
	StartedAt time.Time
	Addr      string // 代理监听地址（PAC/指引展示用）
	Token     string // 非空时 /api/* 需要凭据（?token= 或 Bearer）

	Metrics  *metrics.Store
	Logs     *logx.Manager
	Source   *source.Manager
	Selector *selector.Selector

	Rules func() []rule.Rule // 当前规则快照

	Config     config.Config
	ConfigPath string
	mu         sync.Mutex // 保护 Config：POST 写（UpdateConfig）与 GET/热应用读并发（review C2）

	// OnConfigChange 配置热应用（logLevel/refresh）；addr/token 需重启，由 UI 提示
	OnConfigChange func(config.Config)
	// RefreshRules 手动触发规则刷新，返回新规则数
	RefreshRules func(ctx context.Context) (int, error)

	// 加速开关（托盘与控制台共用）；nil 时端点 503、status 报告 true
	SetAccel     func(on bool)
	AccelEnabled func() bool

	// 系统代理（PAC 一键接入/还原）；nil 时端点 503
	SetSysProxy   func(on bool) error
	SysProxyState func() bool
}

// UpdateConfig 更新内存中的当前配置（cmd 的 OnConfigChange 回调内调用，
// 保证 handler 看到的 Config 与落盘/热应用一致，而非启动快照）。
func (d *Deps) UpdateConfig(c config.Config) {
	d.mu.Lock()
	d.Config = c
	d.mu.Unlock()
}

// CurrentConfig 返回当前配置快照（并发安全）。
func (d *Deps) CurrentConfig() config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Config
}

// envelope 统一响应信封（全局 API 规范：success/data/error）。
type envelope struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Handler 返回完整 Web 服务（/api/*、/pac 与控制台静态页）。
// 该 handler 整体交给 proxy.Server.Web（端口复用）。
func Handler(d *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/stats", d.handleStats)
	mux.HandleFunc("/api/logs", d.handleLogs)
	mux.HandleFunc("/api/rules", d.handleRules)
	mux.HandleFunc("/api/rules/refresh", d.handleRefresh)
	mux.HandleFunc("/api/rules/probe", d.handleProbe)
	mux.HandleFunc("/api/config", d.handleConfig)
	mux.HandleFunc("/api/accel", d.handleAccel)
	mux.HandleFunc("/api/sysproxy", d.handleSysProxy)
	mux.HandleFunc("/pac", d.handlePAC)
	mux.Handle("/", webui.Handler()) // 兜底：控制台单页
	return d.auth(mux)
}

// ---- 鉴权 ----

// auth 两层防线（review H7）：
//  1. 无 token 时 /api/* 仅接受本机 Host——防 DNS rebinding 把恶意页"同源"到
//     127.0.0.1 偷读连接历史/改配置（Firefox/Safari 无 PNA 防护）；
//     有 token 时以鉴权为准（WSL 经宿主 IP 访问的场景不受 Host 白名单约束）
//  2. Token 非空时校验凭据（静态页与 PAC 放开：不含敏感数据，
//     页面 JS 会把 ?token= 透传给 API 请求）
func (d *Deps) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiScope := strings.HasPrefix(r.URL.Path, "/api/")
		if d.Token == "" && apiScope && !isLocalHost(r.Host) {
			writeJSON(w, http.StatusForbidden, envelope{Success: false, Error: "forbidden host"})
			return
		}
		if d.Token != "" && apiScope {
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

// isLocalHost 判断请求头 Host 是否本机（允许带端口与 IPv6 方括号形态）。
func isLocalHost(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// ---- 端点 ----

func (d *Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	src := d.Source.Status()
	ruleCount := 0
	if d.Rules != nil {
		ruleCount = len(d.Rules())
	}
	accelerating := true
	if d.AccelEnabled != nil {
		accelerating = d.AccelEnabled()
	}
	sysProxy := false
	if d.SysProxyState != nil {
		sysProxy = d.SysProxyState()
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{
		"version":      d.Version,
		"uptime":       time.Since(d.StartedAt).String(),
		"addr":         d.Addr,
		"ruleCount":    ruleCount,
		"accelerating": accelerating,
		"sysProxy":     sysProxy,
		"lastRefresh":  src.LastRefreshAt.Format(time.RFC3339),
		"lastOk":       src.LastOK,
		"sources":      src.Sources,
	}})
}

func (d *Deps) handleStats(w http.ResponseWriter, r *http.Request) {
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

func (d *Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
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

func (d *Deps) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	var rules []rule.Rule
	if d.Rules != nil {
		rules = d.Rules()
	}
	var domains []selector.DomainInfo
	if d.Selector != nil { // nil 守卫与其他 Deps 约定一致（review N5）
		domains = d.Selector.Inspect()
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{
		"rules":   rules,
		"domains": domains,
	}})
}

func (d *Deps) handleRefresh(w http.ResponseWriter, r *http.Request) {
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

// handleAccel 加速开关：POST {"on":bool}。关闭后白名单直通、端口与观测保持，
// 配置的代理/PAC 不悬空（托盘「关闭加速」同一入口）。
func (d *Deps) handleAccel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	if d.SetAccel == nil || d.AccelEnabled == nil {
		writeJSON(w, http.StatusServiceUnavailable, envelope{Error: "加速开关未接入"})
		return
	}
	var req struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.On == nil {
		writeJSON(w, http.StatusBadRequest, envelope{Error: `body 须为 {"on":bool}`})
		return
	}
	d.SetAccel(*req.On)
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"accelerating": d.AccelEnabled()}})
}

// handleSysProxy 一键接入/还原系统代理（PAC）。
// 注意与"接管系统代理"类工具（Clash 系统代理等）互斥——后设置者生效。
func (d *Deps) handleSysProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	if d.SetSysProxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, envelope{Error: "系统代理能力未接入"})
		return
	}
	var req struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.On == nil {
		writeJSON(w, http.StatusBadRequest, envelope{Error: `body 须为 {"on":bool}`})
		return
	}
	if err := d.SetSysProxy(*req.On); err != nil {
		writeJSON(w, http.StatusInternalServerError, envelope{Success: false, Error: err.Error()})
		return
	}
	state := false
	if d.SysProxyState != nil {
		state = d.SysProxyState()
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"sysProxy": state}})
}

// handleProbe 单域名强制重测（忽略测速缓存；规则页行内测速按钮）。
func (d *Deps) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
		return
	}
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeJSON(w, http.StatusBadRequest, envelope{Error: "缺少 domain 参数"})
		return
	}
	if d.Selector == nil { // nil 守卫与其他 Deps 约定一致（review N5）
		writeJSON(w, http.StatusServiceUnavailable, envelope{Error: "选择器未接入"})
		return
	}
	if _, err := d.Selector.Reprobe(r.Context(), domain); err != nil {
		writeJSON(w, http.StatusOK, envelope{Success: false, Error: err.Error()})
		return
	}
	// 从 Inspect 快照中取该域名的最新状态（成功后必存在）
	var info *selector.DomainInfo
	for _, di := range d.Selector.Inspect() {
		if di.Domain == domain {
			info = &di
			break
		}
	}
	if info == nil {
		writeJSON(w, http.StatusOK, envelope{Success: false, Error: "测速完成但未找到结果"})
		return
	}
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: info})
}

func (d *Deps) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, envelope{Success: true, Data: d.CurrentConfig()})
	case http.MethodPost:
		d.postConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: "method"})
	}
}

// postConfig 部分更新：请求中出现的字段覆盖现值，其余保留；合法即落盘、
// 同步内存基线（防下一次 POST 基于启动快照回滚本次修改）并回调热应用。
func (d *Deps) postConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr         *string `json:"addr"`
		Token        *string `json:"token"`
		Refresh      *string `json:"refresh"`
		LogLevel     *string `json:"logLevel"`
		AutoSysProxy *bool   `json:"autoSysProxy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, envelope{Error: "body 解析失败"})
		return
	}

	next := d.CurrentConfig() // 基线取当前值而非启动快照（review C2：否则下次 POST 回滚上次修改）
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
	if req.AutoSysProxy != nil {
		next.AutoSysProxy = *req.AutoSysProxy
	}

	if d.ConfigPath != "" {
		if err := config.Save(d.ConfigPath, next); err != nil {
			writeJSON(w, http.StatusInternalServerError, envelope{Error: "写盘失败: " + err.Error()})
			return
		}
	}
	prev := d.CurrentConfig()
	d.UpdateConfig(next) // 内存基线同步，先于回调（回调侧可信任 deps.CurrentConfig）
	if d.OnConfigChange != nil {
		d.OnConfigChange(next)
	}
	needRestart := next.Addr != prev.Addr || next.Token != prev.Token
	writeJSON(w, http.StatusOK, envelope{Success: true, Data: map[string]any{"needRestart": needRestart}})
}

// handlePAC 生成 PAC（白名单经代理，其余 DIRECT；接入指引页展示）。
func (d *Deps) handlePAC(w http.ResponseWriter, r *http.Request) {
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
	b.WriteString("// light-github PAC——白名单 https 走本地代理，其余直连\n")
	// pattern 与 rule.Table.Match 语义已对齐（review N7 查证为一致，勿"成对补裸域"）：
	// 精确规则 d 只命中裸域；通配 *.d 的 shExpMatch 与 Table 逐级后缀同样要求
	// 有点前缀且 * 跨点，两边对裸域/子域的判定完全一致。
	b.WriteString("function FindProxyForURL(url, host) {\n")
	b.WriteString("  if (url.substring(0, 6) !== \"https:\") return \"DIRECT\"; // 仅加速 https 隧道\n")
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
