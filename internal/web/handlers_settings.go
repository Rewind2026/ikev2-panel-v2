// 面板设置卡 API (v2.86 配置统一批次 D 新增)
//
// 路由:
//   - GET  /api/settings/status   返回当前生效值 + 每项来源(panelstate / env)
//   - POST /api/settings/save     保存(form: display_timezone, session_ttl,
//     cookie_secure, trusted_proxy, default_user_password_len)
//   - POST /api/settings/clear    清除 settings.json,全部回退到 env / 出厂
//
// 设计要点:
//
//  1. **为什么这批参数值得搬上 UI**
//     时区 / 会话时长 / Cookie Secure / 信任反代 / 随机密码长度这五项,
//     共同特征是"管理员自己会反复微调"+"手写 env 容易打错"+"改错不致命"。
//     以前改其中任意一项都要:编辑 .env → docker compose up -d → 重新扫码。
//
//  2. **三态(指针)语义**
//     panelstate.Settings 的字段是 *string / *bool / *int:
//     nil        = 跟随 env / 出厂
//     非 nil     = 面板显式覆盖(包括显式的 false)
//     表单侧对应三种控件的"未设置"表达:
//     布尔 → checkbox(hidden 空值兜底,未勾选 = 显式 false)
//     枚举 → <select> 首项 "跟随 env 默认"(value="")
//     数值 → <input type=number> 留空 = 跟随默认
//     "全部恢复默认" = 删文件(跟 cert/subnet 卡的 clear 语义一致)。
//
//  3. **哪些能运行期生效**
//     - SessionTTL / CookieSecure / TrustedProxy / DefaultUserPasswordLen
//     保存后写进 s.settingsRT(atomic.Pointer),handler 通过
//     s.sessionTTL() / s.cookieSecure() / s.randomPasswordLen() 读,
//     **下一次登录**即用新值,不需要重启容器。
//     之所以不直接改 s.SessionTTL / s.Secure 这两个导出字段:
//     它们被测试(server_test.go / pr8 / pr9)直接赋值,必须保留;
//     且它们是普通字段,运行期写入会有 data race。
//     - LogLevel(v2.86 批次 E)运行期生效:config 侧用 slog.LevelVar 承接,
//     applyRuntimeSettings 调 config.SetLogLevel,已建的 logger 立刻按新级别输出。
//     - DisplayTimezone **需要重启**:template FuncMap 里的
//     formatTime / formatTimeDDNS 在 LoadTemplates 时用闭包捕获了
//     displayTZ 字符串,改 env/文件都不会让模板重新读。要支持热改得
//     重载整套模板 + assetVersion,收益不抵风险,这里明确要求重启。
//     - 批次 E 其余 7 项(log_format / audit_retention_days /
//     connlimit_per_ip / hashlimit_per_min / disable_mss_clamp /
//     disable_ddos_protection / nft_family)**需要重启**:它们的消费者是
//     entrypoint.sh(nft 规则 / /etc/cron.d / family.env),
//     entrypoint §-0.6 已加了对 settings.json 的读取,重启后即生效。
//
//  4. **不 mask,但写 audit**
//     settings.json 里没有凭证(不像 aliyun.creds),但"谁改了登录策略"
//     必须能追溯,所以照常 s.writeAudit。
//
//  5. **跟 cert.conf / subnet.conf 的区别**
//     cert.conf 改完要重启 charon;settings.json 只碰面板自身行为,
//     不碰 IPsec 数据面、不碰证书、不碰客户端 IP 段。
//
// 见 docs/architecture.md §1(internal/panelstate)。
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/config"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
)

// SettingsStatusResp GET /api/settings/status 返回。
type SettingsStatusResp struct {
	// Configured settings.json 是否存在(= 面板是否接管过这批设置)。
	Configured bool `json:"configured"`
	// UpdatedAt unix seconds,最后一次保存时间。
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// Source 整体来源 panelstate / env-default。
	Source string `json:"source"`

	// 以下 5 组是"当前生效值 + 来源"的配对。
	// Source = "panelstate" 表示面板显式设过;"env" 表示跟随 env / 出厂。
	DisplayTimezone              string `json:"display_timezone"`
	DisplayTimezoneSource        string `json:"display_timezone_source"`
	SessionTTL                   string `json:"session_ttl"`
	SessionTTLSource             string `json:"session_ttl_source"`
	CookieSecure                 bool   `json:"cookie_secure"`
	CookieSecureSource           string `json:"cookie_secure_source"`
	TrustedProxy                 bool   `json:"trusted_proxy"`
	TrustedProxySource           string `json:"trusted_proxy_source"`
	DefaultUserPasswordLen       int    `json:"default_user_password_len"`
	DefaultUserPasswordLenSource string `json:"default_user_password_len_source"`

	// v2.86 批次 E:新增 8 项,同样"当前生效值 + 来源"配对。
	// 这几项的"当前生效值"来自 env —— entrypoint §-0.6 已把 settings.json
	// 的值 export 回 env,所以 Go 侧读到的就是最终值。
	LogLevel                    string `json:"log_level"`
	LogLevelSource              string `json:"log_level_source"`
	LogFormat                   string `json:"log_format"`
	LogFormatSource             string `json:"log_format_source"`
	AuditRetentionDays          int    `json:"audit_retention_days"`
	AuditRetentionDaysSource    string `json:"audit_retention_days_source"`
	ConnLimitPerIP              int    `json:"connlimit_per_ip"`
	ConnLimitPerIPSource        string `json:"connlimit_per_ip_source"`
	HashLimitPerMin             int    `json:"hashlimit_per_min"`
	HashLimitPerMinSource       string `json:"hashlimit_per_min_source"`
	DisableMSSClamp             bool   `json:"disable_mss_clamp"`
	DisableMSSClampSource       string `json:"disable_mss_clamp_source"`
	DisableDDoSProtection       bool   `json:"disable_ddos_protection"`
	DisableDDoSProtectionSource string `json:"disable_ddos_protection_source"`
	NftFamily                   string `json:"nft_family"`
	NftFamilySource             string `json:"nft_family_source"`

	// RestartRequired 任一"需重启"项被改动时置 true。
	RestartRequired bool `json:"restart_required"`
}

