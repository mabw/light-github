// Package logx 日志系统：运行日志与连接日志双通道，
// 各自 fan-out 到「内存环形缓冲（供 Web UI 秒开）」与「lumberjack 轮转文件（供事后排错）」。
//
// 体积可控（docs/DESIGN.md M2）：app.log / conn.log 各 5MB×2 备份压缩，
// 理论上限 ~30MB，实际压缩后通常 <15MB。
package logx

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// 日志通道 kind（Web UI 的 ?kind= 参数值）
const (
	KindApp  = "app"
	KindConn = "conn"
)

// Options 日志系统配置（零值字段由 New 归一化为默认值）。
type Options struct {
	Dir        string // 日志目录（~/.light-github/logs）
	Level      slog.Level
	AppMaxMB   int  // app.log 单文件轮转阈值（默认 5）
	ConnMaxMB  int  // conn.log 单文件轮转阈值（默认 5）
	MaxBackups int  // 各自保留的轮转备份数（默认 2）
	Compress   bool // 备份是否 gzip（默认 false，cmd 层显式开启）
	AppRing    int  // 运行日志内存环形容量（默认 500，供 UI）
	ConnRing   int  // 连接日志内存环形容量（默认 1000，供 UI）
	Stderr     bool // 运行日志同时输出终端（cmd 常驻进程用；测试关闭避免噪音）
}

// Entry 内存缓冲中的单条日志（Web UI 数据形状）。
type Entry struct {
	At    time.Time      `json:"at"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Manager 双 logger 管理器。
type Manager struct {
	app      *slog.Logger
	conn     *slog.Logger
	appRing  *ring
	connRing *ring
	appFile  *lumberjack.Logger
	connFile *lumberjack.Logger
	appLevel *slog.LevelVar // 运行时可调（Web UI 设置热生效）
}

// New 构建日志系统（连接日志固定 INFO 级别——每条连接都是有效事件，无需级别开关）。
func New(opts Options) *Manager {
	opts = normalize(opts)

	appRing := newRing(opts.AppRing)
	connRing := newRing(opts.ConnRing)
	appFile := &lumberjack.Logger{
		Filename:   filepath.Join(opts.Dir, "app.log"),
		MaxSize:    opts.AppMaxMB,
		MaxBackups: opts.MaxBackups,
		Compress:   opts.Compress,
	}
	connFile := &lumberjack.Logger{
		Filename:   filepath.Join(opts.Dir, "conn.log"),
		MaxSize:    opts.ConnMaxMB,
		MaxBackups: opts.MaxBackups,
		Compress:   opts.Compress,
	}

	level := new(slog.LevelVar)
	level.Set(opts.Level)

	handlers := []slog.Handler{
		newRingHandler(appRing, level),
		slog.NewJSONHandler(appFile, &slog.HandlerOptions{Level: level}),
	}
	if opts.Stderr {
		handlers = append(handlers,
			slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}

	return &Manager{
		app: slog.New(newFanout(handlers...)),
		conn: slog.New(newFanout(
			newRingHandler(connRing, slog.LevelInfo),
			slog.NewJSONHandler(connFile, &slog.HandlerOptions{Level: slog.LevelInfo}),
		)),
		appRing:  appRing,
		connRing: connRing,
		appFile:  appFile,
		connFile: connFile,
		appLevel: level,
	}
}

// SetLevel 运行时调整运行日志级别（内存/文件/终端三路同时生效）。
func (m *Manager) SetLevel(l slog.Level) { m.appLevel.Set(l) }

// App 运行日志（启停/规则刷新/DoH 与源失败/测速决策）。
func (m *Manager) App() *slog.Logger { return m.app }

// Conn 连接日志（每条 CONNECT 隧道一行：domain/via/ip/dial/bytes/ok）。
func (m *Manager) Conn() *slog.Logger { return m.conn }

// Recent 返回 kind 通道最新 n 条（最新在前）；未知 kind 返回空。
func (m *Manager) Recent(kind string, n int) []Entry {
	switch kind {
	case KindApp:
		return m.appRing.recent(n)
	case KindConn:
		return m.connRing.recent(n)
	default:
		return nil
	}
}

// Close 关闭底层轮转文件（触发备份压缩收尾）。
// 两个都关（review M9：首错短路会让 connFile 永不关闭、压缩收尾丢失）。
func (m *Manager) Close() error {
	return errors.Join(m.appFile.Close(), m.connFile.Close())
}

func normalize(o Options) Options {
	if o.AppMaxMB <= 0 {
		o.AppMaxMB = 5
	}
	if o.ConnMaxMB <= 0 {
		o.ConnMaxMB = 5
	}
	if o.MaxBackups <= 0 {
		o.MaxBackups = 2
	}
	if o.AppRing <= 0 {
		o.AppRing = 500
	}
	if o.ConnRing <= 0 {
		o.ConnRing = 1000
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	_ = os.MkdirAll(o.Dir, 0o755)
	return o
}
