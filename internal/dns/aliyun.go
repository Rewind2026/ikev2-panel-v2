// 阿里云 DNS (alidns) API 客户端 - 仅实现 DDNS 同步所需的最小集。
//
// 设计:仅封装 DescribeDomainRecords + UpdateDomainRecord 两个 API,
// 用于 A / AAAA 记录同步(v2-82 仅 AAAA,v2-84 新增 A)。
// 不引入阿里云完整 SDK(避免几十 MB 依赖),
// 用 stdlib crypto/hmac + crypto/sha256 手写签名(阿里云 OpenAPI v3 默认推荐 HMAC-SHA256)。
//
// v2.85-PR5 (Q8-01):签名算法从 HMAC-SHA1 升级到 HMAC-SHA256(NIST 已不推荐 SHA1,
// 阿里云 OpenAPI v3 文档默认推荐 SHA256)。
//
// API 文档:
//   - https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-describedomainrecords
//   - https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-updatedomainrecord
//   - https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature
package dns

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// AlidnsAPIVersion API 版本(2026-09 仍为 2015-01-09,稳定)
	AlidnsAPIVersion = "2015-01-09"

	// RecordTypeA A 记录(IPv4)。v2-84 新增。
	RecordTypeA = "A"

	// RecordTypeAAAA AAAA 记录(IPv6)。v2-82 引入。
	RecordTypeAAAA = "AAAA"
)

// AlidnsEndpoint alidns API endpoint (固定,所有 region 同 endpoint)。
//
// v2.85-PR5:从 const 改成 var + mutex,允许测试用 setAlidnsEndpoint 改成 mock server URL。
// production 代码永远不需要 set;读路径(GetEndpoint)只读,无锁。
var (
	AlidnsEndpoint = "https://alidns.aliyuncs.com/" //nolint:gochecknoglobals // 测试用 setter 覆盖

	endpointMu sync.RWMutex
)

// SetAlidnsEndpoint 临时覆盖 AlidnsEndpoint(测试用,生产环境不需要)。
//
// 用法(在 *_test.go 里):
//
//	dns.SetAlidnsEndpoint(srv.URL)
//	t.Cleanup(func() { dns.SetAlidnsEndpoint("https://alidns.aliyuncs.com/") })
func SetAlidnsEndpoint(url string) {
	endpointMu.Lock()
	AlidnsEndpoint = url
	endpointMu.Unlock()
}

// GetAlidnsEndpoint 读取当前 endpoint(call() 内部用,带读锁)。
func GetAlidnsEndpoint() string {
	endpointMu.RLock()
	defer endpointMu.RUnlock()
	return AlidnsEndpoint
}

// validRecordType 判断 recordType 是否为支持的解析记录类型。
//
// 当前只支持 A 和 AAAA(v2-84 dual DDNS 用)。
// 白名单原因:防止任意字符串传入 alidns API(防御性编程 +
// 行为可预测;不是 oracle 防护,因 aliyun API 公开可调)。
func validRecordType(t string) bool {
	return t == RecordTypeA || t == RecordTypeAAAA
}

// AliyunClient alidns API 客户端。
//
// 凭证:AccessKey ID + AccessKey Secret,RAM 控制台创建。
// 最小权限策略(只读 + 单域名写入):
//
//	{
//	  "Version": "1",
//	  "Statement": [{
//	    "Effect": "Allow",
//	    "Action": [
//	      "alidns:DescribeDomainRecords",
//	      "alidns:UpdateDomainRecord"
//	    ],
//	    "Resource": "acs:alidns:*:*:domain/example.com"
//	  }]
//	}
type AliyunClient struct {
	AccessKeyID     string
	AccessKeySecret string
	HTTPClient      *http.Client
}

