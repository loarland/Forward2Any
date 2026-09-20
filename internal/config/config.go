// Package config 读取进程启动配置。
//
// 这些值只在「首次启动」时作为引导数据写入 settings 表，之后一律以数据库为准
// （见 store.Bootstrap）。这样后台里改端口/账号密码才有意义。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	DataDir   string
	Port      int
	AdminUser string
	AdminPass string // 为空时首启动随机生成并只打印一次，避免出现默认口令
	BaseURL   string
	LogLevel  string
}

func FromEnv() (*Config, error) {
	c := &Config{
		DataDir:   env("F2A_DATA_DIR", "./data"),
		AdminUser: env("F2A_ADMIN_USER", "admin"),
		AdminPass: os.Getenv("F2A_ADMIN_PASSWORD"),
		LogLevel:  env("F2A_LOG_LEVEL", "info"),
	}

	port, err := strconv.Atoi(env("F2A_PORT", "16000"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("F2A_PORT 不是合法端口: %q", os.Getenv("F2A_PORT"))
	}
	c.Port = port

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("解析 F2A_DATA_DIR: %w", err)
	}
	c.DataDir = abs

	c.BaseURL = strings.TrimRight(env("F2A_BASE_URL", fmt.Sprintf("http://localhost:%d", port)), "/")
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
