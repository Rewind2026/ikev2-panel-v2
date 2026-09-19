// v2.85-PR5 (Q3-01):结构化的 alidns 错误 + 错误码分类。
//
// 背景:旧代码用 err.Error() 子串模糊匹配区分"凭证错 / 权限错 / 限流",
// 误判率高(例如业务消息里出现 "forbidden" 英文单词会被判为 RAM 错)。
// 新设计:alidns API 错误响应携带 Code 字段(JSON),直接按 Code 分类。
//
// 公开 API:
//   - AliyunError: 结构化错误,携带 HTTPStatus + Code + Message
//   - ErrorClass: 错误分类 enum(Auth / RAM / Throttling / Transient / Unknown)
//   - Classify(err): 把任意 error 分类为 ErrorClass
//
// 用法(retry 循环):
//
//	if class := dns.Classify(err); class == dns.ClassAuth {
//	    return fmt.Errorf("aliyun auth error: %w", err)  // 立即失败
//	}
//	if class == dns.ClassRAM {
//	    return fmt.Errorf("aliyun RAM error: %w", err)   // 立即失败,提示加权限
//	}
//	// ClassThrottling / ClassTransient / ClassUnknown → 继续 retry
package dns

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorClass 错误分类(用于 retry 决策)。
//
// 跟阿里云官方 SDK(Java/Go/Python)行为一致:
//   - Auth / RAM → fail-fast(凭证错 / 权限错,retry 没用)
//   - Throttling / Transient / Unknown → retry + 指数退避
type ErrorClass int

const (
	// ClassUnknown 未知错误(默认 retry)— 保守策略。
	ClassUnknown ErrorClass = iota
	// ClassAuth 凭证错(AccessKey 失效 / 签名错)— 立即失败,不 retry。
	ClassAuth
	// ClassRAM RAM 权限错 — 立即失败,不 retry,提示用户加权限策略。
	ClassRAM
	// ClassThrottling 限流 — retry + 指数退避。
	ClassThrottling
	// ClassTransient 临时错误(InternalError / 临时网络)— retry。
	ClassTransient
)

// String 用于日志输出。
func (c ErrorClass) String() string {
	switch c {
	case ClassAuth:
		return "AuthError"
	case ClassRAM:
		return "RAMError"
	case ClassThrottling:
		return "ThrottlingError"
	case ClassTransient:
		return "TransientError"
	default:
		return "UnknownError"
	}
}

// AliyunError alidns API 返回的结构化错误。
//
// 替代原来的 fmt.Errorf("alidns API error: code=%s msg=%s"),
// 携带 Code + Message + HTTPStatus,支持 Classify() 分类 + errors.As 提取。
type AliyunError struct {
	HTTPStatus int    // HTTP 状态码(0 = 无 HTTP,纯业务错误)
	Code       string // alidns 返回的 Code(如 "InvalidAccessKeyId.NotFound")
	Message    string // alidns 返回的 Message(可能含原始 body)
}

func (e *AliyunError) Error() string {
	if e.HTTPStatus != 0 && e.HTTPStatus != 200 {
		return fmt.Sprintf("alidns HTTP %d: code=%s msg=%s", e.HTTPStatus, e.Code, e.Message)
	}
	return fmt.Sprintf("alidns API error: code=%s msg=%s", e.Code, e.Message)
}

// IsRetryable 是否可以重试(给 retry 循环用)。
//
// Auth / RAM → false(fail-fast)
// Throttling / Transient / Unknown → true(retry)
func (e *AliyunError) IsRetryable() bool {
	c := Classify(e)
	return c == ClassThrottling || c == ClassTransient || c == ClassUnknown
}

// IsFatal 是否立即失败(给 retry 循环用)。
//
// Auth / RAM → true(fail-fast)
// 其他 → false
func (e *AliyunError) IsFatal() bool {
	c := Classify(e)
	return c == ClassAuth || c == ClassRAM
}

// Classify 把任意 error 分类为 ErrorClass enum。
//
// 规则(基于阿里云 OpenAPI v3 全局错误码 https://next.api.aliyun.com/global-error-code +
//
//	alidns 公共错误码 https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-errorcodes):
//   - AuthError:    InvalidAccessKeyId*, IncompleteSignature, SignatureDoesNotMatch,
//                   Forbidden.AccessKeyDisabled, SignatureNonceUsed
//   - RAMError:     Forbidden.RAM, DomainRecordNotBelongToRAM, NoPermission, AccessDenied
//   - Throttling:   Throttling*, Throttling.User, Throttling.Api
//   - Transient:    InternalError, ServiceUnavailable, DomainRecordNotFound
//   - Unknown:      其他(保守 retry)
//
// 非 AliyunError(error wrap 链里没有 AliyunError)→ ClassUnknown(由 caller 决定)。
func Classify(err error) ErrorClass {
	var ae *AliyunError
	if !errors.As(err, &ae) {
		// 非 alidns 错误(网络错误 / JSON 解析失败)— 保守当 unknown
		return ClassUnknown
	}
	switch ae.Code {
	// Auth
	case "InvalidAccessKeyId", "InvalidAccessKeyId.NotFound", "InvalidAccessKeyId.Inactive",
		"IncompleteSignature", "SignatureDoesNotMatch",
		"Forbidden.AccessKeyDisabled", "SignatureNonceUsed":
		return ClassAuth
	// RAM
	case "Forbidden.RAM", "DomainRecordNotBelongToRAM",
		"NoPermission", "AccessDenied":
		return ClassRAM
	// Throttling
	case "Throttling", "Throttling.User", "Throttling.Api":
		return ClassThrottling
	// Transient
	case "InternalError", "ServiceUnavailable", "DomainRecordNotFound":
		return ClassTransient
	default:
		// 子串匹配兜底(防御未枚举 Code)
		if strings.HasPrefix(ae.Code, "Throttling.") {
			return ClassThrottling
		}
		if strings.HasPrefix(ae.Code, "Forbidden.") {
			// 其他 Forbidden.* 默认按 RAM 处理(常见:Forbidden.NotSupportRAM)
			return ClassRAM
		}
		return ClassUnknown
	}
}