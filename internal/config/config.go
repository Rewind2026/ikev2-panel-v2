// 环境变量 + 默认值。
// 见 docs/architecture.md §6.1
//
// v2.86-PR13.0 重构:本包只负责 env 解析,**不再 import panelstate**。
// panelstate + env 合并逻辑移到 internal/runtime(由 runtime.Merge 完成)。
// 修复审查报告 C3 提出的"config 反向依赖 panelstate"模块错位。
package config

import (
	"fmt"
	"log/slog"
	"net"
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
	CertMode  string // self-signed / letsencrypt
	Domain    string // LE 模式必填
	ACMEEmail string // LE 注册邮箱

	// DDNS (v2-82 新增,v2-84 family-aware)
	DDNSEnabled bool   // 总开关 (env IKEV2_DDNS_ENABLED,默认 true — 与 compose 对齐)
	DDNSFamily  string // v2-84:同步哪些 family — v4 / v6 / dual(默认 dual)
	// v2.86-pr23l:DDNSProbeTarget 字段已删除。IPv4 公网地址由 internal/publicip
	// 多家 API fallback 探测,不依赖任何 target 变量。
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
	// v2.86 配置统一批次 D:是否信任 X-Forwarded-For / X-Real-IP。
	// 之前只在 internal/auth.isTrustedProxy 里直接读 env,面板既看不到也改不了。
	TrustedProxy bool // 默认 false(公网直连时读 XFF 等于让客户端伪造 IP 绕过登录限速)

	// v2.85-PR3:面板/DDNS/mobileconfig 时间显示时区(IANA TZ 名,默认 UTC)。
	// 只影响渲染字符串,不改容器 TZ、证书有效期、cron 计算。
	DisplayTimezone string

	// ---- v2.86 批次 E:compose 里剩下的可调项(面板可改,多数需重启)----
	//
	// 这几项 Go 进程运行期基本不消费,真正的消费者是 entrypoint.sh:
	// 限速阈值 / MSS clamp / DDoS 开关写进 nft,审计保留天数写进 cron。
	// 之所以仍在 Config 里留字段,是为了 status API 能报出"当前生效值",
	// 面板只读区要显示它们 —— 读 os.Getenv 会漏掉 entrypoint 的内部默认值。

	// AuditRetentionDays 审计日志保留天数(env IKEV2_AUDIT_RETENTION_DAYS,默认 90)。
	AuditRetentionDays int
	// ConnLimitPerIP 每 IP 并发 SA 上限(env IKEV2_CONNLIMIT_PER_IP,默认 50)。
	ConnLimitPerIP int
	// HashLimitPerMin 每 IP 每分钟新 SA 上限(env IKEV2_HASHLIMIT_PER_MIN,默认 30)。
	HashLimitPerMin int
	// DisableMSSClamp 关闭 MSS clamping(env IKEV2_DISABLE_MSS_CLAMP,默认 false)。
	DisableMSSClamp bool
	// DisableDDoSProtection 关闭 DDoS 防护(env IKEV2_DISABLE_DDOS_PROTECTION,默认 false)。
	DisableDDoSProtection bool
	// NftFamily nftables 地址族(env IKEV2_FAMILY,空 = 按 IPV6_ONLY 自动判断)。
	NftFamily string
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
		DefaultUserPasswordLen: getint("IKEV2_DEFAULT_USER_PASSWORD_LEN", 12),
		SessionTTL:             getdur("IKEV2_SESSION_TTL", 24*time.Hour),
		CookieSecure:           getbool("IKEV2_COOKIE_SECURE", true),
		// v2.86 配置统一批次 D:信任反向代理开关。
		// 原先只有 internal/auth.isTrustedProxy 直接 os.Getenv 读,面板无从展示/修改。
		// 现在 config 读一次存进字段,auth 侧仍保留 env 读取(见 auth.ClientIP 注释)。
		TrustedProxy: getbool("IKEV2_TRUSTED_PROXY", false),

		// DDNS (v2-82 + v2-83 凭证统一 + v2-84 family)
		// 默认值与 docker-compose 的 ${IKEV2_DDNS_ENABLED:-true} 对齐:
		// 之前 Go 侧写 false、compose 写 true,同一条部署在两种启动路径下默认值不同。
		DDNSEnabled: getbool("IKEV2_DDNS_ENABLED", true),
		DDNSFamily:  parseFamily(getenv("IKEV2_DDNS_FAMILY", "dual")),
		// v2.86-pr23l:DDNSProbeTarget 字段已删除。IPv4 公网地址由 internal/publicip
		// 多家 API fallback 探测,不依赖任何 target 变量;老 env IKEV2_DDNS_PROBE_TARGET
		// 即使还留在 .env 里也不再被本进程读取。
		AliyunDomain: getenv("ALIYUN_DOMAIN", ""),
		AliyunRR:     getenv("ALIYUN_RR", "vpn"),

		// v2.85-PR3:显示时区。LoadLocation 失败时 fallback UTC + stderr WARN
		DisplayTimezone: loadDisplayTimezone(),

		// v2.86 批次 E。默认值跟 entrypoint.sh 里的 `:-` 兜底一字不差,
		// 避免"env 没写"时 Go 侧和 shell 侧报出两个不同的当前值。
		AuditRetentionDays:    getint("IKEV2_AUDIT_RETENTION_DAYS", 90),
		ConnLimitPerIP:        getint("IKEV2_CONNLIMIT_PER_IP", 50),
		HashLimitPerMin:       getint("IKEV2_HASHLIMIT_PER_MIN", 30),
		DisableMSSClamp:       getbool("IKEV2_DISABLE_MSS_CLAMP", false),
		DisableDDoSProtection: getbool("IKEV2_DISABLE_DDOS_PROTECTION", false),
		NftFamily:             normalizeNftFamily(getenv("IKEV2_FAMILY", "")),
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

	// v2.86-pr24(P2-9):非 loopback 监听时提示一次公网暴露面。**只提示,不 refuse。**
	//
	// 为什么不能 refuse:本项目用 network_mode: host,容器与宿主共享网络命名空间,
	// 没有 docker 端口映射这一层。绑 127.0.0.1 绑的是宿主自己的 loopback ——
	// 容器内通、宿主上通,但其他机器一律连不上,而靠管理 IP 直连面板正是常规访问方式。
	// 拦下来等于"装完就失联"(v2.86-PR19 就踩过这个坑,已回退)。
	//
	// 措辞只讲"谁能访问到",不把 LAN 可访问本身说成风险。
	if !listenIsLoopback(c.ListenAddr) {
		fmt.Fprintf(os.Stderr,
			"WARN: IKEV2_LISTEN_ADDR=%s binds a non-loopback address — the panel is reachable from "+
				"every host that can route to it. Keep the upstream firewall/ACL limited to your "+
				"management network, or set 127.0.0.1:8443 to bind loopback only.\n",
			c.ListenAddr)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// listenIsLoopback 判断监听地址是否只绑 loopback(v2.86-pr24 P2-9)。
//
// 判定规则(与 docs/P2 回归矩阵一致):
//   - IPv4 127.0.0.0/8、IPv6 ::1 → true
//   - "localhost" → true
//   - 通配(0.0.0.0 / ::)、内网 IP → false
//   - 其它 hostname → **false**:刻意不解析。解析会把"域名恰好指向 127.0.0.1"
//     判成 loopback,而 DNS 是部署者能改的东西 —— 判定不该依赖一次 DNS 查询。
//   - 畸形输入(空串 / 缺端口)→ false:交给后续 validate 报错,这里不猜。
func listenIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
//
// v2.86 批次 E:级别改用 slog.LevelVar 承接,面板「日志级别」下拉保存后
// 调 SetLogLevel 即可运行期生效,不必重启容器。
// 格式(text/json)仍然是构造期定死的 handler 类型,换格式要重启。
func (c *Config) NewLogger() *slog.Logger {
	logLevelVar.Set(parseLogLevel(c.LogLevel))

	opts := &slog.HandlerOptions{Level: &logLevelVar}
	var h slog.Handler
	if strings.ToLower(c.LogFormat) == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// logLevelVar 全进程共享的日志级别。
//
// 为什么是包级变量而不是存在 Config 上:slog.Handler 构造时只拿到
// 一个 *slog.LevelVar 指针,之后所有写这个 var 的地方(面板保存设置)
// 都要能访问它,而 handler 侧不该为此反向依赖 Config。
var logLevelVar slog.LevelVar

// SetLogLevel 运行期改日志级别,立即对之后创建的 handler 生效。
//
// 返回 false 表示值不合法(调用方负责回 400)。
// 合法值与 panelstate.ValidLogLevels 一致:debug / info / warn / error。
func SetLogLevel(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		logLevelVar.Set(slog.LevelDebug)
	case "info":
		logLevelVar.Set(slog.LevelInfo)
	case "warn":
		logLevelVar.Set(slog.LevelWarn)
	case "error":
		logLevelVar.Set(slog.LevelError)
	default:
		return false
	}
	return true
}

// CurrentLogLevel 当前生效的日志级别字符串(面板渲染用)。
func CurrentLogLevel() string {
	switch logLevelVar.Level() {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
}

// parseLogLevel env 值 → slog.Level,非法值退到 info。
func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
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

// normalizeNftFamily 校验 IKEV2_FAMILY:只接受 v4 / v6,其余(含空)退到空串。
//
// 空串的语义是"跟随 IKEV2_IPV6_ONLY 自动判断",那是 entrypoint §4.5 的默认分支,
// 所以这里不能用 "dual" 之类的值去填 —— 填了就等于悄悄改了自动判断的结果。
func normalizeNftFamily(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "v4":
		return "v4"
	case "v6":
		return "v6"
	default:
		if strings.TrimSpace(s) != "" {
			fmt.Fprintf(os.Stderr,
				"WARN: IKEV2_FAMILY=%q invalid, supported: v4, v6; falling back to auto-detect\n", s)
		}
		return ""
	}
}

// parseFamily v2-84：校验 + 规范化 DDNS family 值。
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
