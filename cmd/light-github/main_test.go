package main

import "testing"

// DEBT-6：非 loopback 监听强制 Token
func TestValidateListen(t *testing.T) {
	cases := []struct {
		addr, token string
		wantErr     bool
	}{
		{"127.0.0.1:12800", "", false},   // 默认回环免鉴权
		{"localhost:12800", "", false},   // localhost 同样回环
		{"0.0.0.0:12800", "", true},      // 全网卡无 Token 拒绝
		{":12800", "", true},             // 省略 host = 绑全部网卡（review C3：ParseIP("") 为 nil 曾绕过）
		{"192.168.1.10:12800", "", true}, // 指定外发网卡无 Token 拒绝
		{"0.0.0.0:12800", "s3cret", false},
		{":12800", "s3cret", false},
		{"bad-addr", "", true}, // 格式异常早失败
	}
	for _, c := range cases {
		err := validateListen(c.addr, c.token)
		if (err != nil) != c.wantErr {
			t.Errorf("validateListen(%q, %q) err=%v, wantErr=%v", c.addr, c.token, err, c.wantErr)
		}
	}
}
