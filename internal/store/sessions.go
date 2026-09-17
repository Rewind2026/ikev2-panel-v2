// sessions 表 CRUD
// 设计见 docs/design.md §6.2
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateSession 插入一条 session。
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (id, csrf_token, created_at, expires_at, user_agent)
		VALUES (?, ?, ?, ?, ?)
	`, sess.ID, sess.CSRFToken, sess.CreatedAt, sess.ExpiresAt, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSessionByID 按主键查 session。
// 同时校验 expires_at > now：过期 session 视为不存在。
func (s *Store) GetSessionByID(ctx context.Context, id string, now time.Time) (*Session, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, csrf_token, created_at, expires_at, user_agent
		FROM sessions
		WHERE id = ? AND expires_at > ?
	`, id, now.Unix())
	return scanSession(row)
}

// DeleteSession 删除单条 session（登出）。
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions 清理过期 session。
// 由 main goroutine 周期调用（比如每小时）。
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanSession(r rowScanner) (*Session, error) {
	var sess Session
	err := r.Scan(&sess.ID, &sess.CSRFToken, &sess.CreatedAt, &sess.ExpiresAt, &sess.UserAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan session: %w", err)
	}
	return &sess, nil
}