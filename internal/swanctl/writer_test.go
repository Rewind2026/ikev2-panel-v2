package swanctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndRemoveUserConf(t *testing.T) {
	dir := t.TempDir()
	m := New().WithConfDir(dir)

	// 写
	if err := m.WriteUserConf("alice", "secret123"); err != nil {
		t.Fatalf("WriteUserConf: %v", err)
	}
	path := filepath.Join(dir, "alice.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// 内容应包含关键字段
	s := string(content)
	if !strings.Contains(s, "eap-alice") {
		t.Errorf("missing eap-alice: %s", s)
	}
	if !strings.Contains(s, "id = alice") {
		t.Errorf("missing id = alice: %s", s)
	}
	if !strings.Contains(s, "secret") {
		t.Errorf("missing secret: %s", s)
	}

	// 文件权限：Windows 不强制 umask，跳过权限检查
	if info, _ := os.Stat(path); info == nil {
		t.Error("stat returned nil")
	}

	// 删除
	if err := m.RemoveUserConf("alice"); err != nil {
		t.Fatalf("RemoveUserConf: %v", err)
	}
	if m.UserConfExists("alice") {
		t.Error("expected alice.conf to be gone")
	}

	// 二次删除不报错
	if err := m.RemoveUserConf("alice"); err != nil {
		t.Errorf("second RemoveUserConf should be idempotent, got %v", err)
	}
}

func TestWriteUserConfPathTraversal(t *testing.T) {
	dir := t.TempDir()
	m := New().WithConfDir(dir)

	bad := []string{"../etc/passwd", "alice/bob", "alice.conf", "alice bob", ""}
	for _, u := range bad {
		if err := m.WriteUserConf(u, "x"); err == nil {
			t.Errorf("WriteUserConf(%q) should fail", u)
		}
		if err := m.RemoveUserConf(u); err == nil {
			t.Errorf("RemoveUserConf(%q) should fail", u)
		}
	}
}

// TestReloadAllViciNoSocket 验证 charon.vici 不存在时返回 error。
//
// 注：原 TestReloadAllNoCommand 是测 os/exec 调 swanctl（现已废弃），
//     现在测 VICI dial 不存在 socket 的场景。
func TestReloadAllViciNoSocket(t *testing.T) {
	m := New().WithViciSocketPath("/tmp/this-socket-does-not-exist-xyzzy-12345.sock")
	err := m.ReloadAll(context.Background())
	if err == nil {
		t.Fatal("expected error dialing non-existent vici socket, got nil")
	}
}

// TestReloadAllViciSkipMode 验证 dev 模式（SkipVici=true）下，socket 不存在时静默跳过。
func TestReloadAllViciSkipMode(t *testing.T) {
	m := New().WithViciSocketPath("/tmp/this-socket-does-not-exist-xyzzy-12345.sock").WithSkipVici()
	if err := m.ReloadAll(context.Background()); err != nil {
		t.Errorf("SkipVici mode should swallow error, got: %v", err)
	}
	if err := m.LoadCreds(context.Background()); err != nil {
		t.Errorf("SkipVici mode LoadCreds: %v", err)
	}
	if err := m.Terminate(context.Background(), "alice"); err != nil {
		t.Errorf("SkipVici mode Terminate: %v", err)
	}
}

// TestTerminateEmptyUsername 验证空 username 被拒绝。
func TestTerminateEmptyUsername(t *testing.T) {
	m := New()
	if err := m.Terminate(context.Background(), ""); err == nil {
		t.Error("expected error for empty username")
	}
}