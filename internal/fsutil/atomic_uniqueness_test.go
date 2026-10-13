package fsutil_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/fsutil"
)

// v2.86-PR19(B16):AtomicWriteFile 的 tmp 名改为 os.CreateTemp 后的回归测试。
//
// 修复的问题:原实现用固定的 path+".tmp",同一 path 并发写会互相覆盖 tmp,
// 且崩溃残留的 tmp 会被下次 O_TRUNC 复用。

// 并发写同一 path:结果必须是某个完整版本,不能是混合/半截内容。
func TestAtomicWriteFile_ConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.conf")

	// 两个"版本":v1 与 v2,长度不同且首字节不同,便于识别混合内容
	v1 := []byte(strings.Repeat("A", 200))
	v2 := []byte(strings.Repeat("B", 37))

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := v1
			if i%2 == 1 {
				data = v2
			}
			if err := fsutil.AtomicWriteFile(path, data, 0o600); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AtomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	allA := len(got) == len(v1) && strings.Trim(string(got), "A") == ""
	allB := len(got) == len(v2) && strings.Trim(string(got), "B") == ""
	if !allA && !allB {
		t.Fatalf("content is a mix of versions (atomicity broken): len=%d body=%q", len(got), got)
	}
}

// 崩溃残留的旧式固定名 tmp 不得被复用为数据来源。
func TestAtomicWriteFile_IgnoresLegacyFixedTmp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swanctl.conf")

	// 模拟 v2.86 之前的崩溃残留:固定名 path.tmp 内含垃圾
	legacy := filepath.Join(dir, "swanctl.conf.tmp")
	if err := os.WriteFile(legacy, []byte("GARBAGE-FROM-CRASH"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fsutil.AtomicWriteFile(path, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "good" {
		t.Fatalf("target file content = %q, want \"good\"", got)
	}
	// 旧的残留文件不做要求(不该被读),但新实现不应再创建固定名 tmp
	if _, err := os.Stat(legacy); err != nil {
		t.Logf("legacy tmp removed: %v", err)
	}
}

// 非 0600 权限必须真正生效(CreateTemp 默认 0600,需要显式 chmod)。
func TestAtomicWriteFile_AppliesRequestedPerm(t *testing.T) {
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o640, 0o644} {
		path := filepath.Join(dir, "p"+perm.String())
		if err := fsutil.AtomicWriteFile(path, []byte("x"), perm); err != nil {
			t.Fatalf("perm %o: %v", perm, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != perm {
			t.Errorf("perm: got %o want %o", info.Mode().Perm(), perm)
		}
	}
}

// 覆盖写时权限从 0644 收到 0600（v2-79.1 的既有场景）。
func TestAtomicWriteFile_TightensPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.key.pem")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm after tighten: got %o want 600", info.Mode().Perm())
	}
}

// 失败路径不留任何 tmp 残留（包括新随机名 tmp）。
func TestAtomicWriteFile_NoResidueOnFailure(t *testing.T) {
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)

	if err := fsutil.AtomicWriteFile(filepath.Join(ro, "x.conf"), []byte("d"), 0o600); err == nil {
		t.Skip("read-only dir bypassed (running as root?)")
	}
	entries, _ := os.ReadDir(ro)
	if len(entries) != 0 {
		t.Fatalf("tmp residue after failed write: %v", entries)
	}
}

// 反复写不产生 tmp 累积（CreateTemp 随机名不应泄漏）。
func TestAtomicWriteFile_RepeatedWritesNoAccumulation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.conf")
	for i := 0; i < 50; i++ {
		if err := fsutil.AtomicWriteFile(path, []byte("iter"), 0o600); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir should contain only the target file, got %v", names)
	}
}
