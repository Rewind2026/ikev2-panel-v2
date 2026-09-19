// DDNS 同步器单元测试(v2-82)。
//
// 测重点(测试不实际调阿里云 API):
//   1. 开关关闭时,任何 tick 都不调 detectV6 也不调 alidns
//   2. 凭证缺失时,enabled=true 也静默 skip
//   3. 节流:连续两次 tick(在 Throttle 窗口内),第二次跳过
//   4. state file 读写
//   5. SetEnabled 切换后 IsEnabled 跟着变
package ddns

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestSync_DisabledSkipsAllLogic 关闭时 tick 不应探测也不调 API。
func TestSync_DisabledSkipsAllLogic(t *testing.T) {
	var (
		detectCalled atomic.Int32
	)
	mockDetect := func(iface string) (string, error) {
		detectCalled.Add(1)
		return "2001:db8::1", nil
	}

	tmp := t.TempDir()
	s := NewSync(Config{
		Enabled:             false,
		AliyunAccessKeyID:   "ak",
		AliyunAccessKeySecret: "sk",
		Domain:              "example.com",
		RR:                  "vpn",
		Period:              100 * time.Millisecond,
		Throttle:            0, // 不节流
		LastFailedFile:      filepath.Join(tmp, "FAILED"),
		StateFile:           filepath.Join(tmp, "state"),
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	s.SetDetectV6(mockDetect)

	// 模拟一次 tick
	s.tick()

	if detectCalled.Load() != 0 {
		t.Errorf("detectV6 should NOT be called when enabled=false, got %d calls", detectCalled.Load())
	}
	if _, err := os.Stat(s.cfg.LastFailedFile); err == nil {
		t.Error("failed flag should not exist when disabled")
	}
}

// TestSync_MissingCredentials 凭证缺失,enabled=true 也静默 skip(不打失败标志)。
func TestSync_MissingCredentials(t *testing.T) {
	var detectCalled atomic.Int32
	mockDetect := func(iface string) (string, error) {
		detectCalled.Add(1)
		return "2001:db8::1", nil
	}

	tmp := t.TempDir()
	s := NewSync(Config{
		Enabled:             true,
		AliyunAccessKeyID:   "", // ← 空
		AliyunAccessKeySecret: "",
		Domain:              "example.com",
		RR:                  "vpn",
		LastFailedFile:      filepath.Join(tmp, "FAILED"),
		StateFile:           filepath.Join(tmp, "state"),
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	s.SetDetectV6(mockDetect)

	s.tick()

	if detectCalled.Load() != 0 {
		t.Errorf("detectV6 should NOT be called when credentials missing, got %d calls", detectCalled.Load())
	}
	if _, err := os.Stat(s.cfg.LastFailedFile); err == nil {
		t.Error("failed flag should not exist when credentials missing")
	}
}

// TestSync_Throttle 节流:连续两次 tick,第二次不调 detectV6。
func TestSync_Throttle(t *testing.T) {
	var detectCalled atomic.Int32
	mockDetect := func(iface string) (string, error) {
		detectCalled.Add(1)
		return "2001:db8::1", nil
	}

	tmp := t.TempDir()
	s := NewSync(Config{
		Enabled:             true,
		AliyunAccessKeyID:   "ak",
		AliyunAccessKeySecret: "sk",
		Domain:              "example.com",
		RR:                  "vpn",
		Throttle:            10 * time.Second, // ← 长节流
		LastFailedFile:      filepath.Join(tmp, "FAILED"),
		StateFile:           filepath.Join(tmp, "state"),
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	s.SetDetectV6(mockDetect)

	// 第一次:会探测(虽然会因为无法解析真实 API 而走 recordFailure 路径,
	// 但节流窗口外,先看探测次数)
	s.tick()
	first := detectCalled.Load()

	// 第二次:节流窗口内,不应该探测
	s.tick()
	second := detectCalled.Load()

	if first != 1 {
		t.Errorf("first tick should call detectV6 once, got %d", first)
	}
	if second != 1 {
		t.Errorf("second tick (within throttle) should NOT call detectV6, got total %d", second)
	}
}

// TestSync_StateFile state file 不存在时返回 (false, nil)。
func TestSync_StateFile_NotExist(t *testing.T) {
	tmp := t.TempDir()
	s := NewSync(Config{
		StateFile: filepath.Join(tmp, "does-not-exist"),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	enabled, err := s.readStateFile()
	if err != nil {
		t.Fatalf("readStateFile: %v", err)
	}
	if enabled {
		t.Error("non-existent state file should default to false")
	}
}

// TestSync_StateFile_Roundtrip v2-84:用 statefile.go 的 package-level helpers。
func TestSync_StateFile_Roundtrip(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")

	// 写 true + dual
	if err := writeStateFile(sf, stateRecord{Enabled: true, Family: "dual"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 读回 true
	rec, err := parseStateFile(sf, "dual")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !rec.Enabled {
		t.Error("expected enabled=true after write")
	}
	if rec.Family != "dual" {
		t.Errorf("expected family=dual, got %q", rec.Family)
	}

	// 改 false → 再读
	if err := writeStateFile(sf, stateRecord{Enabled: false, Family: "dual"}); err != nil {
		t.Fatalf("write false: %v", err)
	}
	rec, _ = parseStateFile(sf, "dual")
	if rec.Enabled {
		t.Error("expected enabled=false after second write")
	}
}

// TestSync_SetEnabled SetEnabled 后 IsEnabled 跟 state file 一致。
func TestSync_SetEnabled(t *testing.T) {
	tmp := t.TempDir()
	s := NewSync(Config{
		Enabled:  false,
		StateFile: filepath.Join(tmp, "state"),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if s.IsEnabled() {
		t.Error("initial enabled should be false")
	}

	if err := s.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !s.IsEnabled() {
		t.Error("IsEnabled should be true after SetEnabled(true)")
	}

	// 重新构造一个 sync,模拟"重启后"读取 state file
	s2 := NewSync(Config{
		StateFile: filepath.Join(tmp, "state"),
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if !s2.IsEnabled() {
		t.Error("after restart, IsEnabled should be true (state file persists)")
	}
}

// TestSync_Defaults 默认值正确。
func TestSync_Defaults(t *testing.T) {
	s := NewSync(Config{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if s.cfg.Period != 60*time.Second {
		t.Errorf("default Period = %v, want 60s", s.cfg.Period)
	}
	if s.cfg.Throttle != 60*time.Second {
		t.Errorf("default Throttle = %v, want 60s", s.cfg.Throttle)
	}
	if s.cfg.LastFailedFile != "/data/le/LAST_DDNS_FAILED" {
		t.Errorf("default LastFailedFile = %q", s.cfg.LastFailedFile)
	}
	if s.cfg.StateFile != "/etc/ikev2/ddns.conf" {
		t.Errorf("default StateFile = %q", s.cfg.StateFile)
	}
	if s.cfg.RR != "@" {
		t.Errorf("default RR = %q, want @", s.cfg.RR)
	}
}

// TestSync_LastSyncSnapshot 默认值。
func TestSync_LastSyncSnapshot(t *testing.T) {
	s := NewSync(Config{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	snap := s.LastSyncSnapshot()
	if !snap.Time.IsZero() {
		t.Errorf("default LastSync.Time should be zero, got %v", snap.Time)
	}
	if snap.Success {
		t.Error("default LastSync.Success should be false")
	}
}
