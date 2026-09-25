// Package doh 提供 RFC 8484 JSON 变体的 DoH 多端点解析客户端。
package doh

import (
	"context"
	"net"
)

// Client 多端点 DoH 解析器。
type Client struct{}

// New 构造客户端。TODO: TDD RED 骨架。
func New(endpoints ...string) *Client { return &Client{} }

// Resolve 并发查询所有端点，返回去重后的公网 IPv4 并集；部分端点失败被容忍。
func (c *Client) Resolve(ctx context.Context, domain string) ([]net.IP, error) {
	return nil, nil
}
