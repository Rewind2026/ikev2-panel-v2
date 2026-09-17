// 环境变量 + 默认值。
// 设计见 docs/design.md §5.1
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// 基础
	DataDir    string // 持久化目录（DB + 证书）
	ListenAddr string // HTTPS 监听地址
	LogLevel   string // debug / info / warn / error
	LogFormat  string // text / json

	// 服务器身份
	ServerCN       string // 服务器证书 CN（mobileconfig 里 RemoteIdentifier）
	ServerAddrV6   string // 服务器 IPv6 地址（mobileconfig 用）
	ServerAddrV4   string // 服务器 IPv4 地址（仅 IPv6_ONLY=false 时用）
	IPv6Only       bool   // 默认 true
	OutIf          string // 出接口（updown 脚本限速用）

	// 证书模式
	CertMode    string // self-signed / letsencrypt
	Domain      string // LE 模式必填
	ACMEEmail   string // LE 注册邮箱
	ACMEWebroot string // HTTP-01 webroot，默认 /var/www/acme

	// 用户密码
	DefaultUserPasswordLen int

	// Session
	SessionTTL  time.Duration
	CookieSecure bool // cookie Secure 标志；dev 测试可置 false
}

// Load 从环境变量读取配置，必要时填默认值。
func Load() (*Config, error) {
	c := &Config{
		DataDir:                getenv("IKEV2_DATA_DIR", "/data"),
		ListenAddr:             getenv("IKEV2_LISTEN_ADDR", "0.0.0.0:8443"),
		LogLevel:               getenv("IKEV2_LOG_LEVEL", "info"),
		LogFormat:              getenv("IKEV2_LOG_FORMAT", "text"),
		ServerCN:               getenv("IKEV2_SERVER_CN", "ikev2.local"),
		ServerAddrV6:           getenv("IKEV2_SERVER_ADDR_V6", ""),
		ServerAddrV4:           getenv("IKEV2_SERVER_ADDR_V4", ""),
		IPv6Only:               getbool("IKEV2_IPV6_ONLY", true),
		OutIf:                  getenv("IKEV2_OUT_IF", "eth0"),
		CertMode:               getenv("IKEV2_CERT_MODE", "self-signed"),
		Domain:                 getenv("IKEV2_DOMAIN", ""),
		ACMEEmail:              getenv("IKEV2_ACME_EMAIL", ""),
		ACMEWebroot:            getenv("IKEV2_ACME_WEBROOT", "/var/www/acme"),
		DefaultUserPasswordLen: getint("IKEV2_DEFAULT_USER_PASSWORD_LEN", 12),
		SessionTTL:             getdur("IKEV2_SESSION_TTL", 24*time.Hour),
		CookieSecure:           getbool("IKEV2_COOKIE_SECURE", true),
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	switch c.CertMode {
	case "self-signed", "letsencrypt":
	default:
		return fmt.Errorf("IKEV2_CERT_MODE must be self-signed or letsencrypt, got %q", c.CertMode)
	}
	if c.CertMode == "letsencrypt" && c.Domain == "" {
		return fmt.Errorf("IKEV2_DOMAIN is required when IKEV2_CERT_MODE=letsencrypt")
	}
	if c.IPv6Only && c.ServerAddrV6 == "" {
		// 仅在生产启动时硬校验；本地开发可不填（entrypoint 不跑 IPv6 校验）
		// 这里允许空，启动后会由 caller 决定是否 fatal
	}
	if c.DefaultUserPasswordLen < 6 || c.DefaultUserPasswordLen > 64 {
		return fmt.Errorf("IKEV2_DEFAULT_USER_PASSWORD_LEN must be in [6,64], got %d", c.DefaultUserPasswordLen)
	}
	return nil
}

// NewLogger 构造 slog.Logger。
// M1 简单实现：text / json 两种格式 + 级别。
func (c *Config) NewLogger() *slog.Logger {
	var level slog.Level
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(c.LogFormat) == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func getint(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getbool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func getdur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}