// NewAliyunClient 构造客户端。
func NewAliyunClient(accessKeyID, accessKeySecret string) *AliyunClient {
	return &AliyunClient{
		AccessKeyID:     accessKeyID,
		AccessKeySecret: accessKeySecret,
		HTTPClient:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Record 解析记录(简化版,只取我们需要的字段)。
type Record struct {
	RecordID string `json:"RecordId"` // 解析记录 ID,更新时必传
	RR       string `json:"RR"`       // 主机记录,如 "vpn"
	Type     string `json:"Type"`     // A / AAAA / CNAME ...
	Value    string `json:"Value"`    // 记录值,如 "2001:db8::1"
	TTL      int    `json:"TTL"`      // 缓存时间(秒)
	Status   string `json:"Status"`   // Enable / Disable
}

// FindRecord 查找指定 (DomainName, RR, recordType) 的解析记录。
//
// v2-84 改造:从 FindAAAARecord 改为通用 FindRecord,recordType 参数化
// (支持 A / AAAA)。白名单校验在内部完成,非法值直接 return error 不调 API。
//
// 返回:
//   - 找到 1 条 → 返回 *Record
//   - 找到 0 条 → 返回 nil, nil (不是错误 - 用户可能还没建记录)
//   - 找到多条 → 返回 nil, fmt.Errorf("multiple %s records found")
//   - API 错误 → 返回 nil, err
//   - recordType 非法 → 返回 nil, error(不调 API)
func (c *AliyunClient) FindRecord(domainName, rr, recordType string) (*Record, error) {
	if !validRecordType(recordType) {
		return nil, fmt.Errorf("invalid recordType %q (supported: %s, %s)",
			recordType, RecordTypeA, RecordTypeAAAA)
	}
	params := url.Values{}
	params.Set("DomainName", domainName)
	params.Set("RRKeyWord", rr)
	params.Set("TypeKeyWord", recordType) // 精确匹配
	params.Set("SearchMode", "EXACT")

	var resp struct {
		DomainRecords struct {
			Record []Record `json:"Record"`
		} `json:"DomainRecords"`
	}
	if err := c.call("DescribeDomainRecords", params, &resp); err != nil {
		return nil, fmt.Errorf("DescribeDomainRecords: %w", err)
	}

	matches := []Record{}
	for _, r := range resp.DomainRecords.Record {
		if r.RR == rr && strings.EqualFold(r.Type, recordType) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("multiple %s records found for RR=%q (%d records), please clean up manually",
			recordType, rr, len(matches))
	}
}

// UpdateRecordValue 更新指定 RecordId 的 Value,其他字段(RR/TTL/Type/Line)保持不变。
//
// v2-84 改造:recordType 参数化(支持 A / AAAA)。
// 白名单校验在内部完成,非法值直接 return error 不调 API。
func (c *AliyunClient) UpdateRecordValue(recordID, rr, recordType, value string, ttl int) error {
	if !validRecordType(recordType) {
		return fmt.Errorf("invalid recordType %q (supported: %s, %s)",
			recordType, RecordTypeA, RecordTypeAAAA)
	}
	params := url.Values{}
	params.Set("RecordId", recordID)
	params.Set("RR", rr)
	params.Set("Type", recordType)
	params.Set("Value", value)
	if ttl <= 0 {
		ttl = 600 // 默认 10 分钟
	}
	params.Set("TTL", fmt.Sprintf("%d", ttl))

	var resp struct {
		RecordID string `json:"RecordId"`
	}
	if err := c.call("UpdateDomainRecord", params, &resp); err != nil {
		return fmt.Errorf("UpdateDomainRecord: %w", err)
	}
	return nil
}

// call 通用 alidns API 调用,负责签名 + HTTP POST。
func (c *AliyunClient) call(action string, params url.Values, result interface{}) error {
	// 公共参数(签名 v3 必需)
	params.Set("AccessKeyId", c.AccessKeyID)
	params.Set("Action", action)
	params.Set("Format", "JSON")
	params.Set("RegionId", "cn-hangzhou") // alidns 不分 region,固定值
	params.Set("SignatureMethod", "HMAC-SHA256")
	params.Set("SignatureNonce", randomNonce())
	params.Set("SignatureVersion", "1.0")
	params.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	params.Set("Version", AlidnsAPIVersion)

	// 计算签名(参见 https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature)
	signature := c.signRequest(params)
	params.Set("Signature", signature)

	// POST form-encoded
	resp, err := c.HTTPClient.PostForm(GetAlidnsEndpoint(), params)
	if err != nil {
		return fmt.Errorf("POST %s: %w", GetAlidnsEndpoint(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 尝试从 body 解析 Code(阿里云错误响应也是 JSON,即使 HTTP 4xx/5xx)
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
		Code    string          `json:"Code"`
		Message string          `json:"Message"`
		Body    json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		// v2.85-PR5 (Q3-01):JSON 解析失败也返回 *AliyunError,便于 Classify 分类。
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

	// 业务成功,把整个 body 反序列化到 result
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("decode result: %w (body=%s)", err, string(body))
	}
	return nil
}

// signRequest 计算阿里云 API v3 签名。
//
// 签名算法:
//   1. 排序所有参数(除 Signature 外)
//   2. 拼 "key1=value1&key2=value2..."
//   3. 加前后缀: "GET&%2F&<urlencoded-canonical-string>"
//   4. 用 AccessKeySecret + "&" HMAC-SHA256
//   5. base64 编码
//
// 文档: https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature
func (c *AliyunClient) signRequest(params url.Values) string {
	// 1. 排序 key
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 2. canonical query string
	var pairs []string
	for _, k := range keys {
		// 注意:阿里云签名要求特殊字符按 RFC 3986 编码
		// url.QueryEscape 跟 RFC 3986 略有差异(空格变 +),但阿里云
		// 文档示例里空格用的是 %20,所以这里手动替换。
		v := url.QueryEscape(params.Get(k))
		v = strings.ReplaceAll(v, "+", "%20")
		v = strings.ReplaceAll(v, "*", "%2A")
		v = strings.ReplaceAll(v, "%7E", "~")
		pairs = append(pairs, url.QueryEscape(k)+"="+v)
	}
	canonical := strings.Join(pairs, "&")

	// 3. 构造待签字符串
	stringToSign := "POST" + "&" +
		url.QueryEscape("/") + "&" +
		url.QueryEscape(canonical)

	// 4. HMAC-SHA256(阿里云 OpenAPI v3 默认推荐,NIST 已不推荐 SHA1)
	//    https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature
	mac := hmac.New(sha256.New, []byte(c.AccessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	sigBytes := mac.Sum(nil)

	// 5. base64 编码(RFC 2104 HMAC-SHA256 规范的输出编码)
	return base64.StdEncoding.EncodeToString(sigBytes)
}

// randomNonce 生成随机 nonce(防重放)。
func randomNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极端情况:用时间戳做 nonce
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
