// light-github 主入口（M2：headless 核心服务 + Web 控制台）。
//
// 组装链：数据源（steampp→GitHub520→内置）→ 规则表 → 选择器（DoH+TCP测速）
//
//	→ CONNECT 隧道代理（与控制台共用端口）→ 指标/日志
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/marvin/light-github/internal/config"
	"github.com/marvin/light-github/internal/doh"
	"github.com/marvin/light-github/internal/logx"
	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/proxy"
	"github.com/marvin/light-github/internal/rule"
	"github.com/marvin/light-github/internal/selector"
	"github.com/marvin/light-github/internal/source"
	"github.com/marvin/light-github/internal/sysproxy"
	"github.com/marvin/light-github/internal/webapi"
)

const (
	version         = "0.2.0"
	steamppAPI      = "https://api.steampp.net/accelerator/projectgroups"
	github520HostsJSON = "https://raw.hellogithub.com/hosts.json"
)

func main() {
	var (
		addrFlag    = flag.String("addr", config.DefaultAddr, "监听地址（WSL 场景可改 0.0.0.0）")
		refreshFlag = flag.Duration("refresh", config.DefaultRefresh, "数据源刷新周期")
		tokenFlag   = flag.String("token", "", "访问令牌（非 loopback 监听时必填）")
		verbose     = flag.Bool("v", false, "调试日志（等价 logLevel=debug，优先级高于配置文件）")
		showVersion = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("light-github " + version + " (M2)")
		return
	}

	// 配置优先级：显式 flags > 配置文件 > 默认值
	cfgPath := config.Path()
	fileCfg, _ := config.Load(cfgPath) // 缺文件/坏 JSON 均降级默认（config 包内保证）
	explicit := map[string]any{}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			explicit["addr"] = *addrFlag
		case "token":
			explicit["token"] = *tokenFlag
		case "refresh":
			explicit["refresh"] = *refreshFlag
		}
	})
	cfg := config.Overlay(fileCfg, explicit)
	if *verbose {
		cfg.LogLevel = "debug"
	}
	if err := validateListen(cfg.Addr, cfg.Token); err != nil {
		fmt.Fprintf(os.Stderr, "监听配置不安全: %v（WSL/局域网场景请配置 token）\n", err)
		os.Exit(1)
	}

	// 日志：内存环形（UI）+ 轮转文件（排错）+ 终端（常驻进程观察）
	baseDir := baseDir()
	logs := logx.New(logx.Options{
		Dir:      filepath.Join(baseDir, "logs"),
		Level:    parseLevel(cfg.LogLevel),
		Compress: true,
		Stderr:   true,
	})
	defer func() { _ = logs.Close() }()
	log := logs.App()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 数据源：steampp（主）→ GitHub520（备）→ 内置清单（兜底），本地缓存降级
	mgr := source.NewManager(
		source.NewSteampp(steamppAPI),
		source.NewGitHub520(github520HostsJSON),
		source.NewBuiltin(),
	)
	mgr.CachePath = filepath.Join(baseDir, "rules.cache.json")

	rules, err := mgr.Load(ctx)
	if err != nil {
		log.Error("规则加载失败（三源不可用且无本地缓存）", "err", err)
		os.Exit(1)
	}
	table := rule.NewTable(rules)
	log.Info("规则加载成功", "count", len(rules))

	// 选择器 + 后台预热（首请求命中缓存）
	sel := selector.New(table, doh.New(), &selector.MedianProber{}, 5*time.Minute)
	go sel.Preload(ctx, preloadDomains(rules), 4)

	// 指标 + 连接日志落盘钩子
	store := metrics.NewStore(5 * time.Second)
	connLog := logs.Conn()
	store.OnRecord = func(i metrics.ConnInfo) {
		args := []any{
			"domain", i.Domain, "via", i.Via, "ip", i.IP,
			"dialMs", i.DialMS, "up", i.Up, "down", i.Down, "ok", i.OK,
		}
		if i.Err != "" {
			args = append(args, "err", i.Err)
		}
		connLog.Info("conn", args...)
	}

	// 规则热更新（定时器与手动刷新共用）；周期热生效故读原子值
	var (
		srv           *proxy.Server
		rulesMu       sync.Mutex
		rulesSnapshot = rules
		refreshEvery  atomic.Int64
	)
	refreshEvery.Store(int64(cfg.Refresh))

	applyRules := func(newRules []rule.Rule) {
		t := rule.NewTable(newRules)
		sel.SetTable(t)
		if srv != nil {
			srv.SetTable(t)
		}
		rulesMu.Lock()
		rulesSnapshot = newRules
		rulesMu.Unlock()
		log.Info("规则已更新", "count", len(newRules))
	}

	// 系统代理一键接入用的 PAC 地址（0.0.0.0 对本机浏览器无意义，归一为回环）
	pacURL := "http://" + loopbackAddr(cfg.Addr) + "/pac"

	deps := &webapi.Deps{
		Version:   version,
		StartedAt: time.Now(),
		Addr:      cfg.Addr,
		Token:     cfg.Token,
		Metrics:   store,
		Logs:      logs,
		Source:    mgr,
		Selector:  sel,
		Rules: func() []rule.Rule {
			rulesMu.Lock()
			defer rulesMu.Unlock()
			return rulesSnapshot
		},
		Config:     cfg,
		ConfigPath: cfgPath,
		OnConfigChange: func(c config.Config) {
			cfg = c // logLevel/refresh 热应用；addr/token 由 UI 提示重启
			logs.SetLevel(parseLevel(c.LogLevel))
			refreshEvery.Store(int64(c.Refresh))
			log.Info("配置已热应用", "logLevel", c.LogLevel, "refresh", c.Refresh.String())
		},
		RefreshRules: func(ctx context.Context) (int, error) {
			newRules, err := mgr.Refresh(ctx)
			if err != nil {
				log.Warn("手动刷新失败", "err", err)
				return 0, err
			}
			applyRules(newRules)
			return len(newRules), nil
		},
		SetAccel: func(on bool) {
			srv.SetEnabled(on)
			if on {
				log.Info("加速已开启")
			} else {
				log.Warn("加速已关闭（全部直通；端口与观测保持可用）")
			}
		},
		AccelEnabled: func() bool { return srv.Enabled() },
		SetSysProxy: func(on bool) error {
			if on {
				if err := sysproxy.Enable(pacURL); err != nil {
					log.Warn("系统代理接入失败", "err", err)
					return err
				}
				log.Info("系统代理已接入（PAC）", "url", pacURL, "note", "与其他代理软件的系统代理互斥")
			} else {
				if err := sysproxy.Disable(); err != nil {
					return err
				}
				log.Info("系统代理已还原")
			}
			return nil
		},
		SysProxyState: func() bool { return sysproxy.Enabled(pacURL) },
	}

	// 代理（与控制台共用端口：CONNECT→隧道；origin-form→Web）
	srv = &proxy.Server{
		Addr:    cfg.Addr,
		Table:   table,
		Dialer:  sel,
		Metrics: store,
		Token:   cfg.Token,
		Web:     webapi.Handler(deps),
	}
	listenAddr, err := srv.ListenAndServe(ctx)
	if err != nil {
		log.Error("监听失败", "err", err, "addr", cfg.Addr)
		os.Exit(1)
	}
	log.Info("light-github 已启动",
		"addr", listenAddr.String(), "ui", "http://"+listenAddr.String(),
		"token", tokenState(cfg.Token), "level", cfg.LogLevel)

	printOnboarding(listenAddr.String())

	// 定时刷新（周期可被 Web UI 热改）
	go func() {
		for {
			d := time.Duration(refreshEvery.Load())
			if d <= 0 {
				d = time.Hour
			}
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if newRules, err := mgr.Refresh(ctx); err == nil {
					applyRules(newRules)
				} else {
					log.Warn("规则刷新失败（沿用现有规则）", "err", err)
				}
			}
		}
	}()

	<-ctx.Done()
	log.Info("正在退出…")
	_ = srv.Close()

	// 退出清理：系统 PAC 指向我们时还原，避免浏览器悬空（零侵入承诺的完整闭环）
	if sysproxy.Enabled(pacURL) {
		if err := sysproxy.Disable(); err == nil {
			log.Info("退出时已还原系统代理设置")
		} else {
			log.Warn("退出时还原系统代理失败", "err", err)
		}
	}

	snap := store.Snapshot()
	log.Info("本次会话统计",
		"conns", snap.TotalConns, "failed", snap.FailedConns,
		"up", snap.TotalUp, "down", snap.TotalDown)
}

