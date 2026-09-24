// DDNS 同步后台 goroutine(v2-82 新增,v2-84 改造 family-aware)。
//
// 背景(设计见 docs/design.md §19):
//   - 用户 VPS 的公网 IPv4 / IPv6 是动态分配的(SLAAC / PPPoE 重拨)
//   - 域名 vpn.example.com 的 A / AAAA 记录如果不跟着变,客户端就连不上
//   - 证书本身跟 IP 无关(SAN 只放域名,DNS-01 签发),所以不需要重新签
//   - ipv6watch(已有)负责更新 swanctl.conf 的 local_addrs;
//     DDNS 负责同步更新 DNS 解析记录
//
// 架构(v2-84 dual):
//
//	┌─────────────────┐      ┌─────────────────┐      ┌──────────────────┐
//	│ detectV4 / V6   │ ───► │ ddns.Sync()     │ ───► │ alidns UpdateRec │
//	│ (swanctl)       │      │ (并发 + 节流)     │      │ (HTTP API)       │
//	└─────────────────┘      └─────────────────┘      └──────────────────┘
//
// 开关:
//   - IKEV2_DDNS_ENABLED=true   启用(运行时可由面板 UI 关)
//   - IKEV2_DDNS_ENABLED=false  禁用
//   - IKEV2_DDNS_FAMILY=v4|v6|dual 同步哪些 family(默认 dual)
//   - 状态文件 /etc/ikev2/ddns.conf 记录运行时开关(优先级高于 env,INI-style)
//
// 节流:per-type(独立 A / AAAA 节流窗口),60 秒内最多同步一次/类型
//
// v2-84 重大改动:
//   - 引入 family 枚举(v4 / v6 / dual),默认 dual
//   - 并发跑 v4 / v6(errgroup),互相不阻塞
//   - LastSync 结构化(V4IP/V4Error/V6IP/V6Error 独立)
//   - 状态文件 INI 格式 + atomic write + 自动迁移 v2-83 老格式
package ddns

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// Config DDNS 同步配置。
type Config struct {
	// Enabled 总开关(env IKEV2_DDNS_ENABLED,默认 false)
	Enabled bool
	// Family v2-84:同步哪些 family。取值 "v4" / "v6" / "dual"。
	// 默认 "dual"。family = "v4" 时仅同步 A 记录;="v6" 时仅同步 AAAA;
	// ="dual" 时并发同步 A + AAAA(v4 retry 不阻塞 v6)。
	//
	// v2.86-pr23a:deprecated — EnableA/EnableAAAA 两个独立 bool 替代。
	// 保留字段是为了兼容老用户的 statefile / env。新代码优先看 EnableA/EnableAAAA。
	Family string
	// EnableA v2.86-pr23a:是否同步 A 记录(IPv4)。panelstate 可改。
	EnableA bool
	// EnableAAAA v2.86-pr23a:是否同步 AAAA 记录(IPv6)。panelstate 可改。
	EnableAAAA bool
	// v2.86-pr23l:DetectTarget 字段废弃。IPv4 公网地址由 internal/publicip 多家
	// API fallback 探测,swanctl.DetectGlobalV4 的 target 参数已忽略。
	// AliyunAccessKeyID 阿里云 AccessKey ID(v2-83:仅作为启动时 fallback;运行时优先用 CredentialGetter)
	AliyunAccessKeyID string
	// AliyunAccessKeySecret 阿里云 AccessKey Secret(同上)
	AliyunAccessKeySecret string
	// CredentialGetter v2-83:运行时获取凭证,优先于 env。
	// 返回值: (keyID, keySecret, ok) — ok=false 时跳过本次 tick。
	// 设计动机:面板 UI 改凭证 → 写到 /data/panel-state/aliyun.creds → getter 读这个文件,
	// DDNS 下次 tick 自动用新凭证(无需重启容器)。
	CredentialGetter func() (string, string, bool)
	// Domain 域名(如 "example.com",完整域名 = RR + "." + Domain)
	Domain string
	// RR 主机记录(如 "vpn")
	RR string
	// Iface 监听的网络接口(空 = 任意接口的 global v6)
	Iface string
	// Period 探测间隔(默认 60 秒)
	Period time.Duration
	// Throttle 同步节流(默认 60 秒,防止阿里云 API 限流)
	// v2-84:per-type(独立 A / AAAA 节流窗口),v4 retry 1s/4s/9s 不阻塞 v6
	Throttle time.Duration
	// LastFailedFile 失败标志文件路径(/data/le/LAST_DDNS_FAILED)
	LastFailedFile string
	// StateFile 运行时开关文件(/etc/ikev2/ddns.conf,不存在时回退到 env)
	// v2-84:INI-style key=value,自动迁移 v2-83 裸 true/false
	StateFile string
	// Logger
	Logger *slog.Logger
}

