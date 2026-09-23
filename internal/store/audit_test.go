package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestAudit_RoundTrip 写 + 读往返。
func TestAudit_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	events := []AuditEvent{
		{Timestamp: 1, Actor: "alice", Event: "user.create", Details: "username=bob"},
		{Timestamp: 2, Actor: "alice", Event: "user.delete", Details: "user_id=3"},
		{Timestamp: 3, Actor: "system", Event: "ddns.toggle", Details: "enabled=true"},
	}
	for _, e := range events {
		if err := s.WriteAudit(ctx, e); err != nil {
			t.Fatalf("WriteAudit(%+v): %v", e, err)
		}
	}

	got, err := s.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	// ListAudit 按 id DESC,所以最后写入的在前
	if got[0].Event != "ddns.toggle" || got[0].Actor != "system" {
		t.Errorf("got[0] = %+v, want ddns.toggle/system", got[0])
	}
	if got[2].Event != "user.create" {
		t.Errorf("got[2] = %+v, want user.create", got[2])
	}
}

// TestAudit_Limit 检查 limit 参数。
func TestAudit_Limit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = s.WriteAudit(ctx, AuditEvent{Timestamp: int64(i), Actor: "test", Event: "x"})
	}

	// limit=2 只返回 2 条
	got, _ := s.ListAudit(ctx, 2)
	if len(got) != 2 {
		t.Errorf("limit=2: got %d events", len(got))
	}
	// limit ≤ 0 返回空
	got, _ = s.ListAudit(ctx, 0)
	if len(got) != 0 {
		t.Errorf("limit=0: got %d events", len(got))
	}
}

// TestAudit_TimestampZero 验证 timestamp=0 时由函数自动填充。
func TestAudit_TimestampZero(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.WriteAudit(ctx, AuditEvent{Actor: "test", Event: "no_ts"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAudit(ctx, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event")
	}
	if got[0].Timestamp <= 0 {
		t.Errorf("Timestamp should be auto-filled, got %d", got[0].Timestamp)
	}
}

// TestAudit_DefaultActor 验证 actor 为空字符串时也写成功(not null + default '').
func TestAudit_DefaultActor(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.WriteAudit(context.Background(), AuditEvent{Event: "no_actor"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListAudit(context.Background(), 1)
	if got[0].Actor != "" {
		t.Errorf("Actor should default to '', got %q", got[0].Actor)
	}
}

// 防止 filepath 被 linter 警告未使用
var _ = filepath.Join
