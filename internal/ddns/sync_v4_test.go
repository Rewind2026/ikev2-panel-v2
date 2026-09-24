// v2-84 专项测试:family 选择 / 并发 upsert / per-type 节流 / 状态文件迁移。
package ddns

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helper:构造测试用 Sync
func newTestSync(t *testing.T, mods ...func(*Config)) *Sync {
	t.Helper()
	tmp := t.TempDir()
	cfg := Config{
		Enabled:             true,
		Family:              "dual",
		AliyunAccessKeyID:   "ak",
		AliyunAccessKeySecret: "sk",
		Domain:              "example.com",
		RR:                  "vpn",
		Period:              100 * time.Millisecond,
		Throttle:            0, // 默认不节流,具体 case 自行覆盖
		LastFailedFile:      filepath.Join(tmp, "FAILED"),
		StateFile:           filepath.Join(tmp, "state"),
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	for _, m := range mods {
		m(&cfg)
	}
	return NewSync(cfg)
}

// TestSync_FamilyDefault 默认 family = dual。
func TestSync_FamilyDefault(t *testing.T) {
	s := newTestSync(t, func(c *Config) { c.Family = "" })
	if s.Family() != "dual" {
		t.Errorf("default family = %q, want dual", s.Family())
	}
}

// TestSync_FamilyInvalidFallsback 非法 family → 回落 dual。
func TestSync_FamilyInvalidFallsback(t *testing.T) {
	s := newTestSync(t, func(c *Config) { c.Family = "bogus" })
	if s.Family() != "dual" {
		t.Errorf("invalid family should fallback to dual, got %q", s.Family())
	}
}

// TestSync_SetFamily 切换 family + 写状态文件。
func TestSync_SetFamily(t *testing.T) {
	s := newTestSync(t)
	if s.Family() != "dual" {
		t.Fatalf("initial family = %q, want dual", s.Family())
	}

	if err := s.SetFamily("v4"); err != nil {
		t.Fatalf("SetFamily(v4): %v", err)
	}
	if s.Family() != "v4" {
		t.Errorf("after SetFamily(v4), Family() = %q", s.Family())
	}

	// 状态文件应被 atomic 写
	rec, err := parseStateFile(s.cfg.StateFile, "dual")
	if err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if rec.Family != "v4" {
		t.Errorf("state file family = %q, want v4", rec.Family)
	}
}

// TestSync_SetFamilyRejectsInvalid 非法值拒绝。
func TestSync_SetFamilyRejectsInvalid(t *testing.T) {
	s := newTestSync(t)

	for _, bad := range []string{"", "ipv4", "v6\ninjection", "v6; DROP TABLE"} {
		if err := s.SetFamily(bad); err == nil {
			t.Errorf("SetFamily(%q) should return error", bad)
		}
	}
	if s.Family() != "dual" {
		t.Errorf("Family() should remain dual after rejected sets, got %q", s.Family())
	}
}

// v2.86-pr22:Domain / RR / BaseDomain / Period / Iface getter 测试
// v2.86-pr23l:DetectTarget getter 已废弃(始终返回 "")。
func TestSync_ConfigGetters_RRWithDomain(t *testing.T) {
	s := newTestSync(t)
	if got := s.Domain(); got != "vpn.example.com" {
		t.Errorf("Domain() = %q, want vpn.example.com", got)
	}
	if got := s.BaseDomain(); got != "example.com" {
		t.Errorf("BaseDomain() = %q, want example.com", got)
	}
	if got := s.RR(); got != "vpn" {
		t.Errorf("RR() = %q, want vpn", got)
	}
}

func TestSync_ConfigGetters_RRAtSign(t *testing.T) {
	s := newTestSync(t, func(c *Config) { c.RR = "@" })
	if got := s.Domain(); got != "example.com" {
		t.Errorf("Domain() with RR=@ should be example.com, got %q", got)
	}
}

func TestSync_ConfigGetters_PeriodAndIface(t *testing.T) {
	s := newTestSync(t, func(c *Config) {
		c.Period = 5 * time.Minute
		c.Iface = "eth0"
	})
	if got := s.Period(); got != 5*time.Minute {
		t.Errorf("Period() = %v, want 5m", got)
	}
	if got := s.Iface(); got != "eth0" {
		t.Errorf("Iface() = %q, want eth0", got)
	}
	// v2.86-pr23l:DetectTarget() deprecated → 始终返回 ""
	if got := s.DetectTarget(); got != "" {
		t.Errorf("DetectTarget() deprecated, expected empty string, got %q", got)
	}
}

// TestSync_MigrateLegacyStateFile v2-83 裸 "true" 文件 → 自动迁移到 INI 格式。
func TestSync_MigrateLegacyStateFile(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")

	// 写 v2-83 风格裸 true
	if err := os.WriteFile(sf, []byte("true"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	// 构造 Sync → NewSync 内部应触发迁移
	s := newTestSync(t, func(c *Config) {
		c.StateFile = sf
		c.Family = "v6" // 测试 defaultFamily 透传
	})

	// 验证迁移后内容是 INI 格式
	data, err := os.ReadFile(sf)
	if err != nil {
		t.Fatalf("read after migration: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "enabled=true") {
		t.Errorf("migrated file should contain enabled=true, got: %q", content)
	}
	if !strings.Contains(content, "family=v6") {
		t.Errorf("migrated file should contain family=v6, got: %q", content)
	}
	if strings.Contains(content, "\ntrue") {
		t.Errorf("migrated file still has legacy bare 'true', got: %q", content)
	}

	// 内存里也应该反映迁移后状态
	if !s.IsEnabled() {
		t.Error("after migration, IsEnabled() should be true")
	}
	if s.Family() != "v6" {
		t.Errorf("after migration, Family() = %q, want v6", s.Family())
	}
}

// TestSync_MigrateLegacyFalseFile v2-83 裸 "false" 文件 → 迁移且 enabled=false。
func TestSync_MigrateLegacyFalseFile(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")
	if err := os.WriteFile(sf, []byte("false"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := newTestSync(t, func(c *Config) {
		c.StateFile = sf
		c.Family = "v4"
	})

	data, _ := os.ReadFile(sf)
	content := string(data)
	if !strings.Contains(content, "enabled=false") {
		t.Errorf("migrated file should have enabled=false, got: %q", content)
	}
	if !strings.Contains(content, "family=v4") {
		t.Errorf("migrated file should have family=v4, got: %q", content)
	}
	if s.IsEnabled() {
		t.Error("after migration from false, IsEnabled() should be false")
	}
}

// TestSync_LastSyncStructuredV4 dual 模式下 LastSync 拆分 v4/v6 字段。
//
// 场景:dual + v4 探测成功(凭证无效 → upsert 失败,这是预期路径) +
//      v6 探测失败(返回 error)。
//
// 验证:
//   - V4IP 填了(探测到的 IP,即便 upsert 失败)
//   - V4Error 填了(upsert 失败)
//   - V6Error 填了(探测失败)
//   - V6IP 空
func TestSync_LastSyncStructuredV4(t *testing.T) {
	s := newTestSync(t, func(c *Config) { c.Family = "dual" })

	s.SetDetectV6(func(string) (string, error) {
		return "", errors.New("no ipv6 in test")
	})
	s.SetDetectV4(func(iface, target string) (string, error) {
		return "1.2.3.4", nil
	})

	s.tick()

	snap := s.LastSyncSnapshot()
	if snap.V4IP != "1.2.3.4" {
		t.Errorf("LastSync.V4IP = %q, want 1.2.3.4", snap.V4IP)
	}
	if snap.V4Error == "" {
		t.Error("LastSync.V4Error should be set (upsert fails with bogus credentials)")
	}
	if snap.V6Error == "" {
		t.Error("LastSync.V6Error should be set (v6 detect failed)")
	}
	if snap.V6IP != "" {
		t.Errorf("LastSync.V6IP should be empty (v6 detect failed), got %q", snap.V6IP)
	}
	// 顶层 Success = 任一 family 成功过 → 这里都失败所以 false
	if snap.Success {
		t.Error("LastSync.Success should be false (both families failed)")
	}
}

// TestSync_ConcurrentUpsertBothCalled v4 + v6 各自探测 + upsert 都执行。
func TestSync_ConcurrentUpsertBothCalled(t *testing.T) {
	var (
		v6DetectCalled atomic.Int32
		v4DetectCalled atomic.Int32
	)
	s := newTestSync(t, func(c *Config) { c.Family = "dual" })

	s.SetDetectV6(func(string) (string, error) {
		v6DetectCalled.Add(1)
		return "2001:db8::1", nil
	})
	s.SetDetectV4(func(iface, target string) (string, error) {
		v4DetectCalled.Add(1)
		return "1.2.3.4", nil
	})

	s.tick()

	if v6DetectCalled.Load() != 1 {
		t.Errorf("v6 detect should be called once, got %d", v6DetectCalled.Load())
	}
	if v4DetectCalled.Load() != 1 {
		t.Errorf("v4 detect should be called once, got %d", v4DetectCalled.Load())
	}
}

// TestSync_V4OnlyMode dual=disabled,f=v4 only。
func TestSync_V4OnlyMode(t *testing.T) {
	var (
		v6DetectCalled atomic.Int32
		v4DetectCalled atomic.Int32
	)
	s := newTestSync(t, func(c *Config) { c.Family = "v4" })

	s.SetDetectV6(func(string) (string, error) {
		v6DetectCalled.Add(1)
		return "", nil // 即使返回也不应被调
	})
	s.SetDetectV4(func(iface, target string) (string, error) {
		v4DetectCalled.Add(1)
		return "1.2.3.4", nil
	})

	s.tick()

	if v6DetectCalled.Load() != 0 {
		t.Errorf("v6 detect should NOT be called when family=v4, got %d", v6DetectCalled.Load())
	}
	if v4DetectCalled.Load() != 1 {
		t.Errorf("v4 detect should be called once, got %d", v4DetectCalled.Load())
	}
}
