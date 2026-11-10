// 集成测试：用 in-memory SQLite（tempfile）跑全量 CRUD。
// 覆盖：users / sessions / admins 三表的增删查改 + 唯一约束冲突。
package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// 注意：Open 内部会追加 panel.db，所以这里只传 dataDir
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUserCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create
	u := &User{
		Username:       "alice",
		Password:       "secret123",
		Enabled:        true,
		Note:           "test user",
		SpeedLimitMbps: 10,
	}
	id, err := s.CreateUser(ctx, u)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	// Get by ID
	got, err := s.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Username != "alice" || !got.Enabled || got.SpeedLimitMbps != 10 {
		t.Errorf("GetUserByID mismatch: %+v", got)
	}

	// Get by Username
	got2, err := s.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got2.ID != id {
		t.Errorf("GetUserByUsername id mismatch: %d vs %d", got2.ID, id)
	}

	// Conflict (重复 username)
	_, err = s.CreateUser(ctx, &User{Username: "alice", Password: "x"})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}

	// Update password
	if err := s.UpdateUserPassword(ctx, id, "newpass456"); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	got, _ = s.GetUserByID(ctx, id)
	if got.Password != "newpass456" {
		t.Errorf("UpdateUserPassword did not persist: %q", got.Password)
	}

	// SetEnabled false
	if err := s.SetUserEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetUserEnabled: %v", err)
	}
	got, _ = s.GetUserByID(ctx, id)
	if got.Enabled {
		t.Error("expected Enabled=false")
	}

	// Increment bytes
	if err := s.IncrementUserBytes(ctx, "alice", 1024, 512); err != nil {
		t.Fatalf("IncrementUserBytes: %v", err)
	}
	got, _ = s.GetUserByID(ctx, id)
	if got.BytesInTotal != 1024 || got.BytesOutTotal != 512 {
		t.Errorf("IncrementUserBytes wrong: in=%d out=%d", got.BytesInTotal, got.BytesOutTotal)
	}

	// CountUsers
	n, err := s.CountUsers(ctx)
	if err != nil || n != 1 {
		t.Errorf("CountUsers: n=%d err=%v", n, err)
	}

	// Delete
	if err := s.DeleteUser(ctx, id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	_, err = s.GetUserByID(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestExpiredUsers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().Unix()
	// 过期用户（1 小时前过期）
	expired := &User{Username: "old", Password: "p", Enabled: true, ExpiresAt: now - 3600}
	if _, err := s.CreateUser(ctx, expired); err != nil {
		t.Fatal(err)
	}
	// 未过期用户（1 小时后过期）
	future := &User{Username: "young", Password: "p", Enabled: true, ExpiresAt: now + 3600}
	if _, err := s.CreateUser(ctx, future); err != nil {
		t.Fatal(err)
	}
	// 永不过期用户
	forever := &User{Username: "immortal", Password: "p", Enabled: true, ExpiresAt: 0}
	if _, err := s.CreateUser(ctx, forever); err != nil {
		t.Fatal(err)
	}

	exp, err := s.GetExpiredUsers(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp) != 1 || exp[0].Username != "old" {
		t.Errorf("expected only 'old' to be expired, got %+v", exp)
	}
}

func TestSessionCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sess := &Session{
		ID:        "abc123",
		CSRFToken: "csrf456",
		CreatedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		UserAgent: "test",
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSessionByID(ctx, "abc123", time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.CSRFToken != "csrf456" {
		t.Errorf("CSRFToken mismatch")
	}

	// 过期 session 视为不存在
	expiredSess := &Session{
		ID:        "expired1",
		CSRFToken: "csrf",
		CreatedAt: time.Now().Add(-2 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}
	if err := s.CreateSession(ctx, expiredSess); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetSessionByID(ctx, "expired1", time.Now(), 0)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for expired session, got %v", err)
	}

	// DeleteExpiredSessions 清理
	n, err := s.DeleteExpiredSessions(ctx, time.Now())
	if err != nil || n != 1 {
		t.Errorf("DeleteExpiredSessions: n=%d err=%v", n, err)
	}

	// Delete
	if err := s.DeleteSession(ctx, "abc123"); err != nil {
		t.Fatal(err)
	}
}

// TestSessionIdleTimeoutAndSlide 滑动空闲窗口的语义:
//   - last_seen 距今超过 idleTTL → 拒绝(视为不存在),但**不销毁行**
//     (登出传 idleTTL=0 时仍能消费,留审计线索);
//   - 命中一次后 last_seen 滑到 now,之后持续使用永不因空闲被踢;
//   - idleTTL=0(关闭)完全不检查。
func TestSessionIdleTimeoutAndSlide(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sess := &Session{
		ID:        "idle-sess",
		CSRFToken: "csrf",
		CreatedAt: time.Now().Add(-3 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(21 * time.Hour).Unix(),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// 1. idleTTL=1h,但 created_at(=last_seen_at 兜底)在 3h 前 → 拒绝
	if _, err := s.GetSessionByID(ctx, "idle-sess", time.Now(), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超窗 session 应视为不存在, got %v", err)
	}

	// 2. 拒绝不应销毁行:idleTTL=0(登出语义)仍能消费
	if _, err := s.GetSessionByID(ctx, "idle-sess", time.Now(), 0); err != nil {
		t.Fatalf("空闲拒绝不应销毁行, got %v", err)
	}

	// 3. 步骤 2 命中已把 last_seen 滑到 now:idleTTL=1h 现在应该命中
	if _, err := s.GetSessionByID(ctx, "idle-sess", time.Now(), time.Hour); err != nil {
		t.Fatalf("滑动后同窗口应命中, got %v", err)
	}

	// 4. 再把 last_seen 拨回 2h 前,1h 窗口立刻拒绝(窗口确实在起作用)
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE id = 'idle-sess'`,
		time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionByID(ctx, "idle-sess", time.Now(), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("拨回后应再次拒绝, got %v", err)
	}
}

func TestAdminCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// 首次 EnsureDefaultAdmin → created
	created, err := s.EnsureDefaultAdmin(ctx, "admin", "$2a$10$fakebcrypthash")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected created=true on first call")
	}

	// 第二次 → 不创建
	created, err = s.EnsureDefaultAdmin(ctx, "admin", "$2a$10$anotherhash")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("expected created=false on second call")
	}

	// 查询
	got, err := s.GetDefaultAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "admin" {
		t.Errorf("expected admin, got %q", got.Username)
	}

	// 改密码 hash
	if err := s.UpdateAdminPassword(ctx, "admin", "$2a$10$changed"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDefaultAdmin(ctx)
	if got.PasswordHash != "$2a$10$changed" {
		t.Errorf("UpdateAdminPassword did not persist")
	}
}
