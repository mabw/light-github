// light-github 主入口（M1：headless 核心服务）。
//
// 组装链：数据源（steampp→GitHub520→内置）→ 规则表 → 选择器（DoH+TCP测速）
//
//	→ CONNECT 隧道代理 → 指标记录
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/marvin/light-github/internal/doh"
	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/proxy"
	"github.com/marvin/light-github/internal/rule"
	"github.com/marvin/light-github/internal/selector"
	"github.com/marvin/light-github/internal/source"
)

const (
	defaultAddr        = "127.0.0.1:12800"
	defaultRefresh     = time.Hour // 数据源刷新周期（克制：≥1h，见 DESIGN.md §3.3）
	steamppAPI         = "https://api.steampp.net/accelerator/projectgroups"
	github520HostsJSON = "https://raw.hellogithub.com/hosts.json"
)

func main() {
	var (
		addr        = flag.String("addr", defaultAddr, "监听地址（WSL 场景可改 0.0.0.0）")
		refresh     = flag.Duration("refresh", defaultRefresh, "数据源刷新周期")
		showVersion = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("light-github 0.1.0 (M1)")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 数据源：steampp（主）→ GitHub520（备）→ 内置清单（兜底），本地缓存降级
	mgr := source.NewManager(
		source.NewSteampp(steamppAPI),
		source.NewGitHub520(github520HostsJSON),
		source.NewBuiltin(),
	)
	mgr.CachePath = cachePath()

	rules, err := mgr.Load(ctx)
	if err != nil {
		log.Fatalf("加载规则失败: %v", err)
	}
	table := rule.NewTable(rules)
	log.Printf("规则加载成功: %d 条（源: steampp → github520 → builtin）", len(rules))

	// 选择器：DoH 多端点解析 + TCP 中位测速 + 失败沉底
	sel := selector.New(table, doh.New(), &selector.MedianProber{}, 5*time.Minute)

	// DEBT-3：后台预热白名单域名测速缓存（首请求不再同步测速）。
	// 通配规则（*.suffix）以去前缀的裸域近似预热。
	preloadDomains := make([]string, 0, len(rules))
	seen := map[string]bool{}
	for _, r := range rules {
		d := strings.TrimPrefix(r.Domain, "*.")
		if d != "" && !seen[d] {
			seen[d] = true
			preloadDomains = append(preloadDomains, d)
		}
	}
	go sel.Preload(ctx, preloadDomains, 4)

	// 代理 + 指标
	store := metrics.NewStore(5 * time.Second)
	srv := &proxy.Server{
		Addr:    *addr,
		Table:   table,
		Dialer:  sel,
		Metrics: store,
	}
	listenAddr, err := srv.ListenAndServe(ctx)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	log.Printf("light-github 已启动: http://%s", listenAddr)

	printOnboarding(listenAddr.String())

	// 定时刷新规则并热更新
	go func() {
		ticker := time.NewTicker(*refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if rules, err := mgr.Refresh(ctx); err == nil {
					srv.SetTable(rule.NewTable(rules))
					log.Printf("规则已刷新: %d 条", len(rules))
				} else {
					log.Printf("规则刷新失败（沿用现有规则）: %v", err)
				}
			}
		}
	}()

	<-ctx.Done()
	log.Println("正在退出…")
	_ = srv.Close()

	// 退出前输出本次会话统计
	snap := store.Snapshot()
	log.Printf("本次会话: 连接 %d（失败 %d）↑%s ↓%s",
		snap.TotalConns, snap.FailedConns,
		humanBytes(snap.TotalUp), humanBytes(snap.TotalDown))
}

// printOnboarding 打印接入命令（不替用户执行任何系统修改）
func printOnboarding(addr string) {
	fmt.Println(`
接入方式（按需选用）:
  git（仅 GitHub 生效，零全局污染）:
    git config --global http.https://github.com.proxy http://` + addr + `
  终端临时:
    export https_proxy=http://` + addr + ` http_proxy=http://` + addr + `
  curl 快速验证:
    curl -x http://` + addr + ` -sI https://github.com | head -1`)
}

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "rules.cache.json"
	}
	return filepath.Join(home, ".light-github", "rules.cache.json")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
