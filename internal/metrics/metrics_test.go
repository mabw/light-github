package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestStore_AccumulatesTotals(t *testing.T) {
	s := NewStore(time.Second)
	s.Add(100, 50)
	s.Add(30, 20)
	snap := s.Snapshot()
	if snap.TotalUp != 130 || snap.TotalDown != 70 {
		t.Fatalf("累计错误: %+v", snap)
	}
}

func TestStore_WindowRate(t *testing.T) {
	s := NewStore(200 * time.Millisecond)
	s.Add(1000, 500)

	snap := s.Snapshot()
	if snap.RateUp <= 0 || snap.RateDown <= 0 {
		t.Fatalf("窗口内应有速率: %+v", snap)
	}

	// 窗口过期后速率归零，但总量保留
	time.Sleep(300 * time.Millisecond)
	snap = s.Snapshot()
	if snap.RateUp != 0 || snap.RateDown != 0 {
		t.Fatalf("窗口过期速率应归零: %+v", snap)
	}
	if snap.TotalUp != 1000 || snap.TotalDown != 500 {
		t.Fatalf("总量应保留: %+v", snap)
	}
}

func TestStore_ConcurrentAdds(t *testing.T) {
	s := NewStore(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add(1, 1)
		}()
	}
	wg.Wait()
	snap := s.Snapshot()
	if snap.TotalUp != 100 || snap.TotalDown != 100 {
		t.Fatalf("并发累计错误: %+v", snap)
	}
}

func TestStore_Connections(t *testing.T) {
	s := NewStore(time.Second)
	s.RecordConn(ConnInfo{Domain: "github.com", Via: "fixed-ip", OK: true, Up: 10, Down: 20})
	s.RecordConn(ConnInfo{Domain: "github.com", Via: "fixed-ip", OK: false})
	conns := s.Conns(10)
	if len(conns) != 2 || !conns[1].OK || conns[0].OK {
		t.Fatalf("连接日志应为最新在前: %+v", conns)
	}
	if snap := s.Snapshot(); snap.TotalConns != 2 || snap.FailedConns != 1 {
		t.Fatalf("连接统计错误: %+v", snap)
	}
}
