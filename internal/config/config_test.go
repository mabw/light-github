package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "不存在.json")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("缺文件应返回默认值而非报错: %v", err)
	}
	if cfg.Addr != DefaultAddr || cfg.Refresh != DefaultRefresh {
		t.Fatalf("默认值: %+v", cfg)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("日志级别默认 info: %q", cfg.LogLevel)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	want := Config{
		Addr:    "0.0.0.0:12800",
		Token:   "t0k3n",
		Refresh: 2 * time.Hour,
		LogLevel: "debug",
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("保存: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if got != want {
		t.Fatalf("回环不一致:\nwant %+v\ngot  %+v", want, got)
	}
}

func TestLoad_BrokenJSONFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"addr": "broken`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("坏 JSON 应降级默认值（不阻断启动）: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Fatalf("坏 JSON 应回退默认: %+v", cfg)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "config.json")

	if err := Save(path, Config{Addr: DefaultAddr}); err != nil {
		t.Fatalf("父目录不存在应自动创建: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件应存在: %v", err)
	}
}

// 显式设置的 flags（flag.Visit 产物）覆盖配置文件值；未设置项保留文件值
func TestOverlay(t *testing.T) {
	fileCfg := Config{Addr: "127.0.0.1:9999", Token: "from-file", Refresh: time.Hour, LogLevel: "debug"}

	// 只显式设置 addr
	got := Overlay(fileCfg, map[string]any{"addr": "0.0.0.0:12800"})

	if got.Addr != "0.0.0.0:12800" {
		t.Fatalf("显式 flag 应覆盖: %+v", got)
	}
	if got.Token != "from-file" {
		t.Fatalf("未设置 flag 应保留文件值: %+v", got)
	}
	if got.Refresh != time.Hour || got.LogLevel != "debug" {
		t.Fatalf("未设置项保留文件值: %+v", got)
	}
}

// 显式设置即使恰好等于默认值也应覆盖文件值（不能靠「值==默认值」猜是否显式）
func TestOverlay_ExplicitEqualsDefaultStillOverrides(t *testing.T) {
	fileCfg := Config{Addr: "127.0.0.1:9999", Token: "t", Refresh: time.Hour, LogLevel: "info"}

	got := Overlay(fileCfg, map[string]any{"addr": DefaultAddr})
	if got.Addr != DefaultAddr {
		t.Fatalf("显式设置即使等于默认值也应覆盖: %+v", got)
	}
}

func TestPath(t *testing.T) {
	home := t.TempDir() // 同一目录只取一次（多次 t.TempDir() 返回不同路径）
	t.Setenv("HOME", home)

	if p := Path(); p != filepath.Join(home, ".light-github", "config.json") {
		t.Fatalf("路径应为 ~/.light-github/config.json: %s", p)
	}
}
