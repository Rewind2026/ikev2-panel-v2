package web

// v2.86-PR19(A15)回归测试:.sswan 配置生成。
//
// 这个文件里 Sprintf 的格式符与实参必须严格配对 —— Go 编译期不检查,
// 数量错了运行时会打出 %!s(MISSING) / %!(EXTRA ...) 之类的垃圾,
// 生成的 .sswan 直接无法被 strongSwan 解析。这类测试是唯一防线。

import (
	"strings"
	"testing"
)

// TestSSwanConfig_RenderNoFormatErrors 断言渲染结果里没有任何格式错误残留。
func TestSSwanConfig_RenderNoFormatErrors(t *testing.T) {
	got := renderSSwanTemplate("rewind", "vpn.example.com", "vpn.example.com")

	for _, bad := range []string{
		"%!s(MISSING)",
		"%!d(MISSING)",
		"%!(EXTRA",
		"%!s(",
		"%!(NOVERB",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("生成的 .sswan 含格式错误 %q —— Sprintf 的%%s 与实参数量不匹配:\n%s",
				bad, got)
		}
	}
}

// TestSSwanConfig_ProposalsNotWeakOnly 是本次修复的核心断言。
//
// 原来 proposals 只发 modp2048,与服务端 curve25519/ecp256/modp3072
// 优先的设计背道而驰 —— 每次连接都落到最弱 DH 组。
func TestSSwanConfig_ProposalsNotWeakOnly(t *testing.T) {
	got := renderSSwanTemplate("rewind", "vpn.example.com", "vpn.example.com")

	var proposalLine string
	for _, line := range strings.Split(got, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "proposals") {
			proposalLine = l
			break
		}
	}
	if proposalLine == "" {
		t.Fatal(".sswan 里找不到 proposals 行")
	}

	// 必须含强 DH 组
	for _, strong := range []string{"curve25519", "ecp256", "modp3072"} {
		if !strings.Contains(proposalLine, strong) {
			t.Errorf("proposals 缺少强 DH 组 %q —— 会让每次协商都落到弱算法:\n%s",
				strong, proposalLine)
		}
	}

	// modp2048 仍应保留作兜底(老设备)
	if !strings.Contains(proposalLine, "modp2048") {
		t.Errorf("proposals 丢了 modp2048 兜底,老设备会协商失败:\n%s", proposalLine)
	}
}

// TestSSwanConfig_SecretEmptyNotPlaceholder 断言密码位留空。
//
// 原来写 secret = "由用户输入",strongSwan 会把它当成字面密码,
// 导入后必须手动改。留空更明确。
func TestSSwanConfig_SecretEmptyNotPlaceholder(t *testing.T) {
	got := renderSSwanTemplate("rewind", "vpn.example.com", "vpn.example.com")

	if strings.Contains(got, "由用户输入") {
		t.Error(".sswan 里 secret 仍是占位文案 \"由用户输入\",strongSwan 会当字面密码")
	}
	if !strings.Contains(got, `secret = ""`) {
		t.Error(".sswan 里 secret 应留空,让用户在 app 里手填")
	}
}

// TestSSwanConfig_NoMisleadingQRComment 断言不再声称"扫码导入"。
//
// 面板的二维码内容是 /install/{token} URL,下发的是 .mobileconfig,
// Android strongSwan app 无法解析 .mobileconfig,扫码拿不到可用配置。
func TestSSwanConfig_NoMisleadingQRComment(t *testing.T) {
	got := renderSSwanTemplate("rewind", "vpn.example.com", "vpn.example.com")

	if strings.Contains(got, "扫码导入") {
		t.Error(".sswan 注释仍写 \"扫码导入配置\" —— 面板二维码下发的是 .mobileconfig," +
			"Android strongSwan app 无法导入,会误导用户")
	}
	// 应给出真正的 app 导入路径
	if !strings.Contains(got, "导入配置") {
		t.Error(".sswan 注释缺少 strongSwan app 的导入指引")
	}
}

// TestSSwanConfig_CriticalFieldsPresent 确认连接必需字段齐全。
func TestSSwanConfig_CriticalFieldsPresent(t *testing.T) {
	got := renderSSwanTemplate("rewind", "vpn.example.com", "vpn.example.com")

	required := []string{
		"auth = eap-mschapv2",
		"eap_id = rewind",
		"id = vpn.example.com",
		"remote_addrs = vpn.example.com",
		"remote_ts = 0.0.0.0/0, ::/0",
		"mobike = yes",
		"unique = replace",
		"version = 2",
	}
	for _, r := range required {
		if !strings.Contains(got, r) {
			t.Errorf(".sswan 缺少关键配置 %q", r)
		}
	}
}
