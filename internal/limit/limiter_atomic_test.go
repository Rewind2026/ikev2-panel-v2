package limit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2.86-PR19(B15)回归测试:WriteLimitFile 改原子写后的行为保障。
//
// 原实现是裸 os.WriteFile(O_TRUNC 覆写)。限速文件被 updown 脚本读取决定
// tc class rate,半截内容 = 限速静默变成别的值("1000" 截成 "1")。

// 反复覆盖写不产生 tmp 残留,且内容始终是完整数值。
func TestWriteLimitFile_NoTmpResidue(t *testing.T) {
	dir := t.TempDir()
	l := New().WithDir(dir)

	for i := 0; i < 20; i++ {
		if err := l.WriteLimitFile("bob", 1000+i); err != nil {
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
		t.Fatalf("tmp residue after repeated writes: %v", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bob"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1019" {
		t.Fatalf("final content = %q, want \"1019\"", data)
	}
}

// 权限保持 0644(updown 脚本以非 root up 身份读)。
func TestWriteLimitFile_Perm(t *testing.T) {
	dir := t.TempDir()
	l := New().WithDir(dir)
	if err := l.WriteLimitFile("carol", 5); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "carol"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %o, want 644", info.Mode().Perm())
	}
}

// 0 = 不限速,是合法取值,不能被当成错误或写成空文件。
func TestWriteLimitFile_ZeroMeansUnlimited(t *testing.T) {
	dir := t.TempDir()
	l := New().WithDir(dir)
	if err := l.WriteLimitFile("dave", 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "dave"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "0" {
		t.Fatalf("content = %q, want \"0\"", data)
	}
}

// 目录不存在时自动创建（WithDir 指向尚未建立的子目录）。
func TestWriteLimitFile_CreatesDir(t *testing.T) {
	l := New().WithDir(filepath.Join(t.TempDir(), "a", "b", "limits"))
	if err := l.WriteLimitFile("erin", 20); err != nil {
		t.Fatal(err)
	}
}
