// PanelState:管理员全局 mobileconfig 默认值(v2.86-PR12.22 新增)。
//
// 背景:
//   v2.86-PR12.21 把 mobileconfig 的可调字段抽到 store.User.MobileConfigOpts
//   (每用户 overlay),但 baseline(无人覆盖时)还是写死在 cert.DefaultMobileConfigOpts()
//   —— 管理员没法批量改默认值(例如把全公司员工的 NAT keepalive 调成 120s,
//   现在只能逐个 user 改 N 次)。
//
// 本文件提供 admin 级别的"全局默认模板",让管理员:
//   1. 在面板后台集中改一组 baseline
//   2. 用户没设 overlay 时,用这组 baseline 渲染 mobileconfig
//   3. 用户设了 overlay 时,字段级覆盖 baseline(overlay > baseline > builtin)
//
// 优先级链(高 → 低):
//   1. store.User.MobileConfigOpts  (per-user overlay,PR12.21)
//   2. panelstate.MobileConfigDefaults (admin global baseline,本 PR)
//   3. cert.BaseMobileConfigDefaults() (builtin 出厂值,不可改)
//
// 跟 cert.conf (PR12.5) 的对比:
//   - cert.conf 是**启动时一次配置**(模式/域名/CN),改了需重启容器
//   - mobileconfig.defaults 是**运行时热改**,用户下次扫码 / 下载立即生效
//     (不需要重启,不需要重启 charon,不需要续签证书)
//     理由:mobileconfig 是 iOS 客户端的描述文件,服务端只负责"组装 XML 字节流",
//     改 admin defaults 只是改 cert.RenderMobileconfig 内部读到的常量表,
//     没有任何 I/O / 进程级副作用,纯 CPU 操作。
//
// 设计见 docs/design.md §19.6(v2-83)+ v2.86-PR12.22 mobileconfig admin defaults。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/cert"
)

