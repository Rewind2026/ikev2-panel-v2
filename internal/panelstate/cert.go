// PanelState:证书配置运行时持久化(v2.86-PR12.5 新增)。
//
// 背景:
//   v2-79 设计里,CertMode / Domain / ServerCN / ACMEEmail 都只能从 env 读,
//   用户要切换 self-signed <-> letsencrypt 必须 docker compose down + 改 env + up,
//   体验糟糕。v2.86-PR12.5 把这些字段搬到面板运行时配置,
//   写 /data/panel-state/cert.conf,Go + entrypoint 都从这里读。
//
// 优先级链(高 → 低):
//   1. /data/panel-state/cert.conf (面板运行时)
//   2. IKEV2_CERT_MODE / IKEV2_DOMAIN / IKEV2_SERVER_CN / IKEV2_ACME_EMAIL env
//
// 文件格式(JSON,跟 aliyun.creds 同风格):
//
//	{
//	  "cert_mode": "letsencrypt",
//	  "domain": "vpn.example.com",
//	  "server_cn": "vpn.example.com",
//	  "acme_email": "admin@example.com",
//	  "updated_at": 1700000000
//	}
//
// 跟 aliyun.creds 的区别:
//   - alyun.creds 是"凭证"(AccessKey),需要 mask + audit
//   - cert.conf 是"配置"(域名/模式),**不需要 audit + mask**,但需要:
//     1. validate cert_mode ∈ {self-signed, letsencrypt}
//     2. validate domain 是合法 FQDN(不能是 IP,不能空除非 self-signed)
//
// 设计权衡:
//   - **重启生效**:Go 进程启动时从 cert.conf 读,运行时改面板需要重启容器
//     (因为 CertMode / Domain 决定 cert 安装路径,不能热改)
//   - 不做"运行时存储"(跟 AliyunCreds 不同):cert 模式变了 cert 文件路径变了,
//     进程里的 tls.Certificate 也要重 load,涉及 swanctl 重启,运行时改不安全
//
// 设计见 docs/design.md §19.6(v2-83) + v2.86-PR12.5 扩展。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CertConfig 证书配置(运行时持久化)。
//
// JSON tag snake_case 方便 jq/sed 检查。
type CertConfig struct {
	CertMode  string `json:"cert_mode"`             // "self-signed" / "letsencrypt"
	Domain    string `json:"domain,omitempty"`      // LE 模式必填,自签模式忽略
	ServerCN  string `json:"server_cn,omitempty"`   // mobileconfig RemoteIdentifier;空时 fallback 到 Domain 或 "vpn.local"
	ACMEEmail string `json:"acme_email,omitempty"`  // LE 注册邮箱(可选,Let's Encrypt 不强制)
	UpdatedAt int64  `json:"updated_at,omitempty"`  // unix seconds,更新时间戳
}

// CertConfigStore 证书配置的读写,带内存缓存。
type CertConfigStore struct {
	mu sync.RWMutex

	// cfg 内存缓存
	cfg *CertConfig
	// dir 测试可覆盖(默认 Dir)
	dir string
}

// NewCertConfigStore 构造(默认 /data/panel-state)。
func NewCertConfigStore() *CertConfigStore {
	return &CertConfigStore{dir: Dir}
}

// NewCertConfigStoreWithDir 测试用,指定目录。
func NewCertConfigStoreWithDir(dir string) *CertConfigStore {
	return &CertConfigStore{dir: dir}
}

// certConfPath 证书配置文件路径。
func (s *CertConfigStore) certConfPath() string {
	return filepath.Join(s.dir, "cert.conf")
}

// domainRegex 合法 FQDN 校验(简化版,允许单 label 用于 dev)。
//
//   - 必须非空
//   - 每个 label 1-63 字符,字母数字 + 连字符(不能以连字符开头/结尾)
//   - 总长 ≤ 253
//   - 不接受纯 IP(那是 IP 不是域名)
//   - 不接受下划线(早期 SSL/TLS 兼容性问题,LE 不签)
//
// 参考:https://www.rfc-editor.org/rfc/rfc1035 + RFC 5890
var domainRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// Validate 校验 CertConfig 字段合法性。
//
// 规则:
//   - CertMode ∈ {"self-signed", "letsencrypt"}
//   - CertMode == "letsencrypt" → Domain 必填且合法
//   - ServerCN / ACMEEmail 可空;非空时 Domain 校验同样规则
//
// 返回错误是用户输入错误(400),不是内部错误。
func (c *CertConfig) Validate() error {
	switch c.CertMode {
	case "self-signed", "letsencrypt":
	default:
		return fmt.Errorf("cert_mode 必须为 self-signed 或 letsencrypt,当前: %q", c.CertMode)
	}
	if c.CertMode == "letsencrypt" {
		if c.Domain == "" {
			return fmt.Errorf("letsencrypt 模式必须填域名(IKEV2_DOMAIN)")
		}
		if !isValidDomain(c.Domain) {
			return fmt.Errorf("域名不合法: %q (必须是 FQDN,不能是 IP,不能含下划线)", c.Domain)
		}
	}
	if c.ServerCN != "" && !isValidDomain(c.ServerCN) {
		return fmt.Errorf("server_cn 不合法: %q (必须是 FQDN)", c.ServerCN)
	}
	if c.ACMEEmail != "" && !isValidEmail(c.ACMEEmail) {
		return fmt.Errorf("acme_email 不合法: %q", c.ACMEEmail)
	}
	return nil
}

