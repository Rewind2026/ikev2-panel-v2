// 环境变量 + 默认值。
// 设计见 docs/design.md §5.1
//
// v2.86-PR13.0 重构:本包只负责 env 解析,**不再 import panelstate**。
// panelstate + env 合并逻辑移到 internal/runtime(由 runtime.Merge 完成)。
// 修复审查报告 C3 提出的"config 反向依赖 panelstate"模块错位。
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
	DataDir        string // 持久化目录（DB + 证书）
	ListenAddr     string // HTTPS 监听地址
	HTTPListenAddr string // 可选明文 HTTP 监听地址（v2.86-PR17,空=不启用）
	LogLevel       string // debug / info / warn / error
	LogFormat      string // text / json

	// 服务器身份
	ServerCN     string // 服务器证书 CN（mobileconfig 里 RemoteIdentifier）
	ServerAddrV6 string // 服务器 IPv6 地址（mobileconfig 用）
	ServerAddrV4 string // 服务器 IPv4 地址（仅 IPv6_ONLY=false 时用）
	IPv6Only     bool   // 默认 true
	OutIf        string // 出接口（updown 脚本限速用）

	// 证书模式
	CertMode    string // self-signed / letsencrypt
	Domain      string // LE 模式必填
	ACMEEmail   string // LE 注册邮箱
	ACMEWebroot string // HTTP-01 webroot，默认 /var/www/acme

	// DDNS (v2-82 新增,v2-84 family-aware)
	DDNSEnabled           bool   // 总开关 (env IKEV2_DDNS_ENABLED,默认 false)
	DDNSFamily            string // v2-84:同步哪些 family — v4 / v6 / dual(默认 dual)
	DDNSProbeTarget       string // v2-84:IPv4 探测目标(默认 8.8.8.8,中国大陆可改 223.5.5.5)
	AliyunAccessKeyID     string // 阿里云 RAM AccessKey ID(env,见 v2-83)
	AliyunAccessKeySecret string // 阿里云 RAM AccessKey Secret
	AliyunAccessKeySource string // 凭证来源: env-new / env-legacy-ddns / env-legacy-acme.sh / ""
	// v2.86-PR12.5:CertMode/Domain/CN/Email 字段来源 env(panelstate 合并在 runtime 包做)
	CertConfigSource string
	AliyunDomain     string // 主域名(如 example.com)
	AliyunRR         string // 主机记录(如 vpn → vpn.example.com)

	// v2.86-PR13.3:客户端虚拟 IP 段(IPv4 pool CIDR / IPv6 ULA pool CIDR)
	//
	// 来源链(env → panelstate → entrypoint auto-detect):
	//   1. /data/panel-state/subnet.conf (面板运行时,需重启容器生效)
	//   2. IKEV2_VPN_SUBNET / IKEV2_VPN_SUBNET_V6 env (老用户兼容,启动期生效)
	//   3. entrypoint §0.6 自动探测 (没填没传时)
	//
	// 这里只存 env 直读值;panelstate 合并由 runtime.Merge 完成(对称 cert.conf)。
	IPv4Subnet         string // IKEV2_VPN_SUBNET(默认空 = 让 entrypoint auto-detect)
	IPv6Subnet         string // IKEV2_VPN_SUBNET_V6(默认空)
	SubnetConfigSource string // 来源标记: panelstate / env-default / ""

	// 用户密码
	DefaultUserPasswordLen int

	// Session
	SessionTTL   time.Duration
	CookieSecure bool // cookie Secure 标志；dev 测试可置 false

	// v2.85-PR3:面板/DDNS/mobileconfig 时间显示时区(IANA TZ 名,默认 UTC)。
	// 只影响渲染字符串,不改容器 TZ、证书有效期、cron 计算。
	DisplayTimezone string
}

