// v2.85-PR5 测试覆盖:
//   1. SHA256 签名(防 SHA1 回归)
//   2. Classify() 每种错误码分类(表驱动,18 个 case)
//   3. Classify() 对非 AliyunError → Unknown
//   4. Classify() 穿透 fmt.Errorf("%w") wrap
//   5. AliyunError.Error() 字符串格式
//   6. IsRetryable / IsFatal 行为
//   7. ErrorClass.String() 输出
package dns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

// TestSignRequest_SHA256 验证签名算法是 SHA256(防回归)。
//
// 关键不变量:
//   - SHA256 输出 32 字节 → base64 编码后 44 字符
//   - 同一个 (params, secret) 输入 → 每次签名**完全一致**(算法是确定性的)
func TestSignRequest_SHA256(t *testing.T) {
	c := &AliyunClient{
		AccessKeyID:     "testid",
		AccessKeySecret: "testsecret",
	}

	// 固定输入(只放 2 个公共参数,call() 会补上其余 8 个,
	// 但这里只测 signRequest 算法本身,跳过公共参数注入)。
	v := url.Values{}
	v.Set("Action", "DescribeDomainRecords")
	v.Set("Format", "JSON")

	sig := c.signRequest(v)

	// 1. 验证是 base64
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	// 2. SHA256 输出 32 字节
	if len(decoded) != 32 {
		t.Errorf("signature decoded length = %d, want 32 (SHA256 output)", len(decoded))
	}
	if len(sig) != 44 {
		t.Errorf("signature length = %d, want 44 (base64(SHA256))", len(sig))
	}
	// 3. 确定性:同一个输入再算一次应完全一致
	sig2 := c.signRequest(v)
	if sig != sig2 {
		t.Errorf("signature not deterministic:\n  first:  %s\n  second: %s", sig, sig2)
	}
}

