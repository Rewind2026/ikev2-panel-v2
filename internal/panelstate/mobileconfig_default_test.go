// v2.86-PR12.22:panelstate.MobileConfigDefaults store 的单元测试。
//
// 覆盖范围:
//   - Validate 复用 cert.MobileConfigOpts.Validate 的所有规则
//   - Load / Read / Write / Clear / Exists 磁盘交互
//   - ToMobileConfigOpts 转 cert.MobileConfigOpts 的语义
//   - IsZero 判定
//   - 文件 atomic write / 重启 idempotent
package panelstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/cert"
)

func boolPtrT(b bool) *bool { return &b }
func intPtrT(i int) *int    { return &i }

func TestMobileConfigDefaults_Validate(t *testing.T) {
	tests := []struct {
		name    string
		d       MobileConfigDefaults
		wantErr string
	}{
		{"zero is ok (no override)", MobileConfigDefaults{}, ""},
		{"full valid", MobileConfigDefaults{
			UserDefinedName:       "Corp VPN",
			DisconnectOnSleep:     boolPtrT(true),
			NATKeepaliveEnabled:   boolPtrT(false),
			NATKeepaliveInterval:  intPtrT(120),
			OnDemandEnabled:       boolPtrT(true),
			OnDemandProfile:       "cellular_only",
			IncludeAllNetworks:    boolPtrT(true),
			ExcludeLocalNetworks:  boolPtrT(false),
			DNSServers:            []string{"10.0.0.1", "10.0.0.2"},
			DeadPeerDetectionRate: "Low",
		}, ""},
		{"invalid nat interval (10 < 20)", MobileConfigDefaults{NATKeepaliveInterval: intPtrT(10)}, "nat_keepalive_interval"},
		{"invalid profile name", MobileConfigDefaults{OnDemandProfile: "bogus"}, "on_demand_profile"},
		{"invalid DPD rate", MobileConfigDefaults{DeadPeerDetectionRate: "Ultra"}, "dead_peer_detection_rate"},
		{"invalid DNS", MobileConfigDefaults{DNSServers: []string{"not-ip"}}, "dns_servers"},
		{"too many DNS", MobileConfigDefaults{DNSServers: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6", "7.7.7.7"}}, "dns_servers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.d.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestMobileConfigDefaults_ToMobileConfigOpts(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var d *MobileConfigDefaults
		if got := d.ToMobileConfigOpts(); got != nil {
			t.Errorf("nil receiver should return nil, got %v", got)
		}
	})

	t.Run("empty receiver returns empty opts", func(t *testing.T) {
		d := &MobileConfigDefaults{}
		got := d.ToMobileConfigOpts()
		if got == nil {
			t.Fatal("empty should return non-nil empty opts")
		}
		if got.UserDefinedName != nil {
			t.Errorf("empty UserDefinedName should be nil, got %v", *got.UserDefinedName)
		}
		if got.DNSServers != nil {
			t.Errorf("empty DNSServers should be nil, got %v", got.DNSServers)
		}
	})

	t.Run("non-empty receiver maps fields", func(t *testing.T) {
		d := &MobileConfigDefaults{
			UserDefinedName:       "Corp VPN",
			DisconnectOnSleep:     boolPtrT(true),
			NATKeepaliveInterval:  intPtrT(120),
			OnDemandProfile:       "cellular_only",
			DNSServers:            []string{"10.0.0.1"},
			DeadPeerDetectionRate: "Low",
		}
		got := d.ToMobileConfigOpts()
		if got == nil {
			t.Fatal("expected non-nil")
		}
		if got.UserDefinedName == nil || *got.UserDefinedName != "Corp VPN" {
			t.Errorf("UserDefinedName: got %v", got.UserDefinedName)
		}
		if got.DisconnectOnSleep == nil || !*got.DisconnectOnSleep {
			t.Errorf("DisconnectOnSleep: got %v", got.DisconnectOnSleep)
		}
		if got.NATKeepaliveInterval == nil || *got.NATKeepaliveInterval != 120 {
			t.Errorf("NATKeepaliveInterval: got %v", got.NATKeepaliveInterval)
		}
		if got.OnDemandProfile != cert.OnDemandProfile("cellular_only") {
			t.Errorf("OnDemandProfile: got %v", got.OnDemandProfile)
		}
		if len(got.DNSServers) != 1 || got.DNSServers[0] != "10.0.0.1" {
			t.Errorf("DNSServers: got %v", got.DNSServers)
		}
		if got.DeadPeerDetectionRate != "Low" {
			t.Errorf("DPD: got %v", got.DeadPeerDetectionRate)
		}
	})

	t.Run("DNSServers slice is copied (no alias)", func(t *testing.T) {
		d := &MobileConfigDefaults{DNSServers: []string{"1.1.1.1"}}
		got := d.ToMobileConfigOpts()
		if len(got.DNSServers) != 1 {
			t.Fatal("expected 1 DNS")
		}
		got.DNSServers[0] = "MUTATED"
		if d.DNSServers[0] == "MUTATED" {
			t.Errorf("DNSServers slice should be copied, not aliased")
		}
	})
}

func TestMobileConfigDefaults_IsZero(t *testing.T) {
	if !(&MobileConfigDefaults{}).IsZero() {
		t.Error("empty should be zero")
	}
	if !((*MobileConfigDefaults)(nil)).IsZero() {
		t.Error("nil should be zero")
	}
	d := &MobileConfigDefaults{DisconnectOnSleep: boolPtrT(false)}
	if d.IsZero() {
		t.Error("with *bool field should not be zero")
	}
}

func TestMobileConfigDefaultsStore_LoadWriteClear(t *testing.T) {
	dir := t.TempDir()
	s := NewMobileConfigDefaultsStoreWithDir(dir)

	// 1) 初始 load:文件不存在 → nil
	got, err := s.LoadMobileConfigDefaults()
	if err != nil {
		t.Fatalf("load (no file): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil when file missing, got %+v", got)
	}

	// 2) write 写入
	want := MobileConfigDefaults{
		UserDefinedName:      "Corp VPN",
		DisconnectOnSleep:    boolPtrT(true),
		NATKeepaliveInterval: intPtrT(120),
		OnDemandProfile:      "cellular_only",
		DNSServers:           []string{"10.0.0.1"},
	}
	if err := s.WriteMobileConfigDefaults(want); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 3) 文件存在 + 权限
	path := filepath.Join(dir, "mobileconfig.defaults.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() == 0 {
		t.Error("written file should not be empty")
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm: got %o, want 0600", perm)
	}

	// 4) read 缓存
	got, err = s.ReadMobileConfigDefaults()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil || got.UserDefinedName != "Corp VPN" {
		t.Errorf("read after write lost data: %+v", got)
	}
	if got.UpdatedAt == 0 {
		t.Errorf("UpdatedAt should be set after write")
	}

	// 5) 新建一个 store(模拟重启),load 从磁盘读到
	s2 := NewMobileConfigDefaultsStoreWithDir(dir)
	got, err = s2.LoadMobileConfigDefaults()
	if err != nil {
		t.Fatalf("load (after restart): %v", err)
	}
	if got == nil || got.UserDefinedName != "Corp VPN" {
		t.Errorf("restart load lost data: %+v", got)
	}
	if got.OnDemandProfile != "cellular_only" {
		t.Errorf("OnDemandProfile roundtrip: got %q", got.OnDemandProfile)
	}

	// 6) clear → 文件消失
	if err := s.ClearMobileConfigDefaults(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be deleted after clear, got err=%v", err)
	}
	got, err = s.ReadMobileConfigDefaults()
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if got != nil {
		t.Errorf("read after clear should be nil, got %+v", got)
	}
}

func TestMobileConfigDefaultsStore_Exists(t *testing.T) {
	dir := t.TempDir()
	s := NewMobileConfigDefaultsStoreWithDir(dir)
	if s.MobileConfigDefaultsExists() {
		t.Error("should not exist initially")
	}
	if err := s.WriteMobileConfigDefaults(MobileConfigDefaults{
		UserDefinedName: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if !s.MobileConfigDefaultsExists() {
		t.Error("should exist after write")
	}
}

func TestMobileConfigDefaultsStore_LoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mobileconfig.defaults.json")
	if err := os.WriteFile(path, []byte("not valid json {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewMobileConfigDefaultsStoreWithDir(dir)
	_, err := s.LoadMobileConfigDefaults()
	if err == nil {
		t.Error("expected error on corrupt JSON")
	}
}

func TestMobileConfigDefaultsStore_WriteRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	s := NewMobileConfigDefaultsStoreWithDir(dir)
	err := s.WriteMobileConfigDefaults(MobileConfigDefaults{
		OnDemandProfile: "bogus-preset",
	})
	if err == nil {
		t.Error("write with invalid profile should fail")
	}
	// 文件没创建
	if s.MobileConfigDefaultsExists() {
		t.Error("failed write should not create file")
	}
}