// LastSync DDNS 最近一次同步状态(给面板用)。
//
// v2-84 结构化:dual 模式下 v4 / v6 独立记录,顶层 Success 取"任一 family 成功过"。
// 保留 NewIP/OldIP/Error 字段(omitempty)做向后兼容,v2-83 JSON 客户端读不到新字段
// 但不会报错。
type LastSync struct {
	Time    time.Time // 整轮最近一次 tick 时间
	Success bool      // 整轮 = 任一 enabled family 成功过(详细见 D5 proposal)

	// v2.86-pr23g:整轮"有没有真正跑过探测"。区分:
	//   - tick 入口早 return(凭证缺 / Domain 空 / 节流整轮跳过)→ Attempted=false
	//   - tick 跑到探测阶段(enabled family 之一跑了)→ Attempted=true
	// UI 用这个判断"等待首次 tick" vs "已经尝试过(成功/失败/无变化)"。
	Attempted bool

	// v2-84:per-family 状态。空字符串 = 该 family 未启用 / 未探测 / 探测失败。
	V4IP    string
	V4Error string
	V6IP    string
	V6Error string

	// v2.86-pr23g:per-family "这次 tick 是否实际尝试过"(区分未启用 / 被节流跳过)。
	//   - V4Attempted=true:tick 至少跑过一次 v4 探测(无论成功失败)
	//   - V4Attempted=false:tick 没跑 v4(unenabled / 全轮 throttle 跳过 / 全轮早 return)
	// UI 用这俩字段把"未同步/未启用/未尝试" 三态分开。
	V4Attempted bool
	V6Attempted bool

	// v2-83 兼容字段(omitempty,dual 模式下置空;单 family 模式才填)。
	// 移除时机:v2-85+ 确认 v2-83 客户端没人用了再删。
	NewIP string `json:",omitempty"`
	OldIP string `json:",omitempty"`
	Error string `json:",omitempty"`
}

// Sync 后台 DDNS 同步器。
//
// 不是 goroutine 本身——把 Run() 放到 goroutine 里调用。
// 提供 SetEnabled / SetFamily / SetConfig / TriggerNow 供面板 UI 切换用。
//
// v2-84:detectV4 注入,per-type 节流 map。
// v2.86-pr23a:面板可改 RR / EnableA / EnableAAAA / Period,内存立即生效;
// 上一组 family 枚举被 EnableA/EnableAAAA 两个独立 bool 替代(更符合
// "DDNS 是阿里云 API 的子功能"的产品认知)。
type Sync struct {
	cfg Config

	mu           sync.Mutex
	lastSync     LastSync
	lastSyncTime map[string]time.Time // v2-84:per-type 节流(key="A"/"AAAA")
	running      bool                 // goroutine 是否在跑
	stopCh       chan struct{}
	stopOnce     sync.Once // v2.85-PR6 (Q1-02):Stop 只关一次 stopCh
	client       *dns.AliyunClient

	// 探测函数(注入式,方便测试;生产代码由 swanctl 包提供)。
	// v2-84 加 detectV4,签名带 target(IPv4 探测目标,可配置)。
	detectV6 func(iface string) (string, error)
	detectV4 func(iface, target string) (string, error)

	// v2.86-pr23a:手动触发 channel(handler POST /api/ddns/sync-now 时塞一个)。
	// buffered=1 → handler 立即返回,不等 tick 完成。
	triggerCh chan struct{}

	// v2.86-pr23a:上一次 "查询阿里云记录值" 的快照(handler POST /api/ddns/fetch-remote)。
	// 暴露给 UI 展示"阿里云实际值 vs DDNS 推上去的值"是否一致。
	remoteSnap RemoteSnapshot
}

// RemoteSnapshot v2.86-pr23a:面板"查询当前阿里云记录值"按钮的返回。
//
// 用于排查"DDNS 显示同步成功但实际解析不对"——直接调 FindRecord 拿阿里云
// 此刻实际值,跟 LastSync.V4IP / V6IP 对比即可看出是否一致。
type RemoteSnapshot struct {
	// FetchedAt 查询时间(用户点按钮时)
	FetchedAt time.Time
	// Domain 查询的完整域名(RR.Domain 或 Domain)
	Domain string
	// A 阿里云当前 A 记录值(空 = 记录不存在)
	A string
	// AAAA 阿里云当前 AAAA 记录值(空 = 记录不存在)
	AAAA string
	// Error 错误信息(查询失败时填;成功时为空)
	Error string
}

