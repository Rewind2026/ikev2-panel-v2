# Proposal v2.85-PR5 — alidns 错误码精确匹配 + HMAC-SHA1 → SHA256

> **类型**:PR 级提案(对应 v2.85 release 第 5 个 PR)
> **目标**:把 alidns 错误处理从"err.Error() 子串模糊匹配"改为"解析 JSON Code 字段 + 错误码分类",同时把签名算法从 HMAC-SHA1 升级到 HMAC-SHA256(NIST 已不推荐 SHA1,阿里云 API v3 同时支持两种)
> **关联审计**:
> - [audit-2026-09-correctness.md §Q3-01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(HIGH — alidns 错误码过宽匹配)
> - [audit-2026-09-correctness.md §Q8-01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — HMAC-SHA1 + 注释矛盾)
> **工作量**:**0.5d**(Q3-01 0.3d + Q8-01 0.2d)
> **作者**:自查

---

## 背景

Phase 1-4 审计在 alidns 客户端(`internal/dns/aliyun.go`)和 DDNS 同步器(`internal/ddns/sync.go`)中发现两个相关问题,都属于"阿里云 API 错误处理不够精细"和"签名算法落后"类:

| 症状 | 现状 | 后果 |
|------|------|------|
| 错误判断过宽 | [sync.go:555-556](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L555-L556) `strings.Contains(err.Error(), "RecordNotBelongToRAM")` 和 `strings.Contains(err.Error(), "Forbidden")` | 任何 error 字符串里包含 "Forbidden" 子串都判为 RAM 失败。但 alidns **真正**返回的 RAM 错误码是 `Forbidden.RAM` / `DomainRecordNotBelongToRAM`,中间穿插 `Forbidden.AccessKeyDisabled` / `Forbidden.NotSupportRAM` / 业务错误消息里出现 "forbidden" 英文单词等都会被误判。后果:Throttling / 配额 / 内部错误本该 retry 的,被当成 RAM 失败**立即停止** retry;真 RAM 错误本应明确报"加权限"的,日志只说 "permission denied" 没法分辨到底是凭证错还是权限错 |
| 注释与实现矛盾 | [aliyun.go:6](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L6) 注释写 "阿里云 alidns RPC v1",[aliyun.go:176](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L176) 设 `SignatureMethod=HMAC-SHA1`,但 [aliyun.go:228](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L228) 函数注释又写 "用 AccessKeySecret + "&" HMAC-SHA256" | 新人读代码一脸懵;NIST 自 2011 年起不推荐 SHA1(SHAttered 攻击);阿里云 OpenAPI v3 文档 v3.0 起**默认推荐 HMAC-SHA256**,SHA1 仅为后向兼容保留 |

**为什么先做这个 PR**:

- Q3-01 是真实可用性问题:用户配错 RAM 权限时,日志只说 "permission denied",不知道是少 `alidns:UpdateDomainRecord` 还是 AK 错了要换。错误码精确后,运维能根据日志**精确判断**该加权限还是换凭证。
- Q8-01 是"做正确的事":SHA256 跟阿里云官方 SDK 默认一致,以后接入 STS Token 时无需再改签名。
- 两件事都集中在 alidns.go,**单 PR 一起做**,review 简单,回滚也简单。

---

## 目标

1. alidns 错误处理:`call()` 返回 error 时携带**结构化的 `AliyunError` 类型**(含 `Code` + `Message` + `Classification`),而不是纯字符串 error
2. 定义 `Classify(err)` 函数,把 alidns 错误码分成 4 类:`AuthError` / `RAMError` / `ThrottlingError` / `TransientError`
3. `sync.go` 的 retry 决策用 `Classify` 而不是 `strings.Contains`
4. HMAC 算法从 SHA1 升级到 SHA256,同时修正函数注释与实际实现的一致性
5. 新增 `internal/dns/aliyun_test.go` 覆盖 SHA256 签名 + 每种错误码分类

---

## 不在范围(明确不做)

- ❌ 不引入阿里云官方 SDK(保持 stdlib 手写签名,避免几十 MB 依赖)
- ❌ 不改签名 Nonce 唯一性保证(每请求 16 字节随机,已在 [aliyun.go:269-276](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L269-L276) 实现,15 分钟内不重复已满足)
- ❌ 不改 `DescribeDomainRecords` / `UpdateDomainRecord` 两个 API 之外的 alidns API(aliyun.go 范围只这两个)
- ❌ 不改 `internal/ddns/sync.go` 的 retry 退避曲线(1s/4s/9s 不变;只换错误分类)
- ❌ 不实现 STS Token 临时凭证(那是 v3 的事,本期只升级签名算法)
- ❌ 不改 `AliyunClient` 公开 API 签名(`NewAliyunClient` / `FindRecord` / `UpdateRecordValue` 不变)
- ❌ 不改 `Record` 结构体(API 响应字段不变)

---

## 设计

### 1. 错误码分类映射

参考阿里云 [OpenAPI v3 全局错误码](https://next.api.aliyun.com/global-error-code)和 [alidns 公共错误码](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-errorcodes)(2026-09 当前版本),把对 DDNS 重要的错误码分四类:

| 分类 | 错误码(Code) | 含义 | 处理 |
|------|--------------|------|------|
| **AuthError**(凭证错,立即失败) | `InvalidAccessKeyId` / `InvalidAccessKeyId.NotFound` / `InvalidAccessKeyId.Inactive` | AccessKey ID 不存在/已停用 | 立即失败,日志 ERROR,提示用户**换凭证**,**不 retry** |
| | `IncompleteSignature` | 签名方法/参数不对(代码 bug) | 立即失败,**不 retry**(修了才能继续) |
| | `SignatureDoesNotMatch` | Secret 错了 / 自定义签名实现错 | 立即失败,**不 retry** |
| | `Forbidden.AccessKeyDisabled` | AK 已被禁用 | 立即失败,**不 retry** |
| | `SignatureNonceUsed` | 15 分钟内 nonce 重复(理论上不应发生) | 立即失败(代码 bug) |
| **RAMError**(权限错,立即失败) | `Forbidden.RAM` | RAM 用户被拒绝 | 立即失败,**不 retry**,提示**加权限策略** |
| | `DomainRecordNotBelongToRAM` | 解析记录不在该 RAM 用户授权范围内 | 立即失败,**不 retry** |
| | `NoPermission` / `AccessDenied` | 权限不足 | 立即失败,**不 retry** |
| **ThrottlingError**(限流,重试 + 退避) | `Throttling` / `Throttling.User` / `Throttling.Api` | 触发 API 限流 | **重试 3 次**,指数退避 1s/4s/9s(沿用现 retry 逻辑) |
| **TransientError**(临时错误,重试) | `InternalError` | 阿里云服务端内部错误 | **重试 3 次** |
| | `ServiceUnavailable` | 服务暂不可用 | **重试 3 次** |
| | `DomainRecordNotFound` | 解析记录已被人删 | **重试 3 次**(并发场景可能暂时看不到) |
| | 其他未识别 Code + 业务 Message | 未枚举的错误 | **重试 3 次**(保守) |

**为什么这样分类**:跟阿里云官方 SDK(Java/Go/Python)的行为一致——Auth/RAM 错误**不 retry**(修了才能继续),Throttling/InternalError **retry**。这是社区共识。

### 2. 新增 `AliyunError` 结构化错误 + `Classify()` 函数

```go
// internal/dns/errors.go(新文件)
package dns

import (
	"errors"
	"strings"
)

// ErrorClass 错误分类(用于 retry 决策)。
type ErrorClass int

const (
	// ClassUnknown 未知错误(默认 retry)。
	ClassUnknown ErrorClass = iota
	// ClassAuth 凭证错(AccessKey 失效 / 签名错)— 立即失败,不 retry。
	ClassAuth
	// ClassRAM RAM 权限错 — 立即失败,不 retry,提示加权限。
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
// 携带 Code + Message + HTTPStatus,支持 Classify() 分类。
type AliyunError struct {
	HTTPStatus int    // HTTP 状态码(0 = 无 HTTP,纯业务错误)
	Code       string // alidns 返回的 Code(如 "InvalidAccessKeyId.NotFound")
	Message    string // alidns 返回的 Message
}

func (e *AliyunError) Error() string {
	if e.HTTPStatus != 0 && e.HTTPStatus != 200 {
		return fmt.Sprintf("alidns HTTP %d: code=%s msg=%s", e.HTTPStatus, e.Code, e.Message)
	}
	return fmt.Sprintf("alidns API error: code=%s msg=%s", e.Code, e.Message)
}

// IsRetryable 是否可以重试(给 retry 循环用)。
func (e *AliyunError) IsRetryable() bool {
	c := Classify(e)
	return c == ClassThrottling || c == ClassTransient || c == ClassUnknown
}

// IsFatal 是否立即失败(给 retry 循环用)。
func (e *AliyunError) IsFatal() bool {
	c := Classify(e)
	return c == ClassAuth || c == ClassRAM
}

// Classify 把任意 error 分类为 ErrorClass enum。
//
// 规则(基于阿里云 OpenAPI v3 全局错误码):
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
		// 非 alidns 错误(网络错误 / JSON 解析失败)— 保守当 transient
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
```

### 3. `call()` 返回 `*AliyunError`(替换原来的 `fmt.Errorf`)

```go
// internal/dns/aliyun.go 改动:第 198-213 行(原 HTTP 状态检查 + JSON 解析)
if resp.StatusCode != http.StatusOK {
	// 尝试从 body 解析 Code(阿里云错误响应也是 JSON)
	var apiResp struct {
		Code, Message string
	}
	_ = json.Unmarshal(body, &apiResp)
	return &AliyunError{
		HTTPStatus: resp.StatusCode,
		Code:       apiResp.Code,
		Message:    string(body),
	}
}

// 解析响应,区分业务错误(Code != "")和成功
var apiResp struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}
if err := json.Unmarshal(body, &apiResp); err != nil {
	// JSON 解析失败 — 视为 transient(返回 *AliyunError 而非裸 error)
	return &AliyunError{
		HTTPStatus: resp.StatusCode,
		Code:       "JSONDecodeError",
		Message:    fmt.Sprintf("%v (body=%s)", err, string(body)),
	}
}
if apiResp.Code != "" {
	return &AliyunError{
		HTTPStatus: resp.StatusCode,
		Code:       apiResp.Code,
		Message:    apiResp.Message,
	}
}
```

### 4. HMAC-SHA1 → HMAC-SHA256(改 3 行 + 注释)

```go
// internal/dns/aliyun.go 改动
import (
	"crypto/hmac"
	"crypto/rand"
	// 删除 "crypto/sha1"
	"crypto/sha256" // 新增
	...
)

// 第 176 行:SignatureMethod 参数
params.Set("SignatureMethod", "HMAC-SHA256") // ← 从 HMAC-SHA1 改

// 第 222-266 行 signRequest 函数
// 注释修正(原来注释写 SHA256,实际用 SHA1 — 矛盾)
func (c *AliyunClient) signRequest(params url.Values) string {
	// 1. 排序 key(不变)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 2. canonical query string(不变)
	var pairs []string
	for _, k := range keys {
		v := url.QueryEscape(params.Get(k))
		v = strings.ReplaceAll(v, "+", "%20")
		v = strings.ReplaceAll(v, "*", "%2A")
		v = strings.ReplaceAll(v, "%7E", "~")
		pairs = append(pairs, url.QueryEscape(k)+"="+v)
	}
	canonical := strings.Join(pairs, "&")

	// 3. 构造待签字符串(不变)
	stringToSign := "POST" + "&" +
		url.QueryEscape("/") + "&" +
		url.QueryEscape(canonical)

	// 4. HMAC-SHA256(阿里云 OpenAPI v3 推荐)
	//    https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature
	mac := hmac.New(sha256.New, []byte(c.AccessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	sigBytes := mac.Sum(nil)

	// 5. base64 编码(RFC 2104 HMAC 输出编码)
	return base64.StdEncoding.EncodeToString(sigBytes)
}
```

**为什么 HMAC-SHA256 兼容**:阿里云 OpenAPI v3 文档 v3.0 起同时支持 `HMAC-SHA1` 和 `HMAC-SHA256`,服务端根据请求里的 `SignatureMethod` 参数自动选择算法。改 SHA256 后,**服务端用同样的 secret + 同样的 stringToSign + SHA256 算法校验**,100% 兼容。

### 5. `sync.go` 用 `Classify` 决策 retry

```go
// internal/ddns/sync.go 改动:第 543-565 行(原 retry 循环)

// 3. retry 3 次(指数退避)
var lastErr error
for attempt := 1; attempt <= 3; attempt++ {
	err := client.UpdateRecordValue(rec.RecordID, s.cfg.RR, recordType, currentIP, rec.TTL)
	if err == nil {
		s.cfg.Logger.Info("ddns: record updated successfully",
			"record_type", recordType,
			"old", rec.Value, "new", currentIP,
			"attempt", attempt)
		return rec.Value, nil
	}

	// v2.85-PR5:用 Classify 代替 strings.Contains
	// - AuthError/RAMError → 立即失败(凭证错/权限错,retry 没用)
	// - ThrottlingError/TransientError/Unknown → 继续 retry
	class := dns.Classify(err)
	if class == dns.ClassAuth {
		s.cfg.Logger.Error("ddns: aliyun credential invalid, NOT retrying",
			"record_type", recordType, "err", err,
			"hint", "check AccessKey ID/Secret in panel UI")
		return rec.Value, fmt.Errorf("aliyun auth error for %s update: %w", recordType, err)
	}
	if class == dns.ClassRAM {
		s.cfg.Logger.Error("ddns: aliyun RAM permission denied, NOT retrying",
			"record_type", recordType, "err", err,
			"hint", "add alidns:DescribeDomainRecords + alidns:UpdateDomainRecord permissions to your RAM user")
		return rec.Value, fmt.Errorf("aliyun RAM error for %s update: %w", recordType, err)
	}
	lastErr = err
	s.cfg.Logger.Warn("ddns: update failed, retrying",
		"record_type", recordType,
		"attempt", attempt, "err", err,
		"class", class.String())
	time.Sleep(time.Duration(attempt*attempt) * time.Second) // 1s, 4s, 9s
}
return rec.Value, fmt.Errorf("update after 3 retries: %w", lastErr)
```

**好处对比**:

| 场景 | 旧(s.Contains) | 新(Classify) |
|------|----------------|---------------|
| `InvalidAccessKeyId.NotFound` AK 失效 | err 含 "AccessKey",不含 "Forbidden",**走 retry 浪费 3 次** | ClassAuth → 立即失败 |
| `Forbidden.RAM` 缺权限 | 含 "Forbidden",**立即失败 ✓** | ClassRAM → 立即失败 |
| `Throttling.User` 限流 | 不含 "Forbidden",**走 retry ✓** | ClassThrottling → retry |
| 业务错误消息里出现 "forbidden" 单词 | 误判!立即失败 ✗ | 解析 Code,不匹配 → retry |
| `DomainRecordNotBelongToRAM` | 含 "RAM",**立即失败 ✓** | ClassRAM → 立即失败 |

### 6. 单元测试覆盖

```go
// internal/dns/aliyun_test.go(新文件,关键 case)
package dns

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSignRequest_SHA256 验证签名算法是 SHA256(防回归)。
func TestSignRequest_SHA256(t *testing.T) {
	// 固定输入 → 期望固定 SHA256 base64 输出
	c := &AliyunClient{
		AccessKeyID:     "testid",
		AccessKeySecret: "testsecret",
	}
	params := map[string][]string{
		"Action":      {"DescribeDomainRecords"},
		"DomainName":  {"example.com"},
		"Timestamp":   {"2026-09-19T12:00:00Z"},
		// 注:实测应填完整公共参数,这里只是示意
	}
	v := url.Values{}
	for k, vs := range params {
		for _, x := range vs {
			v.Add(k, x)
		}
	}
	sig := c.signRequest(v)
	// 1. 验证是 base64
	if _, err := base64.StdEncoding.DecodeString(sig); err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	// 2. 长度 = SHA256 输出 32 字节 → base64 44 字符
	if len(sig) != 44 {
		t.Fatalf("signature length = %d, want 44 (base64(SHA256))", len(sig))
	}
	// 3. 跟 Python SDK 算出来的值对比(预计算好)
	const want = "<预计算的 SHA256 base64 值>"
	if sig != want {
		t.Errorf("signature mismatch:\n  got:  %s\n  want: %s", sig, want)
	}
}

// TestClassify_AllCases 覆盖每种错误码分类。
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
		// RAM
		{"Forbidden.RAM", ClassRAM},
		{"DomainRecordNotBelongToRAM", ClassRAM},
		{"NoPermission", ClassRAM},
		{"AccessDenied", ClassRAM},
		// Throttling
		{"Throttling", ClassThrottling},
		{"Throttling.User", ClassThrottling},
		{"Throttling.Api", ClassThrottling},
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

// TestClassify_NonAliyunError 非 AliyunError(网络错误)→ Unknown。
func TestClassify_NonAliyunError(t *testing.T) {
	err := fmt.Errorf("dial tcp: connection refused")
	if got := Classify(err); got != ClassUnknown {
		t.Errorf("Classify(non-aliyun err) = %v, want ClassUnknown", got)
	}
}

// TestAliyunClient_ParseErrorResponse 模拟阿里云返回错误响应,验证 *AliyunError。
func TestAliyunClient_ParseErrorResponse(t *testing.T) {
	// mock alidns server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"Code":"InvalidAccessKeyId.NotFound","Message":"Invalid Access Key.","RequestId":"xxx"}`))
	}))
	defer srv.Close()

	c := &AliyunClient{
		AccessKeyID:     "bad",
		AccessKeySecret: "bad",
		HTTPClient:      &http.Client{},
	}
	// 把 endpoint 改成 mock srv.URL(测试时只测响应解析,不测真实签名)
	oldEndpoint := AlidnsEndpoint
	// 注:实际测试需要通过 DI 注入 endpoint,这里只是示意
	_ = oldEndpoint

	params := url.Values{}
	params.Set("Action", "DescribeDomainRecords")
	var result interface{}
	err := c.call("DescribeDomainRecords", params, &result)

	var ae *AliyunError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AliyunError, got %T: %v", err, err)
	}
	if ae.Code != "InvalidAccessKeyId.NotFound" {
		t.Errorf("Code = %q, want InvalidAccessKeyId.NotFound", ae.Code)
	}
	if Classify(err) != ClassAuth {
		t.Errorf("Classify = %v, want ClassAuth", Classify(err))
	}
}
```

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [internal/dns/aliyun.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go) | 改 `SignatureMethod=HMAC-SHA256` + 改 `sha1.New → sha256.New` + 修注释 + `call()` 返回 `*AliyunError` 替代 `fmt.Errorf` | +15 / -10 行 |
| [internal/dns/errors.go](file:///opt/ikev2-panel-v2-main/internal/dns/errors.go) | **新文件**:`AliyunError` + `ErrorClass` enum + `Classify()` 函数 | +110 行 |
| [internal/dns/aliyun_test.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun_test.go) | **新文件**:SHA256 签名测试 + `Classify` 表驱动测试 + 错误响应解析测试 | +180 行 |
| [internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go) | `upsertRecord` 内 retry 决策用 `dns.Classify` 代替 `strings.Contains` | +12 / -3 行 |

**总计**:**+317 行 / -13 行**,核心逻辑改动小,**主要是新增 test + 新增 errors.go 文件**。

---

## 测试方案

### 自动化测试

```bash
# 1. 单元测试(主要覆盖 SHA256 签名 + Classify)
go test ./internal/dns/... -v -count=1
# 期望:TestSignRequest_SHA256 PASS、TestClassify_AllCases 17 个子 case 全 PASS

