// admins 表 CRUD
// 设计见 docs/design.md §6.3
//
// v2 全表只能 id=1 一条记录（schema CHECK (id = 1)）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateAdmin 插入管理员。强制 id=1；如果已存在则返回 ErrConflict。
func (s *Store) CreateAdmin(ctx context.Context, a *Admin) error {
	now := time.Now().Unix()
	a.ID = 1
	a.CreatedAt = now
	a.UpdatedAt = now

	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO admins (id, username, password_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, a.ID, a.Username, a.PasswordHash, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("admin already exists: %w", ErrConflict)
		}
		return fmt.Errorf("insert admin: %w", err)
	}
	return nil
}

// GetAdminByUsername 按用户名查管理员。
func (s *Store) GetAdminByUsername(ctx context.Context, username string) (*Admin, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, username, password_hash, created_at, updated_at
		FROM admins WHERE username = ?
	`, username)
	return scanAdmin(row)
}

// GetDefaultAdmin 获取唯一管理员（id=1）。
// v2 只允许一条管理员记录。
func (s *Store) GetDefaultAdmin(ctx context.Context) (*Admin, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, username, password_hash, created_at, updated_at
		FROM admins WHERE id = 1
	`)
	return scanAdmin(row)
}

// UpdateAdminPassword 修改管理员密码（写 bcrypt hash）。
func (s *Store) UpdateAdminPassword(ctx context.Context, username, newPasswordHash string) error {
	now := time.Now().Unix()
	res, err := s.DB.ExecContext(ctx, `
		UPDATE admins SET password_hash = ?, updated_at = ? WHERE username = ?
	`, newPasswordHash, now, username)
	if err != nil {
		return fmt.Errorf("update admin password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnsureDefaultAdmin 如果没有管理员，创建一个默认账号（密码 hash 由 caller 传入）。
// 用于首次启动：caller 随机生成密码 → bcrypt → 调这个方法 → 把明文密码 print 给管理员。
// 返回值：
//   created=true  → 新建成功
//   created=false → 已存在，未做任何事
func (s *Store) EnsureDefaultAdmin(ctx context.Context, username, passwordHash string) (created bool, err error) {
	existing, err := s.GetDefaultAdmin(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("check existing admin: %w", err)
	}
	if existing != nil {
		return false, nil
	}
	a := &Admin{
		ID:           1,
		Username:     username,
		PasswordHash: passwordHash,
	}
	if err := s.CreateAdmin(ctx, a); err != nil {
		return false, fmt.Errorf("create default admin: %w", err)
	}
	return true, nil
}

func scanAdmin(r rowScanner) (*Admin, error) {
	var a Admin
	err := r.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan admin: %w", err)
	}
	return &a, nil
}