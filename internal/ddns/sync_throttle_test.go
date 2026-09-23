// v2.85-PR6 (Q5-01) 测试:DDNS throttle 时间戳持久化 + 启动回填。
//
// 测试覆盖:
//   - PersistThrottle:tick 一次 → 写 statefile → last_sync_a 出现
//   - ReloadThrottle:写 statefile 含 last_sync_a → NewSync → lastSyncTime map 有值
//   - StopOnce:连续调 Stop() N 次不 panic
//   - BackwardCompatOldFormat:v2-83 裸 "true" 文件 → NewSync 不 panic
package ddns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// TestSync_PersistThrottle 验证 tick 后 statefile 含 last_sync_a。
//
// 由于真实的 alidns 调用需要凭证+网络,这里直接调 setLastSyncAndPersist
// (它只依赖 statefile 路径 + logger,不依赖网络)。
func TestSync_PersistThrottle(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "ddns.conf")
	s := newTestSync(t, func(c *Config) {
		c.Enabled = true
		c.Family = "v4"
		c.StateFile = stateFile
	})

	// 模拟一次节流时间戳更新
	now := time.Unix(1726700000, 0).UTC()
	s.setLastSyncAndPersist(dns.RecordTypeA, now)

	// 1. statefile 应被创建并含 last_sync_a
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("statefile not created: %v", err)
	}
	if !strings.Contains(string(data), "last_sync_a=1726700000") {
		t.Errorf("statefile should contain 'last_sync_a=1726700000', got:\n%s", string(data))
	}
}

// TestSync_ReloadThrottle 验证 statefile → NewSync 回填 lastSyncTime map。
func TestSync_ReloadThrottle(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "ddns.conf")

	// 预写一个 statefile 模拟"已经跑过一段时间的 DDNS"
	const oldSyncUnix = "1726700000"
	ini := strings.Join([]string{
		"# test",
		"enabled=true",
		"family=v4",
		"last_sync_a=" + oldSyncUnix,
		"last_sync_aaaa=" + oldSyncUnix,
		"",
	}, "\n")
	if err := os.WriteFile(stateFile, []byte(ini), 0o644); err != nil {
		t.Fatalf("write statefile: %v", err)
	}

	// NewSync 应回填 lastSyncTime
	s := newTestSync(t, func(c *Config) {
		c.Enabled = false // 让 env 是 disabled,statefile enabled=true 应该覆盖
		c.Family = "v4"
		c.StateFile = stateFile
	})
	defer s.Stop()

	// 验证 enabled 被 statefile 覆盖
	if !s.cfg.Enabled {
		t.Errorf("enabled should be true from statefile, got false")
	}

	// 验证 lastSyncTime map 含 A 和 AAAA
	s.mu.Lock()
	a := s.lastSyncTime[dns.RecordTypeA]
	aaaa := s.lastSyncTime[dns.RecordTypeAAAA]
	s.mu.Unlock()

	if a.IsZero() {
		t.Error("lastSyncTime[A] should be reloaded from statefile, got zero")
	}
	if aaaa.IsZero() {
		t.Error("lastSyncTime[AAAA] should be reloaded from statefile, got zero")
	}
	wantTime := time.Unix(1726700000, 0)
	if !a.Equal(wantTime) {
		t.Errorf("lastSyncTime[A] = %v, want %v", a, wantTime)
	}
}

// TestSync_StopOnce 验证连续调 Stop() 不 panic,语义干净。
//
// v2.85-PR6 (Q1-02):旧实现用 select default,NewSync → Stop → NewSync → Stop 第二次
// 会 close 一个已 closed 的 channel → panic。sync.Once 修复此问题。
func TestSync_StopOnce(t *testing.T) {
	s := newTestSync(t)

	// 连续调 5 次 Stop,不应 panic
	for i := 0; i < 5; i++ {
		s.Stop()
	}

	// 之后再 NewSync 一个新实例,仍能正常用
	s2 := newTestSync(t)
	defer s2.Stop()
	// 检查 stopCh 是新的(没被前一个实例污染)— 通过调用 tick 验证不 panic
	s2.tick() // enabled=false → 直接 return,确认结构体可用
}

// TestSync_BackwardCompatOldFormat 验证 v2-83 裸 "true" 文件不破坏。
//
// v2.85-PR6:statefile 新增 last_sync_a/aaaa key,但老格式迁移 + parse 仍工作。
func TestSync_BackwardCompatOldFormat(t *testing.T) {
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "ddns.conf")
	if err := os.WriteFile(stateFile, []byte("true"), 0o644); err != nil {
		t.Fatalf("write old format statefile: %v", err)
	}

	s := newTestSync(t, func(c *Config) {
		c.Enabled = false
		c.Family = "dual"
		c.StateFile = stateFile
	})
	defer s.Stop()

	// 迁移:enable 应被设成 true,family=dual (default)
	if !s.cfg.Enabled {
		t.Errorf("old format 'true' should be parsed as enabled=true")
	}
	if s.cfg.Family != "dual" {
		t.Errorf("family should default to dual, got %q", s.cfg.Family)
	}

	// 迁移后,statefile 应被自动重写为 INI 格式(包含 enabled + family)
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read statefile after migration: %v", err)
	}
	if !strings.Contains(string(data), "enabled=true") {
		t.Errorf("statefile should be migrated to INI format, got:\n%s", string(data))
	}
}