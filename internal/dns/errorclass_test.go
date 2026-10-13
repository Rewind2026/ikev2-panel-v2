// v2.86-PR19(P1-22):错误分类单测 —— DomainRecordNotFound 的新归属。
//
// 这些断言直接锁住 dns 包的公开契约:if哪天有人把 DomainRecordNotFound
// 塞回 ClassTransient,这里会立刻红。
package dns

import "testing"

// TestClassifyDomainRecordNotFound 记录不存在必须是独立的 NotFound 类。
func TestClassifyDomainRecordNotFound(t *testing.T) {
	err := &AliyunError{HTTPStatus: 200, Code: "DomainRecordNotFound", Message: "gone"}
	got := Classify(err)
	if got != ClassNotFound {
		t.Errorf("Classify(DomainRecordNotFound) = %v (%d), want ClassNotFound (%d)",
			got, int(got), int(ClassNotFound))
	}
	if got == ClassTransient {
		t.Error("DomainRecordNotFound must NOT be ClassTransient (that caused 14s of useless backoff)")
	}
}

// TestNotFoundRetrySemantics NotFound 是终态:不可重试、视为 fatal。
func TestNotFoundRetrySemantics(t *testing.T) {
	err := &AliyunError{HTTPStatus: 200, Code: "DomainRecordNotFound", Message: "gone"}
	if err.IsRetryable() {
		t.Error("IsRetryable() = true, want false (retrying a deleted record can never succeed)")
	}
	if !err.IsFatal() {
		t.Error("IsFatal() = false, want true")
	}
	if s := ClassNotFound.String(); s != "NotFoundError" {
		t.Errorf("ClassNotFound.String() = %q, want NotFoundError", s)
	}
}

// TestTransientErrorsStillRetry 反向保护:真正的临时错误分类不能被这次改动影响。
func TestTransientErrorsStillRetry(t *testing.T) {
	for _, code := range []string{"InternalError", "ServiceUnavailable"} {
		err := &AliyunError{HTTPStatus: 200, Code: code, Message: "try later"}
		if got := Classify(err); got != ClassTransient {
			t.Errorf("Classify(%s) = %v, want ClassTransient", code, got)
		}
		if !err.IsRetryable() {
			t.Errorf("IsRetryable(%s) = false, want true", code)
		}
		if err.IsFatal() {
			t.Errorf("IsFatal(%s) = true, want false", code)
		}
	}
}

// TestAuthAndRAMStillFatal 前两类 fail-fast 语义不能被回归。
func TestAuthAndRAMStillFatal(t *testing.T) {
	for _, code := range []string{"InvalidAccessKeyId", "SignatureDoesNotMatch"} {
		err := &AliyunError{HTTPStatus: 200, Code: code, Message: "bad key"}
		if got := Classify(err); got != ClassAuth {
			t.Errorf("Classify(%s) = %v, want ClassAuth", code, got)
		}
		if !err.IsFatal() || err.IsRetryable() {
			t.Errorf("%s: want fatal and non-retryable", code)
		}
	}
	for _, code := range []string{"Forbidden.RAM", "DomainRecordNotBelongToRAM", "NoPermission"} {
		err := &AliyunError{HTTPStatus: 200, Code: code, Message: "denied"}
		if got := Classify(err); got != ClassRAM {
			t.Errorf("Classify(%s) = %v, want ClassRAM", code, got)
		}
		if !err.IsFatal() || err.IsRetryable() {
			t.Errorf("%s: want fatal and non-retryable", code)
		}
	}
}

// TestThrottlingStillRetryable 限流仍然重试(且不属于 fatal)。
func TestThrottlingStillRetryable(t *testing.T) {
	for _, code := range []string{"Throttling", "Throttling.User", "Throttling.Api"} {
		err := &AliyunError{HTTPStatus: 200, Code: code, Message: "slow down"}
		if got := Classify(err); got != ClassThrottling {
			t.Errorf("Classify(%s) = %v, want ClassThrottling", code, got)
		}
		if !err.IsRetryable() || err.IsFatal() {
			t.Errorf("%s: want retryable and non-fatal", code)
		}
	}
}

// TestUnknownStillRetryable 未知错误保持保守重试(现有行为不变)。
func TestUnknownStillRetryable(t *testing.T) {
	for _, code := range []string{"SomeRandomError", ""} {
		err := &AliyunError{HTTPStatus: 200, Code: code, Message: "?"}
		if got := Classify(err); got != ClassUnknown {
			t.Errorf("Classify(%q) = %v, want ClassUnknown", code, got)
		}
		if !err.IsRetryable() {
			t.Errorf("Classify(%q).IsRetryable() = false, want true", code)
		}
	}
}
