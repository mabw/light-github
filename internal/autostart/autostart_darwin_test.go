//go:build darwin

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestHome 注入隔离 HOME，避免触碰真实 ~/Library/LaunchAgents。
func newTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := osUserHomeDir
	osUserHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { osUserHomeDir = old })
	return home
}

func TestEnable_WritesLaunchAgentPlist(t *testing.T) {
	home := newTestHome(t)

	if err := Enable("/usr/local/bin/light-github"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", plistName))
	if err != nil {
		t.Fatalf("plist 未生成: %v", err)
	}
	s := string(raw)
	for _, want := range []string{
		"<key>Label</key>", "<string>com.marvin.light-github</string>",
		"<key>ProgramArguments</key>", "<string>/usr/local/bin/light-github</string>",
		"<key>RunAtLoad</key>", "<true/>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist 缺少 %q\n%s", want, s)
		}
	}
}

func TestEnable_MakesDirs(t *testing.T) {
	newTestHome(t) // ~/Library/LaunchAgents 不存在
	if err := Enable("/x/light-github"); err != nil {
		t.Fatalf("目录不存在时 Enable 应自动创建: %v", err)
	}
}

func TestDisable_RemovesPlist(t *testing.T) {
	home := newTestHome(t)
	if err := Enable("/x/light-github"); err != nil {
		t.Fatal(err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", plistName)); !os.IsNotExist(err) {
		t.Errorf("plist 未删除")
	}
}

func TestDisable_NoFileIsOK(t *testing.T) {
	newTestHome(t)
	if err := Disable(); err != nil {
		t.Errorf("无 plist 时 Disable 应静默成功: %v", err)
	}
}

func TestEnabled_ReflectsPlist(t *testing.T) {
	newTestHome(t)
	if Enabled() {
		t.Fatal("无 plist 时应为 false")
	}
	if err := Enable("/x/light-github"); err != nil {
		t.Fatal(err)
	}
	if !Enabled() {
		t.Fatal("Enable 后应为 true")
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("Disable 后应为 false")
	}
}
