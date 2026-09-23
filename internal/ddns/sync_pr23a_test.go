// v2.86-pr23a 专项测试:DDNS SetConfig / TriggerNow / FetchRemote / 新 getter。
package ddns

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSync_SetConfig_Basic 验证 SetConfig 改 RR/EnableA/EnableAAAA/Period 内存 + 写盘。
func TestSync_SetConfig_Basic(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")
	s := newTestSync(t, func(c *Config) { c.StateFile = sf })

	// 1. 改全套
	if err := s.SetConfig("edge-vpn", true, false, 90, ""); err != nil {
		t.Fatalf("SetConfig err = %v", err)
	}

	// 2. 内存立即生效
	if got := s.RR(); got != "edge-vpn" {
		t.Errorf("RR() = %q, want edge-vpn", got)
	}
	if !s.EnableA() {
		t.Error("EnableA() = false, want true")
	}
	if s.EnableAAAA() {
		t.Error("EnableAAAA() = true, want false")
	}
	if got := s.PeriodSeconds(); got != 90 {
		t.Errorf("PeriodSeconds() = %d, want 90", got)
	}

	// 3. statefile 持久化
	data, err := os.ReadFile(sf)
	if err != nil {
		t.Fatalf("read statefile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "rr=edge-vpn") {
		t.Errorf("statefile missing rr=edge-vpn, got:\n%s", content)
	}
	if !strings.Contains(content, "enable_a=true") {
		t.Errorf("statefile missing enable_a=true, got:\n%s", content)
	}
	if !strings.Contains(content, "enable_aaaa=false") {
		t.Errorf("statefile missing enable_aaaa=false, got:\n%s", content)
	}
	if !strings.Contains(content, "period_seconds=90") {
		t.Errorf("statefile missing period_seconds=90, got:\n%s", content)
	}
}

// TestSync_SetConfig_PeriodClamp 验证 PeriodSeconds 越界被 clamp。
func TestSync_SetConfig_PeriodClamp(t *testing.T) {
	s := newTestSync(t)

	// < 10 → clamp 到 10
	if err := s.SetConfig("vpn", true, true, 5, ""); err != nil {
		t.Fatalf("SetConfig low: %v", err)
	}
	if got := s.PeriodSeconds(); got != 10 {
		t.Errorf("low clamp: PeriodSeconds = %d, want 10", got)
	}

	// > 3600 → clamp 到 3600
	if err := s.SetConfig("vpn", true, true, 9999, ""); err != nil {
		t.Fatalf("SetConfig high: %v", err)
	}
	if got := s.PeriodSeconds(); got != 3600 {
		t.Errorf("high clamp: PeriodSeconds = %d, want 3600", got)
	}
}

// TestSync_SetConfig_PeriodZero 传 0 不改 Period(保留原值)。
func TestSync_SetConfig_PeriodZero(t *testing.T) {
	s := newTestSync(t)
	orig := s.PeriodSeconds()
	if err := s.SetConfig("vpn", true, true, 0, ""); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if got := s.PeriodSeconds(); got != orig {
		t.Errorf("PeriodZero should not change, got %d, want %d", got, orig)
	}
}

// TestSync_TriggerNow 验证 TriggerNow 不阻塞(handler 立即返回)。
func TestSync_TriggerNow(t *testing.T) {
	s := newTestSync(t)

	// 应该立即返回不阻塞
	done := make(chan struct{})
	go func() {
		s.TriggerNow()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TriggerNow blocked (handler would hang)")
	}

	// 多次连续 TriggerNow 不阻塞(buffered=1 满了就 drop)
	for i := 0; i < 10; i++ {
		s.TriggerNow()
	}
}

// TestSync_NewSync_ReadsStatefileFields 验证 NewSync 从 statefile 读 RR/EnableA/EnableAAAA/Period。
func TestSync_NewSync_ReadsStatefileFields(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")

	// 写一份完整 statefile
	rec := stateRecord{
		Enabled:    true,
		Family:     "dual",
		RR:         "edge",
		EnableA:    true,
		EnableAAAA: false, // 显式 false — 不能用 zero value
		PeriodSec:  180,
	}
	if err := writeStateFile(sf, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestSync(t, func(c *Config) { c.StateFile = sf })

	if got := s.RR(); got != "edge" {
		t.Errorf("RR() = %q, want edge", got)
	}
	if !s.EnableA() {
		t.Error("EnableA() = false, want true")
	}
	if s.EnableAAAA() {
		t.Error("EnableAAAA() = true, want false (statefile explicitly disabled)")
	}
	if got := s.PeriodSeconds(); got != 180 {
		t.Errorf("PeriodSeconds() = %d, want 180", got)
	}
}

// TestSync_NewSync_PeriodDefaultMissing 验证 statefile 没 PeriodSec 时走 cfg.Period。
func TestSync_NewSync_PeriodDefaultMissing(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")

	// 写一份没 PeriodSec 的 statefile(只有 enabled + family)
	rec := stateRecord{Enabled: true, Family: "dual"}
	if err := writeStateFile(sf, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestSync(t, func(c *Config) {
		c.StateFile = sf
		c.Period = 75 * time.Second
	})
	if got := s.PeriodSeconds(); got != 75 {
		t.Errorf("PeriodSeconds() = %d, want 75 (cfg.Period fallback)", got)
	}
}

// TestSync_FetchRemote_NoDomain 域名未配 → 立即返回 error,不出 API。
func TestSync_FetchRemote_NoDomain(t *testing.T) {
	s := newTestSync(t, func(c *Config) {
		c.Domain = ""
		c.RR = "" // 没主域名时 RR 也清掉,避免 "vpn." 假阳性
	})
	_, err := s.FetchRemote(context.Background())
	if err == nil {
		t.Fatal("expected error when domain empty")
	}

	snap := s.RemoteSnapshot()
	if snap.Error == "" {
		t.Error("RemoteSnapshot.Error should be set")
	}
	if snap.Domain != "" {
		t.Errorf("snap.Domain = %q, want empty", snap.Domain)
	}
}

// TestSync_FetchRemote_NoCreds 凭证未配 → 立即返回 error。
func TestSync_FetchRemote_NoCreds(t *testing.T) {
	s := newTestSync(t, func(c *Config) {
		c.AliyunAccessKeyID = ""
		c.AliyunAccessKeySecret = ""
	})
	_, err := s.FetchRemote(context.Background())
	if err == nil {
		t.Fatal("expected error when creds empty")
	}
	snap := s.RemoteSnapshot()
	if !strings.Contains(snap.Error, "凭证") {
		t.Errorf("snap.Error = %q, want to mention 凭证", snap.Error)
	}
}

// TestSync_RemoteSnapshot_DefaultIsZero 初始 RemoteSnapshot 是 zero value(没人查过)。
func TestSync_RemoteSnapshot_DefaultIsZero(t *testing.T) {
	s := newTestSync(t)
	snap := s.RemoteSnapshot()
	if !snap.FetchedAt.IsZero() {
		t.Errorf("initial FetchedAt should be zero, got %v", snap.FetchedAt)
	}
	if snap.Error != "" {
		t.Errorf("initial Error should be empty, got %q", snap.Error)
	}
}

// TestSync_SetConfig_AuditPersist 验证 SetConfig 后 statefile 完整 schema。
func TestSync_SetConfig_AuditPersist(t *testing.T) {
	tmp := t.TempDir()
	sf := filepath.Join(tmp, "state")
	s := newTestSync(t, func(c *Config) { c.StateFile = sf })

	// 用 SetEnabled + SetConfig 混合
	if err := s.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := s.SetConfig("vpn", true, true, 60, ""); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	// 重新解析回 stateRecord,确认字段齐
	data, err := os.ReadFile(sf)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	parsed, err := parseStateFile(sf, "dual")
	if err != nil {
		t.Fatalf("parseStateFile: %v", err)
	}
	if parsed.Enabled {
		t.Error("expected Enabled=false")
	}
	if parsed.RR != "vpn" {
		t.Errorf("parsed.RR = %q, want vpn", parsed.RR)
	}
	if !parsed.EnableA || !parsed.EnableAAAA {
		t.Error("expected EnableA and EnableAAAA true")
	}
	if parsed.PeriodSec != 60 {
		t.Errorf("parsed.PeriodSec = %d, want 60", parsed.PeriodSec)
	}
	_ = data
}
