//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

const plistName = "com.marvin.light-github.plist"

// osUserHomeDir 包变量注入测试 HOME（沿 sysproxy.execRun 模式）。
var osUserHomeDir = os.UserHomeDir

// Enable 写入 LaunchAgent plist（登录时 launchd 拉起，RunAtLoad 仅登录语义）。
func Enable(execPath string) error {
	home, err := osUserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.marvin.light-github</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`, execPath)
	return os.WriteFile(filepath.Join(dir, plistName), []byte(plist), 0o644)
}

// Disable 删除 plist（容忍不存在）。
func Disable() error {
	home, err := osUserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	err = os.Remove(filepath.Join(home, "Library", "LaunchAgents", plistName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Enabled plist 存在即视为已开启。
func Enabled() bool {
	home, err := osUserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", plistName))
	return err == nil
}