# 2. DDNS sync 测试(确保 Classify 接入后行为正确)
go test ./internal/ddns/... -v -count=1
# 期望:TestUpsertRecord_AuthError, TestUpsertRecord_RAMError,
#      TestUpsertRecord_ThrottlingRetry 3 个新 case PASS

# 3. 全量测试(确保没破坏其他包)
go test ./... -count=1
```

### 新增 DDNS 测试(关键 case)

```go
// internal/ddns/sync_test.go 新增 case
func TestUpsertRecord_AuthError_NotRetrying(t *testing.T) {
	// mock alidns 返回 InvalidAccessKeyId.NotFound
	mockClient := &mockAliyunClient{
		updateErr: &dns.AliyunError{Code: "InvalidAccessKeyId.NotFound", Message: "Invalid Access Key."},
	}
	s := NewSync(Config{...})
	oldIP, err := s.upsertRecord(mockClient, "A", "1.2.3.4")
	// 期望:err != nil,且 err 含 "auth error"
	// 期望:调用 mock 的次数 == 1(没 retry)
	if mockClient.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (auth error should not retry)", mockClient.updateCalls)
	}
	if !strings.Contains(err.Error(), "auth error") {
		t.Errorf("err = %v, want auth error message", err)
	}
	_ = oldIP
}

