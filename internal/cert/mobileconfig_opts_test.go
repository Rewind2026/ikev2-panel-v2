// v2.86-PR12.21:每用户 mobileconfig 覆盖项的单元测试。
//
// 覆盖范围:
//   - DefaultMobileConfigOpts() 的字段值(防止默认值漂移)
//   - Validate() 所有边界条件
//   - EncodeMobileConfigOpts / DecodeMobileConfigOpts 往返 + 边界
//   - effectiveOpts 合并逻辑
//   - renderOnDemandRules 5 个 preset
//   - RenderMobileconfig 接受 overlay 后,XML 实际反映用户选择
package cert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefaultMobileConfigOpts(t *testing.T) {
	def := DefaultMobileConfigOpts()
	// 必须等于上一版 hardcoded 的值,防止默认漂移
	if def.UserDefinedName != nil {
		t.Errorf("UserDefinedName default should be nil (Render 处 fallback), got %v", *def.UserDefinedName)
	}
	if def.DisconnectOnSleep == nil || *def.DisconnectOnSleep != false {
		t.Errorf("DisconnectOnSleep default should be false (保活), got %v", def.DisconnectOnSleep)
	}
	if def.NATKeepaliveEnabled == nil || !*def.NATKeepaliveEnabled {
		t.Errorf("NATKeepaliveEnabled default should be true")
	}
	if def.NATKeepaliveInterval == nil || *def.NATKeepaliveInterval != 60 {
		t.Errorf("NATKeepaliveInterval default should be 60")
	}
	if def.OnDemandEnabled == nil || !*def.OnDemandEnabled {
		t.Errorf("OnDemandEnabled default should be true")
	}
	if def.OnDemandProfile != OnDemandAlwaysConnect {
		t.Errorf("OnDemandProfile default should be %q, got %q", OnDemandAlwaysConnect, def.OnDemandProfile)
	}
	if def.IncludeAllNetworks == nil || !*def.IncludeAllNetworks {
		t.Errorf("IncludeAllNetworks default should be true")
	}
	if def.ExcludeLocalNetworks == nil || !*def.ExcludeLocalNetworks {
		t.Errorf("ExcludeLocalNetworks default should be true")
	}
	if len(def.DNSServers) != 4 {
		t.Errorf("DNSServers default should have 4 entries, got %d", len(def.DNSServers))
	}
	// v2.86-PR12.22 audit: AuthPasswordRetries 字段 Apple 文档未列,已移除。
	// 默认值不再需要测试。
	if def.DeadPeerDetectionRate != "Medium" {
		t.Errorf("DeadPeerDetectionRate default should be Medium, got %q", def.DeadPeerDetectionRate)
	}
}

