// Package safego 带 panic recovery 的 goroutine 启动器（M5-5）。
//
// Go 运行时中任何 goroutine 的 panic 都会终止整个进程——对常驻代理进程，
// 一个后台协程（预热/刷新/隧道拷贝）的意外 panic 不应带走全部连接。
// 等价 Watt Toolkit 的 BackgroundServiceExceptionBehavior.Ignore（调研 §2.6）。
package safego

import (
	"log/slog"
	"runtime/debug"
)

// Go 启动 fn 于新 goroutine，panic 时记录日志（含堆栈）而非崩溃。
// log 为 nil 时落到 slog.Default()。
func Go(name string, log *slog.Logger, fn func()) {
	if log == nil {
		log = slog.Default()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("后台协程 panic 已恢复", "name", name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}
