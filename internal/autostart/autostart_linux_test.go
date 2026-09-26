//go:build linux && !android

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 注入临时 HOME（不动真实 ~/.config）；CI 与本机容器均可安全执行
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := osUserHomeDir
	osUserHomeDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { osUserHomeDir = old })
	return dir
}

// Enable 写入 XDG autostart 条目，Exec 指向给定路径
func TestEnableWritesDesktopEntry(t *testing.T) {
	home := withHome(t)
	if err := Enable("/usr/local/bin/light-github"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "autostart", "light-github.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Type=Application") || !strings.Contains(s, "Exec=/usr/local/bin/light-github") {
		t.Fatalf("desktop 条目内容异常: %s", s)
	}
	if !Enabled() {
		t.Fatal("写入后 Enabled 应为 true")
	}
}

// Disable 删除条目
func TestDisableRemovesEntry(t *testing.T) {
	withHome(t)
	if err := Enable("/usr/local/bin/light-github"); err != nil {
		t.Fatal(err)
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("删除后 Enabled 应为 false")
	}
}

// 未开启时 Disable 容忍（幂等还原）
func TestDisableToleratesMissing(t *testing.T) {
	withHome(t)
	if err := Disable(); err != nil {
		t.Fatalf("未开启时 Disable 应容忍: %v", err)
	}
}

// 未开启时 Enabled 为 false
func TestEnabledFalseWhenAbsent(t *testing.T) {
	withHome(t)
	if Enabled() {
		t.Fatal("无条目时 Enabled 应为 false")
	}
}
