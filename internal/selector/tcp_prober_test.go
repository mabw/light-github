package selector

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

// TLS 握手正常且证书域匹配：测速结果为中位建连耗时（远小于超时值）。
// 注入 httptest 自签证书的根池（其 SAN 含 example.com）。
func TestMedianProber_TLSHandshakeOK(t *testing.T) {
	srv := httptest.NewTLSServer(nil) // 自签证书的 TLS 监听
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	p := &MedianProber{Count: 3, Timeout: time.Second, Port: port}
	cost := p.Probe(context.Background(), "example.com", net.ParseIP(host))
	if cost >= time.Second {
		t.Fatalf("TLS 可达 IP 测速应远小于超时值: %v", cost)
	}
}

// TCP 可达但 TLS 握手失败（M5：GFW 干扰常在 TLS 层，TCP 通不代表可用）
// 的「假好候选」必须按超时值惩罚——两次断网实测的主因
func TestMedianProber_PlainTCPRejected(t *testing.T) {
	srv := httptest.NewServer(nil) // 纯 HTTP 监听，TLS 握手必失败
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	p := &MedianProber{Count: 2, Timeout: 300 * time.Millisecond, Port: port}
	cost := p.Probe(context.Background(), "127.0.0.1", net.ParseIP(host))
	if cost != 300*time.Millisecond {
		t.Fatalf("TLS 握手失败的候选应按超时值惩罚: %v", cost)
	}
}

func TestMedianProber_UnreachableIPIsPenalized(t *testing.T) {
	p := &MedianProber{Count: 2, Timeout: 300 * time.Millisecond, Port: "443"}
	// 240.0.0.1 保留地址必然不可达（黑洞丢包型，非快速 RST）
	start := time.Now()
	cost := p.Probe(context.Background(), "example.com", net.ParseIP("240.0.0.1"))
	if cost != 300*time.Millisecond {
		t.Fatalf("不可达 IP 应按超时值惩罚: %v", cost)
	}
	// 拨号器必须自带 Timeout：零值 Dialer 的拨号只受 ctx 约束，
	// 黑洞 IP 会等 OS 级 TCP 超时（~75s/次）——实测曾致单用例 225s
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("全部探测应受 Timeout 约束，实际 %v", el)
	}
}

func TestMedianProber_ContextCancelShortCircuits(t *testing.T) {
	p := &MedianProber{Count: 3, Timeout: 200 * time.Millisecond, Port: "443"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_ = p.Probe(ctx, "example.com", net.ParseIP("240.0.0.1"))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("ctx 已取消应立即返回")
	}
}

// 证书域不匹配（借段候选可能连到非目标域的 HTTPS 服务器）必须拒绝：
// 实测 AWS 段 IP 能完成握手但证书非 github.com，曾以 InsecureSkipVerify 放行
func TestMedianProber_CertDomainMismatchRejected(t *testing.T) {
	srv := httptest.NewTLSServer(nil) // 自签证书，SAN 仅 127.0.0.1
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	p := &MedianProber{Count: 1, Timeout: 300 * time.Millisecond, Port: port}
	cost := p.Probe(context.Background(), "github.com", net.ParseIP(host)) // SNI/校验域=github.com
	if cost != 300*time.Millisecond {
		t.Fatalf("证书域不匹配的候选应按超时值惩罚: %v", cost)
	}
}
