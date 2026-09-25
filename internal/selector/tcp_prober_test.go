package selector

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMedianProber_ReachableIPIsFast(t *testing.T) {
	srv := httptest.NewServer(nil) // 任意 TCP 监听即可
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	p := &MedianProber{Count: 3, Timeout: time.Second, Port: port}
	cost := p.Probe(context.Background(), net.ParseIP(host))
	if cost >= time.Second {
		t.Fatalf("可达 IP 测速应远小于超时值: %v", cost)
	}
}

func TestMedianProber_UnreachableIPIsPenalized(t *testing.T) {
	p := &MedianProber{Count: 2, Timeout: 300 * time.Millisecond, Port: "443"}
	// 240.0.0.1 保留地址必然不可达
	cost := p.Probe(context.Background(), net.ParseIP("240.0.0.1"))
	if cost != 300*time.Millisecond {
		t.Fatalf("不可达 IP 应按超时值惩罚: %v", cost)
	}
}

func TestMedianProber_ContextCancelShortCircuits(t *testing.T) {
	p := &MedianProber{Count: 3, Timeout: 200 * time.Millisecond, Port: "443"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_ = p.Probe(ctx, net.ParseIP("240.0.0.1"))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("ctx 已取消应立即返回")
	}
}
