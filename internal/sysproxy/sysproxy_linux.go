//go:build linux && !android

// Package sysproxy Linux 分支：GNOME gsettings 的系统代理（PAC autoconfig-url）。
// KDE/其他桌面环境无统一接口，返回错误提示手动配置；不装证书、不写 hosts。
package sysproxy

import (
	"errors"
	"strings"
)

// Enable 设置 GNOME 系统代理为 PAC 模式。
func Enable(pacURL string) error {
	if _, err := execRun("gsettings", "set", "org.gnome.system.proxy", "autoconfig-url", pacURL); err != nil {
		return errors.New("sysproxy: 仅支持 GNOME（gsettings）；KDE 等桌面请手动配置")
	}
	_, err := execRun("gsettings", "set", "org.gnome.system.proxy", "mode", "auto")
	if err == nil {
		invalidateState()
	}
	return err
}

// Disable 还原为直连模式。
func Disable() error {
	_, err := execRun("gsettings", "set", "org.gnome.system.proxy", "mode", "none")
	if err == nil {
		invalidateState()
	}
	return err
}

// Enabled autoconfig-url 等于 pacURL 且 mode 为 auto 时返回 true（TTL 缓存，review H2）。
func Enabled(pacURL string) bool {
	return enabledCached(func() bool {
		url, err := execRun("gsettings", "get", "org.gnome.system.proxy", "autoconfig-url")
		if err != nil || !strings.Contains(url, pacURL) {
			return false
		}
		mode, err := execRun("gsettings", "get", "org.gnome.system.proxy", "mode")
		return err == nil && strings.Contains(mode, "auto")
	})
}
