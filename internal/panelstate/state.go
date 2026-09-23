// PanelState:面板运行时状态文件读写(v2-83 新增)。
//
// 用途:面板 UI 填的"运行时凭证"(比如阿里云 AccessKey)写到磁盘,
// 重启后 entrypoint.sh 读这个文件注入 acme.sh 的 account.conf。
//
// 设计:
//   - 文件路径:/data/panel-state/<name>.json
//   - 目录权限:0700(root only)
//   - 文件权限:0600
//   - 写入用 atomic rename(防止续期读到半截)
//   - 不加密(KMS / 加密 key 都在容器内,加密无用;过度工程)
//
// 跟 v2-82 的 ddns.Config.LastFailedFile / StateFile 区分:
//   - 那些是"标志文件"(纯文本一字节),不是凭证
//   - panelstate 是"凭证存储"(结构化 JSON)
//
// 设计见 docs/design.md §19.6(v2-83)。
package panelstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Dir 持久化目录(由 entrypoint.sh mkdir + chmod 0700)。
const Dir = "/data/panel-state"

// AliyunCreds 阿里云 AccessKey 凭证。
//
// JSON tag 用 snake_case 方便 shell 工具(jq)查看。
type AliyunCreds struct {
	KeyID     string `json:"key_id"`
	KeySecret string `json:"key_secret"`
}

// ErrNotFound 文件不存在(load 时返回,不算错误)。
var ErrNotFound = errors.New("panelstate: file not found")

// Store 凭证读写,带内存缓存。
//
// 内存缓存:
//   - 避免每次 DDNS tick 都读磁盘
//   - 启动时 Load 一次,后续 Read 直接返回
//   - Write 后立即更新内存
type Store struct {
	mu sync.RWMutex

	// aliyunCreds 缓存
	aliyunCreds *AliyunCreds
}

// NewStore 构造 Store。
func NewStore() *Store {
	return &Store{}
}

// aliyunPath 阿里云凭证 JSON 文件路径。
func aliyunPath() string {
	return filepath.Join(Dir, "aliyun.creds")
}

// LoadAliyun 读阿里云凭证(启动时调一次)。
//
// 行为:
//   - 文件不存在 → 内存留 nil,不报错(用户没填)
//   - 文件存在但 JSON 损坏 → 报错(让启动失败,免得 silent 用错凭证)
//   - 文件存在 + KeyID 为空 → 报错(防御性,不允许半填)
//
// 任何文件 IO 失败都会返回 error;调用方决定是否 fatal。
func (s *Store) LoadAliyun() (*AliyunCreds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(aliyunPath())
	if err != nil {
		if os.IsNotExist(err) {
			s.aliyunCreds = nil
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", aliyunPath(), err)
	}

	var c AliyunCreds
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", aliyunPath(), err)
	}
	if c.KeyID == "" {
		return nil, fmt.Errorf("%s: key_id is empty (file corrupted?)", aliyunPath())
	}
	if c.KeySecret == "" {
		return nil, fmt.Errorf("%s: key_secret is empty (file corrupted?)", aliyunPath())
	}

	s.aliyunCreds = &c
	return &c, nil
}

// ReadAliyun 读缓存(快速路径,DDNS tick 用)。
//
// 行为:
//   - 缓存有 → 返回缓存
//   - 缓存空 → 尝试从磁盘 Load 一次,再返回
func (s *Store) ReadAliyun() (*AliyunCreds, error) {
	s.mu.RLock()
	c := s.aliyunCreds
	s.mu.RUnlock()

	if c != nil {
		return c, nil
	}
	// 缓存空 → 降级到磁盘 Load(可能 LoadAliyun 没在启动时调)
	return s.LoadAliyun()
}

// WriteAliyun 写阿里云凭证(面板 UI 调用)。
//
// 行为:
//   - 保证目录存在(0700)
//   - atomic rename 写(避免 SIGHUP / 重启读到半截)
//   - 文件权限 0600
//   - 写入后更新内存缓存(让后续 ReadAliyun 立即生效)
func (s *Store) WriteAliyun(c AliyunCreds) error {
	if c.KeyID == "" {
		return fmt.Errorf("key_id is required")
	}
	if c.KeySecret == "" {
		return fmt.Errorf("key_secret is required")
	}

	// 1. 目录存在
	if err := os.MkdirAll(Dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", Dir, err)
	}

	// 2. atomic 写:tmp 文件 + rename
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	target := aliyunPath()
	tmp, err := os.CreateTemp(Dir, ".aliyun-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 出错时清理 tmp 文件
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
	s.aliyunCreds = &c2
	s.mu.Unlock()

	return nil
}

// ClearAliyun 清除阿里云凭证(面板 UI "清除" 按钮调用)。
//
// 行为:rm 文件 + 清内存缓存。
func (s *Store) ClearAliyun() error {
	s.mu.Lock()
	s.aliyunCreds = nil
	s.mu.Unlock()

	if err := os.Remove(aliyunPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AliyunExists 文件是否存在(用于面板"已配置/未配置"状态展示)。
//
// 不读文件内容,只 stat。
func (s *Store) AliyunExists() bool {
	_, err := os.Stat(aliyunPath())
	return err == nil
}

// MaskKeyID 凭证 ID 的掩码展示(给面板用)。
//
// 规则:首 4 位 + "..." + 尾 4 位;总长度 < 8 时直接 "***"。
//
// 例:
//   - "LTAI5txxxxxxxxxxxxxab12" → "LTAI...ab12"
//   - "short"                  → "***"
func MaskKeyID(keyID string) string {
	if len(keyID) <= 8 {
		return "***"
	}
	return keyID[:4] + "..." + keyID[len(keyID)-4:]
}