// settingsEffective 解析后的生效值(panelstate ?? env ?? 出厂)。
//
// 这是 UI 显示与 status API 的唯一真相来源 —— 跟 runtime.Merge 第 4 段
// 用同一套优先级,区别只在 runtime 跑在启动期、这里跑在 handler 期。
type settingsEffective struct {
	displayTimezone        string
	sessionTTL             time.Duration
	cookieSecure           bool
	trustedProxy           bool
	defaultUserPasswordLen int

	// v2.86 批次 E
	logLevel              string
	logFormat             string
	auditRetentionDays    int
	connLimitPerIP        int
	hashLimitPerMin       int
	disableMSSClamp       bool
	disableDDoSProtection bool
	nftFamily             string

	displayTimezoneSet bool // settings.json 里显式设过
	sessionTTLSet      bool
	cookieSecureSet    bool
	trustedProxySet    bool
	passwordLenSet     bool
	logLevelSet        bool
	logFormatSet       bool
	auditDaysSet       bool
	connLimitSet       bool
	hashLimitSet       bool
	mssClampSet        bool
	ddosSet            bool
	nftFamilySet       bool

	updatedAt int64
}

// effectiveSettings 合并"面板设置 + env 兜底"。
//
// env 兜底值来自 main.go 注入的 Env* 字段(它们是**合并前**的纯 env 值,
// 见 main.go 里的 envSnapshot 注释)——不能直接用 s.SessionTTL,
// 因为 main.go 把合并后的值也注入了同一个字段。
//
// 批次 E 的 8 项是例外:它们的 Env* 是**合并后**的值,因为消费者在
// entrypoint.sh(§-0.6 把 settings.json export 回 env,Go 进程继承)。
// 所以这 8 项的 "Set" 标记不能靠比较得出,只能直接读 settings.json ——
// 这也正是下面照旧读 st.* 的原因。
func (s *Server) effectiveSettings() settingsEffective {
	eff := settingsEffective{
		displayTimezone:        s.EnvDisplayTimezone,
		sessionTTL:             s.EnvSessionTTL,
		cookieSecure:           s.EnvCookieSecure,
		trustedProxy:           s.EnvTrustedProxy,
		defaultUserPasswordLen: s.EnvDefaultUserPasswordLen,

		logLevel:              s.EnvLogLevel,
		logFormat:             s.EnvLogFormat,
		auditRetentionDays:    s.EnvAuditRetentionDays,
		connLimitPerIP:        s.EnvConnLimitPerIP,
		hashLimitPerMin:       s.EnvHashLimitPerMin,
		disableMSSClamp:       s.EnvDisableMSSClamp,
		disableDDoSProtection: s.EnvDisableDDoSProtection,
		nftFamily:             s.EnvNftFamily,
	}
	// env 兜底为空时的出厂值(与 internal/config 的 getenv 默认值一致)。
	if eff.displayTimezone == "" {
		eff.displayTimezone = "UTC"
	}
	if eff.sessionTTL <= 0 {
		eff.sessionTTL = 24 * time.Hour
	}
	if eff.defaultUserPasswordLen <= 0 {
		eff.defaultUserPasswordLen = defaultPasswordLen
	}
	if eff.logLevel == "" {
		eff.logLevel = "info"
	}
	if eff.logFormat == "" {
		eff.logFormat = "text"
	}
	if eff.auditRetentionDays <= 0 {
		eff.auditRetentionDays = 90
	}
	if eff.connLimitPerIP <= 0 {
		eff.connLimitPerIP = 50
	}
	if eff.hashLimitPerMin <= 0 {
		eff.hashLimitPerMin = 30
	}

	st, err := s.readSettings()
	if err != nil || st == nil {
		return eff
	}
	eff.updatedAt = st.UpdatedAt
	if st.DisplayTimezone != nil {
		eff.displayTimezone = *st.DisplayTimezone
		eff.displayTimezoneSet = true
	}
	if st.SessionTTL != nil {
		if d, perr := time.ParseDuration(*st.SessionTTL); perr == nil {
			eff.sessionTTL = d
			eff.sessionTTLSet = true
		}
	}
	if st.CookieSecure != nil {
		eff.cookieSecure = *st.CookieSecure
		eff.cookieSecureSet = true
	}
	if st.TrustedProxy != nil {
		eff.trustedProxy = *st.TrustedProxy
		eff.trustedProxySet = true
	}
	if st.DefaultUserPasswordLen != nil {
		eff.defaultUserPasswordLen = *st.DefaultUserPasswordLen
		eff.passwordLenSet = true
	}
	// ---- 批次 E ----
	// 值取 st(面板设置优先),env 只是"上次重启时的生效值"。
	if st.LogLevel != nil {
		eff.logLevel = *st.LogLevel
		eff.logLevelSet = true
	}
	if st.LogFormat != nil {
		eff.logFormat = *st.LogFormat
		eff.logFormatSet = true
	}
	if st.AuditRetentionDays != nil {
		eff.auditRetentionDays = *st.AuditRetentionDays
		eff.auditDaysSet = true
	}
	if st.ConnLimitPerIP != nil {
		eff.connLimitPerIP = *st.ConnLimitPerIP
		eff.connLimitSet = true
	}
	if st.HashLimitPerMin != nil {
		eff.hashLimitPerMin = *st.HashLimitPerMin
		eff.hashLimitSet = true
	}
	if st.DisableMSSClamp != nil {
		eff.disableMSSClamp = *st.DisableMSSClamp
		eff.mssClampSet = true
	}
	if st.DisableDDoSProtection != nil {
		eff.disableDDoSProtection = *st.DisableDDoSProtection
		eff.ddosSet = true
	}
	if st.NftFamily != nil {
		eff.nftFamily = *st.NftFamily
		eff.nftFamilySet = true
	}
	return eff
}

