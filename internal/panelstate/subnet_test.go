// v2.86-PR13.2:SubnetConfig + Store 单元测试
package panelstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubnetConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SubnetConfig
		wantErr bool
		errSub  string // 期望错误包含的子串
	}{
		{
			name:    "标准 v4 + ULA v6",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: "fd00:5::/64"},
			wantErr: false,
		},
		{
			name:    "标准 v4 + 另一个 ULA",
			cfg:     SubnetConfig{IPv4Subnet: "172.16.5.0/24", IPv6Subnet: "fdab:cd::/64"},
			wantErr: false,
		},
		{
			name:    "v4 空失败",
			cfg:     SubnetConfig{IPv4Subnet: "", IPv6Subnet: "fd00:1::/64"},
			wantErr: true,
			errSub:  "ipv4_subnet",
		},
		{
			name:    "v6 空失败",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: ""},
			wantErr: true,
			errSub:  "ipv6_subnet",
		},
		{
			name:    "v4 非 CIDR 失败",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0", IPv6Subnet: "fd00:1::/64"},
			wantErr: true,
			errSub:  "无法解析为 CIDR",
		},
		{
			name:    "v4 不是 /24 失败",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/16", IPv6Subnet: "fd00:1::/64"},
			wantErr: true,
			errSub:  "prefix 长度必须是 /24",
		},
		{
			name:    "v4 是 IPv6 失败",
			cfg:     SubnetConfig{IPv4Subnet: "fd00::/24", IPv6Subnet: "fd00:1::/64"},
			wantErr: true,
			errSub:  "必须是 IPv4",
		},
		{
			name:    "v6 不是 /64 失败",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: "fd00:1::/48"},
			wantErr: true,
			errSub:  "prefix 长度必须是 /64",
		},
		{
			name:    "v6 是 IPv4 失败",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: "10.13.0.0/24"},
			wantErr: true,
			errSub:  "必须是 IPv6",
		},
		{
			name:    "v6 是公网段失败(2000::/3)",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: "2001:db8::/64"},
			wantErr: true,
			errSub:  "ULA",
		},
		{
			name:    "v6 是 fc00::/8 也算 ULA(fc + fd 都在 /7 ULA 段)",
			cfg:     SubnetConfig{IPv4Subnet: "10.13.0.0/24", IPv6Subnet: "fc00:1::/64"},
			wantErr: true,
			errSub:  "ULA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("Validate() err=%q, 期望包含 %q", err.Error(), tt.errSub)
			}
		})
	}
}

func TestSubnetConfigStoreLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewSubnetConfigStoreWithDir(tmpDir)

	// 1. 启动时无文件 → LoadSubnetConfig 应返回 nil, nil
	cfg, err := store.LoadSubnetConfig()
	if err != nil {
		t.Fatalf("LoadSubnetConfig 期望无文件不报错,实际 err=%v", err)
	}
	if cfg != nil {
		t.Errorf("LoadSubnetConfig 期望 nil(无文件),实际 %+v", cfg)
	}

	// 2. 不存在 → SubnetConfigExists() 应返回 false
	if store.SubnetConfigExists() {
		t.Error("SubnetConfigExists 期望 false")
	}

	// 3. WriteSubnetConfig 合法值 → 内存 + 文件都该有
	valid := SubnetConfig{
		IPv4Subnet: "10.42.0.0/24",
		IPv6Subnet: "fd00:42::/64",
	}
	if err := store.WriteSubnetConfig(valid); err != nil {
		t.Fatalf("WriteSubnetConfig 期望成功, err=%v", err)
	}
	if !store.SubnetConfigExists() {
		t.Error("SubnetConfigExists 期望 true(刚写入)")
	}

	// 4. ReadSubnetConfig 读缓存
	got, err := store.ReadSubnetConfig()
	if err != nil {
		t.Fatalf("ReadSubnetConfig err=%v", err)
	}
	if got.IPv4Subnet != valid.IPv4Subnet || got.IPv6Subnet != valid.IPv6Subnet {
		t.Errorf("ReadSubnetConfig = %+v, want %+v", got, valid)
	}
	if got.UpdatedAt == 0 {
		t.Error("UpdatedAt 应该被自动设置")
	}

	// 5. 文件权限 0600
	path := filepath.Join(tmpDir, "subnet.conf")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat err=%v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}

	// 6. Validate 失败 → WriteSubnetConfig 应报错且不写文件
	bad := SubnetConfig{
		IPv4Subnet: "not-a-cidr",
		IPv6Subnet: "fd00:1::/64",
	}
	if err := store.WriteSubnetConfig(bad); err == nil {
		t.Error("WriteSubnetConfig(非法) 应该返回 error")
	}

	// 7. 损坏 JSON → LoadSubnetConfig 应报错
	//    重新构造一个新 store(同目录但假装"重启")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupted: %v", err)
	}
	store2 := NewSubnetConfigStoreWithDir(tmpDir)
	if _, err := store2.LoadSubnetConfig(); err == nil {
		t.Error("LoadSubnetConfig 损坏 JSON 应该报错")
	}

	// 8. ClearSubnetConfig → 文件 + 缓存都清
	if err := store.ClearSubnetConfig(); err != nil {
		t.Fatalf("ClearSubnetConfig err=%v", err)
	}
	if store.SubnetConfigExists() {
		t.Error("ClearSubnetConfig 后 SubnetConfigExists 期望 false")
	}
	got2, err := store.ReadSubnetConfig()
	if err != nil {
		t.Fatalf("ReadSubnetConfig (clear 后) err=%v", err)
	}
	if got2 != nil {
		t.Errorf("ClearSubnetConfig 后 ReadSubnetConfig 期望 nil,实际 %+v", got2)
	}

	// 9. ClearSubnetConfig 文件不存在 → 不报错
	if err := store.ClearSubnetConfig(); err != nil {
		t.Errorf("重复 ClearSubnetConfig 应该不报错,实际 err=%v", err)
	}
}
