//go:build windows

package autostart

import (
	"bytes"
	"os/exec"
	"strings"
)

const regRunPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// execRun 包变量注入测试（沿 sysproxy 模式）。
var execRun = func(name string, args ...string) (string, error) {
	var out bytes.Buffer
	c := exec.Command(name, args...) //nolint:gosec // 参数均为程序内构造
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	return out.String(), err
}

// Enable 写入 HKCU Run 键（当前用户，无 UAC）。
func Enable(execPath string) error {
	_, err := execRun("reg", "add", regRunPath,
		"/v", "light-github", "/t", "REG_SZ",
		"/d", `"`+execPath+`"`, "/f")
	return err
}

// Disable 删除 Run 键值。
func Disable() error {
	_, err := execRun("reg", "delete", regRunPath, "/v", "light-github", "/f")
	return err
}

// Enabled reg query 命中即视为已开启。
func Enabled() bool {
	out, err := execRun("reg", "query", regRunPath, "/v", "light-github")
	return err == nil && strings.Contains(out, "light-github")
}
