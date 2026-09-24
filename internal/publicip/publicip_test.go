// publicip 单元测试。
//
// 不依赖外网(避免 CI 跑挂),通过 Parser 函数和 isGlobalV4 逻辑覆盖。
// 真发包烟雾测试由 detect_smoke_test.go 提供(仅在 IKEV2_TEST_PUBLIC_IP=1 时跑)。
package publicip

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePlainIP(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"纯文本", "1.2.3.4\n", "1.2.3.4"},
		{"无尾换行", "8.8.8.8", "8.8.8.8"},
		{"带 HTML 包裹", "<body>1.1.1.1</body>", "1.1.1.1"},
		{"带 pre 包裹", "<pre>203.0.113.1</pre>", "203.0.113.1"},
		{"多个 IP 取首个", "8.8.8.8 1.1.1.1", "8.8.8.8"},
		{"空响应", "", ""},
		{"纯文本无 IP", "hello world", ""},
		{"IPv6 不该被取", "2001:db8::1", ""},
		{"非 IPv4 段不可取", "999.999.999.999", ""},
		{"前后杂字符", "ip: 8.8.8.8 ;", "8.8.8.8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePlainIP([]byte(tt.body))
			if got != tt.want {
				t.Errorf("parsePlainIP(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestParseIPIPNet(t *testing.T) {
	body := []byte(`当前 IP：1.2.3.4  来自于：中国 河北`)
	if got := parseIPIPNet(body); got != "1.2.3.4" {
		t.Errorf("parseIPIPNet = %q, want 1.2.3.4", got)
	}
	if got := parseIPIPNet([]byte("")); got != "" {
		t.Errorf("parseIPIPNet(empty) = %q, want empty", got)
	}
}

func TestParseIPCN(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"JSON 格式", `{"ip":"1.2.3.4","address":"北京"}`, "1.2.3.4"},
		{"HTML 当前 IP", `<html><body>当前 IP：</code>8.8.8.8</body></html>`, "8.8.8.8"},
		{"HTML 当前 IP 无 </code>", `<html><body>当前 IP:1.1.1.1</body></html>`, "1.1.1.1"},
		{"空响应", ``, ""},
		{"IPv6 优先用 IP4 token", `{"ip":"2001:db8::1"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIPCN([]byte(tt.body))
			if got != tt.want {
				t.Errorf("parseIPCN(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestExtractIPv4(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.2.3.4", "1.2.3.4"},
		{"8.8.8.8", "8.8.8.8"},
		{"ip: 8.8.8.8 ;", "8.8.8.8"},
		{"no ip here", ""},
		{"999.999.999.999", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := extractIPv4(tt.in)
			if got != tt.want {
				t.Errorf("extractIPv4(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsGlobalV4(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"0.0.0.0", false},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"172.15.0.1", true}, // 边界
		{"172.32.0.1", true}, // 边界
		{"192.168.1.1", false},
		{"100.64.0.1", false},
		{"100.127.255.255", false},
		{"100.63.0.1", true}, // 边界
		{"100.128.0.1", true},
		{"224.0.0.1", false},
		{"169.254.1.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ip := net.ParseIP(tt.in)
			if got := isGlobalV4(ip); got != tt.want {
				t.Errorf("isGlobalV4(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
	if isGlobalV4(nil) {
		t.Error("isGlobalV4(nil) = true, want false")
	}
}

// TestDetect_WithTestServer 用 httptest 模拟公网 IP API,验证 fallback 链 + 过滤。
func TestDetect_WithTestServer(t *testing.T) {
	// 第一家:返回 private IP (RFC1918) → 应被过滤,fallback 第二家
	privateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("192.168.1.1\n"))
	}))
	defer privateSrv.Close()

	// 第二家:返回 RFC1918 也算 private,继续 fallback
	privateSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("10.0.0.1\n"))
	}))
	defer privateSrv2.Close()

	// 第三家:返回合法公网 IP
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("8.8.8.8\n"))
	}))
	defer goodSrv.Close()

	p := &Probe{
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Endpoints: []Endpoint{
			{URL: privateSrv.URL, Parser: parsePlainIP},
			{URL: privateSrv2.URL, Parser: parsePlainIP},
			{URL: goodSrv.URL, Parser: parsePlainIP},
		},
	}

	got, err := p.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != "8.8.8.8" {
		t.Errorf("Detect = %q, want 8.8.8.8 (third endpoint after private IP filter)", got)
	}
}

// TestDetect_AllFail 所有 endpoint 都失败 → 应返回聚合错误。
func TestDetect_AllFail(t *testing.T) {
	// 404 服务
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer badSrv.Close()

	// 完全不通的端口(取个保留端口)
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadSrv.Close() // 立刻关闭 → dial 会失败

	p := &Probe{
		HTTPClient: &http.Client{Timeout: 1 * time.Second},
		Endpoints: []Endpoint{
			{URL: badSrv.URL, Parser: parsePlainIP},
			{URL: "http://127.0.0.1:1", Parser: parsePlainIP}, // reserved port
		},
	}

	_, err := p.Detect(context.Background())
	if err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
	if !strings.Contains(err.Error(), "publicip:") {
		t.Errorf("error should contain 'publicip:' prefix, got: %v", err)
	}
}

// TestDetect_EmptyEndpoints 没有 endpoint 配置 → 立即报 "no endpoints configured"。
func TestDetect_EmptyEndpoints(t *testing.T) {
	p := &Probe{Endpoints: nil}
	_, err := p.Detect(context.Background())
	if err == nil {
		t.Fatal("expected error with empty endpoints")
	}
	if !strings.Contains(err.Error(), "no endpoints") {
		t.Errorf("expected 'no endpoints' in error, got: %v", err)
	}
}