// MobileConfigDefaults 管理员全局 mobileconfig 默认值。
//
// JSON tag snake_case 方便 jq / sed 检查。
// 字段含义见 cert.MobileConfigOpts(本 struct 是它的镜像子集)。
//
// "零值"含义:
//   - 文件不存在 / JSON 是空 → 全部字段为 nil / 空 → cert.BaseMobileConfigDefaults()
//     出厂值生效
//   - 文件存在但某字段为 nil → 那一项用出厂值
//
// 跟 cert.MobileConfigOpts 的字段一一对应,但没有 OnDemandSSID:
//   - SSID 是 per-network 的(家里/公司/咖啡馆),不能全局;管理员也只能给
//     per-user overlay 改 SSID
//   - 全局默认 OnDemandProfile = "always" 时不需 SSID;profile 改成
//     home_wifi_disconnect 时,管理员必须给**每用户**填 SSID(后台批量模板
//     是后续 PR 范围)。
type MobileConfigDefaults struct {
	// UserDefinedName 全局默认连接显示名;空 = "IKEv2 VPN"(cert 默认)
	UserDefinedName string `json:"user_defined_name,omitempty"`

	// DisconnectOnSleep "lock_screen_break" 行为:
	//   nil  → 用 cert 出厂(false,锁屏保活)
	//   &true → 锁屏断 VPN
	//   &false → 锁屏保持连接
	DisconnectOnSleep *bool `json:"disconnect_on_sleep,omitempty"`

	// NATKeepaliveEnabled nil = cert 出厂(true)
	NATKeepaliveEnabled *bool `json:"nat_keepalive_enabled,omitempty"`

	// NATKeepaliveInterval 秒;nil = cert 出厂(60)
	NATKeepaliveInterval *int `json:"nat_keepalive_interval,omitempty"`

	// OnDemandEnabled nil = cert 出厂(true)
	OnDemandEnabled *bool `json:"on_demand_enabled,omitempty"`

	// OnDemandProfile 空 = cert 出厂("always")
	OnDemandProfile string `json:"on_demand_profile,omitempty"`

	// IncludeAllNetworks nil = cert 出厂(true)
	IncludeAllNetworks *bool `json:"include_all_networks,omitempty"`

	// ExcludeLocalNetworks nil = cert 出厂(true)
	ExcludeLocalNetworks *bool `json:"exclude_local_networks,omitempty"`

	// DNSServers nil/空 = cert 出厂(1.1.1.1/8.8.8.8/...)
	DNSServers []string `json:"dns_servers,omitempty"`

	// DeadPeerDetectionRate 空 = cert 出厂("Medium")
	DeadPeerDetectionRate string `json:"dead_peer_detection_rate,omitempty"`

	// UpdatedAt unix seconds,更新时间戳(便于审计 + 显示)
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// MobileConfigDefaultsStore admin 全局默认值的读写,带内存缓存。
//
// 同 CertConfigStore 的 atomic-write + 缓存模式,但**不需要重启容器**:
// mobileconfig 渲染是 handler 端即时调 cert.RenderMobileconfig,handler
// 通过 s.MobileConfigDefaults 拿最新值,内存缓存已更新即生效。
type MobileConfigDefaultsStore struct {
	mu sync.RWMutex

	// defaults 内存缓存;nil = 还没写过 / 已被 Clear
	defaults *MobileConfigDefaults

	// dir 测试可覆盖(默认 Dir)
	dir string
}

// NewMobileConfigDefaultsStore 构造(默认 /data/panel-state)。
func NewMobileConfigDefaultsStore() *MobileConfigDefaultsStore {
	return &MobileConfigDefaultsStore{dir: Dir}
}

// NewMobileConfigDefaultsStoreWithDir 测试用,指定目录。
func NewMobileConfigDefaultsStoreWithDir(dir string) *MobileConfigDefaultsStore {
	return &MobileConfigDefaultsStore{dir: dir}
}

// defaultsPath 默认值 JSON 文件路径。
func (s *MobileConfigDefaultsStore) defaultsPath() string {
	return filepath.Join(s.dir, "mobileconfig.defaults.json")
}

// Validate 校验 admin defaults 字段合法性。
//
// 复用 cert.MobileConfigOpts.Validate 的所有规则(NATKeepalive 范围 / OnDemandProfile 白名单
// / DNS IP 格式 / DPD 枚举 等),通过临时构造 cert.MobileConfigOpts 校验。
//
// 为什么不直接组合 cert.MobileConfigOpts.Validate:
//   - cert.MobileConfigOpts 有 OnDemandSSID 字段(admin defaults 不含),字段语义不同
//   - cert.MobileConfigOpts.Validate 的"用户必填 SSID 模式"不适用 admin 层
//     (admin 强制全局 home_wifi_disconnect 模式没意义,因为 SSID 是 per-network)
//
// 返回错误是用户输入错误(400),不是内部错误。
func (d *MobileConfigDefaults) Validate() error {
	// 临时构造 cert.MobileConfigOpts 走完整校验
	tmp := cert.MobileConfigOpts{
		UserDefinedName:       strPtrOrNil(d.UserDefinedName),
		DisconnectOnSleep:     d.DisconnectOnSleep,
		NATKeepaliveEnabled:   d.NATKeepaliveEnabled,
		NATKeepaliveInterval:  d.NATKeepaliveInterval,
		OnDemandEnabled:       d.OnDemandEnabled,
		OnDemandProfile:       cert.OnDemandProfile(d.OnDemandProfile),
		IncludeAllNetworks:    d.IncludeAllNetworks,
		ExcludeLocalNetworks:  d.ExcludeLocalNetworks,
		DNSServers:            d.DNSServers,
		DeadPeerDetectionRate: d.DeadPeerDetectionRate,
	}
	return tmp.Validate()
}

// strPtrOrNil 空字符串 → nil(让 cert.MobileConfigOpts.Validate 跳过该字段),
// 非空 → 返回 &s。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// LoadMobileConfigDefaults 启动时读 admin defaults。
//
// 行为:
//   - 文件不存在 → 内存留 nil,不报错(用户没改过,降级到 cert 出厂)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错)
//   - 文件存在但字段不合法 → 报错(防御性)
func (s *MobileConfigDefaultsStore) LoadMobileConfigDefaults() (*MobileConfigDefaults, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.defaultsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.defaults = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.defaultsPath(), err)
	}

	var d MobileConfigDefaults
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.defaultsPath(), err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", s.defaultsPath(), err)
	}

	s.defaults = &d
	return &d, nil
}

// ReadMobileConfigDefaults handler 端读 admin defaults。
//
// 行为:优先返回缓存,缓存空时降级到磁盘 Load。
func (s *MobileConfigDefaultsStore) ReadMobileConfigDefaults() (*MobileConfigDefaults, error) {
	s.mu.RLock()
	d := s.defaults
	s.mu.RUnlock()

	if d != nil {
		return d, nil
	}
	return s.LoadMobileConfigDefaults()
}

