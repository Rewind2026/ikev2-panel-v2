// PanelState:面板设置(v2.86 配置统一批次 D 新增)。
//
// 背景:
//
//	这批参数以前只能改 .env + docker compose up,改一个"登录会话时长"要走一遍
//	「编辑文件 → 重建容器 → 重新扫码」的流程,门槛过高。它们的共同特征是:
//	  - 高频微调(管理员自己会反复调)
//	  - 布尔 / 枚举 / 小整数,手写 env 容易写错(比如 IKEV2_COOKIE_SECURE=flase)
//	  - 不敏感(不是密钥,不像 aliyun.creds 需要 mask + audit)
//	  - 改错不致命(有 env / 出厂值兜底)
//
//	所以这批参数从 env-only 迁到 panelstate,并在首页加一张「面板设置」卡,
//	用开关 / 预配置下拉 / 范围输入框暴露给管理员。
//
// 优先级链(高 → 低):
//  1. /data/panel-state/settings.json (面板 UI / API 写入)
//  2. env(IKEV2_* / IKEV2_COOKIE_SECURE / IKEV2_TRUSTED_PROXY ...)
//  3. 代码出厂默认
//
// 三态语义(为什么用指针字段):
//
//	Go 的 *string / *bool / *int 在 encoding/json 下能区分 "key 缺失" 和
//	"key = 零值",这是本文件全部字段都用指针的原因:
//
//	  key 缺失(nil)  → 用 env / 出厂值          source = env-default
//	  key = 零值     → 显式覆盖(例如 cookie_secure=false) source = panelstate
//	  key = 值       → 显式覆盖                  source = panelstate
//
//	刻意**不引入 null**:普通 encoding/json 无法区分 key 缺失 与 key=null,
//	写 null 会让老版本 / 人工编辑的文件语义漂移。
//	"恢复默认"的 UI 语义 = 删掉该 key(整体删文件,跟 cert/subnet 卡一致)。
//
// 跟 cert.conf / subnet.conf 的区别:
//
//   - cert.conf / subnet.conf:改完要重启容器 / reload charon(影响 IPsec 数据面)
//   - settings.json:改完运行期立即生效(只影响面板自身的渲染与登录态),
//     不碰 charon、不碰证书、不碰 IP 段
//
// 跟 aliyun.creds 的区别:aliyun.creds 是凭证,需要 mask + audit;
//
//	settings.json 是普通配置,不 mask,但仍写 audit(谁改了登录策略要能追)。
//
// 文件格式(JSON,跟 cert.conf / subnet.conf 同风格):
//
//	{
//	  "schema_version": 1,
//	  "display_timezone": "Asia/Shanghai",
//	  "session_ttl": "24h",
//	  "cookie_secure": true,
//	  "trusted_proxy": false,
//	  "default_user_password_len": 12,
//	  "log_level": "debug",
//	  "log_format": "text",
//	  "audit_retention_days": 180,
//	  "connlimit_per_ip": 80,
//	  "hashlimit_per_min": 40,
//	  "disable_mss_clamp": false,
//	  "disable_ddos_protection": false,
//	  "nft_family": "v6",
//	  "updated_at": 1700000000
//	}
//
// 批次 E 追加说明:后 8 个字段里只有 log_level 是运行期立即生效,
// 其余都要重启容器(它们在 entrypoint 启动时就被读走写进 nft / cron)。
// schema_version 保持 1 —— 新增字段全是可选 + omitempty,
// 老文件缺这些 key 时读出来就是 nil,行为跟"跟随 env"完全一致。
//
// 注意:本文件的字段可以带 omitempty(指针为 nil 时 key 不落盘)。
// cert.go 的字段不能加 omitempty 是因为
// entrypoint 用 grep -E '"server_cn"' 取值,缺 key 会让 set -e 直接 exit 1。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SettingsSchemaVersion 当前文件格式版本。
//
// 老文件(没有 schema_version 字段)读到的是 0,行为跟 1 完全一致 ——
// 本版本没有做过字段重命名/语义变更,加这个字段只是为将来预留迁移判据。
const SettingsSchemaVersion = 1

