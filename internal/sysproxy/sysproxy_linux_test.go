//go:build linux && !android

package sysproxy

import (
	"errors"
	"strings"
	"testing"
)

// Enable 依次设置 autoconfig-url 并把 mode 切到 auto（GNOME gsettings）。
// ubuntu CI 上真实执行本组用例——DEBT-7 的 gsettings 分支提前在 CI 验证。
func TestEnable_SetsGnomeAutoconfig(t *testing.T) {
	f := withFake(t)
	if err := Enable("http://127.0.0.1:12800/pac"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "gsettings set org.gnome.system.proxy autoconfig-url http://127.0.0.1:12800/pac") {
		t.Fatalf("应设置 autoconfig-url: %s", joined)
	}
	if !strings.Contains(joined, "gsettings set org.gnome.system.proxy mode auto") {
		t.Fatalf("应切换 mode=auto: %s", joined)
	}
}

// 非 GNOME 环境（gsettings 不存在）应报错且提示手动配置，而非假成功
func TestEnable_NoGnomeReturnsError(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		return "", errors.New("exec: not found")
	}
	err := Enable("http://127.0.0.1:12800/pac")
	if err == nil {
		t.Fatal("gsettings 失败应返回错误")
	}
	if !strings.Contains(err.Error(), "手动") {
		t.Fatalf("应提示手动配置: %v", err)
	}
}

// 还原：mode 切回 none
func TestDisable_SetsModeNone(t *testing.T) {
	f := withFake(t)
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "gsettings set org.gnome.system.proxy mode none") {
		t.Fatalf("应还原 mode=none: %s", joined)
	}
}

// 状态：URL 匹配且 mode=auto 才算接入（gsettings get 输出带单引号）
func TestEnabled_MatchesUrlAndMode(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get org.gnome.system.proxy autoconfig-url"):
			return "'http://127.0.0.1:12800/pac'", nil
		case strings.Contains(joined, "get org.gnome.system.proxy mode"):
			return "'auto'", nil
		}
		return "", nil
	}
	if !Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("URL 匹配且 mode=auto 应返回 true")
	}
}

// URL 匹配但 mode 非 auto（用户手动改过代理模式）不算接入
func TestEnabled_WrongModeIsFalse(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get org.gnome.system.proxy autoconfig-url"):
			return "'http://127.0.0.1:12800/pac'", nil
		case strings.Contains(joined, "get org.gnome.system.proxy mode"):
			return "'manual'", nil
		}
		return "", nil
	}
	if Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("mode 非 auto 不应视为已接入")
	}
}
