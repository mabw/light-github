// Package metrics 连接级日志、流量累计与滑动窗口实时速率
// （滑动窗口思路借鉴 Watt Toolkit 的 FlowAnalyzer，DESIGN.md §3.4）。
package metrics

import (
	"sync"
	"time"
)

// ConnInfo 单条连接记录。
type ConnInfo struct {
	At     time.Time
	Domain string
	Via    string // fixed-ip / cname / dynamic / direct
	IP     string
	DialMS int64
	Up     int64 // 客户端→上游字节
	Down   int64 // 上游→客户端字节
	OK     bool
	Err    string
}

// Snapshot 某时刻统计快照。
type Snapshot struct {
	TotalUp      int64         `json:"totalUp"`
	TotalDown    int64         `json:"totalDown"`
	RateUp       float64       `json:"rateUp"` // B/s
	RateDown     float64       `json:"rateDown"`
	TotalConns   int64         `json:"totalConns"`
	FailedConns  int64         `json:"failedConns"`
	WindowSecond time.Duration `json:"-"`
}

type sample struct {
	at time.Time
	up int64
	dn int64
}

// Store 并发安全的指标存储。
type Store struct {
	window time.Duration

	// OnRecord 每条连接记录后的同步回调（锁外调用）。
	// M2 用途：logx 把连接日志落盘；nil 则跳过。
	OnRecord func(ConnInfo)

	mu        sync.Mutex
	totalUp   int64
	totalDown int64
	conns     int64
	failed    int64
	samples   []sample
	connLog   []ConnInfo // 环形缓冲（最新在前）
	logMax    int
}

// NewStore 构造；window 为实时速率滑动窗口（设计值 5s）。
func NewStore(window time.Duration) *Store {
	return &Store{window: window, logMax: 1000}
}

// Add 累计流量字节。
func (s *Store) Add(up, down int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalUp += up
	s.totalDown += down
	s.samples = append(s.samples, sample{at: time.Now(), up: up, dn: down})
	s.trimLocked()
}

// RecordConn 记录一条连接（同时累计其流量并喂入速率窗口——DEBT-1：
// proxy 只调用本方法，不喂窗口会导致实时速率恒为 0）。
func (s *Store) RecordConn(info ConnInfo) {
	info.At = time.Now()
	s.mu.Lock()
	s.totalUp += info.Up
	s.totalDown += info.Down
	s.conns++
	if !info.OK {
		s.failed++
	}
	if info.Up > 0 || info.Down > 0 {
		s.samples = append(s.samples, sample{at: info.At, up: info.Up, dn: info.Down})
		s.trimLocked()
	}
	s.connLog = append([]ConnInfo{info}, s.connLog...)
	if len(s.connLog) > s.logMax {
		s.connLog = s.connLog[:s.logMax]
	}
	s.mu.Unlock()

	if s.OnRecord != nil {
		s.OnRecord(info) // 锁外回调，实现方自行保证并发安全
	}
}

// Conns 返回最近 n 条连接日志（最新在前）。
func (s *Store) Conns(n int) []ConnInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.connLog) {
		n = len(s.connLog)
	}
	out := make([]ConnInfo, n)
	copy(out, s.connLog[:n])
	return out
}

// Snapshot 当前统计；速率 = 窗口内字节 / 窗口时长。
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var winUp, winDn int64
	cutoff := time.Now().Add(-s.window)
	for _, sp := range s.samples {
		if sp.at.After(cutoff) {
			winUp += sp.up
			winDn += sp.dn
		}
	}
	sec := s.window.Seconds()
	return Snapshot{
		TotalUp: s.totalUp, TotalDown: s.totalDown,
		RateUp: float64(winUp) / sec, RateDown: float64(winDn) / sec,
		TotalConns: s.conns, FailedConns: s.failed,
		WindowSecond: s.window,
	}
}

// trimLocked 惰性裁剪过期样本（调用方持锁）
func (s *Store) trimLocked() {
	cutoff := time.Now().Add(-s.window)
	idx := len(s.samples)
	for i, sp := range s.samples {
		if sp.at.After(cutoff) {
			idx = i
			break
		}
	}
	s.samples = s.samples[idx:]
}
