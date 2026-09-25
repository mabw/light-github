// Package proxy CONNECT 隧道代理：白名单域名经选择器加速，其余直通
// （docs/DESIGN.md §3.1；spike-a 的正式化实现）。
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/marvin/light-github/internal/metrics"
	"github.com/marvin/light-github/internal/rule"
)

// bufioReader 每连接独立的带缓冲读取器
func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

// writeSimpleResponse 写一条极简 HTTP 响应（用于拒绝/错误场景）
func writeSimpleResponse(c net.Conn, code int, msg string) error {
	_, err := fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
	return err
}

// Dialer 出站选择器抽象（selector.Selector 实现之）。
type Dialer interface {
	Pick(ctx context.Context, domain string) ([]net.IP, error)
	ReportFailure(domain string, ip net.IP)
	ReportSuccess(domain string, ip net.IP)
}

// Server CONNECT 隧道代理服务器。并发安全。
type Server struct {
	Addr        string
	Table       *rule.Table // 初始规则表；运行期经 SetTable 热更新
	Dialer      Dialer
	Metrics     *metrics.Store
	DialTimeout time.Duration // 单候选拨号超时；零值默认 5s

	mu    sync.RWMutex
	table *rule.Table
	ln    net.Listener
}

// SetTable 原子替换规则表（数据源定时刷新用）。
func (s *Server) SetTable(t *rule.Table) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table = t
}

// currentTable 返回当前生效规则表
func (s *Server) currentTable() *rule.Table {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.table != nil {
		return s.table
	}
	return s.Table
}

// ListenAndServe 启动监听并在后台 accept，返回实际监听地址。
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
			go s.handleConn(conn)
		}
	}()
	return ln.Addr(), nil
}

// Close 停止服务（已建立的隧道随进程生命周期自然结束）。
func (s *Server) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

func (s *Server) handleConn(client net.Conn) {
	defer client.Close()

	req, err := http.ReadRequest(bufioReader(client))
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_ = writeSimpleResponse(client, http.StatusBadRequest, "only CONNECT supported")
		return
	}
	s.handleConnect(client, req)
}

func (s *Server) handleConnect(client net.Conn, req *http.Request) {
	host := req.URL.Host // host:port
	domain, port, err := net.SplitHostPort(host)
	if err != nil || port == "" {
		domain, port = host, "443"
	}
	start := time.Now()

	// 出站拨号：白名单 → 逐候选尝试；其余 → 直通原目标
	var (
		upstream net.Conn
		via      string
		usedIP   net.IP
	)
	if _, accelerated := s.currentTable().Match(domain); accelerated {
		via = "accel"
		ips, err := s.Dialer.Pick(context.Background(), domain)
		if err == nil {
			for _, ip := range ips {
				conn, derr := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), s.dialTimeout())
				if derr == nil {
					upstream, usedIP = conn, ip
					s.Dialer.ReportSuccess(domain, ip)
					break
				}
				s.Dialer.ReportFailure(domain, ip)
			}
		}
	} else {
		via = "direct"
		upstream, err = net.DialTimeout("tcp", host, s.dialTimeout())
	}

	if upstream == nil {
		// 拨号失败不泄漏给客户端重试语义之外的细节（git/curl 收到 502 不重试 CONNECT）
		_ = writeSimpleResponse(client, http.StatusBadGateway, "upstream unreachable")
		s.record(domain, via, usedIP, start, 0, 0, false, "dial failed")
		return
	}
	defer upstream.Close()

	// 隧道：200 → 双向字节搬运（不解密 TLS）
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		s.record(domain, via, usedIP, start, 0, 0, false, "write 200: "+err.Error())
		return
	}

	up, down := tunnel(client, upstream)
	s.record(domain, via, usedIP, start, up, down, true, "")
}

// tunnel 双向搬运并半关闭，返回 (客户端→上游, 上游→客户端) 字节数
func tunnel(a, b net.Conn) (int64, int64) {
	done := make(chan int64, 2)
	cp := func(dst io.Writer, src io.Reader) {
		n, _ := io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- n
	}
	go cp(b, a)
	go cp(a, b)
	return <-done, <-done
}

func (s *Server) record(domain, via string, ip net.IP, start time.Time, up, down int64, ok bool, errMsg string) {
	if s.Metrics == nil {
		return
	}
	info := metrics.ConnInfo{
		Domain: domain, Via: via,
		DialMS: time.Since(start).Milliseconds(),
		Up:     up, Down: down, OK: ok, Err: errMsg,
	}
	if ip != nil {
		info.IP = ip.String()
	}
	s.Metrics.RecordConn(info)
}

func (s *Server) dialTimeout() time.Duration {
	if s.DialTimeout > 0 {
		return s.DialTimeout
	}
	return 5 * time.Second
}
