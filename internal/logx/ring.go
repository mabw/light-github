// 环形缓冲与 slog handler 适配（内存通道，供 Web UI 读取）。
package logx

import (
	"context"
	"log/slog"
	"sync"
)

// ring 定长环形缓冲：新条目覆盖最老条目。
type ring struct {
	mu   sync.Mutex
	buf  []Entry
	head int // 最老元素下标
	n    int // 当前条数
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]Entry, capacity)}
}

func (r *ring) push(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = e
		r.n++
		return
	}
	r.buf[r.head] = e // 已满：覆盖最老
	r.head = (r.head + 1) % len(r.buf)
}

// recent 返回最新 n 条（最新在前）。
func (r *ring) recent(n int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.n {
		n = r.n
	}
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		idx := (r.head + r.n - 1 - i) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// ringHandler 将 slog 记录写入环形缓冲。
type ringHandler struct {
	r     *ring
	level slog.Leveler
	pre   map[string]any // WithAttrs 预置属性
}

func newRingHandler(r *ring, level slog.Leveler) *ringHandler {
	return &ringHandler{r: r, level: level}
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for k, v := range h.pre {
		attrs[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.r.push(Entry{At: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: attrs})
	return nil
}

// WithAttrs 返回携带预置属性的新 handler（slog.Handler 约束：不可变）。
func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	pre := map[string]any{}
	for _, a := range attrs {
		pre[a.Key] = a.Value.Any()
	}
	return &ringHandler{r: h.r, level: h.level, pre: pre}
}

// WithGroup 本工具不使用分组语义，返回自身。
func (h *ringHandler) WithGroup(string) slog.Handler { return h }

var _ slog.Handler = (*ringHandler)(nil)