// validateListen 非 loopback 监听必须配置 Token（DEBT-6：0.0.0.0 会把未鉴权代理暴露给局域网）
func validateListen(addr, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("地址格式错误: %w", err)
	}
	ip := net.ParseIP(host)
	if ip != nil && !ip.IsLoopback() && token == "" {
		return fmt.Errorf("-addr %s 为非回环地址", addr)
	}
	return nil
}

// printOnboarding 打印接入命令（不替用户执行任何系统修改）
func printOnboarding(addr string) {
	fmt.Println(`
控制台:   http://` + addr + `
接入方式（按需选用）:
  git（仅 GitHub 生效，零全局污染）:
    git config --global http.https://github.com.proxy http://` + addr + `
  终端临时:
    export https_proxy=http://` + addr + ` http_proxy=http://` + addr + `
  curl 快速验证:
    curl -x http://` + addr + ` -sI https://github.com | head -1`)
}

func baseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".light-github")
}

// preloadDomains 通配规则（*.suffix）以去前缀的裸域近似预热
func preloadDomains(rules []rule.Rule) []string {
	out := make([]string, 0, len(rules))
	seen := map[string]bool{}
	for _, r := range rules {
		if d := strings.TrimPrefix(r.Domain, "*."); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func tokenState(token string) string {
	if token == "" {
		return "off"
	}
	return "on"
}

// loopbackAddr 把监听地址归一为浏览器可用的回环形态（0.0.0.0:12800 → 127.0.0.1:12800）
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}