// NewSync 构造同步器。
func NewSync(cfg Config) *Sync {
	if cfg.Period <= 0 {
		cfg.Period = 60 * time.Second
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = 60 * time.Second
	}
	if cfg.LastFailedFile == "" {
		cfg.LastFailedFile = "/data/le/LAST_DDNS_FAILED"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "/etc/ikev2/ddns.conf"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RR == "" {
		cfg.RR = "@"
	}
	if cfg.Family == "" {
		cfg.Family = "dual"
	}
	if !validFamily(cfg.Family) {
		cfg.Logger.Warn("ddns: invalid Config.Family, fallback to dual",
			"got", cfg.Family)
		cfg.Family = "dual"
	}
	// v2.86-pr23a:EnableA/EnableAAAA 默认值 fallback 移到下面 — 必须等 statefile 覆盖完再做。
	// (statefile 显式写 enable_aaaa=false 时,要在 fallback 之前读到 cfg。)

	s := &Sync{
		cfg: cfg,
		// v2-83:client 不在 NewSync 时构造,而是在每次 tick 时按当前凭证构造。
		// 这样面板 UI 改凭证后,DDNS 下次 tick 自动用新凭证构造 client(无需重启)。
		client:   nil,
		detectV6: nil,
		detectV4: nil,
		stopCh:   make(chan struct{}),
		// stopOnce v2.85-PR6 (Q1-02):zero value 即可,无需显式构造。
		// 保留 stopCh + 加 sync.Once:Stop() 只 close 一次,语义干净、可复用实例。
		lastSyncTime: make(map[string]time.Time), // v2-84:per-type 节流
		// v2.86-pr23a:手动触发 channel(buffered=1 → handler 非阻塞,Run() select 时拿到)
		triggerCh: make(chan struct{}, 1),
	}

	// v2-84 状态文件读取(兼容 v2-83 老格式 + INI 自动迁移)
	//
	// 优先级:状态文件(若存在) > env 启动值
	// 注意:状态文件不存在时回退到 env cfg.Enabled,不能简单覆盖为 false。
	defaultFamily := cfg.Family
	migrateStateFileIfNeeded(s.cfg.StateFile, defaultFamily, s.cfg.Logger)
	if _, err := os.Stat(s.cfg.StateFile); err == nil {
		if rec, readErr := parseStateFile(s.cfg.StateFile, defaultFamily); readErr == nil {
			s.cfg.Enabled = rec.Enabled
			s.cfg.Family = rec.Family
			// v2.86-pr23f:Domain 从 statefile 覆盖(空 = 沿用 env 启动值,向后兼容)
			if rec.Domain != "" {
				s.cfg.Domain = rec.Domain
			}
			// v2.86-pr23a:RR / EnableA / EnableAAAA / Period 从 statefile 覆盖
			// (statefile 是 source of truth — writeStateFile 总是写这三个字段,
			// 所以 rec 里的 zero value 也代表"用户显式关了")
			s.cfg.RR = rec.RR
			s.cfg.EnableA = rec.EnableA
			s.cfg.EnableAAAA = rec.EnableAAAA
			// PeriodSec > 0 才覆盖(0 = statefile 里没设,沿用 env 默认)
			if rec.PeriodSec > 0 {
				s.cfg.Period = time.Duration(rec.PeriodSec) * time.Second
			}
			// v2.85-PR6 (Q5-01):回填 throttle 时间戳(unix 秒 → time.Time)。
			// map 未初始化的 key 读出来是 zero value,
			// throttle 检查 !s.lastSyncTime[rt].IsZero() 已能区分"从未同步"和"已同步过"。
			if rec.LastSyncA > 0 {
				s.lastSyncTime[dns.RecordTypeA] = time.Unix(rec.LastSyncA, 0)
			}
			if rec.LastSyncAAAA > 0 {
				s.lastSyncTime[dns.RecordTypeAAAA] = time.Unix(rec.LastSyncAAAA, 0)
			}
		}
	}

	// v2.86-pr23a:EnableA/EnableAAAA 默认值 fallback。
	// 必须在 statefile 覆盖之后再做 — statefile 显式 false 时不能被 fallback 改成 true。
	// 三种情况:
	// 1. caller 显式传了 EnableA 或 EnableAAAA → 用之
	// 2. statefile 写了这俩字段(不管是 true/false)→ 用之
	// 3. 都没有 → 按 cfg.Family 翻译;fallback 到 dual 全开
	//
	// v2.86-pr23b:无改动(parseStateFile 已经无条件覆盖 cfg.EnableA/EnableAAAA,
	// 即使 statefile 显式写 enable_aaaa=false 也会落到 cfg,不会进 fallback)。
	if !s.cfg.EnableA && !s.cfg.EnableAAAA {
		switch s.cfg.Family {
		case "v4":
			s.cfg.EnableA = true
		case "v6":
			s.cfg.EnableAAAA = true
		default: // "dual" 或 fallback 后
			s.cfg.EnableA = true
			s.cfg.EnableAAAA = true
		}
	}
	return s
}

// SetDetectV6 注入 IPv6 探测函数。
//
// 拆分原因:swanctl 包不应 import ddns 包(避免循环);main.go 在装配时
// 把 swanctl.DetectGlobalV6 绑过来。
func (s *Sync) SetDetectV6(fn func(iface string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detectV6 = fn
}

// SetDetectV4 v2-84:注入 IPv4 探测函数。fn 签名带 target 参数
// (探测目标,可配置;生产默认 swanctl.DetectGlobalV4)。
func (s *Sync) SetDetectV4(fn func(iface, target string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detectV4 = fn
}

// IsEnabled 当前是否启用(env 或 state file 任一开启都算)。
func (s *Sync) IsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Enabled
}

// LastSyncSnapshot 面板读取最近一次同步结果。
func (s *Sync) LastSyncSnapshot() LastSync {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync
}

// SetEnabled 切换开关,同步更新 state file(原子写)。
//
// 返回错误:state file 写失败(但内存里已更新,不影响本次运行)。
// v2.86-pr23a:stateRecord 加了新字段,这里用 snapshot() helper 拿全字段。
func (s *Sync) SetEnabled(enabled bool) error {
	s.mu.Lock()
	s.cfg.Enabled = enabled
	rec := s.snapshotStateRecord()
	s.mu.Unlock()

	if err := writeStateFile(s.cfg.StateFile, rec); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// SetFamily v2-84:切换 DDNS family,同步更新 state file(原子写)。
//
// family 必须是 "v4" / "v6" / "dual",否则返回 error(双重白名单,跟 handler 端呼应)。
// 内存立即更新,但下次 tick 才生效(节流保护)。
//
// v2.86-pr23a:deprecated — 由 SetConfig(rr, enableA, enableAAAA, period) 替代。
// 保留此 API 是因为 /api/ddns/family endpoint 还在(handler 端做软兼容)。
func (s *Sync) SetFamily(family string) error {
	if !validFamily(family) {
		return fmt.Errorf("invalid family %q (supported: v4, v6, dual)", family)
	}
	s.mu.Lock()
	s.cfg.Family = family
	// 同步把 EnableA/EnableAAAA 改成跟 family 一致(向后兼容老 API 调用方)
	switch family {
	case "v4":
		s.cfg.EnableA = true
		s.cfg.EnableAAAA = false
	case "v6":
		s.cfg.EnableA = false
		s.cfg.EnableAAAA = true
	case "dual":
		s.cfg.EnableA = true
		s.cfg.EnableAAAA = true
	}
	rec := s.snapshotStateRecord()
	s.mu.Unlock()

	if err := writeStateFile(s.cfg.StateFile, rec); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	s.cfg.Logger.Info("ddns: family updated (deprecated, prefer SetConfig)",
		"family", family, "enable_a", s.cfg.EnableA, "enable_aaaa", s.cfg.EnableAAAA)
	return nil
}

// snapshotStateRecord v2.86-pr23a:在锁内把 cfg + lastSyncTime 打包成完整 stateRecord。
//
// 给 SetEnabled / SetFamily / SetConfig / setLastSyncAndPersist 共用,避免每个
// 持锁片段重复拷贝字段(加新字段时漏一处 → statefile 持久化丢字段)。
func (s *Sync) snapshotStateRecord() stateRecord {
	var lastA, lastAAAA int64
	if v := s.lastSyncTime[dns.RecordTypeA]; !v.IsZero() {
		lastA = v.Unix()
	}
	if v := s.lastSyncTime[dns.RecordTypeAAAA]; !v.IsZero() {
		lastAAAA = v.Unix()
	}
	return stateRecord{
		Enabled:      s.cfg.Enabled,
		Family:       s.cfg.Family,
		Domain:       s.cfg.Domain,
		RR:           s.cfg.RR,
		EnableA:      s.cfg.EnableA,
		EnableAAAA:   s.cfg.EnableAAAA,
		PeriodSec:    int(s.cfg.Period.Seconds()),
		LastSyncA:    lastA,
		LastSyncAAAA: lastAAAA,
	}
}

// FetchRemote v2.86-pr23a:面板 "查询阿里云记录值" 按钮 handler 调用。
//
// 行为:
//   - 阻塞调 FindRecord 两次(A + AAAA),错误聚合到 RemoteSnapshot.Error
//   - 凭证缺失 / 域名缺失 → 立即返回 error(不调 API)
//   - 结果存到 remoteSnap → handler 渲染 home template
//   - handler 端要自己处理 timeout(因为是阻塞调用)
//
// 跟 LastSync 的区别:LastSync 是 DDNS **推**上去的值;RemoteSnapshot 是阿里云
// **当前实际**值。两者不一致 → 说明 DDNS 没同步成功,或者阿里云那边被手动改了。
func (s *Sync) FetchRemote(ctx context.Context) (RemoteSnapshot, error) {
	s.mu.Lock()
	domain := s.cfg.Domain
	rr := s.cfg.RR
	s.mu.Unlock()

	fullDomain := domain
	if rr != "" && rr != "@" {
		fullDomain = rr + "." + domain
	}

	snap := RemoteSnapshot{
		FetchedAt: time.Now(),
		Domain:    fullDomain,
	}

	if domain == "" {
		snap.Error = "DDNS 主域名未配置(env IKEV2_DDNS_DOMAIN)"
		s.mu.Lock()
		s.remoteSnap = snap
		s.mu.Unlock()
		return snap, fmt.Errorf("domain not set")
	}

	keyID, keySecret := s.getCredentials()
	if keyID == "" || keySecret == "" {
		snap.Error = "阿里云凭证未配置(请先在 AccessKey 框填值)"
		s.mu.Lock()
		s.remoteSnap = snap
		s.mu.Unlock()
		return snap, fmt.Errorf("aliyun creds not configured")
	}

	client := dns.NewAliyunClient(keyID, keySecret)

	// A 记录查询(只查用户启用的 family;没启用的 record_type 不查)
	if s.EnableA() {
		rec, err := client.FindRecord(domain, rr, dns.RecordTypeA)
		if err != nil {
			snap.Error = fmt.Sprintf("A 查询失败: %v", err)
		} else if rec != nil {
			snap.A = rec.Value
		}
	}

	if s.EnableAAAA() {
		rec, err := client.FindRecord(domain, rr, dns.RecordTypeAAAA)
		if err != nil {
			prev := snap.Error
			if prev != "" {
				prev += "; "
			}
			snap.Error = prev + fmt.Sprintf("AAAA 查询失败: %v", err)
		} else if rec != nil {
			snap.AAAA = rec.Value
		}
	}

	s.mu.Lock()
	s.remoteSnap = snap
	s.mu.Unlock()

	if snap.Error != "" {
		return snap, fmt.Errorf("%s", snap.Error)
	}
	return snap, nil
}

// getCredentials v2.86-pr23a:跟 tick() 里同样的凭证解析逻辑(extract helper)。
func (s *Sync) getCredentials() (string, string) {
	s.mu.Lock()
	keyID, keySecret := s.cfg.AliyunAccessKeyID, s.cfg.AliyunAccessKeySecret
	getter := s.cfg.CredentialGetter
	s.mu.Unlock()
	if getter != nil {
		if id, secret, ok := getter(); ok && id != "" && secret != "" {
			return id, secret
		}
	}
	return keyID, keySecret
}

// Family v2-84:返回当前 family 配置。
//
// v2.86-pr23a:deprecated — EnableA/EnableAAAA 两个独立 bool 替代了 family 枚举。
// 保留此 getter 是因为 JSON API / 健康检查还引用。优先从 (EnableA, EnableAAAA)
// 推断;两个都关时回退到 cfg.Family 字段(老 statefile 兼容)。
func (s *Sync) Family() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.cfg.EnableA && s.cfg.EnableAAAA:
		return "dual"
	case s.cfg.EnableA:
		return "v4"
	case s.cfg.EnableAAAA:
		return "v6"
	default:
		// 两个都关 → 用 family 字段(向后兼容老用户)
		return s.cfg.Family
	}
}

// EnableA v2.86-pr23a:返回是否同步 A 记录(IPv4)。
func (s *Sync) EnableA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.EnableA
}

