package metrics

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestStore_AccumulatesTotals(t *testing.T) {
	s := NewStore(time.Second)
	s.RecordConn(ConnInfo{Up: 100, Down: 50})
	s.RecordConn(ConnInfo{Up: 30, Down: 20})
	snap := s.Snapshot()
	if snap.TotalUp != 130 || snap.TotalDown != 70 {
		t.Fatalf("累计错误: %+v", snap)
	}
}

func TestStore_WindowRate(t *testing.T) {
	s := NewStore(200 * time.Millisecond)
	s.RecordConn(ConnInfo{Up: 1000, Down: 500})

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
			s.RecordConn(ConnInfo{Up: 1, Down: 1, OK: true})
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

// DEBT-1：RecordConn 也必须产生速率样本（proxy 只调 RecordConn 不调 Add，
// 否则 Web UI 的实时速率恒为 0）
func TestStore_RecordConnFeedsRateWindow(t *testing.T) {
	s := NewStore(time.Second)
	s.RecordConn(ConnInfo{Domain: "github.com", Via: "accel", OK: true, Up: 512, Down: 4096})

	snap := s.Snapshot()
	if snap.RateUp <= 0 || snap.RateDown <= 0 {
		t.Fatalf("RecordConn 后窗口内应有速率: %+v", snap)
	}
}

// OnRecord 钩子：RecordConn 时同步回调（logx 落盘连接日志的挂点）
func TestStore_OnRecordHook(t *testing.T) {
	var got []ConnInfo
	s := NewStore(time.Second)
	s.OnRecord = func(i ConnInfo) { got = append(got, i) }

	s.RecordConn(ConnInfo{Domain: "github.com", Via: "fixed-ip"})
	s.RecordConn(ConnInfo{Domain: "api.github.com"})

	if len(got) != 2 || got[0].Domain != "github.com" || got[1].Domain != "api.github.com" {
		t.Fatalf("钩子应收到每条连接: %+v", got)
	}
}

// 环形覆盖：写入超过 logMax 后保留最后 logMax 条且最新在前（review M1）
func TestStore_ConnLogRingOverwrite(t *testing.T) {
	s := NewStore(time.Second)
	s.logMax = 5
	for i := 0; i < 12; i++ {
		s.RecordConn(ConnInfo{Domain: fmt.Sprintf("d%d", i), OK: true})
	}
	conns := s.Conns(10)
	if len(conns) != 5 {
		t.Fatalf("应保留 logMax 条: %d", len(conns))
	}
	if conns[0].Domain != "d11" || conns[4].Domain != "d7" {
		t.Fatalf("应保留 d7..d11 且最新在前: %v", conns)
	}
}
