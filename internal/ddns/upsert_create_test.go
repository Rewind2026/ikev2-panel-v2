// v2.86-PR19(A13)：DDNS「只改不建」+ 状态误报 的回归测试。
//
// 真机症状：面板里域名 / 凭证 / 开关全配好，DDNS 状态显示
//
//	"success": true, "new_ip": "2408:832e:...", "failed": false
//
// 但 dig 权威 NS（dns27/28.hichina.com）查不到 vpn 这条记录，
// 域名永远解析不了 —— 而面板还在报成功。
//
// 根因：upsertRecord 在 FindRecord 返回 nil（记录不存在）时，
// 只打一条 WARN 就 return ("", nil)。nil error + newIP 非空
// → 调用方 anySuccess = true → UI 显示"同步成功"。
//
// 本文件验证修复后的行为：
//  1. 记录不存在 → 自动 AddDomainRecord（DDNS 名副其实）
//  2. 记录已存在 → **不**重复创建（否则会产生重复记录）
//  3. 创建失败 → 如实返回 error，绝不静默跳过
package ddns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// alidnsCreateMock 是可观测的 alidns mock：记录 addCalls 并按需让Add 失败。
type alidnsCreateMock struct {
	*httptest.Server
	addCalls    atomic.Int32
	addRR       atomic.Value
	addType     atomic.Value
	addValue    atomic.Value
	failAdd     bool
	existingVal string // 非空 = DescribeDomainRecords 返回这条已存在记录
}

func newAlidnsCreateMock(existingVal string, failAdd bool) *alidnsCreateMock {
	m := &alidnsCreateMock{failAdd: failAdd, existingVal: existingVal}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// TestUpsertRecord_CreatesMissingRecord 核心断言：记录不存在时自动创建。
//
// A13 之前这里是 "WARN + return ("", nil)"，用户必须自己去阿里云控制台手建。
func TestUpsertRecord_CreatesMissingRecord(t *testing.T) {
	mock := newAlidnsCreateMock("", false)
	srv := &alidnsMockServer{Server: mock.Server}
	s, c := newSyncWithMockClient(t, srv)

	oldIP, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeAAAA, "2408:832e::1")
	if err != nil {
		t.Fatalf("upsertRecord: %v (期望自动创建成功)", err)
	}

	if got := mock.addCalls.Load(); got != 1 {
		t.Errorf("AddDomainRecord 调用次数 = %d, want 1（记录不存在必须自动创建）", got)
	}
	if got, _ := mock.addRR.Load().(string); got != "vpn" {
		t.Errorf("AddDomainRecord RR = %q, want %q", got, "vpn")
	}
	if got, _ := mock.addType.Load().(string); got != dns.RecordTypeAAAA {
		t.Errorf("AddDomainRecord Type = %q, want %q", got, dns.RecordTypeAAAA)
	}
	if got, _ := mock.addValue.Load().(string); got != "2408:832e::1" {
		t.Errorf("AddDomainRecord Value = %q, want %q", got, "2408:832e::1")
	}
	// 返回 currentIP 而非空串，UI 才能显示"本次是新建的"
	if oldIP != "2408:832e::1" {
		t.Errorf("oldIP = %q, want %q（新建时应返回 currentIP）", oldIP, "2408:832e::1")
	}
}

// TestUpsertRecord_NoCreateWhenRecordExists 确认"自动创建"没改成"每次都新建"。
// 阿里云允许同一 RR+Type 有多条记录，重复创建会让解析结果不确定。
func TestUpsertRecord_NoCreateWhenRecordExists(t *testing.T) {
	mock := newAlidnsCreateMock("2001:db8::1", false)
	srv := &alidnsMockServer{Server: mock.Server}
	s, c := newSyncWithMockClient(t, srv)

	// 传不同的 IP，触发 update 分支（记录存在 → 只更新，不创建）
	if _, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeAAAA, "2408:832e::9"); err != nil {
		t.Fatalf("upsertRecord: %v", err)
	}

	if got := mock.addCalls.Load(); got != 0 {
		t.Errorf("AddDomainRecord 调用次数 = %d, want 0（记录已存在时不应重复创建）", got)
	}
}

// TestUpsertRecord_CreateFailureIsReported 确认创建失败**如实报错**，
// 不再退化成"跳过 + 假成功"。这是 A13 的另一半。
//
// 典型触发：RAM 没勾 alidns:AddDomainRecord 权限。
func TestUpsertRecord_CreateFailureIsReported(t *testing.T) {
	mock := newAlidnsCreateMock("", true)
	srv := &alidnsMockServer{Server: mock.Server}
	s, c := newSyncWithMockClient(t, srv)

	oldIP, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeAAAA, "2408:832e::1")

	if err == nil {
		t.Fatal("创建失败时必须返回 error，got nil（A13 的状态误报就是这么来的）")
	}
	// 错误信息要让用户知道是"创建"这一步挂了
	msg := err.Error()
	if !strings.Contains(msg, "create") && !strings.Contains(msg, "AddDomainRecord") {
		t.Errorf("err = %q，措辞应指明是创建记录失败", msg)
	}
	// 原始阿里云错误码要保留，便于用户去查 RAM 权限
	if !strings.Contains(msg, "NoPermission") && !strings.Contains(msg, "not authorized") {
		t.Errorf("err = %q，应保留阿里云原始错误信息", msg)
	}
	if oldIP != "" {
		t.Errorf("oldIP = %q, want 空（失败时不应声称拿到了旧值）", oldIP)
	}
}

// TestUpsertRecord_NoFalseSuccessOnCreateFailure 端到端确认状态不再误报。
//
// 这是用户实际看到的那个 bug：跳过 → nil error → success=true。
// 这里断言创建失败时 LastSync 层面会看到错误，而不是"同步成功"。
func TestUpsertRecord_NoFalseSuccessOnCreateFailure(t *testing.T) {
	mock := newAlidnsCreateMock("", true)
	srv := &alidnsMockServer{Server: mock.Server}
	s, c := newSyncWithMockClient(t, srv)

	_, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeAAAA, "2408:832e::1")
	if err == nil {
		t.Fatal("创建失败必须报错，不能返回 nil 让上层记成 success")
	}
	// error 非 nil → tick 里 ls.V6Error 会被填 → UI 显示失败。
	// 这里只断言 error 通路存在即可，完整 tick 流程由 sync_v4_test 覆盖。
}

// ---- mock 实现 ----

func (m *alidnsCreateMock) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	switch r.Form.Get("Action") {
	case "DescribeDomainRecords":
		if m.existingVal == "" {
			// 0 条 → 触发 rec == nil 分支
			_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[{"RecordId":"rec-existing",` +
			`"RR":"vpn","Type":"AAAA","Value":"` + m.existingVal + `","TTL":600}]}}`))
	case "AddDomainRecord":
		m.addCalls.Add(1)
		m.addRR.Store(r.Form.Get("RR"))
		m.addType.Store(r.Form.Get("Type"))
		m.addValue.Store(r.Form.Get("Value"))
		if m.failAdd {
			// 阿里云把 API 错误放在 200 响应体的 Code 字段里
			_, _ = w.Write([]byte(`{"Code":"NoPermission",` +
				`"Message":"user is not authorized to do this action"}`))
			return
		}
		_, _ = w.Write([]byte(`{"RecordId":"rec-new-999"}`))
	case "UpdateDomainRecord":
		_, _ = w.Write([]byte(`{"RecordId":"rec-existing"}`))
	default:
		_, _ = w.Write([]byte(`{"Code":"InvalidAction"}`))
	}
}