// WriteMobileConfigDefaults 写 admin defaults(handler 调用)。
//
// 行为:
//   - Validate 失败 → 返回错误
//   - 目录存在(0700)
//   - atomic rename 写
//   - 文件权限 0600
//   - 写入后更新内存缓存 + UpdatedAt
//
// **不需要重启容器** —— mobileconfig 渲染路径(handler 端)每次都从
// s.MobileConfigDefaultsStore.Read 读最新值。
func (s *MobileConfigDefaultsStore) WriteMobileConfigDefaults(d MobileConfigDefaults) error {
	if err := d.Validate(); err != nil {
		return err
	}
	d.UpdatedAt = time.Now().Unix()

	// 1. 目录存在
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	// 2. atomic 写
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	target := s.defaultsPath()
	tmp, err := os.CreateTemp(s.dir, ".mc-defaults-*.json.tmp")
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
	d2 := d
	s.defaults = &d2
	s.mu.Unlock()

	return nil
}

// MobileConfigDefaultsExists 文件是否存在(handler 渲染用)。
func (s *MobileConfigDefaultsStore) MobileConfigDefaultsExists() bool {
	_, err := os.Stat(s.defaultsPath())
	return err == nil
}

// ToMobileConfigOpts 把 panelstate admin defaults 转成 cert.MobileConfigOpts。
//
// 转换规则:
//   - UserDefinedName:空 → nil(让 BaseMobileConfigDefaults 用 "IKEv2 VPN" fallback)
//   - DNSServers:空 → nil
//   - 指针字段(*bool / *int):直接复用 cert.MobileConfigOpts 的指针
//   - 字符串字段:空 → " "(由 EffectiveMobileConfigOpts 跳过)
//
// 返回 *cert.MobileConfigOpts;nil 时 cert.EffectiveMobileConfigOpts 跳过这一层。
//
// 为什么不直接让 panelstate.MobileConfigDefaults 跟 cert.MobileConfigOpts 用同一类型:
//   - cert 是底层模块,被很多地方依赖;让 cert import panelstate 反向依赖
//   - panelstate 是上层 runtime 配置,允许有 cert.MobileConfigOpts 没有的字段
//     (如 UpdatedAt)
//   - 字段变更时,转换函数集中在一处,容易 review
func (d *MobileConfigDefaults) ToMobileConfigOpts() *cert.MobileConfigOpts {
	if d == nil {
		return nil
	}
	out := &cert.MobileConfigOpts{
		DisconnectOnSleep:     d.DisconnectOnSleep,
		NATKeepaliveEnabled:   d.NATKeepaliveEnabled,
		NATKeepaliveInterval:  d.NATKeepaliveInterval,
		OnDemandEnabled:       d.OnDemandEnabled,
		IncludeAllNetworks:    d.IncludeAllNetworks,
		ExcludeLocalNetworks:  d.ExcludeLocalNetworks,
		DeadPeerDetectionRate: d.DeadPeerDetectionRate,
	}
	if d.UserDefinedName != "" {
		v := d.UserDefinedName
		out.UserDefinedName = &v
	}
	if d.OnDemandProfile != "" {
		out.OnDemandProfile = cert.OnDemandProfile(d.OnDemandProfile)
	}
	if len(d.DNSServers) > 0 {
		out.DNSServers = append([]string(nil), d.DNSServers...)
	}
	return out
}

// IsZero 判断是否为零值(用于 ClearMobileConfigDefaults 等的"什么都没设"判定)。
func (d *MobileConfigDefaults) IsZero() bool {
	if d == nil {
		return true
	}
	return d.UserDefinedName == "" &&
		d.DisconnectOnSleep == nil &&
		d.NATKeepaliveEnabled == nil &&
		d.NATKeepaliveInterval == nil &&
		d.OnDemandEnabled == nil &&
		d.OnDemandProfile == "" &&
		d.IncludeAllNetworks == nil &&
		d.ExcludeLocalNetworks == nil &&
		len(d.DNSServers) == 0 &&
		d.DeadPeerDetectionRate == ""
}

// ClearMobileConfigDefaults 清除 admin defaults(回到 cert 出厂)。
//
// 行为:rm 文件 + 清内存缓存。**无需重启**,立即回到 cert 出厂值。
func (s *MobileConfigDefaultsStore) ClearMobileConfigDefaults() error {
	s.mu.Lock()
	s.defaults = nil
	s.mu.Unlock()

	if err := os.Remove(s.defaultsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}