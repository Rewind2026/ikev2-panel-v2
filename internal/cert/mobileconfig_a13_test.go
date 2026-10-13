package cert

// v2.86-PR19(A13)回归测试。
//
// 覆盖这一轮修的 4 个 UI 参数缺陷,每个都对应一个用户可见的故障:
//   1. manual preset → 非法 plist,iOS 拒装
//   2. ExcludeLocalNetworks 语义/文案与 Apple 文档相反
//   3. isValidIP 过松,非法 IP 进 plist
//   4. captive_probe 的空 Domains
//
// 以及 admin 层的 Validate 豁免(在 panelstate 包测)。

import (
	"strings"
	"testing"
)

// TestValidate_ManualProfileWithEnabledTrue —— A13 交叉校验。
//
// manual 语义就是"关掉按需",与 on_demand_enabled=true 组合矛盾,
// 且历史上真的产出过非法 plist。必须拦下。
func TestValidate_ManualProfileWithEnabledTrue(t *testing.T) {
	enabled := true
	o := MobileConfigOpts{
		OnDemandProfile: OnDemandManual,
		OnDemandEnabled: &enabled,
	}
	if err := o.Validate(); err == nil {
		t.Fatal("Validate() = nil,期望报错: manual profile 不能配 on_demand_enabled=true")
	}

	// 反向:manual + enabled=false 应该合法
	disabled := false
	o2 := MobileConfigOpts{
		OnDemandProfile: OnDemandManual,
		OnDemandEnabled: &disabled,
	}
	if err := o2.Validate(); err != nil {
		t.Fatalf("Validate() = %v,期望 nil: manual + enabled=false 是合法组合", err)
	}

	// manual + 未设置 enabled(交给全局默认)也应该放行
	o3 := MobileConfigOpts{OnDemandProfile: OnDemandManual}
	if err := o3.Validate(); err != nil {
		t.Fatalf("Validate() = %v,期望 nil: manual + nil enabled 应放行", err)
	}
}

// TestRenderOnDemandRules_ManualNotEmpty 是**本次最关键**的回归测试。
//
// 原 bug:renderOnDemandRules 对 manual 返回空字符串,而模板是
//
//	{{ if .OnDemandEnabled }}<key>OnDemandEnabled</key><integer>1</integer>
//	<key>OnDemandRules</key>{{ .OnDemandRules }}{{ else }}...
//
// 于是渲染出 <key>OnDemandRules</key> 后面什么都没有 —— plist 结构非法,
// iOS 报"描述文件无效"拒绝安装。
//
// 断言:manual 也必须产出非空、以 <array> 开头的合法数组。
func TestRenderOnDemandRules_ManualNotEmpty(t *testing.T) {
	got := string(renderOnDemandRules(MobileConfigOpts{OnDemandProfile: OnDemandManual}))

	if strings.TrimSpace(got) == "" {
		t.Fatal("renderOnDemandRules(manual) 返回空字符串 —— 会产出 <key>OnDemandRules</key> 无值的非法 plist,iOS 拒装")
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "<array") {
		t.Errorf("renderOnDemandRules(manual) = %q,期望以 <array 开头", got)
	}
	// 合法写法:<array/> 或 <array></array>
	if got != "<array/>" && !strings.Contains(got, "</array>") {
		t.Errorf("renderOnDemandRules(manual) = %q,期望有闭合标签", got)
	}
}

// TestRenderMobileconfig_ManualProfileProducesValidPlist 是端到端断言。
//
// 直接渲染完整 mobileconfig,确认 OnDemandRules key 后面跟着值。
func TestRenderMobileconfig_ManualProfileProducesValidPlist(t *testing.T) {
	enabled := true
	o := DefaultMobileConfigOpts()
	o.OnDemandProfile = OnDemandManual
	o.OnDemandEnabled = &enabled

	raw, err := RenderMobileconfig("testuser", "pass123", "vpn.example.com",
		"vpn.example.com", []byte("dummy-ca"), "UTC", &o)
	if err != nil {
		t.Fatalf("RenderMobileconfig: %v", err)
	}
	out := string(raw)

	// OnDemandRules 必须带值
	idx := strings.Index(out, "<key>OnDemandRules</key>")
	if idx < 0 {
		t.Fatal("渲染结果里找不到 <key>OnDemandRules</key>")
	}
	rest := out[idx:]
	// key 后面(跳过闭合标签)必须紧跟内容,不能是另一个 <key>
	after := strings.TrimPrefix(rest, "<key>OnDemandRules</key>")
	if strings.HasPrefix(after, "<key>") {
		t.Errorf("OnDemandRules 后面紧跟另一个 key → plist 结构非法:\n%s",
			rest[:min(160, len(rest))])
	}
}

