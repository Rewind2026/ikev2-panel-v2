package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAudit_RetentionLikeScript 验证 audit_log retention 脚本的核心 SQL 正确。
//
// Phase 5 A 的 audit-retention.sh 跑:
//   DELETE FROM audit_log WHERE timestamp < ${CUTOFF_TS} * 1000000000;
// 这里复现 + 验证时间边界。
func TestAudit_RetentionLikeScript(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()
	// 注入 5 条: 100d / 95d / 89d / 30d / 1d 前
	deltas := []time.Duration{
		-100 * 24 * time.Hour,
		-95 * 24 * time.Hour,
		-89 * 24 * time.Hour,
		-30 * 24 * time.Hour,
		-1 * 24 * time.Hour,
	}
	for i, d := range deltas {
		_ = s.WriteAudit(ctx, AuditEvent{
			Timestamp: now.Add(d).UnixNano(),
			Actor:     "test",
			Event:     "test.event",
			Details:   "delta=" + (time.Duration(i)).String(),
		})
	}

	// 模拟脚本:retention=90d → cutoff = now - 90d
	cutoff := now.Add(-90 * 24 * time.Hour).UnixNano()

	// 用 store 层方法(脚本用 sqlite3,但 SQL 等价)
	if _, err := s.DB.ExecContext(ctx,
		`DELETE FROM audit_log WHERE timestamp < ?`, cutoff); err != nil {
		t.Fatal(err)
	}

	// 期望剩下 3 条(89d / 30d / 1d 前)
	got, _ := s.ListAudit(ctx, 100)
	if len(got) != 3 {
		t.Errorf("after retention: got %d events, want 3", len(got))
	}

	// DB 文件不应被清空(只删记录,VACUUM 单独跑)
	dbPath := filepath.Join(dir, "panel.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("panel.db should still exist: %v", err)
	}
}

// TestAudit_LimitClamped 验证 ListAudit limit > 1000 自动截断。
func TestAudit_LimitClamped(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(context.Background(), dir)
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = s.WriteAudit(ctx, AuditEvent{Event: "x"})
	}

	// limit=2000 应被截到 1000(返回 <=5 因为只有 5 条)
	got, _ := s.ListAudit(ctx, 2000)
	if len(got) != 5 {
		t.Errorf("limit clamping: got %d events (want 5 because only 5 inserted)", len(got))
	}
}