func TestUpsertRecord_RAMError_NotRetrying(t *testing.T) {
	mockClient := &mockAliyunClient{
		updateErr: &dns.AliyunError{Code: "DomainRecordNotBelongToRAM", Message: "..."},
	}
	s := NewSync(Config{...})
	_, err := s.upsertRecord(mockClient, "A", "1.2.3.4")
	if mockClient.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (RAM error should not retry)", mockClient.updateCalls)
	}
	if !strings.Contains(err.Error(), "RAM error") {
		t.Errorf("err = %v, want RAM error message", err)
	}
}

func TestUpsertRecord_Throttling_Retries(t *testing.T) {
	mockClient := &mockAliyunClient{
		updateErr: &dns.AliyunError{Code: "Throttling.User", Message: "..."},
	}
	s := NewSync(Config{...})
	_, _ = s.upsertRecord(mockClient, "A", "1.2.3.4")
	if mockClient.updateCalls != 3 {
		t.Errorf("updateCalls = %d, want 3 (throttling should retry 3 times)", mockClient.updateCalls)
	}
}
```

### 真实 alidns 集成测试(本地有 AK 时,可选)

```bash
# 用真实 AK 跑一次 DDNS 同步,确认签名算法升级后服务端接受
IKEV2_DDNS_ENABLED=true \
IKEV2_ALIYUN_ACCESS_KEY_ID=<real-ak> \
IKEV2_ALIYUN_ACCESS_KEY_SECRET=<real-secret> \
IKEV2_DOMAIN=example.com \
IKEV2_RR=ddns-test \
go run ./cmd/ikev2-panel &
sleep 5
# 期望:日志 "record updated successfully",无 "SignatureDoesNotMatch"
```

### SHA1 → SHA256 兼容性验证

```bash
# 1. 临时把签名改回 SHA1,跑现有测试(基线)
git stash
go test ./internal/dns/... -run TestSignRequest_SHA256
# 期望:FAIL(SHA1 签名长度 = base64(20字节) = 28 字符,不是 44)

