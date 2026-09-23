// PanelState:客户端虚拟 IP 段(IPv4 pool + IPv6 ULA pool)运行时持久化(v2.86-PR13.2 新增)。
//
// 背景:
//   v2-79 之前 VPN 客户端 IP 段硬编码在 swanctl.conf 模板里(10.10.0.0/24 + fd00:1::/64),
//   改段需要 docker compose down + 改 env + up,体验糟糕。
//   v2-79 改成 entrypoint 自动探测(LAN 不冲突) + 环境变量覆盖,但仍然需要重启容器。
//   v2.86-PR13.2 把这段搬到 panelstate,Go 进程读出来→直接 sed /etc/swanctl/swanctl.conf
//   → swanctl --load-all,**运行期热生效**,客户端重新拨号拿到新 IP。
//
// 优先级链(高 → 低):
//   1. /data/panel-state/subnet.conf (面板 UI / API 填的,运行时改,立即生效)
//   2. IKEV2_VPN_SUBNET / IKEV2_VPN_SUBNET_V6 env(老用户兼容,启动期生效)
//   3. entrypoint §0.6 自动探测(没填没传时)
//
// 文件格式(JSON,跟 aliyun.creds / cert.conf 同风格):
//
//	{
//	  "ipv4_subnet": "10.13.0.0/24",
//	  "ipv6_subnet": "fd00:5::/64",
//	  "updated_at": 1700000000
//	}
//
// 跟 cert.conf 的区别:
//   - cert.conf 是"配置 + 需要重启 charon"(证书路径变了)
//   - subnet.conf 是"配置 + 运行期 reload"(只动 pool 段,charon 不需要重启证书栈)
//
// 跟 aliyun.creds 的区别:
//   - aliyun.creds 是"凭证",需要 mask + audit
//   - subnet.conf 是"配置",**不需要 audit + mask**(IP 段不是凭证)
//
// 设计见 docs/design.md §19.6(v2-83 panelstate 基础)+ v2.86-PR13.2 扩展。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SubnetConfig 客户端虚拟 IP 段配置。
//
// JSON tag snake_case 方便 jq/sed 检查。
type SubnetConfig struct {
	// IPv4Subnet IPv4 pool CIDR,例如 "10.13.0.0/24"。
	// 强制 /24(strongSwan pool addrs 不接受非 /24;256 地址正好够 1-50 人)。
	IPv4Subnet string `json:"ipv4_subnet"`
	// IPv6Subnet IPv6 ULA pool 前缀,例如 "fd00:5::/64"。
	// 强制 /64(iOS ModeConfig 期望 /64)。
	IPv6Subnet string `json:"ipv6_subnet"`
	// UpdatedAt unix seconds,更新时间戳
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// SubnetConfigStore 客户端 IP 段配置的读写,带内存缓存。
type SubnetConfigStore struct {
	mu sync.RWMutex

	// cfg 内存缓存
	cfg *SubnetConfig
	// dir 测试可覆盖(默认 Dir)
	dir string
}

// NewSubnetConfigStore 构造(默认 /data/panel-state)。
func NewSubnetConfigStore() *SubnetConfigStore {
	return &SubnetConfigStore{dir: Dir}
}

// NewSubnetConfigStoreWithDir 测试用,指定目录。
func NewSubnetConfigStoreWithDir(dir string) *SubnetConfigStore {
	return &SubnetConfigStore{dir: dir}
}

// subnetConfPath subnet 配置文件路径。
func (s *SubnetConfigStore) subnetConfPath() string {
	return filepath.Join(s.dir, "subnet.conf")
}

// Validate 校验 SubnetConfig 字段合法性。
//
// 规则:
//   - IPv4Subnet 必填,合法 CIDR,prefix /24,address.family = v4
//   - IPv6Subnet 必填,合法 CIDR,prefix /64,address.family = v6 且必须在 fd00::/8 (ULA)
//     防止用户填错成公网段(会被路由器拦掉)
//
// 返回错误是用户输入错误(400),不是内部错误。
//
// v2.86-PR13.3 改造:IPv6Subnet 允许为空(用户可能在面板只改了 v4,
// v6 保留 entrypoint §0.6b / env 的值)。空时不校验、不写 swanctl.conf,
// runtime.Merge 会 fallback 到 env 的 v6。
func (c *SubnetConfig) Validate() error {
	if err := validatePoolCIDR(c.IPv4Subnet, 24, 4); err != nil {
		return fmt.Errorf("ipv4_subnet 不合法: %w", err)
	}
	if c.IPv6Subnet != "" {
		if err := validatePoolCIDR(c.IPv6Subnet, 64, 6); err != nil {
			return fmt.Errorf("ipv6_subnet 不合法: %w", err)
		}
	}
	return nil
}

// validatePoolCIDR 校验 pool CIDR 合法性(内部辅助)。
//
// 要求:
//   - 合法 CIDR 字符串(如 "10.13.0.0/24")
//   - prefix 长度严格等于 wantBits(不强加,确保 pool 大小可控)
//   - family 匹配 wantFamily(4 / 6)
//   - IPv6 必须是 ULA(fd00::/8, RFC 4193),不能填公网段
func validatePoolCIDR(s string, wantBits, wantFamily int) error {
	if s == "" {
		return fmt.Errorf("必填")
	}
	addr, err := netip.ParsePrefix(s)
	if err != nil {
		return fmt.Errorf("无法解析为 CIDR: %v", err)
	}
	if !addr.IsValid() {
		return fmt.Errorf("CIDR 无效")
	}
	// 先校验 family(避免 IPv4 地址 /64 这种"prefix 越界"被 netip 拦下来,
	// family 检查永远跑不到),再校验 prefix 长度。
	if wantFamily == 4 && !addr.Addr().Is4() {
		return fmt.Errorf("必须是 IPv4 地址")
	}
	if wantFamily == 6 && !addr.Addr().Is6() {
		return fmt.Errorf("必须是 IPv6 地址")
	}
	if addr.Bits() != wantBits {
		return fmt.Errorf("prefix 长度必须是 /%d,当前是 /%d", wantBits, addr.Bits())
	}
	// IPv6 必须 ULA(RFC 4193 fd00::/8),不能用公网段
	// 原因:1) 用户的 LAN 可能正好用同一个公网段 → 路由冲突
	//       2) 用公网段会让客户端拿公网地址,跟 server 出向冲突
	if wantFamily == 6 {
		firstByte := addr.Addr().As16()[0]
		if firstByte != 0xfd {
			return fmt.Errorf("IPv6 pool 必须是 ULA 段(fd00::/8,RFC 4193),不能是公网段")
		}
	}
	return nil
}

// LoadSubnetConfig 启动时读 subnet 配置(供 Go 进程 + entrypoint 用)。
//
// 行为:
//   - 文件不存在 → 内存留 nil,不报错(用户没改过,降级到 env / auto)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错配置)
//   - 文件存在但字段不合法 → 报错(防御性)
func (s *SubnetConfigStore) LoadSubnetConfig() (*SubnetConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.subnetConfPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.cfg = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.subnetConfPath(), err)
	}

	var c SubnetConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.subnetConfPath(), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", s.subnetConfPath(), err)
	}

	s.cfg = &c
	return &c, nil
}

