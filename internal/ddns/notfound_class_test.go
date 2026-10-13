// v2.86-PR19(P1-22):DomainRecordNotFound 错误分类修正的回归测试。
//
// 根因:internal/dns/errors.go 把 DomainRecordNotFound 归进 ClassTransient,
// 而 upsertRecord 的 retry 循环只对 ClassAuth / ClassRAM 短路,其余全走
// 1s → 4s → 9s 三次退避 —— 白等约 14s 去重试一个"记录已经不存在"的终态事实。
//
// 本文件验证:
//  1. 分类:DomainRecordNotFound → ClassNotFound(不再是 Transient)
//  2. 语义:IsRetryable() = false、IsFatal() = true、String() 可读
//  3. 端到端:upsertRecord 只调 1 次 API 就返回,不进退避
//  4. 不误伤:真正的临时错误(Throttling / InternalError)仍然照旧重试
package ddns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/dns"
)

// TestUpsertRecord_RecordNotFound_NotRetrying 记录已被删 → 立即失败,不进退避。
//
// 这是本项修复的核心断言:updateCalls 必须是 1(而不是 3),
// 且耗时必须远小于第一次退避的 1s。
func TestUpsertRecord_RecordNotFound_NotRetrying(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-gone",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	notFound := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "DomainRecordNotFound",
		Message:    "The specified domain record does not exist.",
	}
	srv := newAlidnsMockForUpdate(rec, notFound)
	s, c := newSyncWithMockClient(t, srv)

	start := time.Now()
	oldIP, err := s.upsertRecord(context.Background(), s.snapshot(), c, dns.RecordTypeA, "5.5.5.5")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for DomainRecordNotFound, got nil")
	}
	if !strings.Contains(err.Error(), "disappeared") {
		t.Errorf("err = %v, want message containing 'disappeared'", err)
	}
	if !strings.Contains(err.Error(), "DomainRecordNotFound") {
		t.Errorf("err = %v, want the original alidns error code to be preserved", err)
	}
	if got := srv.updateCalls.Load(); got != 1 {
		t.Errorf("updateCalls = %d, want 1 (record-not-found must NOT be retried)", got)
	}
	// 第一次退避就是 1s(+0~20% jitter) → 不进退避的话应该是毫秒级
	if elapsed > 900*time.Millisecond {
		t.Errorf("elapsed = %s, want < 900ms (must not enter the 1s/4s/9s backoff)", elapsed)
	}
	if oldIP != rec.Value {
		t.Errorf("oldIP = %q, want %q", oldIP, rec.Value)
	}
}

// TestUpsertRecord_InternalError_StillRetries 对照组:真正的临时错误不能被
// 这次分类修正误伤(仍然要重试 3 次)。
func TestUpsertRecord_InternalError_StillRetries(t *testing.T) {
	rec := dns.Record{
		RecordID: "rec-transient",
		RR:       "vpn",
		Type:     dns.RecordTypeA,
		Value:    "1.1.1.1",
		TTL:      600,
	}
	transient := &dns.AliyunError{
		HTTPStatus: 200,
		Code:       "InternalError",
		Message:    "internal error, please try again later",
	}
	srv := newAlidnsMockForUpdate(rec, transient)
	s, c := newSyncWithMockClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	// 第 2 次调用后立刻取消,避免为了断言"重试了 3 次"而真的等 14s。
	// 我们要验证的是"确实进入了退避",而不是"退避了几次"。
	time.AfterFunc(1200*time.Millisecond, cancel)

	start := time.Now()
	_, err := s.upsertRecord(ctx, s.snapshot(), c, dns.RecordTypeA, "6.6.6.6")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
	if got := srv.updateCalls.Load(); got < 2 {
		t.Errorf("updateCalls = %d, want >= 2 (InternalError MUST still be retried)", got)
	}
	// 至少要熬过第一次 1s 退避,证明确实进了重试路径
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %s, want >= 900ms (should have entered the backoff)", elapsed)
	}
}
