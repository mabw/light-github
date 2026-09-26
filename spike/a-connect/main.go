// Spike A：CONNECT 隧道代理 MVP
//
// 验证目标（DESIGN.md §3.1 / §3.2）：
//  1. 纯 stdlib 实现 CONNECT 代理，监听 127.0.0.1:12800（不占 443）
//  2. 白名单域名走固定 IP 直连（TLS 的 SNI 保持原域名，已实测可行）
//  3. 非白名单域名原样直通（"开了等于没开"）
//  4. 连接粒度日志：域名 / 出口 / 拨号耗时 / 双向字节数
//
// 验收：curl -x http://127.0.0.1:12800 https://github.com 返回 200；
//
//	git -c http.proxy=http://127.0.0.1:12800 clone ... 成功
//	（git https clone 的 /git-upload-pack 在 github.com 域名下，无需 codeload）
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// dialTimeout 单个候选 IP 的 TCP 建连超时
const dialTimeout = 5 * time.Second

// fixedRules 白名单：域名 → 固定出口 IP。
// IP 来自 steampp 数据实测（DESIGN.md §3.3 数据源归一化规则）。
var fixedRules = map[string]string{
	"github.com":     "20.207.73.82",   // steampp「Github 网站」Forward 固定 IP
	"api.github.com": "20.205.243.168", // githubapi.rmbgame.net CNAME 链解析结果
}

func matchFixed(host string) (string, bool) {
	ip, ok := fixedRules[strings.ToLower(host)]
	return ip, ok
}

// rewritePort 把 host:port 的地址部分换成 ip，端口保留
func rewritePort(host, ip string) string {
	if _, port, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(ip, port)
	}
	return net.JoinHostPort(ip, "443")
}

func main() {
	const addr = "127.0.0.1:12800"
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				http.Error(w, "spike: only CONNECT supported", http.StatusBadRequest)
				return
			}
			handleConnect(w, r)
		}),
	}
	log.Printf("[spike-a] CONNECT proxy listening on http://%s", addr)
	log.Fatal(server.ListenAndServe())
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Host // 形如 github.com:443
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
	}
	start := time.Now()

	// 出口候选（按优先级）：白名单域名 → [固定IP, 原目标直连兜底]；其余 → [直连]。
	// 单固定 IP 实测不稳定（dial 一次 154ms、下一次即超时），必须有 fallback。
	// 完整的多 IP DoH 候选 + 测速排序是 spike-b 的职责。
	var candidates []string
	if ip, ok := matchFixed(hostname); ok {
		candidates = append(candidates, rewritePort(host, ip), host)
	} else {
		candidates = append(candidates, host)
	}

	var (
		upstream net.Conn
		via      string
	)
	for _, target := range candidates {
		upstream, err = net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			via = "fixed-ip"
			if target == host {
				via = "direct-fallback"
			}
			break
		}
		log.Printf("DIAL-FAIL %s target=%s err=%v", hostname, target, err)
	}
	if upstream == nil {
		http.Error(w, "all upstream candidates failed: "+err.Error(), http.StatusBadGateway)
		log.Printf("FAIL %s all-candidates-failed", hostname)
		return
	}
	defer upstream.Close()

	// 劫持客户端 TCP，回复 200 后开始纯字节隧道（不解密 TLS）
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		log.Printf("hijack %s err: %v", hostname, err)
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		log.Printf("write-200 %s err: %v", hostname, err)
		return
	}
	dialCost := time.Since(start)

	up, down := tunnel(client, upstream)
	log.Printf("DONE %s via=%s dial=%v c2s=%dB s2c=%dB",
		hostname, via, dialCost.Round(time.Millisecond), up, down)
}

// tunnel 双向搬运并半关闭，返回 (客户端→上游, 上游→客户端) 字节数
func tunnel(a, b net.Conn) (int64, int64) {
	done := make(chan int64, 2)
	cp := func(dst io.Writer, src io.Reader) {
		n, _ := io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite() // 半关闭促使另一方向自然结束
		}
		done <- n
	}
	go cp(b, a)
	go cp(a, b)
	return <-done, <-done
}
