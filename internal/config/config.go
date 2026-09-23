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

	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
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
	AliyunAccessKeyID     string // 阿里云 RAM AccessKey ID(优先从 panelstate.aliyun.creds 读,见 v2-83)
	AliyunAccessKeySecret string // 阿里云 RAM AccessKey Secret
	AliyunAccessKeySource string // 凭证来源: panelstate / env-new / env-legacy-ddns / env-legacy-acme.sh / ""
	// v2.86-PR12.5:CertMode/Domain/CN/Email 字段来源 panelstate / env-default / env-override
	CertConfigSource string
	AliyunDomain          string // 主域名(如 example.com)
	AliyunRR              string // 主机记录(如 vpn → vpn.example.com)

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
func Load() (*Config, error) {
	// v2-83:阿里云凭证先尝试从面板运行时文件(/data/panel-state/aliyun.creds)读,
	// 读不到再 fallback 到 env。凭证来源写到 AliyunAccessKeySource,启动日志会输出。
	aliyunID, aliyunSecret, aliyunSource := loadAliyunCreds()

	// v2.86-PR12.5:证书配置(模式/域名/CN/邮箱)从面板运行时文件读优先,
	// fallback 到 env。跟 aliyun.creds 同优先级链风格,启动日志输出来源。
	certMode, certDomain, certCN, certEmail, certSource := loadCertConfig()

	c := &Config{
		DataDir:                getenv("IKEV2_DATA_DIR", "/data"),
		ListenAddr:             getenv("IKEV2_LISTEN_ADDR", "0.0.0.0:8443"),
		HTTPListenAddr:         getenv("IKEV2_HTTP_LISTEN_ADDR", ""),
		LogLevel:               getenv("IKEV2_LOG_LEVEL", "info"),
		LogFormat:              getenv("IKEV2_LOG_FORMAT", "text"),
		// v2.86-PR12.5:ServerCN 优先 panelstate,降级 env。
		ServerCN:               orDefault(certCN, getenv("IKEV2_SERVER_CN", "ikev2.local")),
		ServerAddrV6:           getenv("IKEV2_SERVER_ADDR_V6", ""),
		ServerAddrV4:           getenv("IKEV2_SERVER_ADDR_V4", ""),
		IPv6Only:               getbool("IKEV2_IPV6_ONLY", true),
		OutIf:                  getenv("IKEV2_OUT_IF", "eth0"),
		// v2.86-PR12.5:CertMode/Domain/ACMEEmail 优先 panelstate。
		CertMode:               orDefault(certMode, getenv("IKEV2_CERT_MODE", "self-signed")),
		Domain:                 orDefault(certDomain, getenv("IKEV2_DOMAIN", "")),
		ACMEEmail:              orDefault(certEmail, getenv("IKEV2_ACME_EMAIL", "")),
		ACMEWebroot:            getenv("IKEV2_ACME_WEBROOT", "/var/www/acme"),
		DefaultUserPasswordLen: getint("IKEV2_DEFAULT_USER_PASSWORD_LEN", 12),
		SessionTTL:             getdur("IKEV2_SESSION_TTL", 24*time.Hour),
		CookieSecure:           getbool("IKEV2_COOKIE_SECURE", true),

		// DDNS (v2-82 + v2-83 凭证统一 + v2-84 family)
		DDNSEnabled:           getbool("IKEV2_DDNS_ENABLED", false),
		DDNSFamily:            parseFamily(getenv("IKEV2_DDNS_FAMILY", "dual")),
		DDNSProbeTarget:       getenv("IKEV2_DDNS_PROBE_TARGET", "8.8.8.8"),
		AliyunAccessKeyID:     aliyunID,
		AliyunAccessKeySecret: aliyunSecret,
		AliyunAccessKeySource: aliyunSource,
		// v2.86-PR12.5:CertMode/Domain/CN/Email 来源 panelstate 还是 env。
		CertConfigSource: certSource,
		AliyunDomain:          getenv("ALIYUN_DOMAIN", ""),
		AliyunRR:              getenv("ALIYUN_RR", "vpn"),

		// v2.85-PR3:显示时区。LoadLocation 失败时 fallback UTC + stderr WARN
		//(跟 parseFamily 失败时的 fallback 策略一致,启动不 fatal)。
		DisplayTimezone: loadDisplayTimezone(),
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadDisplayTimezone 读取 IKEV2_DISPLAY_TIMEZONE,LoadLocation 失败时
// fallback UTC 并打 stderr WARN(不 fatal,容器仍能起来)。
//
// 为什么 fallback 而非 fatal:
//   - 配置错误不应阻止容器启动;admin 还能从面板看到 UTC,只是没那么友好
//   - 跟 parseFamily 失败时的处理一致
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

// loadAliyunCreds 解析阿里云凭证(4 级优先级,v2-83 设计)。
//
// 优先级(高 → 低):
//  1. /data/panel-state/aliyun.creds  (面板 UI 填的,运行时可改,需重启才注入到 acme.sh)
//  2. IKEV2_ALIYUN_KEY_ID           (v2-83 新统一命名)
//  3. ALIYUN_ACCESS_KEY_ID          (v2-82 DDNS 用名,deprecate)
//  4. Ali_Key                        (acme.sh dns_ali 插件用名,deprecate)
//
// 返回值:
//   - id:     AccessKey ID(空 = 没设)
//   - secret: 对应 Secret
//   - source: 来源描述,空 = 没找到(便于启动日志/面板显示)
//
// 设计见 docs/design.md §19.6(v2-83)。
func loadAliyunCreds() (id, secret, source string) {
	// 1. 面板运行时文件(最高优先级)
	credStore := panelstate.NewStore()
	if c, err := credStore.LoadAliyun(); err == nil && c != nil {
		return c.KeyID, c.KeySecret, "panelstate"
	} else if err != nil {
		// 文件存在但解析失败 → 报错(让用户知道凭证坏了),不静默 fallback
		// 这里用 stderr 直接打,不 slog(因为 logger 还没构造)
		fmt.Fprintf(os.Stderr, "WARN: panelstate aliyun.creds parse failed: %v\n", err)
	}

	// 2-4. env 优先级链
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
			// 用 deprecated 名时,启动时 WARN 一次(评审 C 建议)
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

// orDefault v2.86-PR12.5:panelstate 值优先;为空才用 env fallback。
//
// 用 *string 不优雅,直接判断空字符串够用——empty cert.conf 字段视为"未设"。
func orDefault(panelstateVal, envVal string) string {
	if panelstateVal != "" {
		return panelstateVal
	}
	return envVal
}

// loadCertConfig v2.86-PR12.5:从 /data/panel-state/cert.conf 读证书配置,
//
// fallback 到环境变量。
//
// 优先级链(高 → 低):
//  1. /data/panel-state/cert.conf   (面板 UI 填的,运行时改,需重启容器)
//  2. IKEV2_CERT_MODE / IKEV2_DOMAIN / IKEV2_SERVER_CN / IKEV2_ACME_EMAIL env
//
// 返回 (certMode, domain, serverCN, acmeEmail, source):
//   - source = "panelstate" (面板填的) / "env-default" (全用 env) / "" (都没设)
//   - 文件损坏 → 返回 env 值 + stderr WARN,不 fatal
//     (跟 loadAliyunCreds 策略一致:面板坏了降级到 env 继续跑)
func loadCertConfig() (certMode, domain, serverCN, acmeEmail, source string) {
	// 1. panelstate 优先
	certStore := panelstate.NewCertConfigStore()
	if c, err := certStore.LoadCertConfig(); err == nil && c != nil {
		return c.CertMode, c.Domain, c.ServerCN, c.ACMEEmail, "panelstate"
	} else if err != nil {
		// 文件存在但解析失败 → 报错降级(env 兜底)
		fmt.Fprintf(os.Stderr, "WARN: panelstate cert.conf parse failed: %v\n", err)
	}
	// 2. env fallback(从 env 读,标注来源)
	cm := getenv("IKEV2_CERT_MODE", "")
	d := getenv("IKEV2_DOMAIN", "")
	cn := getenv("IKEV2_SERVER_CN", "")
	em := getenv("IKEV2_ACME_EMAIL", "")
	if cm == "" && d == "" && cn == "" && em == "" {
		return "", "", "", "", ""
	}
	return cm, d, cn, em, "env-default"
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

// parseFamily v2-84:校验 + 规范化 DDNS family 值。
//
// 合法:"v4" / "v6" / "dual"。
// 空 → 默认 "dual"。
// 非法 → "dual" + stderr WARN(不 fatal,跟 v2-84 plan 一致)。
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
