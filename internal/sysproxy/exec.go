package sysproxy

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
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

// ---- 状态查询缓存（review H2）----
// 托盘 2s + 控制台 5s 轮询都调 Enabled，darwin 实现每次 fork 2+ 个
// networksetup 子进程（24h 八万次量级）。TTL 缓存 + 写路径主动失效。

var stateTTL = 3 * time.Second // 变量供测试缩短

var (
	stateMu  sync.Mutex
	stateVal bool
	stateAt  time.Time
)

// enabledCached 读穿透缓存：TTL 内直接返回上次结果。
func enabledCached(uncached func() bool) bool {
	stateMu.Lock()
	if time.Since(stateAt) < stateTTL {
		defer stateMu.Unlock()
		return stateVal
	}
	stateMu.Unlock()

	v := uncached()
	stateMu.Lock()
	stateVal, stateAt = v, time.Now()
	stateMu.Unlock()
	return v
}

// invalidateState 写路径（Enable/Disable 成功后）主动失效，防止读到陈旧状态。
func invalidateState() {
	stateMu.Lock()
	stateAt = time.Time{}
	stateMu.Unlock()
}