// ReadSubnetConfig 读 subnet 配置(handler 用)。
//
// 行为:优先返回缓存,缓存空时降级到磁盘 Load。
func (s *SubnetConfigStore) ReadSubnetConfig() (*SubnetConfig, error) {
	s.mu.RLock()
	c := s.cfg
	s.mu.RUnlock()

	if c != nil {
		return c, nil
	}
	return s.LoadSubnetConfig()
}

// WriteSubnetConfig 写 subnet 配置(handler 调用)。
//
// 行为:
//   - Validate 失败 → 返回错误
//   - 保证目录存在(0700)
//   - atomic rename 写(避免 SIGHUP / 重启读到半截)
//   - 文件权限 0600(跟 aliyun.creds / cert.conf 一致)
//   - 写入后更新内存缓存 + UpdatedAt
func (s *SubnetConfigStore) WriteSubnetConfig(c SubnetConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().Unix()

	// 1. 目录存在
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	// 2. atomic 写
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	target := s.subnetConfPath()
	tmp, err := os.CreateTemp(s.dir, ".subnet-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 出错时清理 tmp
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
	c2 := c
	s.cfg = &c2
	s.mu.Unlock()

	return nil
}

// SubnetConfigExists 文件是否存在(handler 渲染用)。
//
// 不读文件内容,只 stat。
func (s *SubnetConfigStore) SubnetConfigExists() bool {
	_, err := os.Stat(s.subnetConfPath())
	return err == nil
}

// ClearSubnetConfig 清除 subnet 配置(handler / "回到 auto" 按钮用)。
//
// 行为:rm 文件 + 清内存缓存。重启后回退到 env / auto 探测。
func (s *SubnetConfigStore) ClearSubnetConfig() error {
	s.mu.Lock()
	s.cfg = nil
	s.mu.Unlock()

	if err := os.Remove(s.subnetConfPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
