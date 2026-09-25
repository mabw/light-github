package logx

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newManager(t *testing.T, opts Options) (*Manager, string) {
	t.Helper()
	opts.Dir = t.TempDir()
	if opts.Level == 0 {
		opts.Level = slog.LevelDebug
	}
	m := New(opts)
	t.Cleanup(func() { _ = m.Close() })
	return m, opts.Dir
}

// 运行日志写入后可从内存环形缓冲读回（最新在前，级别与属性完整）
func TestApp_RecentReturnsEntries(t *testing.T) {
	m, _ := newManager(t, Options{})

	m.App().Info("规则已刷新", "count", 67)
	m.App().Warn("DoH 单端点失败", "endpoint", "ali")

	es := m.Recent(KindApp, 10)
	if len(es) != 2 {
		t.Fatalf("应有 2 条, got %d", len(es))
	}
	if es[0].Msg != "DoH 单端点失败" || es[0].Level != "WARN" {
		t.Fatalf("最新在前且级别正确: %+v", es[0])
	}
	if es[0].Attrs["endpoint"] != "ali" {
		t.Fatalf("属性应保留: %+v", es[0].Attrs)
	}
	if es[1].Msg != "规则已刷新" {
		t.Fatalf("次新条目: %+v", es[1])
	}
}

// 环形缓冲容量截断：只保留最新 N 条
func TestRing_TruncatesToCapacity(t *testing.T) {
	m, _ := newManager(t, Options{AppRing: 3})

	for i := 0; i < 5; i++ {
		m.App().Info("msg", "i", i)
	}

	es := m.Recent(KindApp, 10)
	if len(es) != 3 {
		t.Fatalf("容量 3 应截断为 3 条, got %d", len(es))
	}
	// 最新在前：i=4,3,2；i=0,1 被挤出（slog 数值统一为 int64）
	if es[0].Attrs["i"] != int64(4) || es[2].Attrs["i"] != int64(2) {
		t.Fatalf("应保留最新 3 条: %v", es)
	}
}

// 级别过滤：低于设定级别的日志不进入内存与文件
func TestLevel_Filtering(t *testing.T) {
	m, dir := newManager(t, Options{Level: slog.LevelWarn})

	m.App().Info("不应出现")
	m.App().Error("应出现")

	if es := m.Recent(KindApp, 10); len(es) != 1 || es[0].Level != "ERROR" {
		t.Fatalf("INFO 应被过滤: %+v", es)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "app.log"))
	if strings.Contains(string(b), "不应出现") {
		t.Fatalf("文件中也不应有 INFO 日志: %s", b)
	}
}

// 连接日志与运行日志相互独立
func TestConn_SeparateFromApp(t *testing.T) {
	m, _ := newManager(t, Options{})

	m.App().Info("运行事件")
	m.Conn().Info("conn", "domain", "github.com", "ok", true, "down", int64(1024))

	if es := m.Recent(KindApp, 10); len(es) != 1 || es[0].Msg != "运行事件" {
		t.Fatalf("app 环形不应混入连接日志: %+v", es)
	}
	es := m.Recent(KindConn, 10)
	if len(es) != 1 || es[0].Attrs["domain"] != "github.com" || es[0].Attrs["ok"] != true {
		t.Fatalf("conn 环形应完整: %+v", es)
	}
}

// 运行日志落盘为 JSON 行（供事后排错）
func TestApp_WritesJSONLinesFile(t *testing.T) {
	m, dir := newManager(t, Options{})

	m.App().Info("hello-file", "k", "v")

	b, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatalf("app.log 应存在: %v", err)
	}
	if !strings.Contains(string(b), "hello-file") || !strings.Contains(string(b), `"k":"v"`) {
		t.Fatalf("文件应为含属性的 JSON 行: %s", b)
	}
}

// 连接日志落盘 conn.log
func TestConn_WritesFile(t *testing.T) {
	m, dir := newManager(t, Options{})

	m.Conn().Info("conn", "domain", "github.com")

	b, err := os.ReadFile(filepath.Join(dir, "conn.log"))
	if err != nil {
		t.Fatalf("conn.log 应存在: %v", err)
	}
	if !strings.Contains(string(b), "github.com") {
		t.Fatalf("连接日志应落盘: %s", b)
	}
}

// 文件超限触发轮转（lumberjack 集成；Compress=false 保证同步确定性）
func TestRotation_CreatesBackup(t *testing.T) {
	m, dir := newManager(t, Options{AppMaxMB: 1, MaxBackups: 2, Compress: false})

	big := strings.Repeat("x", 2048)
	for i := 0; i < 700; i++ { // ~1.4MB > 1MB
		m.App().Info(big, "i", i)
	}
	_ = m.Close()

	backups, _ := filepath.Glob(filepath.Join(dir, "app-*.log"))
	if len(backups) == 0 {
		t.Fatal("超过 MaxSize 应产生轮转备份")
	}
}

// Recent 的 n 参数限制返回条数；未知 kind 返回空
func TestRecent_LimitAndUnknownKind(t *testing.T) {
	m, _ := newManager(t, Options{})

	for i := 0; i < 5; i++ {
		m.App().Info("m", "i", i)
	}

	if es := m.Recent(KindApp, 2); len(es) != 2 || es[0].Attrs["i"] != int64(4) {
		t.Fatalf("n=2 应只返回最新 2 条: %+v", es)
	}
	if es := m.Recent("bogus", 10); len(es) != 0 {
		t.Fatalf("未知 kind 应返回空: %+v", es)
	}
}
