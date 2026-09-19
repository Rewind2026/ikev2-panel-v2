package swanctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAtomicWriteFile_NewFile 验证：新文件场景正常写入。
func TestAtomicWriteFile_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")

	data := []byte("hello world")
	if err := AtomicWriteFile(path, data, 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content: got %q want %q", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm: got %o want 600", info.Mode().Perm())
	}
}

// TestAtomicWriteFile_Overwrite 验证：覆盖现有文件不丢内容。
func TestAtomicWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")

	// 先写一个老内容
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 原子覆盖
	newData := []byte("new content with more lines\nand stuff")
	if err := AtomicWriteFile(path, newData, 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newData) {
		t.Errorf("content: got %q want %q", got, newData)
	}
}

// TestAtomicWriteFile_CreatesParentDir 验证：父目录不存在时自动 MkdirAll。
func TestAtomicWriteFile_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deeply", "nested", "test.conf")

	if err := AtomicWriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestAtomicWriteFile_CleansUpOnFailure 验证：失败时不留 .tmp。
//
// 场景：父目录权限不够 → os.OpenFile 失败 → tmp 创建过但 cleanup 必须清理。
//
// 实际很难强制 OpenFile 失败而不污染环境。我们用 read-only 父目录 + 模拟。
func TestAtomicWriteFile_CleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "readonly")
	if err := os.Mkdir(subDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(subDir, 0o755) // 清理

	path := filepath.Join(subDir, "test.conf")

	err := AtomicWriteFile(path, []byte("data"), 0o600)
	if err == nil {
		// root 用户可以绕过权限,跳过此断言
		t.Skip("read-only dir test skipped (likely running as root)")
	}

	// 验证没留下 .tmp 残留
	if _, statErr := os.Stat(path + ".tmp"); statErr == nil {
		t.Errorf("failed write left .tmp file behind: %s", path+".tmp")
	}
}

// TestAtomicWriteFile_LargeContent 验证：大文件也能写入（无截断）。
func TestAtomicWriteFile_LargeContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.conf")

	// 1 MB 随机内容
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := AtomicWriteFile(path, data, 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Errorf("size: got %d want %d", len(got), len(data))
	}
	for i := range data {
		if got[i] != data[i] {
			t.Errorf("byte %d: got %d want %d", i, got[i], data[i])
			break
		}
	}
}

// TestAtomicWriteFile_OverwritePermissionChange 验证：覆盖时可以改权限。
//
// 实际场景：v2-79.1 把 server.key.pem 从 0644 改成 0600。
// 如果中间崩溃,原 0644 文件必须保持完整(直到下次成功覆盖)。
func TestAtomicWriteFile_OverwritePermissionChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")

	// 先写 0644
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 原子覆盖成 0600
	if err := AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm after overwrite: got %o want 600", info.Mode().Perm())
	}
}

// TestWriteUserConf_AtomicOverwrite 验证 WriteUserConf 走 AtomicWriteFile 路径：
// 用户密码被原子替换,不存在"新旧密码混在一起"的中间态。
func TestWriteUserConf_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	m := New().WithConfDir(dir)

	// 1) 创建老密码
	if err := m.WriteUserConf("alice", "oldpassword"); err != nil {
		t.Fatalf("first WriteUserConf: %v", err)
	}

	// 2) 验证文件内容是老密码（不应有"oldpasswordnewpassword"这种中间态）
	first, _ := os.ReadFile(filepath.Join(dir, "alice.conf"))
	if !strings.Contains(string(first), "oldpassword") {
		t.Errorf("first write missing old password:\n%s", first)
	}

	// 3) 原子覆盖新密码
	if err := m.WriteUserConf("alice", "newpassword"); err != nil {
		t.Fatalf("second WriteUserConf: %v", err)
	}

	// 4) 文件现在只含新密码
	second, _ := os.ReadFile(filepath.Join(dir, "alice.conf"))
	body := string(second)
	if !strings.Contains(body, "newpassword") {
		t.Errorf("second write missing new password:\n%s", body)
	}
	if strings.Contains(body, "oldpassword") {
		t.Errorf("second write contains old password (atomicity broken):\n%s", body)
	}

	// 5) 不应留 .tmp 残留
	if _, err := os.Stat(filepath.Join(dir, "alice.conf.tmp")); err == nil {
		t.Errorf(".tmp residue after WriteUserConf")
	}
}

// TestWriteUserConf_TmpFileCleanedUpOnOverwrite 验证：多次覆盖不累积 .tmp。
func TestWriteUserConf_TmpFileCleanedUpOnOverwrite(t *testing.T) {
	dir := t.TempDir()
	m := New().WithConfDir(dir)

	for i := 0; i < 10; i++ {
		if err := m.WriteUserConf("alice", "pw"+string(rune('0'+i))); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf(".tmp residue after 10 writes: %s", e.Name())
		}
	}
}