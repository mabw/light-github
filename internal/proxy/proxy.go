// Package proxy CONNECT 隧道代理：白名单域名经选择器加速，其余直通
// （docs/DESIGN.md §3.1；spike-a 的正式化实现）。
package proxy

import (
	"context"
	"net"
	"time"

	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
)

// Dialer 出站选择器抽象（selector.Selector 实现之）。
type Dialer interface {
	Pick(ctx context.Context, domain string) ([]net.IP, error)
	ReportFailure(domain string, ip net.IP)
	ReportSuccess(domain string, ip net.IP)
}

// Server CONNECT 隧道代理服务器。
type Server struct {
	Addr        string
	Table       *rule.Table
	Dialer      Dialer
	Metrics     *metrics.Store
	DialTimeout time.Duration // 单候选拨号超时

	ln net.Listener
}

// ListenAndServe 启动监听并转入后台 accept。TODO: TDD RED 骨架（当前直接断开连接）。
func (s *Server) ListenAndServe(context.Context) (net.Addr, error) {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return nil, err
	}
	s.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close() // RED 骨架：拒绝一切
		}
	}()
	return ln.Addr(), nil
}

// Close 停止服务。
func (s *Server) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}