// isValidDomain 校验域名格式。
//
// 要求:
//   - 匹配 domainRegex (label.label.label,字母数字+连字符)
//   - **至少 2 个 label**:拒绝单 label(避免 "localhost" / 纯 IP / 单字符)
//   - **TLD 全字母**:`1.2.3.4` 看着像域名其实是 IP,TLD 是数字就 reject
//   - **最长 label ≤ 63** (DNS 协议限制,RFC 1035)
//   - **总长 ≤ 253**
//
// 设计:严格度按"过 LE 签发"的最低要求。宽松会让用户填错走 acme.sh 才报错,
// 太晚。提前在 panelstate 校验就拦。
func isValidDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	if !domainRegex.MatchString(s) {
		return false
	}
	// 拒绝纯 IP(看起来像域名但每段是数字,且 TLD 是数字)
	// 检测方法:split by .,如果所有段都是纯数字 → 拒绝
	parts := strings.Split(s, ".")
	allDigits := true
	for _, p := range parts {
		if p == "" {
			return false
		}
		// 检查单个 label 是否太长(DNS 协议规定 63 字符上限)
		if len(p) > 63 {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
	}
	if allDigits && len(parts) == 4 {
		// IPv4 格式:每段都是数字且有 4 段
		return false
	}
	// TLD 必须全字母(避免数字 TLD,如 "vpn.123")
	tld := parts[len(parts)-1]
	for _, c := range tld {
		if c < 'a' || c > 'z' {
			// 接受 A-Z 也算,但 tld 通常小写。简单起见:必须小写字母
			if c < 'A' || c > 'Z' {
				return false
			}
		}
	}
	// 至少 2 个 label(避免 "localhost" / "ikev2")
	if len(parts) < 2 {
		return false
	}
	return true
}

// emailRegex 简易邮箱校验(不追求 RFC 5321 完整,够 LE 用)。
var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// isValidEmail 校验邮箱格式。
func isValidEmail(s string) bool {
	if s == "" || len(s) > 254 {
		return false
	}
	return emailRegex.MatchString(s)
}

// LoadCertConfig 启动时读证书配置(供 Go 进程用)。
//
// 行为:
//   - 文件不存在 → 内存留 nil,不报错(用户没改过,降级到 env)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错配置)
//   - 文件存在但字段不合法 → 报错(防御性)
func (s *CertConfigStore) LoadCertConfig() (*CertConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.certConfPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.cfg = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.certConfPath(), err)
	}

	var c CertConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.certConfPath(), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", s.certConfPath(), err)
	}

	s.cfg = &c
	return &c, nil
}

// ReadCertConfig 读证书配置(handler 用)。
//
// 行为:优先返回缓存,缓存空时降级到磁盘 Load。
func (s *CertConfigStore) ReadCertConfig() (*CertConfig, error) {
	s.mu.RLock()
	c := s.cfg
	s.mu.RUnlock()

	if c != nil {
		return c, nil
	}
	return s.LoadCertConfig()
}

// WriteCertConfig 写证书配置(面板 UI 调用)。
//
// 行为:
//   - Validate 失败 → 返回错误
//   - 保证目录存在(0700)
//   - atomic rename 写
//   - 文件权限 0600(跟 aliyun.creds 一致)
//   - 写入后更新内存缓存 + UpdatedAt
func (s *CertConfigStore) WriteCertConfig(c CertConfig) error {
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

	target := s.certConfPath()
	tmp, err := os.CreateTemp(s.dir, ".cert-*.json.tmp")
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

// CertConfigExists 文件是否存在(handler 渲染用)。
func (s *CertConfigStore) CertConfigExists() bool {
	_, err := os.Stat(s.certConfPath())
	return err == nil
}

// ClearCertConfig 清除证书配置(回到 env 默认)。
func (s *CertConfigStore) ClearCertConfig() error {
	s.mu.Lock()
	s.cfg = nil
	s.mu.Unlock()

	if err := os.Remove(s.certConfPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}