// readSettings 读 settings.json,store 未注入 / 读失败都返回 (nil, nil)。
//
// 读失败(文件损坏)不该让整个首页 500 —— 面板设置不是关键路径,
// 降级到 env 显示,用户从卡片上的"清除面板设置"就能恢复。
func (s *Server) readSettings() (*panelstate.Settings, error) {
	if s.SettingsStore == nil {
		return nil, nil
	}
	st, err := s.SettingsStore.ReadSettings()
	if err != nil {
		return nil, err
	}
	return st, nil
}

// handleSettingsStatus GET /api/settings/status
func (s *Server) handleSettingsStatus(w http.ResponseWriter, r *http.Request) {
	eff := s.effectiveSettings()
	resp := SettingsStatusResp{
		Configured: s.SettingsStore != nil && s.SettingsStore.SettingsExists(),
		UpdatedAt:  eff.updatedAt,
		Source:     "env-default",
		// env 兜底为空时由 effectiveSettings 填了出厂值,这里不会拿到空串。
		DisplayTimezone:              eff.displayTimezone,
		DisplayTimezoneSource:        pickSource(eff.displayTimezoneSet),
		SessionTTL:                   eff.sessionTTL.String(),
		SessionTTLSource:             pickSource(eff.sessionTTLSet),
		CookieSecure:                 eff.cookieSecure,
		CookieSecureSource:           pickSource(eff.cookieSecureSet),
		TrustedProxy:                 eff.trustedProxy,
		TrustedProxySource:           pickSource(eff.trustedProxySet),
		DefaultUserPasswordLen:       eff.defaultUserPasswordLen,
		DefaultUserPasswordLenSource: pickSource(eff.passwordLenSet),

		LogLevel:                    eff.logLevel,
		LogLevelSource:              pickSource(eff.logLevelSet),
		LogFormat:                   eff.logFormat,
		LogFormatSource:             pickSource(eff.logFormatSet),
		AuditRetentionDays:          eff.auditRetentionDays,
		AuditRetentionDaysSource:    pickSource(eff.auditDaysSet),
		ConnLimitPerIP:              eff.connLimitPerIP,
		ConnLimitPerIPSource:        pickSource(eff.connLimitSet),
		HashLimitPerMin:             eff.hashLimitPerMin,
		HashLimitPerMinSource:       pickSource(eff.hashLimitSet),
		DisableMSSClamp:             eff.disableMSSClamp,
		DisableMSSClampSource:       pickSource(eff.mssClampSet),
		DisableDDoSProtection:       eff.disableDDoSProtection,
		DisableDDoSProtectionSource: pickSource(eff.ddosSet),
		NftFamily:                   eff.nftFamily,
		NftFamilySource:             pickSource(eff.nftFamilySet),
	}
	if resp.Configured {
		resp.Source = "panelstate"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// pickSource 三态标记 → 来源标签。
func pickSource(set bool) string {
	if set {
		return "panelstate"
	}
	return "env"
}

// ---------------------------------------------------------------------------
// home 模板数据
// ---------------------------------------------------------------------------

// settingsOption 预配置下拉的一个选项。
//
// 为什么要"当前值不在预置列表里就临时追加一条":
//
//	预置列表只覆盖常见值。管理员 .env 里写了 Asia/Chongqing,
//	而预置里没有 —— <select> 找不到匹配 option 时浏览器会**显示第一项**
//	(也就是"跟随默认"),于是他什么都不改直接点保存,时区就被悄悄改掉了。
//	追加当前值是唯一能避免这种"静默改配置"的写法。
type settingsOption struct {
	Value    string // 提交值(空串 = 跟随 env 默认)
	Label    string // 展示文案
	Selected bool
}

// settingsStatusData 「面板设置」卡的模板数据。
type settingsStatusData struct {
	// Configured settings.json 是否存在(= 卡片底部显示"清除面板设置"按钮)。
	Configured bool
	UpdatedAt  int64
	Source     string // panelstate / env-default

	// --- 当前生效值 ---
	DisplayTimezone        string
	SessionTTL             string
	CookieSecure           bool
	TrustedProxy           bool
	DefaultUserPasswordLen int

	// --- v2.86 批次 E:新增 8 项的当前生效值 ---
	LogLevel              string
	LogFormat             string
	AuditRetentionDays    int
	ConnLimitPerIP        int
	HashLimitPerMin       int
	DisableMSSClamp       bool
	DisableDDoSProtection bool
	NftFamily             string

	// --- env 兜底值(渲染"跟随 env 默认(X)"提示用)---
	EnvDisplayTimezone        string
	EnvSessionTTL             string
	EnvCookieSecure           bool
	EnvTrustedProxy           bool
	EnvDefaultUserPasswordLen int

	// EnvLogLevel 等是 env 侧的原始值;批次 E 的 Env* 已经是合并后的
	// (entrypoint export 过),所以下面不再单列 Env* 副本。
	EnvLogLevel  string
	EnvNftFamily string
	// 批次 E:这两个布尔的 env 原值,渲染"跟随 env 默认(当前 X)"用。
	// effectiveSettings 里 eff.disableXxx 在"面板没设过"时就等于 env 值,
	// 所以正常情况不需要单独存;这里存的是 Env* 字段本身,
	// 保证"面板设过 false"和"env 是 true"两种情况下的提示文案都能区分。
	EnvDisableMSSClamp       bool
	EnvDisableDDoSProtection bool

	// --- 三态:该字段是否被面板显式设过 ---
	SetDisplayTimezone        bool
	SetSessionTTL             bool
	SetCookieSecure           bool
	SetTrustedProxy           bool
	SetDefaultUserPasswordLen bool
	SetLogLevel               bool
	SetLogFormat              bool
	SetAuditRetentionDays     bool
	SetConnLimitPerIP         bool
	SetHashLimitPerMin        bool
	SetDisableMSSClamp        bool
	SetDisableDDoSProtection  bool
	SetNftFamily              bool

	// PlainHTTP 明文 HTTP 入口是否存在(存在时禁止打开 Cookie Secure)。
	PlainHTTP bool

	// 预配置下拉选项
	TimezoneOptions   []settingsOption
	SessionTTLOptions []settingsOption
	LogLevelOptions   []settingsOption
	LogFormatOptions  []settingsOption
	NftFamilyOptions  []settingsOption

	// 数值输入框的 min/max(跟 panelstate 的校验常量同源)
	PasswordLenMin int
	PasswordLenMax int
	AuditDaysMin   int
	AuditDaysMax   int
	ConnLimitMin   int
	ConnLimitMax   int
	HashLimitMin   int
	HashLimitMax   int

	// DeployOnly 只能在 .env / compose 里改的部署级参数(只读清单)。
	DeployOnly []deployOnlyParam
}

// deployOnlyParam 只读部署参数的一行。
type deployOnlyParam struct {
	// Env 环境变量名
	Env string
	// Value 当前值(空串 = 用代码 / entrypoint 默认)
	Value string
	// Default 该参数的出厂默认值,Value 为空时展示它
	Default string
	// Why 为什么不能在面板改 —— 直接展示给用户,避免"为什么这个不能改"的疑问
	Why string
}

// presetTimezones 「显示时区」的预置选项。
//
// 只放覆盖面广、不会引起歧义的 IANA 名。特别**不**放 Asia/Chongqing /
// PRC 这些历史别名 —— tzdata 里它们是单独的条目,用户选了没问题,
// 但会让"预置 = 官方推荐"这个心智模型变得不可靠。
var presetTimezones = []struct{ Value, Label string }{
	{"UTC", "UTC Coordinated Universal"},
	{"Asia/Shanghai", "中国标准时间 · Asia/Shanghai"},
	{"Asia/Hong_Kong", "香港时间 · Asia/Hong_Kong"},
	{"Asia/Taipei", "台北时间 · Asia/Taipei"},
	{"Asia/Tokyo", "东京时间 · Asia/Tokyo"},
	{"Asia/Seoul", "首尔时间 · Asia/Seoul"},
	{"Asia/Singapore", "新加坡时间 · Asia/Singapore"},
	{"Europe/London", "伦敦时间 · Europe/London"},
	{"Europe/Berlin", "柏林时间 · Europe/Berlin"},
	{"America/New_York", "美东时间 · America/New_York"},
	{"America/Los_Angeles", "美西时间 · America/Los_Angeles"},
	{"Australia/Sydney", "悉尼时间 · Australia/Sydney"},
}

// presetSessionTTLs 「登录会话时长」的预置选项。
//
// 值是 Go duration 字符串,直接写进 settings.json 并原样回显,
// 前端不做单位换算 —— 少一处转换就少一处"下拉显示 24h 实际存 86400"的坑。
var presetSessionTTLs = []struct{ Value, Label string }{
	{"30m", "30 分钟 · 误触即锁"},
	{"1h", "1 小时"},
	{"8h", "8 小时 · 一个工作日"},
	{"24h", "24 小时 · 推荐"},
	{"168h", "7 天"},
	{"720h", "30 天 · 上限"},
}

// v2.86 批次 E 的枚举下拉。取值集合直接引用 panelstate 的白名单 ——
// 白名单是校验的唯一真相,这里再抄一份就会漂移。
var (
	presetLogLevels = func() []struct{ Value, Label string } {
		labels := map[string]string{
			"debug": "debug · 排查问题时用,日志量大",
			"info":  "info · 默认",
			"warn":  "warn · 只看告警",
			"error": "error · 只看错误",
		}
		return toOptionList(panelstate.ValidLogLevels, labels)
	}()

	presetLogFormats = func() []struct{ Value, Label string } {
		labels := map[string]string{
			"text": "text · 人读的,默认",
			"json": "json · 给日志采集(Loki / ELK)",
		}
		return toOptionList(panelstate.ValidLogFormats, labels)
	}()

	presetNftFamilies = func() []struct{ Value, Label string } {
		labels := map[string]string{
			"v4": "v4 · 只处理 IPv4 流量",
			"v6": "v6 · 只处理 IPv6 流量",
		}
		return toOptionList(panelstate.ValidNftFamilies, labels)
	}()
)

// toOptionList 白名单 + 展示文案 → 下拉源数据。
func toOptionList(values []string, labels map[string]string) []struct{ Value, Label string } {
	out := make([]struct{ Value, Label string }, 0, len(values))
	for _, v := range values {
		lbl, ok := labels[v]
		if !ok {
			lbl = v
		}
		out = append(out, struct{ Value, Label string }{v, lbl})
	}
	return out
}

// deployOnlyParamList 只能在 .env / compose 里改的部署级参数。
//
// 为什么不做成 UI 开关:这些参数描述的是**容器的部署形态**(挂哪张网卡、
// 数据放哪个目录、监听哪个端口、网络模式是哪一种),而不是"这个面板怎么跑"。
// 运行时改它们要么不生效(网段变了要重建 swanctl.conf),要么直接把面板
// 改成连不上自己 —— 让管理员在面板里点一下就把服务改废,风险远大于收益。
//
// 展示它们的价值在于:用户问"还有哪些参数能配"时,这里能一次看全,
// 而不是要去翻 compose 文件。Env 列为变量名,Value 是容器内实际生效值。
func deployOnlyParamList() []deployOnlyParam {
	get := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
		return def
	}
	return []deployOnlyParam{
		{"IKEV2_NETWORK_MODE", get("IKEV2_NETWORK_MODE", "host"), "host",
			"由 compose 文件决定,必须换 compose 文件而不是改变量"},
		{"IKEV2_DATA_DIR", get("IKEV2_DATA_DIR", "/data"), "/data",
			"数据目录,换目录等于换一套空数据库"},
		{"IKEV2_LISTEN_ADDR", get("IKEV2_LISTEN_ADDR", "0.0.0.0:8443"), "0.0.0.0:8443",
			"HTTPS 监听地址,改错会连不上面板"},
		{"IKEV2_HTTP_LISTEN_ADDR", get("IKEV2_HTTP_LISTEN_ADDR", ""), "(不启用)",
			"明文 HTTP 备用入口,开启会被强制关闭 Secure cookie"},
		{"IKEV2_SERVER_CN", get("IKEV2_SERVER_CN", "vpn.example.com"), "vpn.example.com",
			"服务器证书 CN,在「证书」卡改"},
		{"IKEV2_IPV6_ONLY", get("IKEV2_IPV6_ONLY", "true"), "true",
			"单栈 / 双栈,切换要重建 nft 与路由"},
		{"IKEV2_SERVER_ADDR_V6", get("IKEV2_SERVER_ADDR_V6", ""), "(自动探测)",
			"公网 IPv6 静态兜底,进 charon local_addrs"},
		{"IKEV2_SERVER_ADDR_V4", get("IKEV2_SERVER_ADDR_V4", ""), "(自动探测)",
			"公网 IPv4 静态兜底,仅双栈模式有意义"},
		{"IKEV2_OUT_IF", get("IKEV2_OUT_IF", ""), "(自动探测)",
			"出口网卡,填错 entrypoint 会 FATAL 退出"},
		{"IKEV2_L2_PARENT", get("IKEV2_L2_PARENT", ""), "(空)",
			"macvlan / ipvlan 父接口,仅 L2 模式使用"},
		{"IKEV2_L2_SUBNET", get("IKEV2_L2_SUBNET", ""), "(空)",
			"L2 模式 IPv4 子网,填 IPv6 会导致公网不可达"},
		{"IKEV2_L2_GATEWAY", get("IKEV2_L2_GATEWAY", ""), "(空)",
			"L2 模式宿主网关"},
		{"IKEV2_L2_IPV4", get("IKEV2_L2_IPV4", ""), "(自动分配)",
			"L2 模式容器固定 IPv4,留空由 Docker 自动分配"},
		{"IKEV2_MACVLAN_MODE", get("IKEV2_MACVLAN_MODE", ""), "(默认 bridge)",
			"macvlan 模式:bridge / private / vepa"},
		{"IKEV2_IPVLAN_MODE", get("IKEV2_IPVLAN_MODE", ""), "(默认 l2)",
			"ipvlan 模式:l2 / l3 / l3s"},
		{"IKEV2_IPVLAN_FLAG", get("IKEV2_IPVLAN_FLAG", ""), "(默认 bridge)",
			"ipvlan flag:bridge / private / vepa"},
		{"IKEV2_PANEL_DB", get("IKEV2_PANEL_DB", ""), "/data/panel.db",
			"SQLite 路径,改错审计清理会找不到库"},
		{"IKEV2_AUDIT_RETENTION_LOG", get("IKEV2_AUDIT_RETENTION_LOG", ""), "/var/log/ikev2-audit-retention.log",
			"审计清理脚本的输出日志路径"},
	}
}

// loadSettingsStatus 组装「面板设置」卡的模板数据。
//
// 关键点:下拉的"当前值"取**生效值**(panelstate ?? env),不是 settings.json
// 里的原始值。这样 env-only 部署的用户看到的也是自己 .env 里那个时区,
// 而不是空白表单 —— 这正是"预配置项"要解决的"我不知道现在是什么"。
func loadSettingsStatus(s *Server) settingsStatusData {
	eff := s.effectiveSettings()

	out := settingsStatusData{
		DisplayTimezone:        eff.displayTimezone,
		SessionTTL:             normalizeTTLString(eff.sessionTTL),
		CookieSecure:           eff.cookieSecure,
		TrustedProxy:           eff.trustedProxy,
		DefaultUserPasswordLen: eff.defaultUserPasswordLen,

		LogLevel:              eff.logLevel,
		LogFormat:             eff.logFormat,
		AuditRetentionDays:    eff.auditRetentionDays,
		ConnLimitPerIP:        eff.connLimitPerIP,
		HashLimitPerMin:       eff.hashLimitPerMin,
		DisableMSSClamp:       eff.disableMSSClamp,
		DisableDDoSProtection: eff.disableDDoSProtection,
		NftFamily:             eff.nftFamily,

		EnvDisplayTimezone:        s.EnvDisplayTimezone,
		EnvSessionTTL:             normalizeTTLString(s.EnvSessionTTL),
		EnvCookieSecure:           s.EnvCookieSecure,
		EnvTrustedProxy:           s.EnvTrustedProxy,
		EnvDefaultUserPasswordLen: s.EnvDefaultUserPasswordLen,

		EnvLogLevel:  s.EnvLogLevel,
		EnvNftFamily: s.EnvNftFamily,

		EnvDisableMSSClamp:       s.EnvDisableMSSClamp,
		EnvDisableDDoSProtection: s.EnvDisableDDoSProtection,

		SetDisplayTimezone:        eff.displayTimezoneSet,
		SetSessionTTL:             eff.sessionTTLSet,
		SetCookieSecure:           eff.cookieSecureSet,
		SetTrustedProxy:           eff.trustedProxySet,
		SetDefaultUserPasswordLen: eff.passwordLenSet,
		SetLogLevel:               eff.logLevelSet,
		SetLogFormat:              eff.logFormatSet,
		SetAuditRetentionDays:     eff.auditDaysSet,
		SetConnLimitPerIP:         eff.connLimitSet,
		SetHashLimitPerMin:        eff.hashLimitSet,
		SetDisableMSSClamp:        eff.mssClampSet,
		SetDisableDDoSProtection:  eff.ddosSet,
		SetNftFamily:              eff.nftFamilySet,

		UpdatedAt:      eff.updatedAt,
		Source:         "env-default",
		PlainHTTP:      s.PlainHTTPEnabled,
		PasswordLenMin: panelstate.MinPasswordLen,
		PasswordLenMax: panelstate.MaxPasswordLen,
		AuditDaysMin:   panelstate.MinAuditRetentionDays,
		AuditDaysMax:   panelstate.MaxAuditRetentionDays,
		ConnLimitMin:   panelstate.MinConnLimitPerIP,
		ConnLimitMax:   panelstate.MaxConnLimitPerIP,
		HashLimitMin:   panelstate.MinHashLimitPerMin,
		HashLimitMax:   panelstate.MaxHashLimitPerMin,

		DeployOnly: deployOnlyParamList(),
	}
	if s.SettingsStore != nil && s.SettingsStore.SettingsExists() {
		out.Configured = true
		out.Source = "panelstate"
	}

	// env 兜底为空时(e.g. 测试里没注入 Env*)给个能显示的默认值。
	if out.EnvDisplayTimezone == "" {
		out.EnvDisplayTimezone = "UTC"
	}
	if out.EnvSessionTTL == "" {
		out.EnvSessionTTL = "24h"
	}
	if out.EnvDefaultUserPasswordLen <= 0 {
		out.EnvDefaultUserPasswordLen = defaultPasswordLen
	}
	if out.EnvLogLevel == "" {
		out.EnvLogLevel = "info"
	}
	if out.EnvNftFamily == "" {
		out.EnvNftFamily = "auto"
	}

	out.TimezoneOptions = buildTimezoneOptions(eff.displayTimezone)
	out.SessionTTLOptions = buildSessionTTLOptions(out.SessionTTL)
	out.LogLevelOptions = buildEnumOptions(presetLogLevels, eff.logLevel)
	out.LogFormatOptions = buildEnumOptions(presetLogFormats, eff.logFormat)
	out.NftFamilyOptions = buildEnumOptions(presetNftFamilies, eff.nftFamily)
	return out
}

// buildEnumOptions 通用枚举下拉:首项"跟随 env 默认",其余预置,
// 最后追加"当前生效值"(不在预置里时)。
//
// 跟 buildTimezoneOptions 同构,单独抽出来是因为批次 E 一口气加了三个
// 枚举下拉,复制三遍同样的循环没有意义。
func buildEnumOptions(preset []struct{ Value, Label string }, current string) []settingsOption {
	opts := make([]settingsOption, 0, len(preset)+2)
	inPreset := false
	for _, p := range preset {
		if p.Value == current {
			inPreset = true
		}
	}
	opts = append(opts, settingsOption{Value: "", Label: "跟随 env 默认", Selected: !inPreset})

	found := false
	for _, p := range preset {
		sel := p.Value == current
		if sel {
			found = true
		}
		opts = append(opts, settingsOption{p.Value, p.Label, sel})
	}
	if !found && current != "" {
		opts = append(opts, settingsOption{
			Value: current, Label: "当前生效 · " + current, Selected: true,
		})
	}
	return opts
}

// buildTimezoneOptions 生成时区下拉:首项"跟随 env 默认",其余预置,
// 最后追加"当前生效值"(不在预置里时),见 settingsOption 的注释。
func buildTimezoneOptions(current string) []settingsOption {
	opts := make([]settingsOption, 0, len(presetTimezones)+2)
	opts = append(opts, settingsOption{Value: "", Label: "跟随 env 默认", Selected: !inPresetTimezone(current)})

	found := false
	for _, p := range presetTimezones {
		sel := p.Value == current
		if sel {
			found = true
		}
		opts = append(opts, settingsOption{Value: p.Value, Label: p.Label, Selected: sel})
	}
	if !found && current != "" {
		opts = append(opts, settingsOption{
			Value: current, Label: "当前生效 · " + current, Selected: true,
		})
	}
	return opts
}

func inPresetTimezone(tz string) bool {
	for _, p := range presetTimezones {
		if p.Value == tz {
			return true
		}
	}
	return false
}

// buildSessionTTLOptions 生成会话时长下拉(逻辑同 buildTimezoneOptions)。
func buildSessionTTLOptions(current string) []settingsOption {
	opts := make([]settingsOption, 0, len(presetSessionTTLs)+2)
	opts = append(opts, settingsOption{Value: "", Label: "跟随 env 默认", Selected: !inPresetTTL(current)})

	found := false
	for _, p := range presetSessionTTLs {
		sel := p.Value == current
		if sel {
			found = true
		}
		opts = append(opts, settingsOption{Value: p.Value, Label: p.Label, Selected: sel})
	}
	if !found && current != "" {
		opts = append(opts, settingsOption{
			Value: current, Label: "当前生效 · " + current, Selected: true,
		})
	}
	return opts
}

func inPresetTTL(v string) bool {
	for _, p := range presetSessionTTLs {
		if p.Value == v {
			return true
		}
	}
	return false
}

// normalizeTTLString time.Duration → 预置友好的字符串。
//
// 直接用 d.String() 的话 30 分钟会变成 "30m0s",在下拉里既不匹配
// "30m" 预设(会被当成"不在列表里"追加一条重复项),用户看着也别扭。
// 命中预置就返回预置写法,否则返回 d.String() 原样。
func normalizeTTLString(d time.Duration) string {
	if d <= 0 {
		return "24h"
	}
	for _, p := range presetSessionTTLs {
		if pd, err := time.ParseDuration(p.Value); err == nil && pd == d {
			return p.Value
		}
	}
	return d.String()
}

// handleSettingsSave POST /api/settings/save
//
// 表单语义(每个控件的"未设置"表达见文件头第 2 条):
//   - display_timezone            <select>,空 = 跟随 env
//   - session_ttl                 <select>,空 = 跟随 env
//   - cookie_secure               checkbox + hidden 空值兜底
//   - trusted_proxy               同上
//   - default_user_password_len   <input type=number>,空 = 跟随 env
//
// 副作用:
//   - 写 /data/panel-state/settings.json(atomic rename, 0600)
//   - 运行期生效:SessionTTL / CookieSecure / TrustedProxy / DefaultUserPasswordLen
//   - DisplayTimezone 需重启容器(模板 FuncMap 闭包捕获,见文件头第 3 条)
//   - 写审计(不 mask —— 本文件无凭证)
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if s.SettingsStore == nil {
		http.Error(w, "面板设置存储未初始化", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.respondSettingsFlash(w, r, "error", "表单解析失败")
		return
	}

	// 先把 4 个数值字段解析成指针,任一非法立即 400。
	// 放在构造 st 之前,避免"解析失败的那项退化成 nil"后
	// 继续往下走 —— 那正是我们要消除的静默改配置。
	passwordLen, err := optInt(r.PostForm, "default_user_password_len")
	if err != nil {
		s.respondSettingsFlash(w, r, "error", err.Error())
		return
	}
	auditDays, err := optInt(r.PostForm, "audit_retention_days")
	if err != nil {
		s.respondSettingsFlash(w, r, "error", err.Error())
		return
	}
	connLimit, err := optInt(r.PostForm, "connlimit_per_ip")
	if err != nil {
		s.respondSettingsFlash(w, r, "error", err.Error())
		return
	}
	hashLimit, err := optInt(r.PostForm, "hashlimit_per_min")
	if err != nil {
		s.respondSettingsFlash(w, r, "error", err.Error())
		return
	}

	st := panelstate.Settings{
		DisplayTimezone:        optString(r.PostForm, "display_timezone"),
		SessionTTL:             optString(r.PostForm, "session_ttl"),
		CookieSecure:           optBool(r.PostForm, "cookie_secure"),
		TrustedProxy:           optBool(r.PostForm, "trusted_proxy"),
		DefaultUserPasswordLen: passwordLen,

		// v2.86 批次 E
		LogLevel:              optString(r.PostForm, "log_level"),
		LogFormat:             optString(r.PostForm, "log_format"),
		AuditRetentionDays:    auditDays,
		ConnLimitPerIP:        connLimit,
		HashLimitPerMin:       hashLimit,
		DisableMSSClamp:       optBool(r.PostForm, "disable_mss_clamp"),
		DisableDDoSProtection: optBool(r.PostForm, "disable_ddos_protection"),
		NftFamily:             optString(r.PostForm, "nft_family"),
	}

	// 全空 = 用户点了保存但什么都没设 → 等价于"恢复默认",直接清文件。
	// 不写一个全是 nil 的 settings.json(那样会凭空多一个 panelstate 文件,
	// 还会让 cert/subnet 卡的"配置来源"显示误导)。
	if st.IsZero() {
		s.settingsClear(w, r)
		return
	}

	// 明文 HTTP 入口存在时禁止打开 Cookie Secure。
	//
	// 必须在写盘**之前**拦:settings.json 落盘后它就是"用户显式配置",
	// 下次启动 runtime.Merge 会把 cookie_secure=true 合进 cfg,
	// 虽然 main.go 仍会强制压成 false,但排查时"文件里写着 true、
	// 实际是 false"这种不一致比直接拒绝更难查。
	if st.CookieSecure != nil && *st.CookieSecure && s.PlainHTTPEnabled {
		s.respondSettingsFlash(w, r, "error",
			"已启用明文 HTTP 入口(IKEV2_HTTP_LISTEN_ADDR),浏览器会拒收 Secure cookie,不能打开 Cookie Secure;请先关掉明文入口")
		return
	}

	before := s.effectiveSettings()
	if err := s.SettingsStore.WriteSettings(st); err != nil {
		s.Logger.Warn("settings save failed", "err", err)
		s.respondSettingsFlash(w, r, "error", "保存失败："+err.Error())
		return
	}
	after := s.effectiveSettings()
	s.applyRuntimeSettings(after)

	details := fmt.Sprintf("display_timezone=%s session_ttl=%s cookie_secure=%t trusted_proxy=%t password_len=%d",
		after.displayTimezone, after.sessionTTL, after.cookieSecure,
		after.trustedProxy, after.defaultUserPasswordLen)
	s.Logger.Info("settings saved via panel", "before_tz", before.displayTimezone, "after_tz", after.displayTimezone)
	s.writeAudit(r, "settings.save", details)

	msg := "面板设置已保存"
	if after.displayTimezone != before.displayTimezone {
		msg += "；显示时区需重启容器生效"
	}
	// v2.86 批次 E:只要有"需重启"的项变了就提示。
	// 逐项比较 before/after 而不是看 Set 标记 —— 后者只能说明"设过",
	// 说明不了"这次改没改"。
	if restartFieldsChanged(before, after) {
		msg += "；防火墙 / 审计 / 日志格式类设置需重启容器生效"
	}
	s.respondSettingsFlash(w, r, "success", msg)
}

// restartFieldsChanged 判断"需重启"的字段里有没有变化的。
//
// 覆盖:显示时区、时区以外的日志格式、审计保留天数、限速阈值 ×2、
// MSS clamp、DDoS 开关、nft 地址族。
//
// 刻意**不含** log_level —— 它是运行期生效的(见 applyRuntimeSettings)。
func restartFieldsChanged(before, after settingsEffective) bool {
	return before.displayTimezone != after.displayTimezone ||
		before.logFormat != after.logFormat ||
		before.auditRetentionDays != after.auditRetentionDays ||
		before.connLimitPerIP != after.connLimitPerIP ||
		before.hashLimitPerMin != after.hashLimitPerMin ||
		before.disableMSSClamp != after.disableMSSClamp ||
		before.disableDDoSProtection != after.disableDDoSProtection ||
		before.nftFamily != after.nftFamily
}

// handleSettingsClear POST /api/settings/clear
func (s *Server) handleSettingsClear(w http.ResponseWriter, r *http.Request) {
	if s.SettingsStore == nil {
		http.Error(w, "面板设置存储未初始化", http.StatusInternalServerError)
		return
	}
	s.settingsClear(w, r)
}

// settingsClear 清 settings.json 并把运行期值回退到 env / 出厂。
func (s *Server) settingsClear(w http.ResponseWriter, r *http.Request) {
	before := s.effectiveSettings()
	if err := s.SettingsStore.ClearSettings(); err != nil {
		s.Logger.Error("settings clear failed", "err", err)
		s.respondSettingsFlash(w, r, "error", "清除失败："+err.Error())
		return
	}
	after := s.effectiveSettings()
	s.applyRuntimeSettings(after)

	s.Logger.Info("settings cleared via panel")
	s.writeAudit(r, "settings.clear", fmt.Sprintf("restored_tz=%s session_ttl=%s",
		after.displayTimezone, after.sessionTTL))

	msg := "已清除面板设置，全部回退到部署默认值"
	if before.displayTimezone != after.displayTimezone {
		msg += "；显示时区需重启容器生效"
	}
	s.respondSettingsFlash(w, r, "success", msg)
}

// applyRuntimeSettings 把生效值写进运行期快照 + auth 的 trusted-proxy 开关。
//
// 只覆盖"读起来不依赖启动期捕获"的三类值 + 密码长度:
//   - SessionTTL           → s.sessionTTL()(登录时读)
//   - CookieSecure         → s.cookieSecure()(SetSessionCookie / QR scheme)
//   - TrustedProxy         → auth.SetTrustedProxy(ClientIP 每次读)
//   - DefaultUserPasswordLen → s.randomPasswordLen()(新建/重置用户时读)
//
// DisplayTimezone 不在此列(模板 FuncMap 闭包,改不动)。
func (s *Server) applyRuntimeSettings(eff settingsEffective) {
	cookieSecure := eff.cookieSecure
	if s.PlainHTTPEnabled {
		// 明文入口存在时 Secure 必须为 false,跟 EnablePlaintextHTTP 保持一致。
		// 清除面板设置后 cookie_secure 会回落到 env,而 env 可能写着 true
		// —— 不在这里压平,用户点一次"清除面板设置"就登不上了。
		cookieSecure = false
	}
	s.settingsRT.Store(&settingsRuntime{
		sessionTTL:             eff.sessionTTL,
		cookieSecure:           cookieSecure,
		trustedProxy:           eff.trustedProxy,
		defaultUserPasswordLen: eff.defaultUserPasswordLen,
		logLevel:               eff.logLevel,
	})
	auth.SetTrustedProxy(eff.trustedProxy)

	// v2.86 批次 E:日志级别是这批里唯一运行期生效的。
	// config.SetLogLevel 内部只动 slog.LevelVar,不会重建 handler,
	// 所以已经在跑的 logger 也立刻按新级别输出。
	if !config.SetLogLevel(eff.logLevel) {
		s.Logger.Warn("settings: invalid log level, keeping current",
			"want", eff.logLevel, "current", config.CurrentLogLevel())
	}
}

// logLevel 当前生效的日志级别。
//
// 优先级:面板设置(settingsRT.logLevel)> config.CurrentLogLevel()
// (启动期 env 值,或运行期已被 applyRuntimeSettings 改过的值)。
func (s *Server) logLevel() string {
	if rt := s.settingsRT.Load(); rt != nil && rt.logLevel != "" {
		return rt.logLevel
	}
	return config.CurrentLogLevel()
}

// optString 解析"可空字符串"表单字段:空串 → nil(跟随 env)。
func optString(form map[string][]string, key string) *string {
	vals, ok := form[key]
	if !ok || len(vals) == 0 {
		return nil
	}
	v := strings.TrimSpace(vals[len(vals)-1])
	if v == "" {
		return nil
	}
	return &v
}

// optBool 解析布尔开关的三态。
//
// 表单侧有两套控件(见 home_content.html 的 settings-card-content):
//  1. <input type="hidden" name=key value="">  ← 保证 key 一定被提交
//     <input type="checkbox" name=key value="true">
//     勾选 → ["", "true"];未勾选 → [""]
//  2. <input type="checkbox" name=key_follow value="1">  ← "跟随 env 默认"
//     勾选 → 本项不落盘(等价 key 缺失)
//
// 判定顺序:follow 优先 —— 勾了"跟随默认"就一定是 nil,
// 否则按"任一值为 true 即 true,只有空串即 false,key 完全没提交也 nil"处理。
func optBool(form map[string][]string, key string) *bool {
	if _, ok := form[key+"_follow"]; ok {
		return nil
	}
	vals, ok := form[key]
	if !ok || len(vals) == 0 {
		return nil
	}
	for _, v := range vals {
		if v == "true" {
			t := true
			return &t
		}
	}
	f := false
	return &f
}

// optInt 解析"可空整数"表单字段:空串 → nil(跟随 env);非数字 → **报错**。
//
// v2.86 批次 E 起不再静默吞掉非法值。
//
// 原来非数字返回 nil,等于把"用户填错了"悄悄退化成"跟随 env 默认" ——
// 管理员填了 connlimit_per_ip=abc,保存成功、页面显示 50(来自 env),
// 但他以为自己已经改过了。这类"静默改错配置"比直接拒绝危险得多:
// 它既不会报错,也不会留下任何痕迹,而这批参数是防火墙限速阈值。
//
// 现在返回 error,由 handleSettingsSave 映射成 400 + flash,
// 用户能立刻看到"connlimit_per_ip 必须是整数"。
//
// 范围(上/下界)仍由 panelstate.Validate 负责,这里只管"是不是整数",
// 避免同一份约束出现两个真相。
func optInt(form map[string][]string, key string) (*int, error) {
	vals, ok := form[key]
	if !ok || len(vals) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(vals[len(vals)-1])
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("%s 必须是整数,当前是 %q", key, raw)
	}
	return &n, nil
}

// respondSettingsFlash 统一 save / clear 的响应出口(跟 respondAliyunFlash 同构)。
//
// v2.94:实现收敛到 respondFlashSlot,这里只固定住 "settings" 这个 slot。
func (s *Server) respondSettingsFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	s.respondFlashSlot(w, r, "settings", kind, msg, nil)
}
