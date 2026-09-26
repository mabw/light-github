package sysproxy

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// execTimeout 子命令上限：networksetup 偶发等待 configd 挂起是已知现象，
// 退出清理路径若被卡住则进程永不退出（review H4）。变量形态供测试缩短。
var execTimeout = 5 * time.Second

// execRun 包变量注入测试；三平台分支共用一份（各分支仅传不同命令参数）。
var execRun = func(name string, args ...string) (string, error) {
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // 参数均为常量/程序内构造
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	return out.String(), err
}
