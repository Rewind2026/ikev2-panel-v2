// v2.86-PR15:internal/fsutil 包测试。
//
// 这些测试是从 internal/swanctl/atomic_test.go 1:1 复制的(测试 swanctl.AtomicWriteFile
// 现在转调 fsutil.AtomicWriteFile,所以保留等价覆盖)。
package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/fsutil"
)

// TestAtomicWriteFile_NewFile 验证:新文件场景正常写入。
func TestAtomicWriteFile_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")

	data := []byte("hello world")
	if err := fsutil.AtomicWriteFile(path, data, 0o600); err != nil {
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

// TestAtomicWriteFile_Overwrite 验证:覆盖现有文件不丢内容。
func TestAtomicWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")

	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}

	newData := []byte("new content with more lines\nand stuff")
	if err := fsutil.AtomicWriteFile(path, newData, 0o600); err != nil {
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

// TestAtomicWriteFile_CreatesParentDir 验证:父目录不存在时自动 MkdirAll。
func TestAtomicWriteFile_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deeply", "nested", "test.conf")

	if err := fsutil.AtomicWriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestAtomicWriteFile_LargeContent 验证:大文件也能写入(无截断)。
func TestAtomicWriteFile_LargeContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.conf")

	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := fsutil.AtomicWriteFile(path, data, 0o600); err != nil {
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

// TestAtomicWriteFile_OverwritePermissionChange 验证:覆盖时可以改权限。
func TestAtomicWriteFile_OverwritePermissionChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")

	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm after overwrite: got %o want 600", info.Mode().Perm())
	}
}

// TestAtomicWriteFile_NoTmpResidue 验证:成功后不留下 .tmp 残留。
func TestAtomicWriteFile_NoTmpResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.conf")

	for i := 0; i < 5; i++ {
		if err := fsutil.AtomicWriteFile(path, []byte("iter"), 0o600); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf(".tmp residue: %s", e.Name())
		}
	}
}