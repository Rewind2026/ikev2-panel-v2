// v2.86-PR13.2:UpdatePools / ReadCurrentPools 单元测试
//
// 覆盖:
//   - ReadCurrentPools:正常解析、空 addrs、不存在
//   - UpdatePools:正常替换、ipv4 空报错、ipv6 空报错、没找到 addrs 行报错
//   - UpdatePoolsAndReload:整体路径(写 tmp + rename + reload)
//     但 SkipVici=true 跳过真实 reload,避免依赖 swanctl 二进制
package swanctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 标准 swanctl.conf 测试片段(摘自 configs/swanctl-ipv6-only.conf)。
const testConfSnippet = `connections {
    ikev2-rw {
        pools = ikev2-pool
    }
}

pools {
    ikev2-pool {
        addrs = 10.10.0.0/24, fd00:1::/64
        dns = 1.1.1.1, 8.8.8.8
    }
}

include /etc/swanctl/conf.d/*.conf
`

func TestReadCurrentPools(t *testing.T) {
	v4, v6, err := ReadCurrentPools([]byte(testConfSnippet))
	if err != nil {
		t.Fatalf("ReadCurrentPools err=%v", err)
	}
	if v4 != "10.10.0.0/24" {
		t.Errorf("ipv4 = %q, want 10.10.0.0/24", v4)
	}
	if v6 != "fd00:1::/64" {
		t.Errorf("ipv6 = %q, want fd00:1::/64", v6)
	}
}

func TestReadCurrentPools_Missing(t *testing.T) {
	_, _, err := ReadCurrentPools([]byte("no pools here\n"))
	if err == nil {
		t.Error("期望报错(没找到 addrs 行)")
	}
}

func TestUpdatePools_Success(t *testing.T) {
	original := []byte(testConfSnippet)
	updated, err := UpdatePools(original, "10.13.0.0/24", "fd00:5::/64")
	if err != nil {
		t.Fatalf("UpdatePools err=%v", err)
	}
	out := string(updated)

	// 必须包含新 addrs
	if !strings.Contains(out, "addrs = 10.13.0.0/24, fd00:5::/64") {
		t.Errorf("updated should contain new addrs, got:\n%s", out)
	}
	// 不应包含旧 addrs
	if strings.Contains(out, "addrs = 10.10.0.0/24") {
		t.Errorf("updated should not contain old addrs, got:\n%s", out)
	}
	// 周围内容保留(pools 段、dns 段)
	if !strings.Contains(out, "dns = 1.1.1.1, 8.8.8.8") {
		t.Errorf("dns 段应保留")
	}
	if !strings.Contains(out, "pools = ikev2-pool") {
		t.Errorf("connections 段应保留")
	}
	if !strings.Contains(out, "include /etc/swanctl/conf.d/*.conf") {
		t.Errorf("尾部 include 应保留")
	}
}

func TestUpdatePools_EmptyV4(t *testing.T) {
	_, err := UpdatePools([]byte(testConfSnippet), "", "fd00:5::/64")
	if err == nil {
		t.Error("ipv4 空应该报错")
	}
}

func TestUpdatePools_EmptyV6(t *testing.T) {
	_, err := UpdatePools([]byte(testConfSnippet), "10.13.0.0/24", "")
	if err == nil {
		t.Error("ipv6 空应该报错")
	}
}

func TestUpdatePools_NoMatch(t *testing.T) {
	_, err := UpdatePools([]byte("no addrs line here\n"), "10.13.0.0/24", "fd00:5::/64")
	if err == nil {
		t.Error("没找到 addrs 行应该报错")
	}
}

func TestUpdatePoolsAndReload_AtomicReplace(t *testing.T) {
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "swanctl.conf")
	if err := os.WriteFile(confPath, []byte(testConfSnippet), 0o644); err != nil {
		t.Fatalf("write test conf: %v", err)
	}

	m := New()
	m.SkipVici = true
	m.WithSwanctlConfPath(confPath)

	if err := m.UpdatePoolsAndReload(context.Background(), "10.13.0.0/24", "fd00:5::/64"); err != nil {
		t.Fatalf("UpdatePoolsAndReload err=%v", err)
	}

	// 文件应被替换为新 addrs
	content, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(content), "addrs = 10.13.0.0/24, fd00:5::/64") {
		t.Errorf("conf file not updated, got:\n%s", string(content))
	}

	// 不应残留 .tmp 文件
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".swanctl.conf.") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留 tmp 文件: %s", e.Name())
		}
	}

	// 文件权限保持 0644(v2-79 entrypoint 写入)
	st, err := os.Stat(confPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o644 {
		t.Errorf("file mode = %o, want 0644", mode)
	}
}

func TestUpdatePoolsAndReload_ConcurrentSafe(t *testing.T) {
	// 两个并发 UpdatePoolsAndReload 调用,因为 reloadMu 串行化,不会互相覆盖。
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "swanctl.conf")
	if err := os.WriteFile(confPath, []byte(testConfSnippet), 0o644); err != nil {
		t.Fatalf("write test conf: %v", err)
	}

	m := New()
	m.SkipVici = true
	m.WithSwanctlConfPath(confPath)

	done := make(chan error, 2)
	go func() { done <- m.UpdatePoolsAndReload(context.Background(), "10.13.0.0/24", "fd00:5::/64") }()
	go func() { done <- m.UpdatePoolsAndReload(context.Background(), "10.42.0.0/24", "fd00:42::/64") }()

	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("goroutine %d err=%v", i, err)
		}
	}

	// 文件内容必须是两个之一(具体哪个取决于调度),但不应是 testConfSnippet 的旧值
	content, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, "addrs = 10.13.0.0/24, fd00:5::/64") &&
		!strings.Contains(got, "addrs = 10.42.0.0/24, fd00:42::/64") {
		t.Errorf("file should be one of the two new addrs, got:\n%s", got)
	}
}

func TestReadCurrentPoolsFromFile_DevMode(t *testing.T) {
	// 不存在的文件 → 返回 "", ""(不报错)
	m := New()
	m.SkipVici = true
	m.WithSwanctlConfPath("/nonexistent/path/swanctl.conf")
	v4, v6 := m.ReadCurrentPoolsFromFile()
	if v4 != "" || v6 != "" {
		t.Errorf("dev 模式下应返回空, got v4=%q v6=%q", v4, v6)
	}
}

func TestUpdatePoolsAndReload_DevMode(t *testing.T) {
	// dev 模式:文件不存在 → 静默返回 nil(不报错)
	m := New()
	m.SkipVici = true
	m.WithSwanctlConfPath("/nonexistent/path/swanctl.conf")
	if err := m.UpdatePoolsAndReload(context.Background(), "10.13.0.0/24", "fd00:5::/64"); err != nil {
		t.Errorf("dev 模式下应静默成功,实际 err=%v", err)
	}
}