# 2. 恢复 SHA256 改动
git stash pop
go test ./internal/dns/... -run TestSignRequest_SHA256
# 期望:PASS

# 3. 跟 Python aliyun-openapi 签名对比(端到端验证)
python3 -c "
import hmac, hashlib, base64
msg = 'POST&%2F&...<同上 stringToSign>'
sig = base64.b64encode(hmac.new(b'testsecret&', msg.encode(), hashlib.sha256).digest()).decode()
print(sig)
"
# 对比 Go 算出来的值,应一致
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| SHA1 → SHA256 后服务端拒绝(签名错) | **极低** | 整个 DDNS 失效 | 阿里云 OpenAPI v3 文档明确支持两种算法,服务端根据 `SignatureMethod` 自动选;改动同时改参数和算法;集成测试覆盖真实 AK |
| 用户已有的 AK 不支持 SHA256(老 AK) | **几乎 0** | DDNS 失败 | 阿里云 SHA256 支持是平台能力,跟 AK 创建时间无关 |
| `Classify` 把真 RAM 错误归到 Unknown | 低 | 走 retry 浪费 3 次,然后返回错误 | 表驱动测试覆盖所有已知 Code;兜底 Unknown 也走 retry(原行为是 fail-fast,新行为是 retry-then-fail,不会更糟) |
| `errors.As` 链断(wrap 太多层) | 极低 | `Classify` 退化为 ClassUnknown | 在 `call()` 顶层就返回 `*AliyunError`,`sync.go` 用 `fmt.Errorf("%w", err)` wrap 一次,`errors.As` 能穿透 |
| `aliyun_test.go` 用真实 HTTP server 跑导致 CI 慢 | 低 | CI 慢 1-2 秒 | 用 `httptest.NewServer`,内存 mock,无网络 |

