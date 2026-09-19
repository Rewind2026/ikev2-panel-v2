// SQLite 打开 + 迁移
// 设计见 docs/design.md §6
package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store 是所有表 CRUD 的根类型。Open 时建表 + 跑迁移。
type Store struct {
	DB *sql.DB
}

// Open 打开（或创建）SQLite 文件，并跑全部迁移。
// 数据库文件路径 = dataDir/panel.db
func Open(ctx context.Context, dataDir string) (*Store, error) {
	dbPath := filepath.Join(dataDir, "panel.db")

	// DSN 说明：
	//   _pragma=foreign_keys(1)  启用外键（v2 暂未用外键，但开着）
	//   _pragma=journal_mode(WAL)  并发读写不互锁
	//   _pragma=busy_timeout(5000)  写锁等待 5s
	// 用 forward slash，Windows + modernc/sqlite 都接受
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(dbPath))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Ping: %w (path=%s)", err, dbPath)
	}

	s := &Store{DB: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.DB.Close()
}

// migrate 跑迁移。v2 只有一个 001_init（design §6 完整 schema）。
// 未来 schema 变更加 002_xxx、003_xxx 即可。
func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			username        TEXT    NOT NULL UNIQUE,
			password        TEXT    NOT NULL,
			enabled         INTEGER NOT NULL DEFAULT 1,
			note            TEXT    NOT NULL DEFAULT '',
			speed_limit_mbps INTEGER NOT NULL DEFAULT 10,
			expires_at      INTEGER NOT NULL DEFAULT 0,
			bytes_in_total  INTEGER NOT NULL DEFAULT 0,
			bytes_out_total INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL,
			last_used_at    INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS idx_users_enabled ON users(enabled);`,
		`CREATE INDEX IF NOT EXISTS idx_users_expires ON users(expires_at);`,

		`CREATE TABLE IF NOT EXISTS sessions (
			id            TEXT    PRIMARY KEY,
			csrf_token    TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER NOT NULL,
			user_agent    TEXT    NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);`,

		`CREATE TABLE IF NOT EXISTS admins (
			id            INTEGER PRIMARY KEY CHECK (id = 1),
			username      TEXT    NOT NULL UNIQUE,
			password_hash TEXT    NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		);`,

		// v2.85-PR8(U11):审计日志表。记录所有敏感操作(user.* / aliyun.* / ddns.*)。
		// actor = admin username 或 'system';details 简单 k=v 串(避免引入 JSON 依赖)。
		`CREATE TABLE IF NOT EXISTS audit_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp  INTEGER NOT NULL,
			actor      TEXT    NOT NULL DEFAULT '',
			event      TEXT    NOT NULL,
			details    TEXT    NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(timestamp DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_audit_event ON audit_log(event);`,
	}

	for _, q := range stmts {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("exec %q: %w", q[:40]+"...", err)
		}
	}
	return nil
}