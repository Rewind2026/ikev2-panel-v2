// v2.86-PR19(B13):DDNS 侧 ctx 透传的回归测试。
//
// 背景:alidns 的 call() 改成绑定调用方 ctx 之后,DDNS 停机(rootCtx cancel)
// 才能真正中断在途调用。但 retry 的退避 sleep 也必须听 ctx —— 否则第一次
// 失败后要干等 1s(+jitter)才有机会退出,停机仍然被拖住。
//
// 被测行为:upsertRecord 在退避期间收到 ctx cancel → 立刻返回 error,
// 不再发起第 2 次 UpdateDomainRecord。
package ddns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// TestUpsertRecordAbortsBackoffOnContextCancel 退避中取消 → 快速失败。
func TestUpsertRecordAbortsBackoffOnContextCancel(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-cancel",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	throttling := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "Throttling.User",
		Message:    "request was throttled",
	}
	srv := newAlidnsMockForUpdate(rec, throttling)
	s, c := newSyncWithMockClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())

	// 第一次 UpdateDomainRecord 失败后进入退避(attempt=1 → ~1s+),
	// 退避刚开始就取消 → 应立刻返回。
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	oldIP, err := s.upsertRecord(ctx, s.snapshot(), c, dns.RecordTypeA, "6.6.6.6")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after ctx cancel, got nil")
	}
	if !strings.Contains(err.Error(), "aborted during backoff") {
		t.Errorf("err = %v, want message containing 'aborted during backoff'", err)
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if oldIP != rec.Value {
		t.Errorf("oldIP = %q, want %q", oldIP, rec.Value)
	}
	// 退避是 1s(+0~20% jitter);取消在 200ms 触发,不该等到 1s 之后。
	if elapsed > 900*time.Millisecond {
		t.Errorf("elapsed = %s, want < 900ms (backoff must be interruptible)", elapsed)
	}
	if got := srv.updateCalls.Load(); got != 1 {
		t.Errorf("updateCalls = %d, want 1 (no retry after ctx cancel)", got)
	}
}

// TestUpsertRecordSucceedsWithLiveContext 对照组:ctx 还活着时行为不变
// (找到记录 + IP 没变 → 不调 Update API)。
func TestUpsertRecordSucceedsWithLiveContext(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-ctx-ok",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "7.7.7.7",
		TTL:      600,
	}
	srv := newAlidnsMockForUpdate(rec, nil)
	s, c := newSyncWithMockClient(t, srv)

	oldIP, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeA, rec.Value)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oldIP != rec.Value {
		t.Errorf("oldIP = %q, want %q", oldIP, rec.Value)
	}
	if got := srv.updateCalls.Load(); got != 0 {
		t.Errorf("updateCalls = %d, want 0 (IP unchanged, no API call)", got)
	}
}
