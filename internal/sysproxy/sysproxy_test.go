package sysproxy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fake 执行器：记录调用并按脚本返回（不真改系统）
type fakeExec struct {
	calls   []string
	respond func(name string, args []string) (string, error)
}

func (f *fakeExec) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if f.respond != nil {
		return f.respond(name, args)
	}
	return "", nil
}

func withFake(t *testing.T) *fakeExec {
	t.Helper()
	f := &fakeExec{}
	old := execRun
	execRun = f.run
	invalidateState() // 隔离上一用例的 TTL 缓存（review H2 引入）
	t.Cleanup(func() {
		execRun = old
		invalidateState()
	})
	return f
}

const svcList = `An asterisk (*) denotes that a network service is disabled.
Wi-Fi
Thunderbolt Bridge
*Bluetooth PAN
`

// 服务列表解析：跳过标题与禁用（* 前缀）服务
func TestParseServices_SkipsHeaderAndDisabled(t *testing.T) {
	got := parseServices(svcList)
	if len(got) != 2 || got[0] != "Wi-Fi" || got[1] != "Thunderbolt Bridge" {
		t.Fatalf("解析结果: %v", got)
	}
}

// 接入：对每个启用服务执行 setautoproxyurl
func TestEnable_SetsPACPerService(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		return "", nil
	}

	if err := Enable("http://127.0.0.1:12800/pac"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "networksetup -setautoproxyurl Wi-Fi http://127.0.0.1:12800/pac") {
		t.Fatalf("应为 Wi-Fi 设置 PAC: %s", joined)
	}
	if !strings.Contains(joined, "networksetup -setautoproxyurl Thunderbolt Bridge http://127.0.0.1:12800/pac") {
		t.Fatalf("应为 Thunderbolt Bridge 设置 PAC: %s", joined)
	}
	if strings.Contains(joined, "Bluetooth PAN") {
		t.Fatalf("禁用服务应跳过: %s", joined)
	}
}

// 还原：对每个启用服务关闭自动代理
func TestDisable_TurnsOffPerService(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		return "", nil
	}

	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "networksetup -setautoproxystate Wi-Fi off") {
		t.Fatalf("应关闭 Wi-Fi 自动代理: %s", joined)
	}
}

// 状态查询：任一启用服务的 PAC URL 匹配且 Enabled: Yes 即认为已接入
func TestEnabled_MatchesURLAndState(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		switch {
		case strings.Contains(args[0], "listallnetworkservices"):
			return svcList, nil
		case strings.Contains(args[0], "-getautoproxyurl"):
			return "URL: http://127.0.0.1:12800/pac\nEnabled: Yes\n", nil
		}
		return "", nil
	}

	if !Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("URL 匹配且启用应返回 true")
	}
	if enabledUncached("http://127.0.0.1:99999/pac") {
		t.Fatal("URL 不匹配应返回 false")
	}
}

// Enabled: No（曾设置过后关闭）不算接入
func TestEnabled_IgnoresDisabledState(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		switch {
		case strings.Contains(args[0], "listallnetworkservices"):
			return svcList, nil
		case strings.Contains(args[0], "-getautoproxyurl"):
			return "URL: http://127.0.0.1:12800/pac\nEnabled: No\n", nil
		}
		return "", nil
	}

	if Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("Enabled: No 不应视为已接入")
	}
}

// ---- review H3：全部服务失败必须报错（否则退出还原假成功，PAC 悬空无告警）----

func TestEnable_AllServicesFailedReturnsError(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		return "", errors.New("networksetup: busy")
	}
	if err := Enable("http://127.0.0.1:12800/pac"); err == nil {
		t.Fatal("全部服务失败应返回错误")
	}
}

func TestDisable_AllServicesFailedReturnsError(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		return "", errors.New("networksetup: busy")
	}
	if err := Disable(); err == nil {
		t.Fatal("全部服务失败应返回错误")
	}
}

// 部分失败容忍（虚拟网卡/蓝牙异常不影响整体，review 保持原语义）
func TestEnable_PartialFailureStillOK(t *testing.T) {
	f := withFake(t)
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		if strings.Contains(strings.Join(args, " "), "Thunderbolt") {
			return "", errors.New("service error")
		}
		return "", nil
	}
	if err := Enable("http://127.0.0.1:12800/pac"); err != nil {
		t.Fatalf("部分失败应容忍: %v", err)
	}
}

// ---- review H4：execRun 必须有超时（挂死的 networksetup 会卡死退出清理路径）----

func TestExecRun_TimesOut(t *testing.T) {
	old := execTimeout
	execTimeout = 150 * time.Millisecond
	t.Cleanup(func() { execTimeout = old })

	start := time.Now()
	_, err := execRun("sleep", "5")
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("应按超时截断，实际耗时 %v", elapsed)
	}
}

// ---- review H2：状态查询 TTL 缓存 ----

func TestEnabled_TTLCache(t *testing.T) {
	f := withFake(t)
	queries := 0
	f.respond = func(name string, args []string) (string, error) {
		if strings.Contains(args[0], "listallnetworkservices") {
			return svcList, nil
		}
		if strings.Contains(args[0], "-getautoproxyurl") {
			queries++
			return "URL: http://127.0.0.1:12800/pac\nEnabled: Yes\n", nil
		}
		return "", nil
	}

	if !Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("首次查询")
	}
	n := queries
	for i := 0; i < 5; i++ { // TTL 内重复查询不再 fork 子进程
		if !Enabled("http://127.0.0.1:12800/pac") {
			t.Fatal("缓存值应保持 true")
		}
	}
	if queries != n {
		t.Fatalf("TTL 内应命中缓存：fork 次数 %d → %d", n, queries)
	}

	// TTL 过期后重新查询
	old := stateTTL
	stateTTL = time.Millisecond
	t.Cleanup(func() { stateTTL = old })
	time.Sleep(5 * time.Millisecond)
	_ = Enabled("http://127.0.0.1:12800/pac")
	if queries == n {
		t.Fatal("过期后应重新查询")
	}
}

// Enable 成功后立即查询不吃旧缓存（写路径主动失效）
func TestEnabled_InvalidatedByWrite(t *testing.T) {
	f := withFake(t)
	enabled := false
	f.respond = func(name string, args []string) (string, error) {
		switch {
		case strings.Contains(args[0], "listallnetworkservices"):
			return svcList, nil
		case strings.Contains(args[0], "-setautoproxyurl"):
			enabled = true
			return "", nil
		case strings.Contains(args[0], "-getautoproxyurl"):
			if enabled {
				return "URL: http://127.0.0.1:12800/pac\nEnabled: Yes\n", nil
			}
			return "URL: \nEnabled: No\n", nil
		}
		return "", nil
	}

	if Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("初始未接入")
	}
	if err := Enable("http://127.0.0.1:12800/pac"); err != nil {
		t.Fatal(err)
	}
	if !Enabled("http://127.0.0.1:12800/pac") {
		t.Fatal("Enable 后立即查询不应命中旧缓存")
	}
}
