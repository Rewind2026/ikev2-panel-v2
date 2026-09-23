// Package panelstate:DDNS 同步配置运行时持久化(v2.86-pr23a 新增)。
//
// 背景:
//   v2-82 ~ v2-84 DDNS 配置(主域 / RR / family / period)全部从 env 启动时读,
//   运行时只能改 family + enabled。改主域 / RR / 周期都要 docker compose down
//   + 改 env + up,体验糟糕。v2.86-pr23a 把 RR / EnableA / EnableAAAA / Period
//   搬到 panelstate(运行时面板可改,内存立即生效);主域名 Domain 仍然走 env
//   (向后兼容,entrypoint / acme.sh 都依赖它)。
//
// 优先级链(高 → 低):
//   1. /data/panel-state/ddns.conf  (面板 UI / API 填的,运行时改,立即生效)
//   2. env 启动值(cfg.AliyunRR / IKEV2_DDNS_FAMILY / IKEV2_DDNS_PERIOD)
//   3. 代码默认值(RR="@" / family="dual" / period=60s)
//
// 文件格式(JSON,跟 cert.conf / subnet.conf 同风格):
//
//	{
//	  "rr": "vpn",
//	  "enable_a": true,
//	  "enable_aaaa": true,
//	  "period_seconds": 60,
//	  "updated_at": 1700000000
//	}
//
// 跟 cert.conf 的区别:
//   - cert.conf 改完需重启容器(cert 栈需要重建)
//   - ddns.conf 改完内存立即生效(goroutine 在跑,Sleep 完就 pick up 新 config)
//
// 跟 aliyun.creds 的区别:
//   - aliyun.creds 是凭证,需要 mask + audit
//   - ddns.conf 是配置,**不需要 audit + mask**
//
// 设计见 docs/design.md §19 + v2.86-pr23a 设计文档。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DDNSConfig DDNS 同步配置(运行时持久化)。
//
// JSON tag snake_case 方便 jq/sed 检查。
//
// 字段说明:
//   - RR:主机记录(面板可改)。空 = "@" (主域本身);常用 "vpn" 等子域。
//     校验:仅允许 [a-zA-Z0-9_-] 字符(1-63),不允许 "@" / "." / "/" 等
//   - EnableA:同步 A 记录(IPv4)。面板 checkbox。
//   - EnableAAAA:同步 AAAA 记录(IPv6)。面板 checkbox。
//   - PeriodSeconds:同步周期(秒)。下限 10s 上限 3600s,防止用户误填
//     10ms 把阿里云 API 配额打爆。
//   - UpdatedAt:unix seconds,更新时间戳
//
// 主域名 Domain **不存**这里 — 仍然走 env IKEV2_DDNS_DOMAIN(向后兼容,
// entrypoint / acme.sh 都依赖 env,搬动成本大且容易出 bug)。
type DDNSConfig struct {
	RR            string `json:"rr,omitempty"`              // 默认 "vpn";空 / "@" = 主域本身
	EnableA       bool   `json:"enable_a"`                 // 默认 true
	EnableAAAA    bool   `json:"enable_aaaa"`              // 默认 true
	PeriodSeconds int    `json:"period_seconds,omitempty"` // 默认 60;范围 10-3600
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// DDNSConfigStore DDNS 同步配置的读写,带内存缓存。
type DDNSConfigStore struct {
	mu sync.RWMutex

	// cfg 内存缓存
	cfg *DDNSConfig
	// dir 测试可覆盖(默认 Dir)
	dir string
}

// NewDDNSConfigStore 构造(默认 /data/panel-state)。
func NewDDNSConfigStore() *DDNSConfigStore {
	return &DDNSConfigStore{dir: Dir}
}

// NewDDNSConfigStoreWithDir 测试用,指定目录。
func NewDDNSConfigStoreWithDir(dir string) *DDNSConfigStore {
	return &DDNSConfigStore{dir: dir}
}

// ddnsConfPath DDNS 配置文件路径。
func (s *DDNSConfigStore) ddnsConfPath() string {
	return filepath.Join(s.dir, "ddns.conf")
}

// Validate 校验 DDNSConfig 字段合法性。
//
// 规则:
//   - RR:字符集 [a-zA-Z0-9_-],长度 1-63(空时赋默认值,不报错)
//   - PeriodSeconds:范围 10-3600(0 走默认 60)
//
// 返回错误是用户输入错误(400),不是内部错误。
func (c *DDNSConfig) Validate() error {
	if c.RR != "" && c.RR != "@" {
		if !validRR(c.RR) {
			return fmt.Errorf("rr 不合法: %q (只允许字母/数字/_/-,1-63 字符)", c.RR)
		}
	}
	if c.PeriodSeconds != 0 && (c.PeriodSeconds < 10 || c.PeriodSeconds > 3600) {
		return fmt.Errorf("period_seconds 必须在 10-3600 之间,当前 %d", c.PeriodSeconds)
	}
	return nil
}

// validRR 校验主机记录名合法性。
//
// RFC 1035:每个 label 1-63 字符,只允许 [a-zA-Z0-9-](我们的扩展加 _ 因为阿里云
// DNS 实际接受 _)。我们不允许 "."(那是完整域名,不是 RR),不允许 "/" 等。
func validRR(rr string) bool {
	if len(rr) == 0 || len(rr) > 63 {
		return false
	}
	for _, r := range rr {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// LoadDDNSConfig 启动时读 DDNS 配置(供 main.go + handler 用)。
//
// 行为:
//   - 文件不存在 → 内存留 nil,不报错(用户没改过,降级到 env / 默认)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错配置)
//   - 文件存在但字段不合法 → 报错(防御性)
func (s *DDNSConfigStore) LoadDDNSConfig() (*DDNSConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.ddnsConfPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.cfg = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.ddnsConfPath(), err)
	}

	var c DDNSConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.ddnsConfPath(), err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", s.ddnsConfPath(), err)
	}

	s.cfg = &c
	return &c, nil
}