// EnableAAAA v2.86-pr23a:返回是否同步 AAAA 记录(IPv6)。
func (s *Sync) EnableAAAA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.EnableAAAA
}

// PeriodSeconds v2.86-pr23a:返回同步周期秒数(给面板显示 + statefile 持久化)。
func (s *Sync) PeriodSeconds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.cfg.Period.Seconds())
}

// SetConfig v2.86-pr23a:面板改 RR / EnableA / EnableAAAA / Period 的统一入口。
// v2.86-pr23f:加 Domain 参数(空 = 不改 env fallback 的 domain)。
//
// 设计要点:
//   - 内存立即更新(cfg 字段直接赋值)
//   - 持久化到 statefile(跟 SetEnabled / SetFamily 同 mode,handler 重启后值不丢)
//   - Period 改动:**不立即重启 ticker**(避免并发 race);Run() 下一轮
//     ticker.C fire 后会读最新 Period,新周期自然生效
//   - EnableA / EnableAAAA 改动:内存改了,tick 下次 fire 时走新分支
//
// 参数:
//   - rr:主机记录(空 = "@")
//   - enableA / enableAAAA:同步哪些 family
//   - periodSeconds:同步周期(0 = 不改;传有效值时 clamp 到 [10, 3600])
//   - domain:v2.86-pr23f 主域名(空 = 不改,沿用 env / 已配值)
//
// 返回 error:statefile 写失败(内存已更新,只是不持久化)。
func (s *Sync) SetConfig(rr string, enableA, enableAAAA bool, periodSeconds int, domain string) error {
	s.mu.Lock()
	if domain != "" {
		s.cfg.Domain = domain
	}
	s.cfg.RR = rr
	s.cfg.EnableA = enableA
	s.cfg.EnableAAAA = enableAAAA
	if periodSeconds > 0 {
		if periodSeconds < 10 {
			periodSeconds = 10
		}
		if periodSeconds > 3600 {
			periodSeconds = 3600
		}
		s.cfg.Period = time.Duration(periodSeconds) * time.Second
	}
	rec := s.snapshotStateRecord()
	s.mu.Unlock()

	// 持久化到 /etc/ikev2/ddns.conf(INI-style,跟 SetEnabled/SetFamily 走同 path)
	if err := writeStateFile(s.cfg.StateFile, rec); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	s.cfg.Logger.Info("ddns: config updated",
		"domain", domain, "rr", rr,
		"enable_a", enableA, "enable_aaaa", enableAAAA,
		"period_seconds", int(s.cfg.Period.Seconds()),
	)
	return nil
}

