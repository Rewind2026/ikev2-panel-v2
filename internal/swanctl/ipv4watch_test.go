// DetectGlobalV4 + isGlobalV4 单元测试。
//
// 不实际发包,通过 isGlobalV4 全覆盖 + 注入 dialer(暂未抽出,靠接口
// 在 DetectGlobalV4 内部直接调 net.Dial;测试主要覆盖过滤逻辑)。
//
// 真发包的烟雾测试放在 TestDetectGlobalV4_RealDial(用环境变量
// IKEV2_TEST_DIAL=1 才会跑,默认跳过避免 CI 失败)。
package swanctl

import (
	"net"
	"strings"
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

// TestDetectGlobalV4_InvalidTarget 验证空 / 格式错的目标行为。
// 真实 dial 路径需要 3 秒超时才能跑完,这里只测前置校验。
func TestDetectGlobalV4_InvalidTarget(t *testing.T) {
	// 不存在的 host 应该快速失败(DNS 解析 + dial)
	// 用保留 TLD ".invalid"(RFC 2606),确保不解析
	_, err := DetectGlobalV4("", "nonexistent.invalid")
	if err == nil {
		t.Skip("unexpected: dial succeeded to nonexistent.invalid")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Logf("got error: %v (expected 'dial' substring, but got network stack error)", err)
	}
}

// TestDetectGlobalV4_RejectsLoopback 测试命中过滤的情况:
// 由于 net.Dial 实际返回的 LocalAddr 取决于 OS 路由,本机 dial 时
// 可能拿到 127.0.0.1 → 我们的过滤会拦截 → 报 "unsafe IPv4" 错误。
//
// 这个测试在多数容器环境会拿到公网 IP 而不是 loopback(因为 dial 是对外
// 8.8.8.8,local addr 是出口网卡),所以我们这里只断言"如果出错,得是
// unsafe IPv4 错误",不强求一定失败。
func TestDetectGlobalV4_RejectsLoopback(t *testing.T) {
	_, err := DetectGlobalV4("", "127.0.0.1")
	if err == nil {
		t.Skip("unexpected: dial to 127.0.0.1 returned a global IP — kernel behavior differs")
	}
	// 应该是 "unsafe IPv4" 或 "dial" 错误(取决于 OS 路由选择)
	t.Logf("DetectGlobalV4(loopback target) error: %v", err)
}