// TestIsValidIP_RejectsOutOfRange 是 A13 第三项。
//
// 原来的手写校验只查 "4 段 + 全数字",不查每段 0-255,
// 于是 300.300.300.300 / 999.999.999.999 全部通过并被写进 plist。
func TestIsValidIP_RejectsOutOfRange(t *testing.T) {
	bad := []string{
		"300.300.300.300",
		"999.999.999.999",
		"1.2.3.256",
		"256.1.1.1",
		"::::",       // 非法:冒号数量不对
		"deadbeef::", // 非法:末段缺冒号
		"",
		"1.2.3", // 3 段
		"not-an-ip",
		"1.2.3.4.5",
	}
	for _, s := range bad {
		if isValidIP(s) {
			t.Errorf("isValidIP(%q) = true,期望 false", s)
		}
	}

	good := []string{
		"1.1.1.1",
		"8.8.8.8",
		"0.0.0.0",
		"255.255.255.255",
		"2606:4700:4700::1111",
		"2001:4860:4860::8888",
		"::1",
		"fd00:1::1",
	}
	for _, s := range good {
		if !isValidIP(s) {
			t.Errorf("isValidIP(%q) = false,期望 true", s)
		}
	}
}

// TestValidate_DNSServersRejectsOutOfRange 端到端:非法 DNS 必须被 Validate 拦下。
func TestValidate_DNSServersRejectsOutOfRange(t *testing.T) {
	o := DefaultMobileConfigOpts()
	o.DNSServers = []string{"1.1.1.1", "300.300.300.300"}
	if err := o.Validate(); err == nil {
		t.Fatal("Validate() = nil,期望拦截 300.300.300.300")
	}
}

// TestRenderOnDemandRules_CaptiveProbe_NoEmptyDomain 修 A13 第四项。
//
// 原来第二个 ActionParameters 的 Domains 渲染成 <array><string></string></array>,
// Apple 要求非空域名列表。
func TestRenderOnDemandRules_CaptiveProbe_NoEmptyDomain(t *testing.T) {
	got := string(renderOnDemandRules(MobileConfigOpts{OnDemandProfile: OnDemandCaptiveProbe}))

	if strings.Contains(got, "<string></string>") {
		t.Errorf("captive_probe 规则含空 <string></string>(非法 Domains):\n%s", got)
	}
	// 通配符 "*" 应存在
	if !strings.Contains(got, "<string>*</string>") {
		t.Errorf("captive_probe 规则缺少 <string>*</string> 通配域名:\n%s", got)
	}
}

// TestRenderOnDemandRules_AllProfilesProduceNonEmpty 兜底:所有 preset 都必须
// 产出非空合法数组,防止将来再加 preset 时重蹈 manual 的覆辙。
func TestRenderOnDemandRules_AllProfilesProduceNonEmpty(t *testing.T) {
	profiles := []OnDemandProfile{
		"",
		OnDemandAlwaysConnect,
		OnDemandHomeWiFiDisconnect,
		OnDemandCellularOnly,
		OnDemandCaptiveProbe,
		OnDemandManual,
	}
	for _, p := range profiles {
		o := MobileConfigOpts{OnDemandProfile: p}
		if p == OnDemandHomeWiFiDisconnect {
			o.OnDemandSSID = "Home-5G"
		}
		got := strings.TrimSpace(string(renderOnDemandRules(o)))
		if got == "" {
			t.Errorf("profile %q 渲染出空 OnDemandRules —— 会产出非法 plist", p)
			continue
		}
		// 合法的空数组有两种写法:<array></array> 或自闭合的 <array/>
		isEmptyArray := got == "<array/>" || got == "<array></array>"
		if !strings.HasPrefix(got, "<array") {
			t.Errorf("profile %q 渲染结果不以 <array 开头: %q", p, got)
			continue
		}
		if !isEmptyArray && !strings.HasSuffix(got, "</array>") {
			t.Errorf("profile %q 渲染结果不是完整 <array>...</array>: %q", p, got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
