package safego

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf 并发安全的日志收集（safego goroutine 写与测试断言读并发）
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// panic 的 fn 不得崩溃进程（测试进程存活即证明 recover 生效），且留日志
func TestGo_RecoversPanic(t *testing.T) {
	var out lockedBuf
	log := slog.New(slog.NewTextHandler(&out, nil))

	done := make(chan struct{})
	Go("boom", log, func() {
		defer close(done)
		panic("kaboom")
	})

	select {
	case <-done: // fn 的 defer 先于外层 recover 执行
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine 未在时限内执行")
	}
	// 等 recover 路径写完日志
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "kaboom") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := out.String()
	if !strings.Contains(s, "boom") || !strings.Contains(s, "kaboom") {
		t.Fatalf("日志应含协程名与 panic 值: %s", s)
	}
	if !strings.Contains(s, "stack") && !strings.Contains(s, "safego") {
		t.Fatalf("日志应含堆栈线索: %s", s)
	}
}

// 正常路径不受影响
func TestGo_NormalRun(t *testing.T) {
	done := make(chan struct{})
	Go("ok", nil, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("正常 fn 应执行完毕")
	}
}
