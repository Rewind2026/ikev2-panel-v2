// sessions 表 CRUD
// 见 docs/architecture.md §5
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateSession 插入一条 session。last_seen_at = created_at(登录即活跃)。
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (id, csrf_token, created_at, expires_at, last_seen_at, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sess.ID, sess.CSRFToken, sess.CreatedAt, sess.ExpiresAt, sess.CreatedAt, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSessionByID 按主键取一条「活着」的 session。
//
// 三重校验 + 滑动续期在同一条原子 UPDATE...RETURNING 里完成:
//  1. 绝对过期:expires_at > now(登录会话时长 sessionTTL,面板可配)
//  2. 空闲窗口:idleTTL <= 0 时不检查;否则要求 last_seen_at > now - idleTTL
//  3. 命中即把 last_seen_at 滑到 now —— 每次通过认证的请求都是一次活跃续期
//
// 空闲窗口解决的是「只有绝对过期」的缺口:管理员哪怕一直在用,24h 一到照样
// 被踢;反过来闲置 23h59m 也只是"还没到点"。滑动语义让持续使用永不超时、
// 闲置超过窗口就地失效(借鉴 DDNS 的滑动空闲,机制按本仓库自己的 SQL 会话实现)。
//
// 原子性:校验和滑动必须在同一条语句里。拆成先查后更,两个并发请求都能
// 通过校验,且一个请求的滑动可能被另一个的读覆盖。
func (s *Store) GetSessionByID(ctx context.Context, id string, now time.Time, idleTTL time.Duration) (*Session, error) {
	row := s.DB.QueryRowContext(ctx, `
		UPDATE sessions SET last_seen_at = ?
		WHERE id = ?
		  AND expires_at > ?
		  AND (? <= 0 OR last_seen_at > ?)
		RETURNING id, csrf_token, created_at, expires_at, user_agent
	`, now.Unix(), id, now.Unix(), int64(idleTTL.Seconds()), now.Unix()-int64(idleTTL.Seconds()))
	return scanSession(row)
}

// DeleteSessionsExcept 删除除 keepID 外的全部 session。
// 调用时机:admin 改密成功后 —— 旧凭据验证通过的浏览器会话一律吊销,
// 当前会话保留(改密的那个浏览器刚刚证明过自己知道密码,踢掉只会丢 flash 提示)。
func (s *Store) DeleteSessionsExcept(ctx context.Context, keepID string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id != ?`, keepID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions except: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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
