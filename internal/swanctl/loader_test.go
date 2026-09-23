package swanctl

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWaitForCharonReady_SocketMissing 验证:不存在的 socket 路径应在 timeout 后返回 error。
func TestWaitForCharonReady_SocketMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "charon.vici")

	start := time.Now()
	err := WaitForCharonReady(missing, 2*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error message should contain 'not ready', got: %v", err)
	}
	// 2s timeout,1s interval → 期望 2 次 attempt 后返回,总耗时 1~3s
	if elapsed < 1*time.Second || elapsed > 4*time.Second {
		t.Errorf("elapsed %s out of expected range [1s, 4s]", elapsed)
	}
}

// TestWaitForCharonReady_SocketAppears 验证:socket 出现后能成功识别 ready。
//
// 用一个真实 unix socket(可 listen + accept)模拟 charon.vici,等 socket
// 文件就位后 WaitForCharonReady 应立即返回(nil error)。
func TestWaitForCharonReady_SocketAppears(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "charon.vici")

	// 1s 后创建可 listen 的 unix socket
	go func() {
		time.Sleep(1 * time.Second)
		// 删除可能存在的残留文件
		_ = os.Remove(sockPath)
		l, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Errorf("listen on %s: %v", sockPath, err)
			return
		}
		defer l.Close()
		// 维持 socket 存活 5s,期间 WaitForCharonReady 应能 connect
		time.Sleep(5 * time.Second)
	}()

	start := time.Now()
	err := WaitForCharonReady(sockPath, 5*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitForCharonReady failed: %v", err)
	}
	// socket 在 ~1s 出现,期望 ~1s 后成功
	if elapsed > 4*time.Second {
		t.Errorf("elapsed %s too long (expected ~1s)", elapsed)
	}
}

// TestWaitForCharonReady_DefaultSocket 验证:空 sockPath 走 defaultViciSocketPath
// 且容器/dev 环境下不存在时也能正常返回 error(不 panic)。
func TestWaitForCharonReady_DefaultSocket(t *testing.T) {
	// 用很短 timeout 测一下默认路径也能正常工作
	err := WaitForCharonReady("", 1*time.Second)
	// 在沙箱里没有 /var/run/charon.vici,期望 error(但不 panic)
	if err == nil {
		t.Skip("default charon.vici exists in this env; cannot verify fallback path")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error message should contain 'not ready', got: %v", err)
	}
}

// TestWaitForCharonReady_StatOKButNewSessionFails 验证:stat 成功但 NewSession
// 失败时仍会重试,直到 timeout。
//
// 在 socket 文件存在但不是有效 unix socket 的情况下,
// vici.NewSession 会失败,函数应继续重试。
func TestWaitForCharonReady_StatOKButNewSessionFails(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "charon.vici")

	// 创建一个普通文件(不是 socket),stat 成功但 NewSession 失败
	if err := os.WriteFile(sockPath, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("write fake sock: %v", err)
	}

	start := time.Now()
	err := WaitForCharonReady(sockPath, 2*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error (stat ok but NewSession always fails), got nil")
	}
	// 2s timeout,期望 1~3s 后返回
	if elapsed < 1*time.Second || elapsed > 4*time.Second {
		t.Errorf("elapsed %s out of expected range [1s, 4s]", elapsed)
	}
}