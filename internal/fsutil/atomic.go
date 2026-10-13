// 原子文件写入工具。
//
// 设计动机（v2.86-PR15 from internal/swanctl/atomic.go 抽离）：
//
// v2 之前所有持久化配置文件 + 写入都用 os.WriteFile 直接写。
// 如果进程在 write 系统调用中途被杀掉（容器 OOM kill / 系统重启 / 进程崩溃），
// 会留下半截文件。下次读取时读这个半截 → 解析失败 → 功能失效。
//
// 在 swanctl 场景下后果特别严重:半截 swanctl.conf → 启动失败 → **VPN 全断**。
// v2.86-PR15 把这个 helper 从 swanctl 包抽到 fsutil 公共包,统一治理。
// 早期原子写调用方(swanctl.AtomicWriteFile)保留 thin wrapper 转调本函数,
// 不破坏老调用方代码。
//
// 解决:write-to-tmp + os.Rename 两步:
//  1. 写到 path.tmp(失败 → 不影响原文件)
//  2. os.Rename 原子替换(POSIX 保证同分区下 rename 是原子的)
//
// POSIX rename 原子性保证:观察者要么看到旧文件,要么看到新文件,永远不会
// 看到"半截文件"。ext4 / xfs / btrfs / tmpfs 都支持。
//
// 已知边界:
//   - tmp 文件创建失败 → 原文件不变,错误返回
//   - write 中失败 → 清理 tmp,返回错误
//   - rename 失败 → 清理 tmp,返回错误(原文件不变)
//   - 跨文件系统 rename 失败 → caller 算好 path 在 confDir 下,同目录一定同 fs
//
// 为什么不用 github.com/google/go-safeweb 之类:
//   - 依赖越小越好;这个 helper 20 行就够用
//
// 已知调用方:
//   - internal/swanctl/writer.go (per-user EAP secret conf.d/*.conf)
//   - internal/swanctl/ipv6watch.go (swanctl.conf local_addrs sed 替换 — v2.86-PR15 改造)
//   - internal/swanctl/loader.go 安装证书 / cert 文件
//   - internal/ddns/statefile.go (DDNS 状态文件)
//   - internal/auth/ratelimit.go (rate-limit 持久化 — v2.86-PR15 改造)
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWriteFile 把 data 原子写入 path(write 唯一 tmp + rename)。
//
// 步骤:
//  1. 确保父目录存在(0o755)
//  2. CreateTemp 建唯一 tmp(perm)
//  3. fsync tmp(让数据真正落盘再 rename)
//  4. os.Rename(tmp, path)(原子替换)
//  5. 失败时清理 tmp
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// tmp 名必须唯一(v2.86-pr24 B16)。
	//
	// 老实现用固定的 path+".tmp",同一 path 并发写会互相踩:一个 goroutine
	// rename 走之后,另一个的 rename 拿到 ENOENT 直接失败;更糟的是两个写者
	// 交错地 truncate + write,最终内容是"A 的头 + B 的尾"—— 而 rename 的原子性
	// 保证的是"整份替换",保证不了"写自己那份数据"。后者才是真正的数据损坏。
	//
	// 崩溃残留也被顺带解决:固定名的 tmp 会被下一次的 O_TRUNC 当成有效数据源复用,
	// CreateTemp 的随机名则每次都是全新文件。
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	tmp := f.Name()

	// cleanup 闭包:失败时清掉自己的 tmp(成功路径下已被 rename 走,Remove 会失败,
	// 无害)。只删自己那个随机名,不会误删别人的中间文件。
	done := false
	defer func() {
		if !done {
			_ = os.Remove(tmp)
		}
	}()

	// CreateTemp 一律 0600,调用方要的 perm 必须显式 chmod。
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	// fsync 确保数据落盘再 rename(防止断电场景下新文件内容是 0)
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
	done = true
	return nil
}
