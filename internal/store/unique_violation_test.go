// v2.86-PR19(P2-19c):isUniqueViolation 不再依赖错误消息文本的回归测试。
//
// 原实现:strings.Contains(err.Error(), "UNIQUE constraint failed")。
// 风险:绑死驱动当前英文文案 —— 换驱动版本 / 换措辞后判定静默失效,
// 重名用户从 409 Conflict 变成 500,且无任何告警。
//
// 本文件用**真实驱动**验证,而不是 mock:
//   - UNIQUE / PRIMARY KEY 冲突 → 判定为唯一冲突
//   - NOT NULL / CHECK 冲突 → **不得**误判为唯一冲突
//   - 非 sqlite 错误 → 不判定
//   - 端到端:CreateUser 重名必须返回 ErrConflict
package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestUniqueViolationUsesDriverCode UNIQUE 冲突必须靠错误码识别。
func TestUniqueViolationUsesDriverCode(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()

	ctx := context.Background()
	ins := func(name string) error {
		_, err := s.DB.ExecContext(ctx, insertUserSQL, name)
		return err
	}
	if err := ins("dup"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := ins("dup")
	if err == nil {
		t.Fatal("expected unique violation")
	}

	// 先确认驱动确实给了扩展码(若哪天驱动改成只给主码,这里会失败并提醒改判定)
	var se *sqlite.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	if se.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		t.Errorf("driver code = %d, want %d (SQLITE_CONSTRAINT_UNIQUE);判定逻辑需要跟着驱动调整",
			se.Code(), sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	}
	if !isUniqueViolation(err) {
		t.Error("isUniqueViolation should detect the UNIQUE violation")
	}
	// 关键性质:即使错误文本被换成不含 "UNIQUE constraint failed" 的内容,
	// 判定也必须依然成立 —— 这正是本次修复的核心。
	if isUniqueViolation(errors.New("some totally different message")) {
		t.Error("unrelated error must not be treated as unique violation")
	}
}

// TestUniqueViolationDoesNotMatchOtherConstraints NOT NULL 冲突不能被
// 误判成"用户已存在"(否则用户会看到莫名其妙的"用户名已存在")。
func TestUniqueViolationDoesNotMatchOtherConstraints(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()

	_, err := s.DB.ExecContext(context.Background(),
		`INSERT INTO users (username, password, enabled, note, speed_limit_mbps,
			expires_at, bytes_in_total, bytes_out_total, created_at, updated_at)
		 VALUES (NULL, 'x', 1, '', 10, 0, 0, 0, 0, 0)`)
	if err == nil {
		t.Skip("schema no longer enforces NOT NULL on username")
	}
	if isUniqueViolation(err) {
		t.Errorf("NOT NULL violation must NOT be treated as unique violation (err=%v)", err)
	}
}

// TestUniqueViolationNilAndOtherErrors nil / 非约束错误 → false。
func TestUniqueViolationNilAndOtherErrors(t *testing.T) {
	if isUniqueViolation(nil) {
		t.Error("nil must not be a unique violation")
	}
	if isUniqueViolation(errors.New("disk I/O error")) {
		t.Error("unrelated error must not be a unique violation")
	}
}

// TestUniqueViolationFallbackForNonSQLiteError 非 *sqlite.Error 的错误
// 仍走文本兜底(测试 stub / 上层包装丢类型信息的场景)。
func TestUniqueViolationFallbackForNonSQLiteError(t *testing.T) {
	cases := []error{
		errors.New("constraint failed: UNIQUE constraint failed: users.username"),
		fmt.Errorf("wrapped: %w", errors.New("UNIQUE constraint failed: admins.username")),
		errors.New("PRIMARY KEY constraint failed: users.id"),
	}
	for i, err := range cases {
		if !isUniqueViolation(err) {
			t.Errorf("case #%d: text fallback should still detect the violation (%v)", i, err)
		}
	}
}

// TestUniqueViolationErrorChainStillMatched 被 fmt.Errorf 包过层的错误
// errors.As 仍能穿透(否则包装一次就退化成 ClassUnknown 之外的分支)。
func TestUniqueViolationErrorChainStillMatched(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.DB.ExecContext(ctx, insertUserSQL, "chain"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, rawErr := s.DB.ExecContext(ctx, insertUserSQL, "chain")
	if rawErr == nil {
		t.Fatal("expected unique violation")
	}
	wrapped := fmt.Errorf("create user: %w", rawErr)
	if !isUniqueViolation(wrapped) {
		t.Error("wrapped sqlite error should still be detected")
	}
}

// TestCreateUserDuplicateReturnsConflict 端到端:重名用户必须拿到 ErrConflict
// (handler 靠它映射 409,拿不到就会返回误导性的 500)。
func TestCreateUserDuplicateReturnsConflict(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()
	ctx := context.Background()

	u := &User{Username: "same", Password: "x", Enabled: true, SpeedLimitMbps: 10}
	if _, err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := s.CreateUser(ctx, &User{
		Username: "same", Password: "y", Enabled: true, SpeedLimitMbps: 10,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate CreateUser error = %v, want ErrConflict", err)
	}
}

// TestRecoverUserDuplicateReturnsConflict 唯一冲突在显式指定 id 的
// INSERT(RecoverUser 删除回滚路径)上同样要能识别。
func TestRecoverUserDuplicateReturnsConflict(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()
	ctx := context.Background()

	a := &User{Username: "rec_a", Password: "x", Enabled: true, SpeedLimitMbps: 10}
	if _, err := s.CreateUser(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	other := &User{Username: "rec_b", Password: "x", Enabled: true, SpeedLimitMbps: 10}
	if _, err := s.CreateUser(ctx, other); err != nil {
		t.Fatalf("create b: %v", err)
	}

	// 用 b 的 id 插一个已存在的 username → UNIQUE 冲突
	clash := *other
	clash.Username = "rec_a"
	if err := s.RecoverUser(ctx, &clash); !errors.Is(err, ErrConflict) {
		t.Fatalf("RecoverUser conflict error = %v, want ErrConflict", err)
	}
}

// TestRecoverUserPrimaryKeyConflictReturnsConflict 主键冲突(ID 重复)也要能识别。
func TestRecoverUserPrimaryKeyConflictReturnsConflict(t *testing.T) {
	s := openRawStore(t)
	defer s.Close()
	ctx := context.Background()

	a := &User{Username: "pk_a", Password: "x", Enabled: true, SpeedLimitMbps: 10}
	if _, err := s.CreateUser(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}

	// 用户名不冲突、但 ID 撞了 → 命中 SQLITE_CONSTRAINT_PRIMARYKEY(1555)
	clash := *a
	clash.Username = "pk_b"
	clash.ID = 0 // 让 SQLite 自己分配,避开主键冲突
	if err := s.RecoverUser(ctx, &clash); err == nil {
		t.Fatal("RecoverUser with id<=0 should fail validation")
	}

	dup := *a
	dup.ID = a.ID // 显式重复 ID
	dup.Username = "pk_b"
	err := s.RecoverUser(ctx, &dup)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("primary-key conflict error = %v, want ErrConflict", err)
	}
}

const insertUserSQL = `INSERT INTO users (username, password, enabled, note, speed_limit_mbps,
	expires_at, bytes_in_total, bytes_out_total, created_at, updated_at)
	VALUES (?, 'x', 1, '', 10, 0, 0, 0, 0, 0)`

func openRawStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