---

## 不破坏兼容性的承诺

- **公开 API 不变**:`AliyunClient` / `NewAliyunClient` / `FindRecord` / `UpdateRecordValue` 签名 100% 不变,调用方零改动。
- **DDNS 行为兼容**:
  - **真凭证错**:旧代码 retry 3 次后报错;新代码立即报错。**更严格**(对用户更好),不算破坏。
  - **真 RAM 错**:旧代码立即报错;新代码立即报错。**不变**。
  - **限流错**:旧代码 retry;新代码 retry。**不变**。
  - **InternalError 等**:旧代码 retry;新代码 retry。**不变**。
- **签名算法升级**:服务端完全兼容(算法是请求参数协商的),用户无感。
- **无新依赖**:`crypto/sha256` 已在 Go stdlib,不引入新包。
- **错误信息可读性**:`*AliyunError.Error()` 输出 `code=InvalidAccessKeyId.NotFound msg=Invalid Access Key.`,比原来的 `code=xxx msg=yyy` 多了 `HTTP xxx:` 前缀(只在 HTTP 非 200 时),人类可读性更好。

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 5 个,**只做**alidns 错误处理精细化 + 签名算法升级。后续 PR:

- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING
- **PR-7**:tc IPv6 限速
- **(v3 候选)** 接入 STS Token 临时凭证(本 PR 的 SHA256 升级为那时铺路)

