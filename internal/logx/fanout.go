// fan-out handler：同一条日志分发到多个下游（环形缓冲 + 文件）。
package logx

import (
	"context"
	"log/slog"
)

type fanoutHandler struct {
	hs []slog.Handler
}

func newFanout(hs ...slog.Handler) slog.Handler {
	return &fanoutHandler{hs: hs}
}

func (f *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range f.hs {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err // 日志失败不打日志（防递归），保留首个错误返回
		}
	}
	return firstErr
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.hs))
	for i, h := range f.hs {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{hs: hs}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.hs))
	for i, h := range f.hs {
		hs[i] = h.WithGroup(name)
	}
	return &fanoutHandler{hs: hs}
}

var _ slog.Handler = (*fanoutHandler)(nil)
