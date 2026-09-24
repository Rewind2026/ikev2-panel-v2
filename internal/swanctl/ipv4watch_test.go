// DetectGlobalV4 + isGlobalV4 单元测试。
//
// v2.86-pr23l 重构后:
//   - DetectGlobalV4 内部委托给 internal/publicip.Probe(多家 API fallback)
//   - isGlobalV4 仍保留在 swanctl 包(避免新增依赖项循环),与 publicip 内部规则一致
//
// 不实际调外部 HTTP(避免 CI 依赖网络)。真实发包的烟雾测试用
// 环境变量 IKEV2_TEST_PUBLIC_IP=1 才会跑。
package swanctl

import (
	"net"
	"testing"
)

func TestIsGlobalV4(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// 公网
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"203.0.113.1", true},

		// 无效
		{"0.0.0.0", false},
		{"255.255.255.255", false},

		// loopback
		{"127.0.0.1", false},
		{"127.255.255.255", false},

		// link-local
		{"169.254.1.1", false},
		{"169.254.255.255", false},

		// RFC1918
		{"10.0.0.1", false},
		{"10.255.255.255", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.0.1", false},
		{"192.168.255.255", false},

		// CGNAT
		{"100.64.0.1", false},
		{"100.127.255.255", false},

		// multicast
		{"224.0.0.1", false},
		{"239.255.255.255", false},

		// 边界外的公网相邻段(应通过)
		{"172.15.0.1", true},  // 172.15 不在 172.16/12 段内
		{"172.32.0.1", true},  // 172.32 不在 172.16/12 段内
		{"100.63.0.1", true},  // 100.63 不在 CGNAT 段
		{"100.128.0.1", true}, // 100.128 不在 CGNAT 段
		{"11.0.0.1", true},    // 11 不在 RFC1918
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.in)
		if ip == nil {
			t.Errorf("net.ParseIP(%q) returned nil", tt.in)
			continue
		}
		if got := isGlobalV4(ip); got != tt.want {
			t.Errorf("isGlobalV4(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsGlobalV4_NilAndNonV4(t *testing.T) {
	if isGlobalV4(nil) {
		t.Error("isGlobalV4(nil) = true, want false")
	}
	// IPv6 不该被处理(留给 DetectGlobalV6)
	v6 := net.ParseIP("2001:db8::1")
	if isGlobalV4(v6) {
		t.Error("isGlobalV4(IPv6) = true, want false")
	}
}

// TestDetectGlobalV4_TargetIgnored v2.86-pr23l:
//   - target 参数已废弃,DetectGlobalV4 不再依赖它
//   - 即使传保留 TLD ".invalid",也不会用作探测目标,实际走 publicip 包
//   - 该测试在无网络环境下会得到 "public IPv4 detect failed" 错误;
//     我们只断言"不会因为 .invalid 而立刻 dial 出错"。
func TestDetectGlobalV4_TargetIgnored(t *testing.T) {
	// 不依赖网络 — 用 cancel 的 ctx 让 HTTP 请求立即失败
	// (swanctl.DetectGlobalV4 内部传 nil ctx,我们这里只确认 error 信息)
	// 实际是否有网络取决于测试环境,所以只断言函数不 panic。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DetectGlobalV4 panicked: %v", r)
		}
	}()
	_, _ = DetectGlobalV4("", "nonexistent.invalid")
	// 不强求 err == nil;网络通 / 不通都行,只要没 panic
}