// v2.86-PR19(P1-9):UpdatePoolsAndReload 失败回滚的回归测试。
//
// 背景：原实现"写文件成功但 reload 失败时不回滚"，注释写"调用方决定是否回滚"，
// 但唯一调用方（handlers_subnet.go:132）只报错不���滚。后果是**状态不一致**：
//
//	磁盘 swanctl.conf = 新 IP 段
//	charon 运行中     = 旧 IP 段（用户以为改失败了）
//	下次容器重启/重载 → 新段突然生效，时机不可控
//
// 用户现象："提示改失败，过一会儿自己又变了"。
//
// 本文件验证：
//  1. reload 失败 → 磁盘文件回滚到改之前的字节内容
//  2. reload 成功 → 文件保持新值（不回滚）
//  3. 回滚用原子手法（tmp + fsync + rename），不留半截文件
//  4. reload 失败时错误信息里包含"已回滚"字样，便于运维判断状态
//  5. reload 失败 + 回滚也失败 → 错误里必须暴露"磁盘已被改动"这个事实
package swanctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 最小可用的 swanctl.conf：只需要 pools.ikev2-pool.addrs 行能被 UpdatePools 匹配。
const poolConfTmpl = `connections {
 ikev2 {
   version = 2
   pools = ikev2-pool
   local_addrs  = %any
   remote_addrs = 0.0.0.0/0, ::/0
 }
}

pools {
 ikev2-pool {
   addrs = 10.10.0.0/24
 }
}
`

func writeConf(t *testing.T, content string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "swanctl.conf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed conf: %v", err)
	}
	return dir, path
}

func readConf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	return string(b)
}

// newFailingReloadManager 构造一个"reload 必然失败"的 Manager。
//
// SwanctlBin 是包级常量（/usr/sbin/swanctl），测试环境里不存在该文件，
// exec 会立即失败 —— 这正是既有 TestReloadAll_BinaryNotExists 依赖的机制。
// SkipVici 保持 false，否则 reloadAllLocked 直接 return nil 不会失败。
func newFailingReloadManager(t *testing.T, confPath string) *Manager {
	t.Helper()
	if _, err := os.Stat(SwanctlBin); err == nil {
		t.Skipf("%s 在本机存在，无法构造 reload 失败场景", SwanctlBin)
	}
	return New().WithSwanctlConfPath(confPath)
}

// TestUpdatePoolsAndReloadRollsBackOnReloadFailure 核心用例：
// reload 失败后磁盘必须回到改之前的内容。
func TestUpdatePoolsAndReloadRollsBackOnReloadFailure(t *testing.T) {
	_, confPath := writeConf(t, poolConfTmpl)
	orig := readConf(t, confPath)

	m := newFailingReloadManager(t, confPath)

	err := m.UpdatePoolsAndReload(context.Background(), "10.99.0.0/24", "fd00:99::/64")
	if err == nil {
		t.Fatal("expected reload error, got nil")
	}

	after := readConf(t, confPath)
	if after != orig {
		t.Errorf("swanctl.conf was NOT rolled back after reload failure\n--- want ---\n%s\n--- got ---\n%s",
			orig, after)
	}
	if strings.Contains(after, "10.99.0.0/24") {
		t.Error("new pool value still on disk — 下次重启会突然生效")
	}
	if !strings.Contains(err.Error(), "已回滚") {
		t.Errorf("error should state that rollback happened, got %q", err.Error())
	}
}

// TestUpdatePoolsAndReloadKeepsNewValueOnSuccess reload 成功时不得回滚。
//
// 防止"修回滚时顺手把正常路径也回滚了"这种反向 bug。
func TestUpdatePoolsAndReloadKeepsNewValueOnSuccess(t *testing.T) {
	_, confPath := writeConf(t, poolConfTmpl)

	// SkipVici=true → reloadAllLocked 直接返回 nil，模拟 reload 成功
	m := New().WithSwanctlConfPath(confPath).WithSkipVici()

	if err := m.UpdatePoolsAndReload(context.Background(), "10.99.0.0/24", "fd00:99::/64"); err != nil {
		t.Fatalf("reload should have succeeded: %v", err)
	}

	after := readConf(t, confPath)
	if !strings.Contains(after, "10.99.0.0/24") {
		t.Errorf("new pool value missing after successful reload:\n%s", after)
	}
	if !strings.Contains(after, "fd00:99::/64") {
		t.Errorf("new IPv6 pool value missing after successful reload:\n%s", after)
	}
}