// ReadDDNSConfig 读 DDNS 配置(handler 用)。
//
// 行为:优先返回缓存,缓存空时降级到磁盘 Load。
func (s *DDNSConfigStore) ReadDDNSConfig() (*DDNSConfig, error) {
	s.mu.RLock()
	c := s.cfg
	s.mu.RUnlock()

	if c != nil {
		return c, nil
	}
	return s.LoadDDNSConfig()
}

// WriteDDNSConfig 写 DDNS 配置(handler 调用)。
//
// 行为:
//   - Validate 失败 → 返回错误
//   - 保证目录存在(0700)
//   - atomic rename 写
//   - 文件权限 0600
//   - 写入后更新内存缓存
func (s *DDNSConfigStore) WriteDDNSConfig(c DDNSConfig) error {
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

	target := s.ddnsConfPath()
	tmp, err := os.CreateTemp(s.dir, ".ddns-*.json.tmp")
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
	c2 := c
	s.cfg = &c2
	s.mu.Unlock()

	return nil
}

// DDNSConfigExists 文件是否存在(handler 渲染用)。
//
// 不读文件内容,只 stat。
func (s *DDNSConfigStore) DDNSConfigExists() bool {
	_, err := os.Stat(s.ddnsConfPath())
	return err == nil
}

// ClearDDNSConfig 清除 DDNS 配置(handler "回到 env 默认" 按钮用)。
//
// 行为:rm 文件 + 清内存缓存。重启后回退到 env 默认(RR="vpn", enable=true+true, period=60s)。
func (s *DDNSConfigStore) ClearDDNSConfig() error {
	s.mu.Lock()
	s.cfg = nil
	s.mu.Unlock()

	if err := os.Remove(s.ddnsConfPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
