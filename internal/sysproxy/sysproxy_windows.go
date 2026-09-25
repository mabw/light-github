//go:build windows

// Package sysproxy Windows 分支：注册表 Internet Settings 的 AutoConfigURL（PAC）。
// 仅写 HKCU 当前用户配置，退出删除即还原；不装证书、不写 hosts。
// 注意：WinINET 可能不立即感知变更（需要 InternetSetOption 广播），M3/M4 真机验证。
package sysproxy

import (
	"strings"
)

const regPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// Enable 设置 PAC 自动代理（AutoConfigURL）。
func Enable(pacURL string) error {
	_, err := execRun("reg", "add", regPath, "/v", "AutoConfigURL", "/t", "REG_SZ", "/d", pacURL, "/f")
	return err
}

// Disable 删除 AutoConfigURL（还原）。
func Disable() error {
	_, _ = execRun("reg", "delete", regPath, "/v", "AutoConfigURL", "/f")
	return nil
}

// Enabled AutoConfigURL 当前值等于 pacURL 时返回 true。
func Enabled(pacURL string) bool {
	out, err := execRun("reg", "query", regPath, "/v", "AutoConfigURL")
	if err != nil {
		return false
	}
	return strings.Contains(out, pacURL)
}
