//go:build darwin

// Package sysproxy 系统代理（PAC 自动配置）一键接入/还原。
// macOS 经 networksetup 设置所有启用网络服务的自动代理 URL；
// 仅触碰「自动代理配置」一项设置，退出/关闭可完全还原，不装证书、不写 hosts。
package sysproxy

import (
	"strings"
)

// Enable 为全部启用的网络服务设置 PAC 自动代理。
// 单个服务失败容忍（如虚拟网卡/蓝牙），任一成功即整体成功。
func Enable(pacURL string) error {
	services, err := activeServices()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return errNoService
	}
	for _, s := range services {
		if _, err := execRun("networksetup", "-setautoproxyurl", s, pacURL); err != nil {
			continue
		}
	}
	return nil
}

// Disable 关闭全部启用网络服务的自动代理（还原）。
func Disable() error {
	services, err := activeServices()
	if err != nil {
		return err
	}
	for _, s := range services {
		if _, err := execRun("networksetup", "-setautoproxystate", s, "off"); err != nil {
			continue
		}
	}
	return nil
}

// Enabled 任一启用服务的 PAC 指向 pacURL 且已启用时返回 true。
func Enabled(pacURL string) bool {
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
