// Package config 配置持久化：~/.light-github/config.json，
// 优先级 命令行 flags（显式设置）> 配置文件 > 内置默认值。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 默认值（与 cmd flags 默认保持一致）
const (
	DefaultAddr     = "127.0.0.1:12800"
	DefaultRefresh  = time.Hour
	DefaultLogLevel = "info"
)

// Config 用户配置。字段约定：
//   - Addr / Token：重启生效（监听建立后不可变）
//   - Refresh / LogLevel：热生效（Web UI 修改后立即应用）
type Config struct {
	Addr     string        `json:"addr"`
	Token    string        `json:"token,omitempty"`
	Refresh  time.Duration `json:"refresh"`
	LogLevel string        `json:"logLevel"`
}

func defaults() Config {
	return Config{
		Addr:     DefaultAddr,
		Refresh:  DefaultRefresh,
		LogLevel: DefaultLogLevel,
	}
}

// Path 返回配置文件路径（~/.light-github/config.json）。
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".light-github", "config.json")
}

// Load 读取配置；缺文件或坏 JSON 均降级为默认值（配置问题不应阻断启动）。
func Load(path string) (Config, error) {
	cfg := defaults()

	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil // 缺文件：默认值，非错误
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaults(), nil // 坏 JSON：降级默认（不打断启动，问题在 UI/日志可见）
	}

	// 字段级兜底：半损文件（缺字段）不应产生零值
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = DefaultRefresh
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = DefaultLogLevel
	}
	return cfg, nil
}

// Save 原子写盘（tmp + rename），父目录自动创建。
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Overlay 用显式设置的 flags 覆盖文件配置。
// explicit 为 flag.Visit 产物 map[flag名]值（只含用户显式传入的项）。
func Overlay(file Config, explicit map[string]any) Config {
	if v, ok := explicit["addr"].(string); ok && v != "" {
		file.Addr = v
	}
	if v, ok := explicit["token"].(string); ok {
		file.Token = v
	}
	if v, ok := explicit["refresh"].(time.Duration); ok && v > 0 {
		file.Refresh = v
	}
	if v, ok := explicit["logLevel"].(string); ok && v != "" {
		file.LogLevel = v
	}
	return file
}
