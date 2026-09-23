// v2.86-pr23a:DDNSConfig + Store 单元测试
package panelstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDDNSConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DDNSConfig
		wantErr bool
		errSub  string
	}{
		{
			name:    "默认空配置 OK(全用代码默认值)",
			cfg:     DDNSConfig{},
			wantErr: false,
		},
		{
			name:    "合法 RR 'vpn'",
			cfg:     DDNSConfig{RR: "vpn", EnableA: true, EnableAAAA: true},
			wantErr: false,
		},
		{
			name:    "合法 RR '@' (主域本身)",
			cfg:     DDNSConfig{RR: "@", EnableA: true},
			wantErr: false,
		},
		{
			name:    "合法 RR 含数字/下划线",
			cfg:     DDNSConfig{RR: "vpn_2-edge"},
			wantErr: false,
		},
		{
			name:    "非法 RR 含 . (那是完整域名)",
			cfg:     DDNSConfig{RR: "vpn.example.com"},
			wantErr: true,
			errSub:  "rr 不合法",
		},
		{
			name:    "非法 RR 含 /",
			cfg:     DDNSConfig{RR: "vpn/sub"},
			wantErr: true,
			errSub:  "rr 不合法",
		},
		{
			name:    "非法 RR 含空格",
			cfg:     DDNSConfig{RR: "vpn host"},
			wantErr: true,
			errSub:  "rr 不合法",
		},
		{
			name:    "非法 RR 过长(>63)",
			cfg:     DDNSConfig{RR: strings.Repeat("a", 64)},
			wantErr: true,
			errSub:  "rr 不合法",
		},
		{
			name:    "Period 0 OK(走默认 60)",
			cfg:     DDNSConfig{PeriodSeconds: 0},
			wantErr: false,
		},
		{
			name:    "Period 10 OK(下限)",
			cfg:     DDNSConfig{PeriodSeconds: 10},
			wantErr: false,
		},
		{
			name:    "Period 3600 OK(上限)",
			cfg:     DDNSConfig{PeriodSeconds: 3600},
			wantErr: false,
		},
		{
			name:    "Period 9 失败(过低会撞 alidns 限流)",
			cfg:     DDNSConfig{PeriodSeconds: 9},
			wantErr: true,
			errSub:  "period_seconds",
		},
		{
			name:    "Period 3601 失败(过高也无意义)",
			cfg:     DDNSConfig{PeriodSeconds: 3601},
			wantErr: true,
			errSub:  "period_seconds",
		},
		{
			name:    "A + AAAA 全关也是合法(用户主动停止同步)",
			cfg:     DDNSConfig{EnableA: false, EnableAAAA: false},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errSub)
			}
		})
	}
}

// TestDDNSConfigStore_RoundTrip 走完整 Read/Write 链路,验证 atomic + 缓存。
func TestDDNSConfigStore_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	st := NewDDNSConfigStoreWithDir(tmp)

	// 1. 初始:不存在
	if st.DDNSConfigExists() {
		t.Fatal("expected file not exists initially")
	}
	cfg, err := st.ReadDDNSConfig()
	if err != nil {
		t.Fatalf("initial ReadDDNSConfig err = %v", err)
	}
	if cfg != nil {
		t.Fatalf("initial cfg should be nil, got %+v", cfg)
	}

	// 2. 写入
	want := DDNSConfig{
		RR:            "vpn",
		EnableA:       true,
		EnableAAAA:    false,
		PeriodSeconds: 90,
	}
	if err := st.WriteDDNSConfig(want); err != nil {
		t.Fatalf("WriteDDNSConfig err = %v", err)
	}
	if !st.DDNSConfigExists() {
		t.Fatal("file should exist after write")
	}

	// 3. 读回来(走缓存)
	got, err := st.ReadDDNSConfig()
	if err != nil {
		t.Fatalf("ReadDDNSConfig err = %v", err)
	}
	if got == nil {
		t.Fatal("got nil after write")
	}
	if got.RR != want.RR || got.EnableA != want.EnableA || got.EnableAAAA != want.EnableAAAA || got.PeriodSeconds != want.PeriodSeconds {
		t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", got, want)
	}
	if got.UpdatedAt == 0 {
		t.Error("UpdatedAt should be set by WriteDDNSConfig")
	}

	// 4. 文件路径正确(<dir>/ddns.conf)
	wantPath := filepath.Join(tmp, "ddns.conf")
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected file at %s: %v", wantPath, err)
	}
}

