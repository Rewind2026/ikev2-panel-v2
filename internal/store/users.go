// users 表 CRUD
// 设计见 docs/design.md §6.1
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound 查询无结果时的统一错误。
var ErrNotFound = errors.New("store: not found")

// ErrConflict 唯一键冲突（如 username 已存在）。
var ErrConflict = errors.New("store: conflict")

// CreateUser 插入新用户。
func (s *Store) CreateUser(ctx context.Context, u *User) (int64, error) {
	now := time.Now().Unix()
	u.CreatedAt = now
	u.UpdatedAt = now

	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO users
			(username, password, enabled, note, speed_limit_mbps, expires_at,
			 bytes_in_total, bytes_out_total, created_at, updated_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?, ?, 0)
	`,
		u.Username, u.Password, boolToInt(u.Enabled), u.Note, u.SpeedLimitMbps, u.ExpiresAt,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("username %q: %w", u.Username, ErrConflict)
		}
		return 0, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("LastInsertId: %w", err)
	}
	u.ID = id
	return id, nil
}

// GetUserByID 按主键查用户。
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, username, password, enabled, note, speed_limit_mbps, expires_at,
		       bytes_in_total, bytes_out_total, created_at, updated_at, last_used_at
		FROM users WHERE id = ?
	`, id)
	return scanUser(row)
}

// GetUserByUsername 按用户名查用户。
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, username, password, enabled, note, speed_limit_mbps, expires_at,
		       bytes_in_total, bytes_out_total, created_at, updated_at, last_used_at
		FROM users WHERE username = ?
	`, username)
	return scanUser(row)
}

// ListUsers 列出全部用户，按 id 升序。
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, username, password, enabled, note, speed_limit_mbps, expires_at,
		       bytes_in_total, bytes_out_total, created_at, updated_at, last_used_at
		FROM users ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserPassword 重置用户 VPN 密码（明文写回）。
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, newPassword string) error {
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`UPDATE users SET password = ?, updated_at = ? WHERE id = ?`,
		newPassword, now, id,
	)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserEnabled 启用 / 停用用户。
func (s *Store) SetUserEnabled(ctx context.Context, id int64, enabled bool) error {
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`UPDATE users SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), now, id,
	)
	if err != nil {
		return fmt.Errorf("set enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserNote 更新备注。
func (s *Store) UpdateUserNote(ctx context.Context, id int64, note string) error {
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx,
		`UPDATE users SET note = ?, updated_at = ? WHERE id = ?`,
		note, now, id,
	)
	if err != nil {
		return fmt.Errorf("update note: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser 删除用户。
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountUsers 总用户数（含停用）。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// GetExpiredUsers 返回 expires_at > 0 且 < nowUnix 的用户列表。
// 用于 expiry goroutine（design §5.9）。
func (s *Store) GetExpiredUsers(ctx context.Context, nowUnix int64) ([]*User, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, username, password, enabled, note, speed_limit_mbps, expires_at,
		       bytes_in_total, bytes_out_total, created_at, updated_at, last_used_at
		FROM users
		WHERE expires_at > 0 AND expires_at < ?
		ORDER BY expires_at ASC
	`, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("get expired: %w", err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// IncrementUserBytes 累加单个用户的流量字节。
// 由 collector goroutine 调用（design §3.6）。
// 用 SQL 表达式直接累加，避免读-改-写竞争。
func (s *Store) IncrementUserBytes(ctx context.Context, username string, bytesIn, bytesOut int64) error {
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx, `
		UPDATE users
		SET bytes_in_total  = bytes_in_total + ?,
		    bytes_out_total = bytes_out_total + ?,
		    last_used_at    = ?
		WHERE username = ?
	`, bytesIn, bytesOut, now, username)
	if err != nil {
		return fmt.Errorf("increment bytes: %w", err)
	}
	// 注意：用户不存在时 RowsAffected = 0 是允许的（用户可能刚被删除）
	if n, _ := res.RowsAffected(); n == 0 {
		// 不返回 ErrNotFound：sa 解析后用户已被删除是合理情况
		return nil
	}
	return nil
}

// scanUser 扫描一行到 User。支持 *sql.Row 和 *sql.Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(r rowScanner) (*User, error) {
	var u User
	var enabled int
	err := r.Scan(
		&u.ID, &u.Username, &u.Password, &enabled, &u.Note,
		&u.SpeedLimitMbps, &u.ExpiresAt, &u.BytesInTotal, &u.BytesOutTotal,
		&u.CreatedAt, &u.UpdatedAt, &u.LastUsedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.Enabled = enabled != 0
	return &u, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation 判断是否为 SQLite 唯一约束冲突。
// modernc.org/sqlite 错误消息形如 "constraint failed: UNIQUE constraint failed: users.username"
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY constraint failed")
}