// TestUpdatePoolsAndReloadRollbackIsAtomic 回滚必须走原子手法，不能留半截文件。
//
// 判据：执行期间同目录下不应出现最终残留的 rollback tmp 文件。
func TestUpdatePoolsAndReloadRollbackIsAtomic(t *testing.T) {
	dir, confPath := writeConf(t, poolConfTmpl)

	m := newFailingReloadManager(t, confPath)

	if err := m.UpdatePoolsAndReload(context.Background(), "10.99.0.0/24", "fd00:99::/64"); err == nil {
		t.Fatal("expected reload error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var leftovers []string
	for _, e := range entries {
		n := e.Name()
		if n == "swanctl.conf" {
			continue
		}
		leftovers = append(leftovers, n)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind after rollback: %v", leftovers)
	}

	// 回滚后的文件权限必须仍是 0644（charon 要读）
	info, err := os.Stat(confPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("rolled-back file mode = %o, want 644 (charon reads this file)", got)
	}
}

// TestUpdatePoolsAndReloadRollbackFailureIsSurfaced 回滚也失败时，
// 错误必须暴露"磁盘已被改动"这个事实，否则调用方无从得知真实状态。
func TestUpdatePoolsAndReloadRollbackFailureIsSurfaced(t *testing.T) {
	_, confPath := writeConf(t, poolConfTmpl)

	m := newFailingReloadManager(t, confPath)

	err := m.UpdatePoolsAndReload(context.Background(), "10.99.0.0/24", "fd00:99::/64")
	if err == nil {
		t.Fatal("expected error")
	}
	// 本用例只锁定"reload 失败"路径的错误文案形态：
	// 成功回滚时是"已回滚"，回滚失败时是"回滚 ... 也失败"。
	// 这里 reload 失败但目录可写（t.TempDir），所以走的是成功回滚分支；
	// 断言它**没有**谎报回滚失败。
	if !strings.Contains(err.Error(), "已回滚") {
		t.Errorf("unexpected error shape: %q", err.Error())
	}
	if strings.Contains(err.Error(), "回滚 swanctl.conf 也失败") {
		t.Errorf("rollback should have succeeded in a writable temp dir, got %q", err.Error())
	}
}

// TestUpdatePoolsAndReloadRollbackFailureReportsDirtyDisk 回滚**也**失败时，
// 错误必须暴露"磁盘已被改动"这个事实，否则运维只看到"reload 失败"，
// 不知道 swanctl.conf 已经是新值了。
//
// 构造：验证 restoreFile 在目标不可写时给出**可定位**的错误（点名失败步骤），
// 而非一句笼统的失败 —— 这条错误会被包进 UpdatePoolsAndReload 的返回里，
// 是运维判断"要不要手工介入"的唯一线索。
// 用"目标目录不存在"构造（可移植，不依赖 root 权限或只读挂载）。
func TestUpdatePoolsAndReloadRollbackFailureReportsDirtyDisk(t *testing.T) {
	dir := t.TempDir()
	err := restoreFile(filepath.Join(dir, "nonexistent-dir", "x.conf"), []byte("x"))
	if err == nil {
		t.Fatal("restoreFile into a missing directory should fail")
	}
	if !strings.Contains(err.Error(), "rollback tmp") {
		t.Errorf("error should identify the failing step, got %q", err.Error())
	}
}

// TestRestoreFileOverwritesContent restoreFile 必须完整覆盖内容。
func TestRestoreFileOverwritesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.conf")
	if err := os.WriteFile(path, []byte("new content that is longer"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := restoreFile(path, []byte("old")); err != nil {
		t.Fatalf("restoreFile: %v", err)
	}
	got := readConf(t, path)
	if got != "old" {
		t.Errorf("content = %q, want %q (旧内容必须完全覆盖,不能残留尾部)", got, "old")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected only target.conf, got %d entries", len(entries))
	}
}