// Settings 面板设置的运行时持久化结构。
//
// 所有可覆盖字段都是指针:nil = 跟随 env / 出厂默认(见文件头三态说明)。
type Settings struct {
	// SchemaVersion 文件格式版本,见 SettingsSchemaVersion。
	SchemaVersion int `json:"schema_version,omitempty"`

	// DisplayTimezone 面板 / DDNS / mobileconfig 的时间显示时区(IANA 名,如 Asia/Shanghai)。
	// nil = 用 env IKEV2_DISPLAY_TIMEZONE,再退到 UTC。
	DisplayTimezone *string `json:"display_timezone,omitempty"`

	// SessionTTL 登录会话有效期,Go duration 字符串("30m" / "24h" / "168h")。
	// nil = 用 env IKEV2_SESSION_TTL,再退到 24h。
	// 用字符串而不是 int 秒:面板下拉给的是 "24h" 这种人类可读值,
	// 存字符串可以原样回显,不用在 UI 层反复换算。
	SessionTTL *string `json:"session_ttl,omitempty"`

	// CookieSecure session cookie 的 Secure 标志。
	// nil = 用 env IKEV2_COOKIE_SECURE,再退到 true。
	// 指针而非 bool:需要区分"没配"(跟 env)和"显式关掉"。
	CookieSecure *bool `json:"cookie_secure,omitempty"`

	// TrustedProxy 是否信任 X-Forwarded-For / X-Real-IP(面板前面有反向代理时)。
	// nil = 用 env IKEV2_TRUSTED_PROXY,再退到 false。
	// 安全默认值 false:公网直连时读 XFF 等于让任意客户端伪造来源 IP 绕过登录锁定。
	TrustedProxy *bool `json:"trusted_proxy,omitempty"`

	// DefaultUserPasswordLen 新建 / 重置用户时随机密码的长度。
	// nil = 用 env IKEV2_DEFAULT_USER_PASSWORD_LEN,再退到 12。
	DefaultUserPasswordLen *int `json:"default_user_password_len,omitempty"`

	// ---- v2.86 批次 E:把 compose 里剩下"高频微调"的那批 env 也搬上 UI ----
	//
	// 挑选标准(跟批次 D 一致,见文件头):
	//   - 管理员会反复调,且改一次不想走"编辑 .env → 重建容器"
	//   - 布尔 / 枚举 / 小整数,手写 env 容易写错
	//   - 有明确的安全默认值兜底,改错不致命
	// 不满足的(网络拓扑 / 部署形态 / 路径类)一律留在 env,见
	// handlers_settings.go 的 deployOnlyParams 只读清单。

	// LogLevel slog 级别:debug / info / warn / error。
	// nil = 用 env IKEV2_LOG_LEVEL,再退到 info。
	// 运行期生效:config 侧用 slog.LevelVar 承接,保存后立刻换级别。
	LogLevel *string `json:"log_level,omitempty"`

	// LogFormat slog 输出格式:text / json。
	// nil = 用 env IKEV2_LOG_FORMAT,再退到 text。
	// **需重启**:handler 类型(text/json)在 slog.SetDefault 时就定死了,
	// 换格式等于换 handler,不做运行期热切。
	LogFormat *string `json:"log_format,omitempty"`

	// AuditRetentionDays 审计日志保留天数(cron 每天 04:00 删旧记录)。
	// nil = 用 env IKEV2_AUDIT_RETENTION_DAYS,再退到 90。
	// **需重启**:entrypoint 把这个值写进 /etc/cron.d 的注释与脚本 env。
	AuditRetentionDays *int `json:"audit_retention_days,omitempty"`

	// ConnLimitPerIP nftables 每 IP 并发 SA 上限(默认 50)。
	// nil = 用 env IKEV2_CONNLIMIT_PER_IP,再退到 entrypoint 的 50。
	// **需重启**:entrypoint 启动时把规则写进 iptables/nft。
	ConnLimitPerIP *int `json:"connlimit_per_ip,omitempty"`

	// HashLimitPerMin nftables 每 IP 每分钟新 SA 协商上限(默认 30)。
	// nil = 用 env IKEV2_HASHLIMIT_PER_MIN,再退到 30。
	// **需重启**:同上。
	HashLimitPerMin *int `json:"hashlimit_per_min,omitempty"`

	// DisableMSSClamp 关闭 TCP MSS clamping。
	// nil = 用 env IKEV2_DISABLE_MSS_CLAMP,再退到 false(即保持开启)。
	// **需重启**:entrypoint §4 决定要不要写 mangle 表。
	DisableMSSClamp *bool `json:"disable_mss_clamp,omitempty"`

	// DisableDDoSProtection 关闭 nft ikev2/ikev26 的 DDoS 防护。
	// nil = 用 env IKEV2_DISABLE_DDOS_PROTECTION,再退到 false(即保持开启)。
	// **需重启**:entrypoint §5 决定要不要 apply_ddos_protection。
	DisableDDoSProtection *bool `json:"disable_ddos_protection,omitempty"`

	// NftFamily 限定 nftables 只处理某个地址族:v4 / v6。
	// nil = 用 env IKEV2_FAMILY,空时 entrypoint 按 IPV6_ONLY 自动判断。
	// **需重启**:entrypoint §4.5 写 family.env,updown 限速按它路由。
	NftFamily *string `json:"nft_family,omitempty"`

	// UpdatedAt unix seconds,最后一次保存时间。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// IsZero 判断是否"什么都没设"(面板渲染"尚未配置"提示用)。
func (s *Settings) IsZero() bool {
	if s == nil {
		return true
	}
	return s.DisplayTimezone == nil &&
		s.SessionTTL == nil &&
		s.CookieSecure == nil &&
		s.TrustedProxy == nil &&
		s.DefaultUserPasswordLen == nil &&
		s.LogLevel == nil &&
		s.LogFormat == nil &&
		s.AuditRetentionDays == nil &&
		s.ConnLimitPerIP == nil &&
		s.HashLimitPerMin == nil &&
		s.DisableMSSClamp == nil &&
		s.DisableDDoSProtection == nil &&
		s.NftFamily == nil
}

// SettingsStore 面板设置的读写,带内存缓存。
//
// 读写骨架跟 CertConfigStore / SubnetConfigStore 完全一致
// (Validate → MkdirAll(0700) → MarshalIndent → CreateTemp → 0600 → atomic Rename → 刷缓存)。
type SettingsStore struct {
	mu sync.RWMutex

	// settings 内存缓存;nil = 文件不存在 / 已被 Clear
	settings *Settings

	// dir 测试可覆盖(默认 Dir)
	dir string
}

// NewSettingsStore 构造(默认 /data/panel-state)。
func NewSettingsStore() *SettingsStore {
	return &SettingsStore{dir: Dir}
}

// NewSettingsStoreWithDir 测试用,指定目录。
func NewSettingsStoreWithDir(dir string) *SettingsStore {
	return &SettingsStore{dir: dir}
}

// settingsPath 面板设置文件路径。
func (s *SettingsStore) settingsPath() string {
	return filepath.Join(s.dir, "settings.json")
}

// MinSessionTTL / MaxSessionTTL 会话时长允许范围。
//
// 下界 5 分钟:再短的话管理员改一个参数就把自己踢下线,且用户来不及察觉。
// 上界 30 天:超过这个量级用"长期不过期"之类的专门机制更合适。
const (
	MinSessionTTL = 5 * time.Minute
	MaxSessionTTL = 30 * 24 * time.Hour
)

// MinPasswordLen / MaxPasswordLen 随机密码长度允许范围。
//
// 下界跟 config.validate 的 [6,64] 保持一致 —— 同一份约束不能有两个真相。
const (
	MinPasswordLen = 6
	MaxPasswordLen = 64
)

// 批次 E 的取值范围。
//
// AuditRetentionDays 下界 1:0 / 负数会让 audit-retention.sh 把 cutoff 算成
// 未来时间,等于**删光整张审计表**。上界 3650(10 年)挡住手滑填的 999999。
// ConnLimitPerIP 下界 1:0 意味着"任何并发都 DROP",等于全员连不上。
// HashLimitPerMin 同理。
const (
	MinAuditRetentionDays = 1
	MaxAuditRetentionDays = 3650

	MinConnLimitPerIP  = 1
	MaxConnLimitPerIP  = 10000
	MinHashLimitPerMin = 1
	MaxHashLimitPerMin = 10000
)

// ValidLogLevels / ValidLogFormats / ValidNftFamilies 白名单。
//
// 为什么用白名单而不是"非空即可":这三个值直接进 slog handler 选项和
// entrypoint 的 grep/sed 提取链,放进一个拼错的值会出现"配置写了但没生效"
// 或 shell 侧行为漂移。宁可拒绝,也不让它静默降级。
var (
	ValidLogLevels  = []string{"debug", "info", "warn", "error"}
	ValidLogFormats = []string{"text", "json"}
	// NftFamily 留空表示"跟随 IKEV2_IPV6_ONLY 自动判断",那是 entrypoint 的默认行为,
	// 所以下拉的"跟随 env 默认"项对应 nil,而不是这里的某个值。
	ValidNftFamilies = []string{"v4", "v6"}
)

// Validate 校验面板设置。
//
// 只校验**显式设置**的字段(nil 直接跳过,交给 env 兜底):
//   - DisplayTimezone:必须是 time.LoadLocation 能加载的 IANA 名
//   - SessionTTL:必须是 time.ParseDuration 能解析的,且落在 [5m, 720h]
//   - DefaultUserPasswordLen:必须是 [6,64]
//   - CookieSecure / TrustedProxy:bool 无需校验
//
// 返回错误是用户输入错误(handler 映射成 400),不是内部错误。
func (s *Settings) Validate() error {
	if s.DisplayTimezone != nil {
		tz := *s.DisplayTimezone
		if tz == "" {
			return fmt.Errorf("display_timezone 不能为空(要跟随默认请清空该项)")
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("display_timezone %q 不是有效的 IANA 时区名(如 Asia/Shanghai)", tz)
		}
	}
	if s.SessionTTL != nil {
		raw := *s.SessionTTL
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("session_ttl %q 不是合法时长(如 30m / 24h / 168h)", raw)
		}
		if d < MinSessionTTL || d > MaxSessionTTL {
			return fmt.Errorf("session_ttl 必须在 %s ~ %s 之间,当前是 %s",
				MinSessionTTL, MaxSessionTTL, d)
		}
	}
	if s.DefaultUserPasswordLen != nil {
		n := *s.DefaultUserPasswordLen
		if n < MinPasswordLen || n > MaxPasswordLen {
			return fmt.Errorf("default_user_password_len 必须在 %d ~ %d 之间,当前是 %d",
				MinPasswordLen, MaxPasswordLen, n)
		}
	}
	if s.LogLevel != nil {
		if err := validateEnum("log_level", *s.LogLevel, ValidLogLevels); err != nil {
			return err
		}
	}
	if s.LogFormat != nil {
		if err := validateEnum("log_format", *s.LogFormat, ValidLogFormats); err != nil {
			return err
		}
	}
	if s.NftFamily != nil {
		if err := validateEnum("nft_family", *s.NftFamily, ValidNftFamilies); err != nil {
			return err
		}
	}
	if s.AuditRetentionDays != nil {
		if err := validateRange("audit_retention_days", *s.AuditRetentionDays,
			MinAuditRetentionDays, MaxAuditRetentionDays); err != nil {
			return err
		}
	}
	if s.ConnLimitPerIP != nil {
		if err := validateRange("connlimit_per_ip", *s.ConnLimitPerIP,
			MinConnLimitPerIP, MaxConnLimitPerIP); err != nil {
			return err
		}
	}
	if s.HashLimitPerMin != nil {
		if err := validateRange("hashlimit_per_min", *s.HashLimitPerMin,
			MinHashLimitPerMin, MaxHashLimitPerMin); err != nil {
			return err
		}
	}
	return nil
}