// TriggerNow v2.86-pr23a:通知 Run() 立即跑一次 tick(handler POST /api/ddns/sync-now)。
//
// 非阻塞:channel buffered=1,handler 立即返回。Run() 在 select 里取到 → tick。
// 如果 channel 已 buffered 但 Run() 没消费 → 本次塞入被丢弃(下次 Run() 取到的是旧值,
// 仍会触发一次 tick;不会被卡住)。
func (s *Sync) TriggerNow() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
		// channel 已有 pending → 不重复塞,Run() 反正会跑
	}
}

// RemoteSnapshot v2.86-pr23a:面板 "查询阿里云记录值" 按钮触发的结果。
//
// handler POST /api/ddns/fetch-remote → 立即同步调 FindRecord 拿 A/AAAA → 存到
// remoteSnap → 渲染到 home template 的 status 表。
func (s *Sync) RemoteSnapshot() RemoteSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteSnap
}

// Domain v2.86-pr22:返回完整域名(RR + "." + Domain,如 "vpn.example.com")。
// 供面板 DDNS 卡展示当前同步目标。
func (s *Sync) Domain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.RR == "" || s.cfg.RR == "@" {
		return s.cfg.Domain
	}
	return s.cfg.RR + "." + s.cfg.Domain
}

// DetectTarget v2.86-pr23l:已废弃,IPv4 探测不再使用 target 参数。
// 保留 getter 以兼容调用方(返回空字符串即可)。
func (s *Sync) DetectTarget() string {
	return "" // deprecated
}

// Period v2.86-pr22:返回同步周期(如 60s)。
// 供面板 DDNS 卡展示调度间隔。
func (s *Sync) Period() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Period
}

// RR v2.86-pr22:返回主机记录名(如 "vpn" / "@")。
func (s *Sync) RR() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.RR
}

// BaseDomain v2.86-pr22:返回裸域名(如 "example.com",不含 RR)。
func (s *Sync) BaseDomain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Domain
}

// Iface v2.86-pr22:返回监听的网络接口名(空 = 任意)。
func (s *Sync) Iface() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Iface
}

// Run 启动后台循环,阻塞到 ctx.Done 或 stopCh。
//
// 行为:
//  1. 立即跑一次(确保状态正确)
//  2. 每 Period 跑一次
//  3. 探测到 IP 变 → 节流判断 → 调 alidns → 重试 3 次 → 写 LastSync
func (s *Sync) Run(ctx context.Context) error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.cfg.Logger.Info("ddns sync starting",
		"enabled", s.cfg.Enabled,
		"domain", s.cfg.Domain,
		"rr", s.cfg.RR,
		"period", s.cfg.Period,
		"throttle", s.cfg.Throttle,
	)

	// 启动时立即跑一次(如果 enabled)
	if s.cfg.Enabled {
		s.tick()
	}

	ticker := time.NewTicker(s.cfg.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.cfg.Logger.Info("ddns sync stopping (ctx done)")
			return nil
		case <-s.stopCh:
			s.cfg.Logger.Info("ddns sync stopping (stop signal)")
			return nil
		case <-s.triggerCh:
			// v2.86-pr23a:面板 "手动同步一次" 按钮触发的立即 tick。
			// 不等 ticker 周期,直接跑(节流照常生效,刚同步过就跳过)。
			s.tick()
		case <-ticker.C:
			s.tick()
		}
	}
}

