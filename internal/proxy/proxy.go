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
	"sync/atomic"
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
	IdleTimeout time.Duration // 隧道空闲超时（DEBT-2）；零值默认 5min
	Token       string        // 非空时 CONNECT 必须携带 Proxy-Authorization: Bearer（DEBT-6）

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

	// 连接级 ctx（DEBT-5）：拨号与隧道均绑定本连接生命周期，连接结束即取消
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.ReadRequest(bufioReader(client))
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_ = writeSimpleResponse(client, http.StatusBadRequest, "only CONNECT supported")
		return
	}
	// DEBT-6：非 loopback 监听场景的访问控制（cmd 层强制 0.0.0.0 必须配 Token）
	if s.Token != "" && req.Header.Get("Proxy-Authorization") != "Bearer "+s.Token {
		_, _ = fmt.Fprintf(client, "HTTP/1.1 %d %s\r\nProxy-Authenticate: Bearer\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			http.StatusProxyAuthRequired, http.StatusText(http.StatusProxyAuthRequired))
		return
	}
	s.handleConnect(ctx, cancel, client, req)
}

func (s *Server) handleConnect(ctx context.Context, cancel context.CancelFunc, client net.Conn, req *http.Request) {
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
		ips, perr := s.Dialer.Pick(ctx, domain)
		if perr == nil {
			var d net.Dialer
			d.Timeout = s.dialTimeout()
			for _, ip := range ips {
				if ctx.Err() != nil {
					break // 客户端已断开，停止尝试后续候选
				}
				conn, derr := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
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
		var d net.Dialer
		d.Timeout = s.dialTimeout()
		upstream, err = d.DialContext(ctx, "tcp", host)
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

	up, down := tunnel(ctx, cancel, client, upstream, s.idleTimeout())
	s.record(domain, via, usedIP, start, up, down, true, "")
}

// countingWriter 记录写入进度的包装（watchdog 据此判断空闲）
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// tunnel 双向搬运并半关闭，返回 (客户端→上游, 上游→客户端) 字节数。
// DEBT-2：空闲超过 idleTimeout 即双向强制关闭，防止半开连接泄漏 goroutine。
func tunnel(ctx context.Context, cancel context.CancelFunc, a, b net.Conn, idleTimeout time.Duration) (int64, int64) {
	var up, down atomic.Int64
	done := make(chan int64, 2)
	cp := func(dst io.Writer, counter *atomic.Int64, src io.Reader) {
		n, _ := io.Copy(&countingWriter{w: dst, n: counter}, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- n
	}
	go cp(b, &up, a)
	go cp(a, &down, b)

	finished := make(chan struct{})
	defer close(finished)
	tick := idleTimeout / 4
	if tick <= 0 {
		tick = 50 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	go func() {
		var lastUp, lastDown int64 = -1, -1
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				_ = a.Close()
				_ = b.Close()
				return
			case <-ticker.C:
				cu, cd := up.Load(), down.Load()
				if cu == lastUp && cd == lastDown { // 一个检测周期内零字节
					_ = a.Close()
					_ = b.Close()
					return
				}
				lastUp, lastDown = cu, cd
			}
		}
	}()

	u, d := <-done, <-done
	cancel() // 任一方向结束后取消 ctx，促使另一方向的 watchdog/读写尽快收尾
	return u, d
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

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout > 0 {
		return s.IdleTimeout
	}
	return 5 * time.Minute
}