// validateEnum 白名单校验(小写比较,宽容处理 UI 传上来的大小写)。
func validateEnum(field, val string, allowed []string) error {
	low := strings.ToLower(strings.TrimSpace(val))
	if low == "" {
		return fmt.Errorf("%s 不能为空(要跟随默认请选「跟随 env 默认」)", field)
	}
	for _, a := range allowed {
		if low == a {
			return nil
		}
	}
	return fmt.Errorf("%s 只支持 %s,当前是 %q", field, strings.Join(allowed, " / "), val)
}

// validateRange 闭区间校验。
func validateRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s 必须在 %d ~ %d 之间,当前是 %d", field, lo, hi, v)
	}
	return nil
}

// LoadSettings 启动时读面板设置(供 Go 进程用)。
//
// 行为跟其它 Store 一致:
//   - 文件不存在 → 内存留 nil,不报错(用户没改过,降级到 env / 出厂)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错配置)
//   - 文件存在但字段不合法 → 报错(防御性)
func (s *SettingsStore) LoadSettings() (*Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.settingsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.settings = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.settingsPath(), err)
	}

	var st Settings
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.settingsPath(), err)
	}
	if err := st.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", s.settingsPath(), err)
	}

	s.settings = &st
	return &st, nil
}

// ReadSettings handler 端读面板设置。
//
// 行为:优先返回缓存,缓存空时降级到磁盘 Load。
func (s *SettingsStore) ReadSettings() (*Settings, error) {
	s.mu.RLock()
	st := s.settings
	s.mu.RUnlock()

	if st != nil {
		return st, nil
	}
	return s.LoadSettings()
}