// Stop 通知 Run 退出(下一轮 ticker 时退出)。
//
// v2.85-PR6 (Q1-02):用 sync.Once 代替 select default,语义更干净。
//   - 多次调用 Stop() 不 panic
//   - 测试可以反复 NewSync 而不踩到 stopCh 残留状态
func (s *Sync) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// setLastSyncAndPersist v2.85-PR6 (Q5-01):节流时间戳更新 + 落盘(statefile)。
//
// 设计要点:
//   - 单写入口,避免后续 tick 内多个 goroutine 重复 persist
//   - 锁内只收集数据(避免持锁时做 IO),锁外 writeStateFile
//   - 写盘失败仅 warn,不影响本次 upsert 结果(节流窗口已在内存里生效)
func (s *Sync) setLastSyncAndPersist(rt string, t time.Time) {
	s.mu.Lock()
	s.lastSyncTime[rt] = t
	rec := s.snapshotStateRecord()
	s.mu.Unlock()

	if err := writeStateFile(s.cfg.StateFile, rec); err != nil {
		s.cfg.Logger.Warn("ddns: persist throttle time failed",
			"rt", rt, "err", err)
	}
}

// tick 单次探测 + 同步(v2-84 family-aware,errgroup 并发跑 v4/v6)。
//
// 流程:
//  1. 凭证校验(整轮共享,失败整轮 skip)
//  2. 按 cfg.Family 起 v4 / v6 子任务(各自独立节流 + upsert)
//  3. 聚合 per-family 结果到 LastSync
//  4. 顶层 Success = 任一 enabled family 成功过
//  5. dual 模式下 v4 retry 1s/4s/9s 不阻塞 v6(并发)
func (s *Sync) tick() {
	if !s.cfg.Enabled {
		return
	}

	// 凭证解析(v2-83):优先用 CredentialGetter,fallback 到 env。
	keyID, keySecret := s.cfg.AliyunAccessKeyID, s.cfg.AliyunAccessKeySecret
	if s.cfg.CredentialGetter != nil {
		if id, secret, ok := s.cfg.CredentialGetter(); ok && id != "" && secret != "" {
			keyID, keySecret = id, secret
		}
	}

	// 凭证校验:缺失 → 整轮 skip(dual/v4/v6 都不跑)
	if keyID == "" || keySecret == "" {
		s.cfg.Logger.Debug("ddns: aliyun credentials not set, skipping tick")
		return
	}
	if s.cfg.Domain == "" {
		s.cfg.Logger.Debug("ddns: domain not set, skipping tick")
		return
	}

	// v2-83:每次 tick 构造 client(支持运行时凭证变更)
	client := dns.NewAliyunClient(keyID, keySecret)

	// 节流检查(per-type,在探测之前):v2-83 节流检查也放在 detect 之前,
	// 这样 Throttle 窗口内的 tick 完全跳过,既不探测也不调 API。
	// v2-84 per-type:每个 family 独立节流。
	s.mu.Lock()
	now := time.Now()
	throttledV6 := !s.lastSyncTime[dns.RecordTypeAAAA].IsZero() &&
		now.Sub(s.lastSyncTime[dns.RecordTypeAAAA]) < s.cfg.Throttle
	throttledV4 := !s.lastSyncTime[dns.RecordTypeA].IsZero() &&
		now.Sub(s.lastSyncTime[dns.RecordTypeA]) < s.cfg.Throttle
	s.mu.Unlock()

	// 每个 family 独立判断:enabled 的 family 如果被 throttle 就 skip,
	// 没被 throttle 就跑(dual 下两个 family 各自独立)。
	// v2.86-pr23a:走 EnableA/EnableAAAA 而不是 cfg.Family 枚举 — 面板直接控制。
	runV6 := !throttledV6 && s.cfg.EnableAAAA
	runV4 := !throttledV4 && s.cfg.EnableA

	// dual 模式下两个 family 都被 throttle → 整轮 skip(节省 goroutine 开销)
	if !runV6 && !runV4 {
		return
	}

	// 探测(各 family 独立):失败只影响该 family,不影响另一 family。
	// detector 未注入(nil)时直接 return ("", nil),由 runFamily 判断空 IP 跳过。
	var v6IP string
	var v6Err error
	if runV6 {
		v6IP, v6Err = s.detectFamily("AAAA", s.detectV6, s.cfg.Iface)
	}
	var v4IP string
	var v4Err error
	if runV4 {
		v4IP, v4Err = s.detectFamily("A", func(string) (string, error) {
			if s.detectV4 == nil {
				return "", nil
			}
			// v2.86-pr23l:第二个参数(target)已废弃,内部走公网 IP API 检测。
			return s.detectV4(s.cfg.Iface, "")
		}, s.cfg.Iface)
	}

	// 节流检查 + upsert,按 family 并发(用 WaitGroup 而不是 errgroup,
	// 避免引入 golang.org/x/sync 依赖)
	var wg sync.WaitGroup
	type result struct {
		recordType string
		oldIP      string
		newIP      string
		err        error
	}
	results := make(chan result, 2)

	runFamily := func(rt, ip string, err error) {
		defer wg.Done()
		if err != nil || ip == "" {
			// 探测失败/空 IP:跳过 upsert,err 留给外层聚合
			results <- result{recordType: rt, err: err}
			return
		}
		// 节流已经在 tick 入口检查过(tick 入口直接 return),
		// 这里不需要再检查。直接更新节流时间戳 + upsert。
		// v2.85-PR6 (Q5-01):用 setLastSyncAndPersist 替换原来的直接赋值,
		// 顺带把 throttle 时间戳持久化到 statefile,重启后窗口保留。
		now := time.Now()
		s.setLastSyncAndPersist(rt, now)

		// upsert(FindRecord → 比对 → UpdateRecordValue retry)
		oldIP, upsertErr := s.upsertRecord(client, rt, ip)
		results <- result{recordType: rt, oldIP: oldIP, newIP: ip, err: upsertErr}
	}

	// 起任务(按 EnableA/EnableAAAA 决定跑哪些 family)
	// v2.86-pr23a:替代 v2-84 的 family 枚举 switch — 面板可独立控制 A / AAAA。
	switch {
	case s.cfg.EnableA && s.cfg.EnableAAAA:
		wg.Add(2)
		go runFamily(dns.RecordTypeAAAA, v6IP, v6Err)
		go runFamily(dns.RecordTypeA, v4IP, v4Err)
	case s.cfg.EnableA:
		wg.Add(1)
		go runFamily(dns.RecordTypeA, v4IP, v4Err)
	case s.cfg.EnableAAAA:
		wg.Add(1)
		go runFamily(dns.RecordTypeAAAA, v6IP, v6Err)
	default:
		// 两个都关 → 没有 task 要起;但 tick 入口已 enable 检查,正常路径不会到这里
		s.cfg.Logger.Debug("ddns: tick called but both A and AAAA disabled, skipping")
	}

	wg.Wait()
	close(results)

	// 聚合 LastSync
	ls := LastSync{Time: time.Now()}
	anySuccess := false
	anyAttempted := false
	for r := range results {
		anyAttempted = true
		switch r.recordType {
		case dns.RecordTypeAAAA:
			ls.V6IP = r.newIP
			ls.V6Attempted = true
			if r.err != nil {
				ls.V6Error = r.err.Error()
			} else if r.newIP != "" {
				anySuccess = true
			}
		case dns.RecordTypeA:
			ls.V4IP = r.newIP
			ls.V4Attempted = true
			if r.err != nil {
				ls.V4Error = r.err.Error()
			} else if r.newIP != "" {
				anySuccess = true
			}
		}
		// 节流 skip(r.err==nil 但 newIP 非空 且 oldIP 为空):不算成功也不算失败
		// 等下次 tick 再试
	}
	// v2.86-pr23g:Attempted 字段 — 区分"tick 跑过"和"tick 早 return / 全 throttle 跳过"。
	// 只有 runV6/runV4 至少一个为 true 且产生 results 时 Attempted=true。
	ls.Attempted = anyAttempted

	// Success = 任一 enabled family 成功过
	if anyAttempted {
		ls.Success = anySuccess
		// 单 family 模式时填充兼容字段,给 v2-83 客户端读
		// v2.86-pr23a:走 EnableA/EnableAAAA 判断,不再是 cfg.Family 枚举
		if s.cfg.EnableAAAA && !s.cfg.EnableA {
			ls.NewIP = ls.V6IP
			if ls.V6Error != "" {
				ls.Error = ls.V6Error
			}
		} else if s.cfg.EnableA && !s.cfg.EnableAAAA {
			ls.NewIP = ls.V4IP
			if ls.V4Error != "" {
				ls.Error = ls.V4Error
			}
		}
	}

	s.recordResult(ls)
}

