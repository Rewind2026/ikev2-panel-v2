// 限速文件写入器（Go 端管理文件，updown 脚本读取）。
// 设计见 docs/design.md §9.2.1 + architecture §3.8
package limit

import (
	"fmt"
	"os"
	"path/filepath"
)

// LimitDir 默认限速目录（updown 脚本读取位置）。
// 容器内：/var/lib/ikev2-panel/limits/
// 本地 dev：dataDir/limits/
const LimitDir = "/var/lib/ikev2-panel/limits"

// Limiter 管理限速文件。
type Limiter struct {
	dir string
}

// New 构造 Limiter。
func New() *Limiter {
	return &Limiter{dir: LimitDir}
}

// WithDir 自定义目录（用于本地开发测试）。
func (l *Limiter) WithDir(dir string) *Limiter {
	l.dir = dir
	return l
}

// WriteLimitFile 写一个用户的限速文件（内容是数字 Mbps，0 = 不限速）。
func (l *Limiter) WriteLimitFile(username string, mbps int) error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", l.dir, err)
	}
	path := filepath.Join(l.dir, username)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", mbps)), 0o644)
}

// RemoveLimitFile 删除限速文件。文件不存在视为成功。
func (l *Limiter) RemoveLimitFile(username string) error {
	path := filepath.Join(l.dir, username)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}