// WriteSettings 写面板设置(handler 调用)。
//
// 行为:
//   - Validate 失败 → 返回错误
//   - 保证目录存在(0700)
//   - atomic rename 写(避免 SIGHUP / 重启读到半截)
//   - 文件权限 0600
//   - 写入后更新内存缓存 + UpdatedAt + SchemaVersion
func (s *SettingsStore) WriteSettings(st Settings) error {
	if err := st.Validate(); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().Unix()
	st.SchemaVersion = SettingsSchemaVersion

	// 1. 目录存在
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	// 2. atomic 写
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	target := s.settingsPath()
	tmp, err := os.CreateTemp(s.dir, ".settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}

	// 3. atomic rename
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename tmp -> target: %w", err)
	}

	// 4. 更新内存缓存
	s.mu.Lock()
	st2 := st
	s.settings = &st2
	s.mu.Unlock()

	return nil
}

// SettingsExists 文件是否存在(handler 渲染用)。
//
// 不读文件内容,只 stat。
func (s *SettingsStore) SettingsExists() bool {
	_, err := os.Stat(s.settingsPath())
	return err == nil
}

// ClearSettings 清除面板设置(回到 env / 出厂默认)。
//
// 行为:rm 文件 + 清内存缓存。重启或下次 handler 读时回退到 env。
func (s *SettingsStore) ClearSettings() error {
	s.mu.Lock()
	s.settings = nil
	s.mu.Unlock()

	if err := os.Remove(s.settingsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
