package sysproxy

import (
	"bytes"
	"os/exec"
)

// execRun 包变量注入测试；三平台分支共用一份（各分支仅传不同命令参数）。
var execRun = func(name string, args ...string) (string, error) {
	var out bytes.Buffer
	c := exec.Command(name, args...) //nolint:gosec // 参数均为常量/程序内构造
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	return out.String(), err
}