func TestMobileConfigOpts_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    MobileConfigOpts
		wantErr string // 子串;空 = 期望通过
	}{
		{"all default", DefaultMobileConfigOpts(), ""},
		{"NATKeepaliveInterval too small", MobileConfigOpts{NATKeepaliveInterval: intPtr(19)}, "nat_keepalive_interval"},
		{"NATKeepaliveInterval too big", MobileConfigOpts{NATKeepaliveInterval: intPtr(601)}, "nat_keepalive_interval"},
		{"NATKeepaliveInterval 20 ok", MobileConfigOpts{NATKeepaliveInterval: intPtr(20)}, ""},
		{"NATKeepaliveInterval 600 ok", MobileConfigOpts{NATKeepaliveInterval: intPtr(600)}, ""},
		{"OnDemandProfile invalid", MobileConfigOpts{OnDemandProfile: "bogus"}, "on_demand_profile"},
		{"OnDemandProfile home_wifi no SSID", MobileConfigOpts{OnDemandProfile: OnDemandHomeWiFiDisconnect}, "ondemand_ssid"},
		{"OnDemandProfile home_wifi with SSID ok", MobileConfigOpts{OnDemandProfile: OnDemandHomeWiFiDisconnect, OnDemandSSID: "Home"}, ""},
		{"OnDemandSSID too long", MobileConfigOpts{OnDemandProfile: OnDemandHomeWiFiDisconnect, OnDemandSSID: strings.Repeat("a", 33)}, "ondemand_ssid"},
		{"DNSServers too many", MobileConfigOpts{DNSServers: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6", "7.7.7.7"}}, "dns_servers"},
		{"DNSServers bad IP", MobileConfigOpts{DNSServers: []string{"not-an-ip"}}, "非法 IP"},
		{"DNSServers IPv6 ok", MobileConfigOpts{DNSServers: []string{"2606:4700:4700::1111"}}, ""},
		{"UserDefinedName too long", MobileConfigOpts{UserDefinedName: strPtr(strings.Repeat("a", 65))}, "user_defined_name"},
		{"UserDefinedName with <", MobileConfigOpts{UserDefinedName: strPtr("a<b")}, "特殊字符"},
		{"UserDefinedName with &", MobileConfigOpts{UserDefinedName: strPtr("a&b")}, "特殊字符"},
		{"UserDefinedName plain ok", MobileConfigOpts{UserDefinedName: strPtr("Home VPN")}, ""},
		{"DeadPeerDetectionRate invalid", MobileConfigOpts{DeadPeerDetectionRate: "Ultra"}, "dead_peer_detection_rate"},
		{"DeadPeerDetectionRate High ok", MobileConfigOpts{DeadPeerDetectionRate: "High"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
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

func TestMobileConfigOpts_EncodeDecode(t *testing.T) {
	t.Run("zero value -> empty string", func(t *testing.T) {
		got, err := EncodeMobileConfigOpts(&MobileConfigOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("zero value should encode to empty, got %q", got)
		}
	})
	t.Run("nil pointer -> empty string", func(t *testing.T) {
		got, err := EncodeMobileConfigOpts(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("nil pointer should encode to empty, got %q", got)
		}
	})
	t.Run("non-zero roundtrip", func(t *testing.T) {
		in := MobileConfigOpts{
			UserDefinedName:     strPtr("Home"),
			DisconnectOnSleep:   boolPtr(true),
			NATKeepaliveEnabled: boolPtr(false),
			NATKeepaliveInterval: intPtr(120),
			OnDemandEnabled:     boolPtr(true),
			OnDemandProfile:     OnDemandHomeWiFiDisconnect,
			OnDemandSSID:        "Home-5G",
			IncludeAllNetworks:  boolPtr(true),
			ExcludeLocalNetworks: boolPtr(false),
			DNSServers:          []string{"1.1.1.1", "8.8.8.8"},
			DeadPeerDetectionRate: "Low",
		}
		encoded, err := EncodeMobileConfigOpts(&in)
		if err != nil {
			t.Fatal(err)
		}
		if encoded == "" {
			t.Fatal("non-zero should not encode to empty")
		}
		// 验证 JSON 合法
		var raw map[string]any
		if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
			t.Fatalf("encoded not valid JSON: %v", err)
		}
		// Decode 回去
		decoded, err := DecodeMobileConfigOpts(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded == nil {
			t.Fatal("decoded nil")
		}
		// 检查几个关键字段
		if decoded.UserDefinedName == nil || *decoded.UserDefinedName != "Home" {
			t.Errorf("UserDefinedName roundtrip lost: %v", decoded.UserDefinedName)
		}
		if decoded.DisconnectOnSleep == nil || !*decoded.DisconnectOnSleep {
			t.Errorf("DisconnectOnSleep roundtrip lost: %v", decoded.DisconnectOnSleep)
		}
		if decoded.OnDemandSSID != "Home-5G" {
			t.Errorf("OnDemandSSID roundtrip lost: %q", decoded.OnDemandSSID)
		}
		if len(decoded.DNSServers) != 2 {
			t.Errorf("DNSServers roundtrip lost: %v", decoded.DNSServers)
		}
	})
	t.Run("Decode empty -> nil", func(t *testing.T) {
		decoded, err := DecodeMobileConfigOpts("")
		if err != nil {
			t.Fatal(err)
		}
		if decoded != nil {
			t.Errorf("empty should decode to nil, got %v", decoded)
		}
	})
	t.Run("Decode whitespace -> nil", func(t *testing.T) {
		decoded, err := DecodeMobileConfigOpts("   \t  ")
		if err != nil {
			t.Fatal(err)
		}
		if decoded != nil {
			t.Errorf("whitespace should decode to nil, got %v", decoded)
		}
	})
	t.Run("Decode corrupt -> error", func(t *testing.T) {
		_, err := DecodeMobileConfigOpts("not json {{{")
		if err == nil {
			t.Fatal("expected error on corrupt JSON")
		}
	})
}

func TestEffectiveOpts(t *testing.T) {
	// nil overlay -> 完全等于 default
	def := DefaultMobileConfigOpts()
	eff := effectiveOpts(nil)
	if eff.UserDefinedName != def.UserDefinedName {
		t.Errorf("nil overlay UserDefinedName: got %v want %v", eff.UserDefinedName, def.UserDefinedName)
	}
	if eff.DisconnectOnSleep == def.DisconnectOnSleep {
		t.Errorf("DisconnectOnSleep pointer should not alias")
	}
	if eff.DisconnectOnSleep == nil || def.DisconnectOnSleep == nil {
		t.Errorf("DisconnectOnSleep pointers should be non-nil")
	} else if *eff.DisconnectOnSleep != *def.DisconnectOnSleep {
		t.Errorf("DisconnectOnSleep value: got %v want %v", *eff.DisconnectOnSleep, *def.DisconnectOnSleep)
	}
	if eff.OnDemandProfile != def.OnDemandProfile {
		t.Errorf("OnDemandProfile: got %v want %v", eff.OnDemandProfile, def.OnDemandProfile)
	}

	// 部分覆盖:只设 DisconnectOnSleep,其它用 default
	overlay := &MobileConfigOpts{DisconnectOnSleep: boolPtr(true)}
	eff = effectiveOpts(overlay)
	if eff.DisconnectOnSleep == nil || !*eff.DisconnectOnSleep {
		t.Errorf("DisconnectOnSleep overlay should win, got %v", eff.DisconnectOnSleep)
	}
	// UserDefinedName 应该是 default(nil)
	if eff.UserDefinedName != nil {
		t.Errorf("UserDefinedName should fallback to default nil, got %v", *eff.UserDefinedName)
	}
	// OnDemandProfile 应该是 default
	if eff.OnDemandProfile != def.OnDemandProfile {
		t.Errorf("OnDemandProfile should fallback to default")
	}
}

func TestRenderOnDemandRules(t *testing.T) {
	tests := []struct {
		name    string
		profile OnDemandProfile
		ssid    string
		must    []string // 必须包含的子串
		mustNot []string // 不能包含的子串
	}{
		{"always default", "", "", []string{`<array>`, `Action`, `Connect`}, nil},
		{"home_wifi_disconnect with SSID", OnDemandHomeWiFiDisconnect, "Home", []string{`SSIDMatch`, `Home`, `Disconnect`}, nil},
		{"cellular_only", OnDemandCellularOnly, "", []string{`Cellular`, `WiFi`, `Connect`, `Disconnect`, `Ignore`}, nil},
		{"captive_probe", OnDemandCaptiveProbe, "", []string{`EvaluateConnection`, `apple.com`, `captive.apple.com`, `NeverConnect`, `ConnectIfNeeded`}, nil},
		{"manual", OnDemandManual, "", []string{}, []string{`<array>`}}, // manual 不输出 array
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff := effectiveOpts(&MobileConfigOpts{
				OnDemandProfile: tt.profile,
				OnDemandSSID:    tt.ssid,
			})
			got := renderOnDemandRules(eff)
			s := string(got)
			for _, m := range tt.must {
				if !strings.Contains(s, m) {
					t.Errorf("expected substring %q in %q", m, s)
				}
			}
			for _, m := range tt.mustNot {
				if strings.Contains(s, m) {
					t.Errorf("did not expect substring %q in %q", m, s)
				}
			}
		})
	}
}

func TestRenderMobileconfig_AcceptsOverlay(t *testing.T) {
	// 准备一个 opts 让字段全部偏离默认
	overlay := &MobileConfigOpts{
		UserDefinedName:       strPtr("My Custom VPN"),
		DisconnectOnSleep:     boolPtr(true),
		NATKeepaliveEnabled:   boolPtr(false),
		NATKeepaliveInterval:  intPtr(120),
		OnDemandEnabled:       boolPtr(true),
		OnDemandProfile:       OnDemandHomeWiFiDisconnect,
		OnDemandSSID:          "Home-5G",
		IncludeAllNetworks:    boolPtr(false),
		ExcludeLocalNetworks:  boolPtr(false),
		DNSServers:            []string{"9.9.9.11", "149.112.112.11"},
		DeadPeerDetectionRate: "Low",
	}
	out, err := RenderMobileconfig("alice", "pw", "vpn.example.com", "vpn.example.com", nil, "UTC", overlay)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)

	// 关键断言:overlay 里的字段必须反映到 XML
	checks := map[string]string{
		"UserDefinedName":       "My Custom VPN",
		"NATKeepaliveInterval":  `<key>NATKeepAliveInterval</key><integer>120</integer>`,
		"DisconnectOnSleep=true": `<key>DisconnectOnSleep</key><true/>`,
		"NATKeepaliveEnabled=false": `<key>NATKeepAliveOffloadEnable</key><false/>`,
		"OnDemandRules with SSID": `<key>SSIDMatch</key><array><string>Home-5G</string>`,
		"IncludeAllNetworks=false": `<key>IncludeAllNetworks</key><false/>`,
		"DNS 9.9.9.11":         `<string>9.9.9.11</string>`,
		"DNS 149.112.112.11":   `<string>149.112.112.11</string>`,
		"DPD Low":               `<key>DeadPeerDetectionRate</key><string>Low</string>`,
	}
	for name, sub := range checks {
		if !strings.Contains(s, sub) {
			t.Errorf("%s: expected substring %q in output", name, sub)
		}
	}

	// 反向断言:overlay 不影响的字段必须保留 default 行为
	// OnDemandEnabled=true 时必须包含 <key>OnDemandEnabled</key><integer>1</integer>
	if !strings.Contains(s, `<key>OnDemandEnabled</key><integer>1</integer>`) {
		t.Errorf("OnDemandEnabled should be 1")
	}
}

func TestRenderMobileconfig_ManualOnDemand_NoRules(t *testing.T) {
	overlay := &MobileConfigOpts{
		OnDemandEnabled: boolPtr(false),
	}
	out, err := RenderMobileconfig("alice", "pw", "vpn.example.com", "vpn.example.com", nil, "UTC", overlay)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// OnDemandEnabled=false → 不应输出 OnDemandRules
	if strings.Contains(s, "<key>OnDemandRules</key>") {
		t.Errorf("OnDemandEnabled=false should not emit OnDemandRules, got: %s", s)
	}
	// 应输出 OnDemandEnabled=0
	if !strings.Contains(s, `<key>OnDemandEnabled</key><integer>0</integer>`) {
		t.Errorf("OnDemandEnabled should be 0")
	}
}

func TestRenderMobileconfig_NilOpts_UsesDefault(t *testing.T) {
	out, err := RenderMobileconfig("alice", "pw", "vpn.example.com", "vpn.example.com", nil, "UTC", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// 默认值
	if !strings.Contains(s, `<key>UserDefinedName</key><string>IKEv2 VPN</string>`) {
		t.Errorf("nil opts should use default UserDefinedName, got: %s", s)
	}
	if !strings.Contains(s, `<key>NATKeepAliveInterval</key><integer>60</integer>`) {
		t.Errorf("nil opts should use default NATKeepAliveInterval=60")
	}
	if !strings.Contains(s, `<array><dict><key>Action</key><string>Connect</string></dict></array>`) {
		t.Errorf("nil opts should use default OnDemandRules (always)")
	}
}

// helpers

func strPtr(s string) *string { return &s }

// TestEffectiveMobileConfigOpts_ThreeLayer v2.86-PR12.22:
// 测试三层合并 builtin → admin → overlay 的优先级。
//
// 规则:
//   - admin / overlay 字段 nil → 跳过该层,用下一层
//   - 字段已设 → 覆盖上一层的值
//   - overlay 优先级最高
func TestEffectiveMobileConfigOpts_ThreeLayer(t *testing.T) {
	t.Run("both nil → builtin", func(t *testing.T) {
		eff := EffectiveMobileConfigOpts(nil, nil)
		base := BaseMobileConfigDefaults()
		if eff.NATKeepaliveInterval == nil || *eff.NATKeepaliveInterval != *base.NATKeepaliveInterval {
			t.Errorf("nil+nil should use builtin, got %v", eff.NATKeepaliveInterval)
		}
		if eff.OnDemandProfile != base.OnDemandProfile {
			t.Errorf("nil+nil should use builtin profile, got %v", eff.OnDemandProfile)
		}
	})

	t.Run("admin only (overlay=nil)", func(t *testing.T) {
		// admin 把 NATKeepaliveInterval 改成 120,其它不动
		admin := &MobileConfigOpts{NATKeepaliveInterval: intPtr(120)}
		eff := EffectiveMobileConfigOpts(admin, nil)
		if eff.NATKeepaliveInterval == nil || *eff.NATKeepaliveInterval != 120 {
			t.Errorf("admin NATKeepaliveInterval should win, got %v", eff.NATKeepaliveInterval)
		}
		// DisconnectOnSleep 用 builtin = false
		if eff.DisconnectOnSleep == nil || *eff.DisconnectOnSleep != false {
			t.Errorf("admin only → builtin DisconnectOnSleep=false, got %v", eff.DisconnectOnSleep)
		}
	})

	t.Run("overlay only (admin=nil) → builtin + overlay", func(t *testing.T) {
		overlay := &MobileConfigOpts{DisconnectOnSleep: boolPtr(true)}
		eff := EffectiveMobileConfigOpts(nil, overlay)
		if eff.DisconnectOnSleep == nil || !*eff.DisconnectOnSleep {
			t.Errorf("overlay DisconnectOnSleep=true should win, got %v", eff.DisconnectOnSleep)
		}
		// NATKeepaliveInterval 用 builtin = 60
		if eff.NATKeepaliveInterval == nil || *eff.NATKeepaliveInterval != 60 {
			t.Errorf("overlay only → builtin NATKeepaliveInterval=60, got %v", eff.NATKeepaliveInterval)
		}
	})

	t.Run("overlay beats admin", func(t *testing.T) {
		admin := &MobileConfigOpts{DisconnectOnSleep: boolPtr(true)}
		overlay := &MobileConfigOpts{DisconnectOnSleep: boolPtr(false)}
		eff := EffectiveMobileConfigOpts(admin, overlay)
		if eff.DisconnectOnSleep == nil || *eff.DisconnectOnSleep {
			t.Errorf("overlay false should beat admin true, got %v", *eff.DisconnectOnSleep)
		}
	})

	t.Run("admin supplies NATKeepalive, overlay supplies SSID (independent)", func(t *testing.T) {
		admin := &MobileConfigOpts{NATKeepaliveInterval: intPtr(120)}
		overlay := &MobileConfigOpts{OnDemandSSID: "Home-5G"}
		eff := EffectiveMobileConfigOpts(admin, overlay)
		if eff.NATKeepaliveInterval == nil || *eff.NATKeepaliveInterval != 120 {
			t.Errorf("admin NATKeepaliveInterval=120, got %v", eff.NATKeepaliveInterval)
		}
		if eff.OnDemandSSID != "Home-5G" {
			t.Errorf("overlay SSID should pass through, got %q", eff.OnDemandSSID)
		}
	})

	t.Run("DNSServers slice copied (no alias)", func(t *testing.T) {
		admin := &MobileConfigOpts{DNSServers: []string{"10.0.0.1"}}
		eff := EffectiveMobileConfigOpts(admin, nil)
		if len(eff.DNSServers) != 1 {
			t.Fatal("expected 1 DNS")
		}
		eff.DNSServers[0] = "MUTATED"
		if admin.DNSServers[0] == "MUTATED" {
			t.Errorf("DNS slice should be copied")
		}
	})

	t.Run("overlay.DNSServers overrides admin.DNSServers", func(t *testing.T) {
		admin := &MobileConfigOpts{DNSServers: []string{"10.0.0.1"}}
		overlay := &MobileConfigOpts{DNSServers: []string{"8.8.8.8"}}
		eff := EffectiveMobileConfigOpts(admin, overlay)
		if len(eff.DNSServers) != 1 || eff.DNSServers[0] != "8.8.8.8" {
			t.Errorf("overlay DNS should win, got %v", eff.DNSServers)
		}
	})

	t.Run("admin.DNSServers used when overlay.DNSServers empty", func(t *testing.T) {
		admin := &MobileConfigOpts{DNSServers: []string{"10.0.0.1", "10.0.0.2"}}
		overlay := &MobileConfigOpts{} // 空 overlay
		eff := EffectiveMobileConfigOpts(admin, overlay)
		if len(eff.DNSServers) != 2 || eff.DNSServers[0] != "10.0.0.1" {
			t.Errorf("admin DNS should pass through when overlay empty, got %v", eff.DNSServers)
		}
	})
}