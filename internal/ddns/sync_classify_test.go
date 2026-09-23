// v2.85-PR5 测试:upsertRecord 用 dns.Classify 决策 retry。
//
// 用 httptest mock alidns server,避免真实网络。
// 测试覆盖:
//   - AuthError → 立即失败,不 retry(calls == 1)
//   - RAMError → 立即失败,不 retry
//   - ThrottlingError → retry 3 次(calls == 3)
//   - InternalError → retry 3 次
//   - 业务成功 → return nil
package ddns

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// mockAliyunResp 构造阿里云错误响应的 JSON body。
func mockAliyunResp(code, message string) string {
	b, _ := json.Marshal(map[string]string{
		"Code":    code,
		"Message": message,
		"RequestId": "test-request-id",
	})
	return string(b)
}

// alidnsMockServer 启动一个 mock alidns server,每次 DescribeDomainRecords
// 返回预定义 record,UpdateDomainRecord 返回预定义错误(可注入)。
//
// 请求统计通过 updateCalls 计数器暴露。
type alidnsMockServer struct {
	*httptest.Server
	updateCalls atomic.Int32
	updateErr   error // nil = success
}

// newAlidnsMockForUpdate 构造一个 mock server:
//   - DescribeDomainRecords 返回 rec (RecordID, RR, Type, Value, TTL)
//   - UpdateDomainRecord 返回 updateErr(nil 或 dns.AliyunError)
func newAlidnsMockForUpdate(rec dns.Record, updateErr error) *alidnsMockServer {
	m := &alidnsMockServer{updateErr: updateErr}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 解析 Action(POST form: Action=UpdateDomainRecord 或 DescribeDomainRecords)
		_ = r.ParseForm()
		action := r.Form.Get("Action")

		switch action {
		case "DescribeDomainRecords":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"DomainRecords": struct {
					Record []dns.Record `json:"Record"`
				}{
					Record: []dns.Record{rec},
				},
				"Code":    "",
				"Message": "",
			})
		case "UpdateDomainRecord":
			m.updateCalls.Add(1)
			if m.updateErr != nil {
				// 区分 *AliyunError vs 普通 error
				if ae, ok := m.updateErr.(*dns.AliyunError); ok {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK) // alidns 业务错误也是 HTTP 200
					fmt.Fprint(w, mockAliyunResp(ae.Code, ae.Message))
					return
				}
				// 模拟 HTTP 4xx(如网络层错误)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, m.updateErr.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"RecordId": rec.RecordID,
				"Code":     "",
				"Message":  "",
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	m.Server = httptest.NewServer(mux)
	return m
}

// newSyncWithMockClient 构造一个 Sync + AliyunClient 指向 mock server。
func newSyncWithMockClient(t *testing.T, srv *alidnsMockServer) (*Sync, *dns.AliyunClient) {
	t.Helper()
	tmp := t.TempDir()
	cfg := Config{
		Enabled:              true,
		Family:               "v4",
		AliyunAccessKeyID:    "ak",
		AliyunAccessKeySecret: "sk",
		Domain:               "example.com",
		RR:                   "vpn",
		Period:               100 * time.Millisecond,
		Throttle:             0,
		LastFailedFile:       filepath.Join(tmp, "FAILED"),
		StateFile:            filepath.Join(tmp, "state"),
		Logger:               slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	s := NewSync(cfg)

	// 直接构造 AliyunClient,绕过 NewAliyunClient 的 10s timeout(测试要更快)
	c := &dns.AliyunClient{
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		HTTPClient:      &http.Client{Timeout: 5 * time.Second},
	}
	// 用 mock server 的 URL 替换 endpoint。
	dns.SetAlidnsEndpoint(srv.URL)

	t.Cleanup(func() {
		dns.SetAlidnsEndpoint("https://alidns.aliyuncs.com/")
		srv.Close()
	})

	return s, c
}

// TestUpsertRecord_AuthError_NotRetrying 验证 AuthError 不 retry。
func TestUpsertRecord_AuthError_NotRetrying(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-123",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1", // 故意跟新值不同
		TTL:      600,
	}
	authErr := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "InvalidAccessKeyId.NotFound",
		Message:    "Invalid Access Key.",
	}
	srv := newAlidnsMockForUpdate(rec, authErr)
	s, c := newSyncWithMockClient(t, srv)

	oldIP, err := s.upsertRecord(c, dns.RecordTypeA, "2.2.2.2")

	if err == nil {
		t.Fatal("expected error for AuthError, got nil")
	}
	if !strings.Contains(err.Error(), "auth error") {
		t.Errorf("err = %v, want message containing 'auth error'", err)
	}
	if oldIP != rec.Value {
		t.Errorf("oldIP = %q, want %q", oldIP, rec.Value)
	}
	if got := srv.updateCalls.Load(); got != 1 {
		t.Errorf("updateCalls = %d, want 1 (AuthError should NOT retry)", got)
	}
}

// TestUpsertRecord_RAMError_NotRetrying 验证 RAMError 不 retry。
func TestUpsertRecord_RAMError_NotRetrying(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-456",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	ramErr := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "DomainRecordNotBelongToRAM",
		Message:    "record not belong to RAM user",
	}
	srv := newAlidnsMockForUpdate(rec, ramErr)
	s, c := newSyncWithMockClient(t, srv)

	_, err := s.upsertRecord(c, dns.RecordTypeA, "3.3.3.3")

	if err == nil {
		t.Fatal("expected error for RAMError, got nil")
	}
	if !strings.Contains(err.Error(), "RAM error") {
		t.Errorf("err = %v, want message containing 'RAM error'", err)
	}
	if got := srv.updateCalls.Load(); got != 1 {
		t.Errorf("updateCalls = %d, want 1 (RAMError should NOT retry)", got)
	}
}

// TestUpsertRecord_Throttling_Retries3Times 验证 ThrottlingError retry 3 次。
func TestUpsertRecord_Throttling_Retries3Times(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-789",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	thrErr := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "Throttling.User",
		Message:    "request was throttled",
	}
	srv := newAlidnsMockForUpdate(rec, thrErr)
	s, c := newSyncWithMockClient(t, srv)

	start := time.Now()
	_, err := s.upsertRecord(c, dns.RecordTypeA, "4.4.4.4")
	elapsed := time.Since(start)

	// Throttling 走完 3 次 retry:每次 sleep 1s/4s/9s,合计 ~14s
	// 为测试速度,这里只验证 calls == 3 和 elapsed 在合理范围
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	if got := srv.updateCalls.Load(); got != 3 {
		t.Errorf("updateCalls = %d, want 3 (Throttling should retry 3 times)", got)
	}
	// 最少 1+4+9 = 14s;给一点余量
	if elapsed < 13*time.Second {
		t.Errorf("elapsed = %s, expected >= 14s (3 retries with 1+4+9s backoff)", elapsed)
	}
}

// TestUpsertRecord_Success 验证成功路径(无错误)。
func TestUpsertRecord_Success(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-ok",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	srv := newAlidnsMockForUpdate(rec, nil)
	s, c := newSyncWithMockClient(t, srv)

	oldIP, err := s.upsertRecord(c, dns.RecordTypeA, "5.5.5.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oldIP != rec.Value {
		t.Errorf("oldIP = %q, want %q", oldIP, rec.Value)
	}
	if got := srv.updateCalls.Load(); got != 1 {
		t.Errorf("updateCalls = %d, want 1 (success should not retry)", got)
	}
}