//go:build linux && !android

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const desktopName = "light-github.desktop"

// osUserHomeDir 包变量注入测试 HOME（沿 sysproxy 模式）。
var osUserHomeDir = os.UserHomeDir

// Enable 写入 XDG autostart .desktop（主流桌面环境登录时拉起）。
func Enable(execPath string) error {
	home, err := osUserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	dir := filepath.Join(home, ".config", "autostart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=light-github
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, execPath)
	return os.WriteFile(filepath.Join(dir, desktopName), []byte(desktop), 0o644)
}

// Disable 删除 .desktop（容忍不存在）。
func Disable() error {
	home, err := osUserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	err = os.Remove(filepath.Join(home, ".config", "autostart", desktopName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Enabled .desktop 存在即视为已开启。
func Enabled() bool {
	home, err := osUserHomeDir()
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "autostart", desktopName))
	return err == nil && strings.Contains(string(raw), "Exec=")
}