每个 PR 独立,可单独 revert。

---

## 评审检查项(给自己)

- [x] 改动集中在 alidns.go + sync.go,跨 2 个包,**可单独 revert**
- [x] 不引入新依赖(`crypto/sha256` stdlib)
- [x] 不改任何公开 API 签名
- [x] 公开类型新增 `AliyunError` / `ErrorClass` / `Classify()`,都是**新增**,不改旧符号
- [x] 测试覆盖每种错误码分类(17 个 case)+ SHA256 签名
- [x] 错误日志 hint 信息可执行(告诉用户**怎么修**:"check AccessKey" / "add alidns:UpdateDomainRecord permissions")
- [x] 跟阿里云官方 SDK 行为一致(Auth/RAM fail-fast,Throttling/Transient retry)
- [x] SHA256 兼容性已被阿里云 OpenAPI v3 文档保证

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| `internal/dns/errors.go` 新增 `AliyunError` + `Classify` | 0.10d |
| `internal/dns/aliyun.go` `call()` 返回 `*AliyunError`(解析 Code) | 0.05d |
| `internal/dns/aliyun.go` HMAC-SHA1 → SHA256(改 3 行 + import) | 0.02d |
| `internal/dns/aliyun.go` 注释修正(`RPC v1` → `OpenAPI v3` 之类) | 0.01d |
| `internal/dns/aliyun_test.go` 新增(SHA256 测试 + Classify 表驱动) | 0.10d |
| `internal/ddns/sync.go` retry 决策改用 `Classify` | 0.05d |
| `internal/ddns/sync_test.go` 新增 3 个 case(Auth / RAM / Throttling) | 0.10d |
| 跑测试 + 集成验证 | 0.07d |
| **合计** | **0.5d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] internal/dns/errors.go 新建,定义 AliyunError + ErrorClass + Classify()
- [ ] internal/dns/aliyun.go 第 6 行注释 "RPC v1" → "OpenAPI v3"
- [ ] internal/dns/aliyun.go 删除 "crypto/sha1" import,加 "crypto/sha256"
- [ ] internal/dns/aliyun.go 第 176 行 SignatureMethod: HMAC-SHA1 → HMAC-SHA256
- [ ] internal/dns/aliyun.go 第 260 行 sha1.New → sha256.New
- [ ] internal/dns/aliyun.go 第 228 行函数注释 "HMAC-SHA256" 已正确(无需改)
- [ ] internal/dns/aliyun.go call() 函数返回 *AliyunError 替代 fmt.Errorf
- [ ] internal/dns/aliyun_test.go 新建,覆盖 SHA256 + Classify 17 case
- [ ] internal/ddns/sync.go 第 555-558 行 strings.Contains 改用 dns.Classify
- [ ] internal/ddns/sync.go 错误日志加 hint("check AccessKey" / "add permissions")
- [ ] internal/ddns/sync_test.go 加 3 个 case:Auth/RAM/Throttling
- [ ] go test ./internal/dns/... PASS
- [ ] go test ./internal/ddns/... PASS
- [ ] go test ./... PASS(全量回归)
- [ ] git commit "v2.85-PR5: precise alidns error code classification + HMAC-SHA256"
```

---

> 关联:
> - [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) §"Phase 2-4 合并 Top-10"
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10
> - 阿里云 [alidns 公共错误码](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-errorcodes)(2026-09 最新)
> - 阿里云 [OpenAPI v3 全局错误码](https://next.api.aliyun.com/global-error-code)
> - [阿里云 OpenAPI v3 签名机制](https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature)(SHA256 默认推荐)
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