// detectFamily 探测指定 family 的当前 IP,带节流 skip 和详细错误日志。
//
// 返回 (ip, err):
//   - 探测成功 → (ip, nil)
//   - 探测失败 → ("", err)
//   - detector 未注入(测试场景)→ ("", nil)
func (s *Sync) detectFamily(recordType string, detector func(string) (string, error), iface string) (string, error) {
	if detector == nil {
		return "", nil
	}
	ip, err := detector(iface)
	if err != nil {
		s.cfg.Logger.Warn("ddns: detect failed",
			"family", recordType, "err", err)
		return "", err
	}
	return ip, nil
}

// upsertRecord 单条记录的查-比-更流程,retry 3 次指数退避。
//
// 返回 (oldIP, error):
//   - oldIP:阿里云当前记录值(IP 没变时 = currentIP)
//   - error:retry 3 次都失败 / 阿里云 RAM 权限错误(立即失败不重试)
//
// 行为约定(评审 K3 / A 修正):
//   - FindRecord 返回 nil → 用户没建记录 → WARN 日志,return ("", nil),不算失败
//   - FindRecord 返回多条 → WARN 日志,return ("", nil),不算失败(避免覆盖别处)
//   - UpdateRecordValue 返回 RAM 权限错误 → 立即停止 retry,return error
//   - 其他错误 → retry 1s/4s/9s
func (s *Sync) upsertRecord(client *dns.AliyunClient, recordType, currentIP string) (string, error) {
	// 1. 查现有记录
	rec, err := client.FindRecord(s.cfg.Domain, s.cfg.RR, recordType)
	if err != nil {
		return "", fmt.Errorf("find %s record: %w", recordType, err)
	}
	if rec == nil {
		// 0 条:用户没建记录 — 不自动建,只打日志(避免误操作)
		s.cfg.Logger.Warn("ddns: no existing record, skipping update",
			"domain", s.cfg.Domain, "rr", s.cfg.RR,
			"record_type", recordType,
			"hint", "create the record manually in aliyun console first")
		return "", nil
	}
	// 多条:FindRecord 内部已返回 error(switch default 分支)
	// 但根据评审 K3 改 WARN-skip 后,这里其实走不到。保留防御性检查。
	_ = rec

	// 2. 比对 IP
	if rec.Value == currentIP {
		// 没变 → 不调 API(节省配额)
		return currentIP, nil
	}

	s.cfg.Logger.Info("ddns: record outdated, updating",
		"record_type", recordType,
		"old", rec.Value, "new", currentIP,
		"record_id", rec.RecordID)

	// 3. retry 3 次(指数退避)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := client.UpdateRecordValue(rec.RecordID, s.cfg.RR, recordType, currentIP, rec.TTL)
		if err == nil {
			s.cfg.Logger.Info("ddns: record updated successfully",
				"record_type", recordType,
				"old", rec.Value, "new", currentIP,
				"attempt", attempt)
			return rec.Value, nil
		}
		// v2.85-PR5 (Q3-01):用 dns.Classify 替代原 strings.Contains 模糊匹配。
		// - AuthError / RAMError → 立即失败(凭证错/权限错,retry 没用)
		// - ThrottlingError / TransientError / Unknown → 继续 retry
		class := dns.Classify(err)
		if class == dns.ClassAuth {
			s.cfg.Logger.Error("ddns: aliyun credential invalid, NOT retrying",
				"record_type", recordType, "err", err,
				"hint", "check AccessKey ID/Secret in panel UI")
			return rec.Value, fmt.Errorf("aliyun auth error for %s update: %w", recordType, err)
		}
		if class == dns.ClassRAM {
			s.cfg.Logger.Error("ddns: aliyun RAM permission denied, NOT retrying",
				"record_type", recordType, "err", err,
				"hint", "add alidns:DescribeDomainRecords + alidns:UpdateDomainRecord permissions to your RAM user")
			return rec.Value, fmt.Errorf("aliyun RAM error for %s update: %w", recordType, err)
		}
		lastErr = err
		s.cfg.Logger.Warn("ddns: update failed, retrying",
			"record_type", recordType,
			"attempt", attempt, "err", err,
			"class", class.String())
		// v2.86-PR14:指数退避 + jitter(±20% 抖动防多节点 thunder herd)
		// 1s, 4s, 9s + 随机 0-20%,避免全球 v2.85 用户在同一秒 retry
		backoff := time.Duration(attempt*attempt) * time.Second
		jitter := time.Duration(rand.Int63n(int64(backoff) / 5)) // 0-20%
		time.Sleep(backoff + jitter)
	}
	return rec.Value, fmt.Errorf("update after 3 retries: %w", lastErr)
}

