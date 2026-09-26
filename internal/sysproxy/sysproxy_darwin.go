//go:build darwin

// Package sysproxy 系统代理（PAC 自动配置）一键接入/还原。
// macOS 经 networksetup 设置所有启用网络服务的自动代理 URL；
// 仅触碰「自动代理配置」一项设置，退出/关闭可完全还原，不装证书、不写 hosts。
package sysproxy

import (
	"fmt"
	"strings"
)

// Enable 为全部启用的网络服务设置 PAC 自动代理。
// 单个服务失败容忍（如虚拟网卡/蓝牙），但全部失败必须报错——
// 否则退出还原假成功，PAC 悬空且无任何告警（review H3）。
func Enable(pacURL string) error {
	services, err := activeServices()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return errNoService
	}
	return applyAll(services, "-setautoproxyurl", pacURL)
}

// Disable 关闭全部启用网络服务的自动代理（还原）。全部失败必须报错（H3）。
func Disable() error {
	services, err := activeServices()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return errNoService
	}
	return applyAll(services, "-setautoproxystate", "off")
}

// applyAll 逐服务执行 networksetup 子命令：任一成功即整体成功，
// 全部失败返回聚合错误（保留最后错误详情）。
func applyAll(services []string, flag, value string) error {
	ok, last := 0, error(nil)
	for _, s := range services {
		if _, err := execRun("networksetup", flag, s, value); err != nil {
			last = err
			continue
		}
		ok++
	}
	if ok == 0 && last != nil {
		return fmt.Errorf("sysproxy: %d 个网络服务全部失败，最后错误: %w", len(services), last)
	}
	invalidateState()
	return nil
}

// Enabled 任一启用服务的 PAC 指向 pacURL 且已启用时返回 true（TTL 缓存，review H2）。
func Enabled(pacURL string) bool {
	return enabledCached(func() bool { return enabledUncached(pacURL) })
}

func enabledUncached(pacURL string) bool {
	services, err := activeServices()
	if err != nil {
		return false
	}
	for _, s := range services {
		out, err := execRun("networksetup", "-getautoproxyurl", s)
		if err != nil {
			continue
		}
		if strings.Contains(out, "URL: "+pacURL) && strings.Contains(out, "Enabled: Yes") {
			return true
		}
	}
	return false
}

func activeServices() ([]string, error) {
	out, err := execRun("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	return parseServices(out), nil
}

// parseServices 解析服务列表：首行为标题（本地化文案不定），* 前缀为已禁用。
func parseServices(out string) []string {
	var services []string
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 { // 标题行
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	return services
}

var errNoService = &serviceError{}

type serviceError struct{}

func (*serviceError) Error() string { return "sysproxy: 未发现已启用的网络服务" }
