// aliyun.go 的白名单 + URL 构造测试。
//
// 不实际调 alidns API(那需要 access key + 网络),
// 只验证 recordType 入参校验 + 参数构造正确性。
package dns

import (
	"net/url"
	"strings"
	"testing"
)

func TestValidRecordType(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{RecordTypeA, true},
		{RecordTypeAAAA, true},
		{"CNAME", false},
		{"TXT", false},
		{"", false},
		{"a", false}, // 大小写敏感(阿里云 API 也要求大写)
		{"AAAA\n", false},
		{"AAAA; DROP TABLE", false},
	}
	for _, tt := range tests {
		if got := validRecordType(tt.in); got != tt.want {
			t.Errorf("validRecordType(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestFindRecordRejectsInvalidType 验证 FindRecord 在 recordType 非法时
// 直接 return error,不构造 params 也不发 HTTP 请求。
func TestFindRecordRejectsInvalidType(t *testing.T) {
	c := NewAliyunClient("test-id", "test-secret")
	// 不应该 panic 也不应该尝试拨号 — 错误立即返回
	rec, err := c.FindRecord("example.com", "vpn", "CNAME")
	if err == nil {
		t.Fatal("expected error for invalid recordType, got nil")
	}
	if rec != nil {
		t.Errorf("expected nil record, got %+v", rec)
	}
	if !strings.Contains(err.Error(), "invalid recordType") {
		t.Errorf("expected error to mention 'invalid recordType', got: %v", err)
	}
}

// TestUpdateRecordValueRejectsInvalidType 验证 UpdateRecordValue 同样白名单。
func TestUpdateRecordValueRejectsInvalidType(t *testing.T) {
	c := NewAliyunClient("test-id", "test-secret")
	err := c.UpdateRecordValue("rec-123", "vpn", "TXT", "1.2.3.4", 600)
	if err == nil {
		t.Fatal("expected error for invalid recordType, got nil")
	}
	if !strings.Contains(err.Error(), "invalid recordType") {
		t.Errorf("expected error to mention 'invalid recordType', got: %v", err)
	}
}

// TestValidRecordTypeConstants 检查常量值跟 alidns API 文档一致。
func TestValidRecordTypeConstants(t *testing.T) {
	if RecordTypeA != "A" {
		t.Errorf("RecordTypeA = %q, want %q", RecordTypeA, "A")
	}
	if RecordTypeAAAA != "AAAA" {
		t.Errorf("RecordTypeAAAA = %q, want %q", RecordTypeAAAA, "AAAA")
	}
}

// 确认 url.Values 接受我们的参数(防止我们误传 nil)。
// 这只是 sanity check;真正的 URL 编码由 call() 里的 signRequest 负责。
func TestURLValuesConstruction(t *testing.T) {
	p := url.Values{}
	p.Set("TypeKeyWord", RecordTypeA)
	if p.Get("TypeKeyWord") != "A" {
		t.Errorf("TypeKeyWord = %q, want A", p.Get("TypeKeyWord"))
	}
	p.Set("TypeKeyWord", RecordTypeAAAA)
	if p.Get("TypeKeyWord") != "AAAA" {
		t.Errorf("TypeKeyWord = %q, want AAAA", p.Get("TypeKeyWord"))
	}
}