// TestSignRequest_SHA256_KnownVector 用 Go 独立算的 reference HMAC-SHA256 交叉验证。
//
// stringToSign 是 "POST&%2F&" + urlencoded(canonical),其中 canonical 是
// 按 key 排序后的 "k1=v1&k2=v2"。对单参数 Action=DescribeDomainRecords + Format=JSON:
//
//	canonical = "Action=DescribeDomainRecords&Format=JSON"
//	stringToSign = "POST&%2F&Action%3DDescribeDomainRecords%26Format%3DJSON"
//
// 期望 HMAC-SHA256(key="testsecret&", msg=...)base64 值。
func TestSignRequest_SHA256_KnownVector(t *testing.T) {
	c := &AliyunClient{
		AccessKeyID:     "testid",
		AccessKeySecret: "testsecret",
	}

	v := url.Values{}
	v.Set("Action", "DescribeDomainRecords")
	v.Set("Format", "JSON")

	got := c.signRequest(v)

	// 用同样的 (key, stringToSign) 独立算 reference
	// (这里构造的 stringToSign 必须跟 c.signRequest 内部完全一致)
	// signRequest 内部:
	//   pairs[i] = url.QueryEscape(k) + "=" + escape(v)
	//   canonical = join(pairs, "&")
	//   stringToSign = "POST&" + url.QueryEscape("/") + "&" + url.QueryEscape(canonical)
	// 由于我们的输入无特殊字符,url.QueryEscape(canonical) == canonical = "Action=DescribeDomainRecords&Format=JSON"
	const stringToSign = "POST&%2F&Action%3DDescribeDomainRecords%26Format%3DJSON"
	mac := hmac.New(sha256.New, []byte("testsecret&"))
	mac.Write([]byte(stringToSign))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("signature mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestClassify_AllCases 表驱动覆盖每种错误码分类。
func TestClassify_AllCases(t *testing.T) {
	tests := []struct {
		code string
		want ErrorClass
	}{
		// Auth
		{"InvalidAccessKeyId", ClassAuth},
		{"InvalidAccessKeyId.NotFound", ClassAuth},
		{"InvalidAccessKeyId.Inactive", ClassAuth},
		{"IncompleteSignature", ClassAuth},
		{"SignatureDoesNotMatch", ClassAuth},
		{"Forbidden.AccessKeyDisabled", ClassAuth},
		{"SignatureNonceUsed", ClassAuth},
		// RAM
		{"Forbidden.RAM", ClassRAM},
		{"DomainRecordNotBelongToRAM", ClassRAM},
		{"NoPermission", ClassRAM},
		{"AccessDenied", ClassRAM},
		// Throttling
		{"Throttling", ClassThrottling},
		{"Throttling.User", ClassThrottling},
		{"Throttling.Api", ClassThrottling},
		// 防御子串匹配兜底
		{"Throttling.Other", ClassThrottling},
		{"Forbidden.NotSupportRAM", ClassRAM},
		// Transient
		{"InternalError", ClassTransient},
		{"ServiceUnavailable", ClassTransient},
		{"DomainRecordNotFound", ClassTransient},
		// Unknown
		{"SomeRandomError", ClassUnknown},
		{"", ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := &AliyunError{Code: tt.code}
			got := Classify(err)
			if got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestClassify_NonAliyunError 非 AliyunError(网络错误 / 字符串 err)→ Unknown。
func TestClassify_NonAliyunError(t *testing.T) {
	tests := []error{
		fmt.Errorf("dial tcp: connection refused"),
		errors.New("context deadline exceeded"),
	}
	for i, err := range tests {
		got := Classify(err)
		if got != ClassUnknown {
			t.Errorf("test %d: Classify(non-aliyun err) = %v, want ClassUnknown", i, got)
		}
	}
}

// TestClassify_Nil 不应 panic(虽然 production 不会传 nil,这里是防御性回归)。
func TestClassify_Nil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Classify(nil) panicked: %v", r)
		}
	}()
	if got := Classify(nil); got != ClassUnknown {
		t.Errorf("Classify(nil) = %v, want ClassUnknown", got)
	}
}

// TestClassify_WrappedAliyunError 验证 errors.As 能穿透 fmt.Errorf("%w", ...) wrap。
func TestClassify_WrappedAliyunError(t *testing.T) {
	inner := &AliyunError{Code: "Forbidden.RAM", Message: "denied"}
	wrapped := fmt.Errorf("UpdateDomainRecord: %w", inner)
	if got := Classify(wrapped); got != ClassRAM {
		t.Errorf("Classify(wrapped Forbidden.RAM) = %v, want ClassRAM", got)
	}

	// 再 wrap 一层
	doubleWrapped := fmt.Errorf("ddns upsert: %w", wrapped)
	if got := Classify(doubleWrapped); got != ClassRAM {
		t.Errorf("Classify(double-wrapped Forbidden.RAM) = %v, want ClassRAM", got)
	}
}

// TestAliyunError_Error 验证错误字符串格式。
func TestAliyunError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  AliyunError
		want string
	}{
		{
			name: "业务错误(HTTP 200)",
			err:  AliyunError{HTTPStatus: 200, Code: "InvalidAccessKeyId.NotFound", Message: "Invalid Access Key."},
			want: "alidns API error: code=InvalidAccessKeyId.NotFound msg=Invalid Access Key.",
		},
		{
			name: "HTTP 4xx",
			err:  AliyunError{HTTPStatus: 403, Code: "Forbidden.RAM", Message: "denied"},
			want: "alidns HTTP 403: code=Forbidden.RAM msg=denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAliyunError_IsRetryable_IsFatal 验证便利方法。
func TestAliyunError_IsRetryable_IsFatal(t *testing.T) {
	tests := []struct {
		code      string
		retryable bool
		fatal     bool
	}{
		{"InvalidAccessKeyId.NotFound", false, true},
		{"Forbidden.RAM", false, true},
		{"DomainRecordNotBelongToRAM", false, true},
		{"Throttling.User", true, false},
		{"Throttling.Api", true, false},
		{"InternalError", true, false},
		{"ServiceUnavailable", true, false},
		{"DomainRecordNotFound", true, false},
		{"SomeRandom", true, false}, // Unknown → retryable
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := &AliyunError{Code: tt.code}
			if got := err.IsRetryable(); got != tt.retryable {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.retryable)
			}
			if got := err.IsFatal(); got != tt.fatal {
				t.Errorf("IsFatal() = %v, want %v", got, tt.fatal)
			}
		})
	}
}

// TestErrorClass_String 验证 ErrorClass.String() 输出。
func TestErrorClass_String(t *testing.T) {
	tests := []struct {
		c    ErrorClass
		want string
	}{
		{ClassAuth, "AuthError"},
		{ClassRAM, "RAMError"},
		{ClassThrottling, "ThrottlingError"},
		{ClassTransient, "TransientError"},
		{ClassUnknown, "UnknownError"},
		{ErrorClass(99), "UnknownError"}, // 越界 → UnknownError
	}
	for _, tt := range tests {
		if got := tt.c.String(); got != tt.want {
			t.Errorf("ErrorClass(%d).String() = %q, want %q", tt.c, got, tt.want)
		}
	}
}