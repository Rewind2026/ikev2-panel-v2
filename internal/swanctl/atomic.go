// 原子文件写入工具。
//
// 设计动机（P0-3 修复 v2-80+ backlog）：
//
// v2 之前所有 swanctl 配置文件 + cert 文件都用 os.WriteFile 直接写。
// 如果进程在 write 系统调用中途被杀掉（容器 OOM kill / 系统重启 / 进程崩溃），
// 会留下半截文件。下次 charon 启动时读这个半截 conf → 解析失败 → SA 全挂。
//
// 解决：write-to-tmp + os.Rename 两步：
//   1. 写到 path.tmp（失败 → 不影响原文件）
//   2. os.Rename 原子替换（POSIX 保证同分区下 rename 是原子的）
//
// POSIX rename 原子性保证：观察者要么看到旧文件，要么看到新文件，永远不会
// 看到"半截文件"。ext4 / xfs / btrfs / tmpfs 都支持。
//
// 已知边界：
//   - tmp 文件创建失败 → 原文件不变，错误返回
//   - write 中失败 → 清理 tmp，返回错误
//   - rename 失败 → 清理 tmp，返回错误（原文件不变）
//   - 跨文件系统 rename 失败 → 我们已经在 caller 算好 path 在 confDir 下，
//     同目录一定同 fs
//
// 这也是为什么把 helper 放在 swanctl 包内（不放在 internal/fsutil 等通用包）:
// 它对 swanctl 场景特化（0600 权限、conf.d 目录）。如果要其他包复用
// 再单独抽 fsutil 包。
package swanctl

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWriteFile 把 data 原子写入 path（write tmp + rename）。
//
// 步骤：
//  1. 确保父目录存在（0o755）
//  2. 写到 path.tmp（0o600 或 caller 指定）
//  3. fsync tmp（如果支持；让数据真正落盘再 rename）
//  4. os.Rename(path.tmp, path)（原子替换）
//  5. 失败时清理 tmp
//
// 为什么不直接用 os.WriteFile + rename：
//   - os.WriteFile(path) 在某些 fs 上不是 atomic（POSIX 没要求）
//   - write 失败留下半截 path
//
// 为什么不用 github.com/google/go-safeweb 之类：
//   - 依赖越小越好；这个 helper 20 行就够用
//
// 导出原因：cmd/ikev2-panel/main.go installCertsToSwanctl 也需要原子写 cert 文件
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}

	// cleanup 闭包：write 或 rename 失败时清理 tmp
	tmpRemoved := false
	defer func() {
		if !tmpRemoved {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	// fsync 确保数据落盘再 rename（防止断电场景下新文件内容是 0）
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync tmp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	tmpRemoved = true
	return nil
}