// recordResult v2-84:统一记录整轮结果,替代 v2-83 的 recordSuccess / recordFailure。
//
// 语义:
//   - Success=true → 清除 LAST_DDNS_FAILED 标志
//   - Success=false 且有 attempt(任何 family 跑过) → 写 LAST_DDNS_FAILED
//   - 节流时间戳更新:在 runFamily 内部完成(upsert 完成后立即更新),
//     本函数只更新 lastSync 字段,避免重复写 map 引起 race。
func (s *Sync) recordResult(ls LastSync) {
	now := time.Now()

	s.mu.Lock()
	s.lastSync = ls
	s.mu.Unlock()

	if ls.Success {
		_ = os.Remove(s.cfg.LastFailedFile)
		return
	}

	// 失败:构造错误信息并写 LAST_DDNS_FAILED
	var errMsg string
	if ls.V4Error != "" && ls.V6Error != "" {
		errMsg = fmt.Sprintf("v4=%s; v6=%s", ls.V4Error, ls.V6Error)
	} else if ls.V4Error != "" {
		errMsg = fmt.Sprintf("v4=%s", ls.V4Error)
	} else if ls.V6Error != "" {
		errMsg = fmt.Sprintf("v6=%s", ls.V6Error)
	} else {
		errMsg = "unknown"
	}
	s.cfg.Logger.Error("ddns: sync failed",
		"family", s.cfg.Family,
		"v4_err", ls.V4Error,
		"v6_err", ls.V6Error)

	if dir := filepath.Dir(s.cfg.LastFailedFile); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	msg := fmt.Sprintf("family=%s err=%s time=%s",
		s.cfg.Family, errMsg, now.UTC().Format(time.RFC3339))
	if writeErr := os.WriteFile(s.cfg.LastFailedFile, []byte(msg), 0o644); writeErr != nil {
		s.cfg.Logger.Warn("ddns: failed to write LAST_DDNS_FAILED file", "err", writeErr)
	}
}

// readStateFile v2-83 旧接口,保留以防外部直接调用(实际 NewSync 已用 statefile.go)。
func (s *Sync) readStateFile() (bool, error) {
	rec, err := parseStateFile(s.cfg.StateFile, s.cfg.Family)
	if err != nil {
		return false, err
	}
	return rec.Enabled, nil
}

// detectV6Proc 默认 IPv6 探测实现占位(不会被调用,留作 reference)。
//
// 实际探测逻辑在 swanctl.DetectGlobalV6(导出)。main.go 在装配时通过
// SetDetectV6 注入。这里只保留签名,方便单元测试用 mock 替换。
func detectV6Proc(iface string) (string, error) {
	return "", fmt.Errorf("detectV6Proc not bound; call SetDetectV6 first")
}
