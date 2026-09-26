package sysproxy

import (
	"strings"
	"testing"
	"time"
)

// fake 执行器：记录调用并按脚本返回（不真改系统）。
// 平台断言测试在各 *_平台_test.go（命令行差异大，不共享断言）。
type fakeExec struct {
	calls   []string
	respond func(name string, args []string) (string, error)
}

func (f *fakeExec) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if f.respond != nil {
		return f.respond(name, args)
	}
	return "", nil
}

func withFake(t *testing.T) *fakeExec {
	t.Helper()
	f := &fakeExec{}
	old := execRun
	execRun = f.run
	invalidateState() // 隔离上一用例的 TTL 缓存（review H2 引入）
	t.Cleanup(func() {
		execRun = old
		invalidateState()
	})
	return f
}

// ---- review H4：execRun 必须有超时（挂死的子进程会卡死退出清理路径）----
// 注：sleep 在 unix 系均存在；Windows 无 sleep（该平台 CI 不跑本测试，
// 语义由 darwin/linux 覆盖）。

func TestExecRun_TimesOut(t *testing.T) {
	old := execTimeout
	execTimeout = 150 * time.Millisecond
	t.Cleanup(func() { execTimeout = old })

	start := time.Now()
	_, err := execRun("sleep", "5")
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("应按超时截断，实际耗时 %v", elapsed)
	}
}
