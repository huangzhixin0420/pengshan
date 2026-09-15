// Package config 管理 ~/.pengshan/ 下的运行时配置。
// 布局：config.json（本文件）、identity.key（daemon 长期私钥）、pengshan.db（SQLite）、logs/（JSONL）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DirName  = ".pengshan"
	FileName = "config.json"
)

// Config 是 pengshan daemon 的持久配置。
type Config struct {
	// ServeAddr 是本地 hermes serve 的 HTTP 地址，如 "127.0.0.1:9121"。
	// 空 = 未配置，bridge 拒连（doctor 提示）。
	ServeAddr string `json:"serve_addr,omitempty"`
	// RelayURLs 是可用的 relay 地址（wss://…），优先级从高到低。
	RelayURLs []string `json:"relay_urls,omitempty"`
	// RelayToken 是 /control Bearer（公网 relay 鉴权；本机回环可空）。
	RelayToken string `json:"relay_token,omitempty"`
	// LogLevel: debug|info|warn|error。
	LogLevel string `json:"log_level,omitempty"`
}

// Dir 返回 ~/.pengshan（家目录解析失败时退到 cwd/.pengshan）。
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", DirName)
	}
	return filepath.Join(home, DirName)
}

func path() string { return filepath.Join(Dir(), FileName) }

// Load 读配置；文件不存在返回零值 Config 与 nil error。
func Load() (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save 原子写配置（先写临时文件再 rename，防中途断电留半个 JSON）。
func Save(cfg *Config) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path()); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
