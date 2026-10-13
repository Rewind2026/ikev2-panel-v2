package panelstate

// v2.86-PR19(A13)回归测试:admin 层 Validate 的豁免规则。
//
// 背景:cert.MobileConfigOpts.Validate 里有
//   if OnDemandProfile == home_wifi_disconnect && OnDemandSSID == "" → error
// 但 MobileConfigDefaults **刻意没有 OnDemandSSID 字段**(SSID 是 per-network 的),
// 于是管理员在全局默认里选"家里 Wi-Fi 断开 VPN"→ 保存 100% 失败,
// 而 admin 表单里根本没有 SSID 输入框 —— 一个纯死选项。

import "testing"

func TestValidate_HomeWiFiDisconnectAllowedAtAdminLayer(t *testing.T) {
	d := MobileConfigDefaults{
		OnDemandProfile: "home_wifi_disconnect",
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate() = %v,期望 nil —— admin 层选 home_wifi_disconnect 应能保存(SSID 由 per-user 提供)", err)
	}
}

func TestValidate_ManualAllowedAtAdminLayer(t *testing.T) {
	// admin 选 manual + on_demand_enabled=1 是合法意图(全局关掉按需),
	// cert 层的交叉校验不该在这里报错。
	enabled := true
	d := MobileConfigDefaults{
		OnDemandProfile: "manual",
		OnDemandEnabled: &enabled,
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate() = %v,期望 nil —— manual + enabled 在 admin 层应放行", err)
	}
}

func TestValidate_StillRejectsBadValuesAtAdminLayer(t *testing.T) {
	// 豁免只针对上面两条,其它校验必须照常生效。
	tests := []struct {
		name string
		d    MobileConfigDefaults
	}{
		{
			name: "非法 profile",
			d:    MobileConfigDefaults{OnDemandProfile: "not_a_real_preset"},
		},
		{
			name: "NAT 间隔越界",
			d:    MobileConfigDefaults{NATKeepaliveInterval: intPtrForTest(9999)},
		},
		{
			name: "非法 DPD",
			d:    MobileConfigDefaults{DeadPeerDetectionRate: "UltraFast"},
		},
		{
			name: "非法 DNS",
			d:    MobileConfigDefaults{DNSServers: []string{"300.300.300.300"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.d.Validate(); err == nil {
				t.Errorf("Validate() = nil,期望报错 —— admin 层豁免不应放宽其它校验")
			}
		})
	}
}

func intPtrForTest(n int) *int { return &n }