// TestDDNSConfigStore_Clear 验证 ClearDDNSConfig 清文件 + 清缓存。
func TestDDNSConfigStore_Clear(t *testing.T) {
	tmp := t.TempDir()
	st := NewDDNSConfigStoreWithDir(tmp)

	if err := st.WriteDDNSConfig(DDNSConfig{RR: "vpn", EnableA: true}); err != nil {
		t.Fatalf("WriteDDNSConfig err = %v", err)
	}
	if !st.DDNSConfigExists() {
		t.Fatal("file should exist after write")
	}

	if err := st.ClearDDNSConfig(); err != nil {
		t.Fatalf("ClearDDNSConfig err = %v", err)
	}
	if st.DDNSConfigExists() {
		t.Fatal("file should not exist after clear")
	}
	cfg, err := st.ReadDDNSConfig()
	if err != nil {
		t.Fatalf("ReadDDNSConfig after clear err = %v", err)
	}
	if cfg != nil {
		t.Errorf("cfg after clear should be nil, got %+v", cfg)
	}

	// 二次 Clear 不报错
	if err := st.ClearDDNSConfig(); err != nil {
		t.Errorf("second ClearDDNSConfig err = %v", err)
	}
}

// TestDDNSConfigStore_LoadFromDisk 验证 LoadDDNSConfig 从磁盘读(冷启动)。
func TestDDNSConfigStore_LoadFromDisk(t *testing.T) {
	tmp := t.TempDir()

	// 1. 直接落盘 JSON(模拟"上次运行写过")
	jsonData := []byte(`{
  "rr": "edge-vpn",
  "enable_a": true,
  "enable_aaaa": true,
  "period_seconds": 120,
  "updated_at": 1700000000
}`)
	if err := os.WriteFile(filepath.Join(tmp, "ddns.conf"), jsonData, 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// 2. 新 store,从磁盘读
	st := NewDDNSConfigStoreWithDir(tmp)
	cfg, err := st.LoadDDNSConfig()
	if err != nil {
		t.Fatalf("LoadDDNSConfig err = %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg from disk, got nil")
	}
	if cfg.RR != "edge-vpn" || cfg.PeriodSeconds != 120 || !cfg.EnableA || !cfg.EnableAAAA {
		t.Errorf("disk read mismatch: %+v", cfg)
	}
}

// TestDDNSConfigStore_RejectsInvalidFile 校验失败时返回 error 不静默。
func TestDDNSConfigStore_RejectsInvalidFile(t *testing.T) {
	tmp := t.TempDir()

	// 写入非法 RR 的 JSON
	bad := []byte(`{"rr": "vpn.example.com", "enable_a": true}`)
	if err := os.WriteFile(filepath.Join(tmp, "ddns.conf"), bad, 0o600); err != nil {
		t.Fatalf("seed bad file: %v", err)
	}

	st := NewDDNSConfigStoreWithDir(tmp)
	if _, err := st.LoadDDNSConfig(); err == nil {
		t.Fatal("expected error loading invalid RR file")
	}
}

// TestDDNSConfigStore_FilePermissions 验证文件权限 0600。
func TestDDNSConfigStore_FilePermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping permission check on Windows")
	}
	tmp := t.TempDir()
	st := NewDDNSConfigStoreWithDir(tmp)
	if err := st.WriteDDNSConfig(DDNSConfig{RR: "vpn", EnableA: true}); err != nil {
		t.Fatalf("WriteDDNSConfig err = %v", err)
	}

	info, err := os.Stat(filepath.Join(tmp, "ddns.conf"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("expected 0600, got %#o", mode)
	}
}

// TestDDNSConfigStore_UpdatedAt 验证 UpdatedAt 字段每次 write 都更新。
func TestDDNSConfigStore_UpdatedAt(t *testing.T) {
	tmp := t.TempDir()
	st := NewDDNSConfigStoreWithDir(tmp)

	if err := st.WriteDDNSConfig(DDNSConfig{RR: "vpn"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, _ := st.ReadDDNSConfig()
	if first.UpdatedAt == 0 {
		t.Fatal("first UpdatedAt should be set")
	}

	// 第二次 write(间隔不查,只验 > first)
	if err := st.WriteDDNSConfig(DDNSConfig{RR: "edge"}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second, _ := st.ReadDDNSConfig()
	if second.UpdatedAt < first.UpdatedAt {
		t.Errorf("UpdatedAt should be monotonic, first=%d second=%d", first.UpdatedAt, second.UpdatedAt)
	}
}