// Load 从环境变量读取配置，必要时填默认值。
//
// v2.86-PR13.0 重构:本函数**只读 env**,不再读 panelstate 文件。
// panelstate 合并由 runtime.Merge(env) 完成。
func Load() (*Config, error) {
	c := &Config{
		DataDir:                getenv("IKEV2_DATA_DIR", "/data"),
		ListenAddr:             getenv("IKEV2_LISTEN_ADDR", "0.0.0.0:8443"),
		HTTPListenAddr:         getenv("IKEV2_HTTP_LISTEN_ADDR", ""),
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

		// DDNS (v2-82 + v2-83 凭证统一 + v2-84 family)
		DDNSEnabled:           getbool("IKEV2_DDNS_ENABLED", false),
		DDNSFamily:            parseFamily(getenv("IKEV2_DDNS_FAMILY", "dual")),
		DDNSProbeTarget:       getenv("IKEV2_DDNS_PROBE_TARGET", "8.8.8.8"),
		AliyunDomain:          getenv("ALIYUN_DOMAIN", ""),
		AliyunRR:              getenv("ALIYUN_RR", "vpn"),

		// v2.85-PR3:显示时区。LoadLocation 失败时 fallback UTC + stderr WARN
		DisplayTimezone: loadDisplayTimezone(),
	}

	// Aliyun 凭证(env 4 级优先级):从 env 读,不在 Load 里碰 panelstate
	aliyunID, aliyunSecret, aliyunSource := loadAliyunCredsFromEnv()
	c.AliyunAccessKeyID = aliyunID
	c.AliyunAccessKeySecret = aliyunSecret
	c.AliyunAccessKeySource = aliyunSource

	// Cert 配置(env 来源标记,实际值可能被 runtime.Merge 用 panelstate 覆盖)
	c.CertConfigSource = "env-default"

	// v2.86-PR13.3:客户端虚拟 IP 段 env 直读;空 = 让 entrypoint §0.6 auto-detect。
	// panelstate 合并在 runtime.Merge(env) 里做(对称 cert.conf)。
	c.IPv4Subnet = getenv("IKEV2_VPN_SUBNET", "")
	c.IPv6Subnet = getenv("IKEV2_VPN_SUBNET_V6", "")
	c.SubnetConfigSource = "env-default"

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadAliyunCredsFromEnv 从 env 读阿里云凭证(env 4 级优先级,跟 v2 一致)。
//
// v2.86-PR13.0:从原 config.go 抽出,只读 env,不碰 panelstate。
// panelstate 读盘交给 runtime.Merge。
func loadAliyunCredsFromEnv() (id, secret, source string) {
	type envSrc struct {
		idEnv, secretEnv, label string
	}
	envs := []envSrc{
		{"IKEV2_ALIYUN_KEY_ID", "IKEV2_ALIYUN_KEY_SECRET", "env-new"},
		{"ALIYUN_ACCESS_KEY_ID", "ALIYUN_ACCESS_KEY_SECRET", "env-legacy-ddns"},
		{"Ali_Key", "Ali_Secret", "env-legacy-acme.sh"},
	}
	for _, e := range envs {
		id = getenv(e.idEnv, "")
		secret = getenv(e.secretEnv, "")
		if id != "" && secret != "" {
			// 用 deprecated 名时,启动时 WARN 一次
			if e.label != "env-new" {
				fmt.Fprintf(os.Stderr,
					"WARN: aliyun credentials using deprecated env %s; rename to IKEV2_ALIYUN_KEY_ID/IKEV2_ALIYUN_KEY_SECRET (will be removed in v3.0.0)\n",
					e.idEnv)
			}
			return id, secret, e.label
		}
	}
	return "", "", ""
}

// loadDisplayTimezone 读取 IKEV2_DISPLAY_TIMEZONE,LoadLocation 失败时
// fallback UTC 并打 stderr WARN(不 fatal,容器仍能起来)。
func loadDisplayTimezone() string {
	tz := getenv("IKEV2_DISPLAY_TIMEZONE", "UTC")
	if _, err := time.LoadLocation(tz); err != nil {
		fmt.Fprintf(os.Stderr,
			"WARN: IKEV2_DISPLAY_TIMEZONE=%q invalid (%v); falling back to UTC\n",
			tz, err)
		return "UTC"
	}
	return tz
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

// parseFamily v2-84:校验 + 规范化 DDNS family 值。
func parseFamily(s string) string {
	switch s {
	case "", "v4", "v6", "dual":
		if s == "" {
			return "dual"
		}
		return s
	default:
		fmt.Fprintf(os.Stderr,
			"WARN: IKEV2_DDNS_FAMILY=%q invalid, supported: v4, v6, dual; falling back to dual\n",
			s)
		return "dual"
	}
}