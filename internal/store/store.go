// SQLite 打开 + 迁移
// 设计见 docs/design.md §6
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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

	// v2.86-PR13.0:审查报告 C2-子。SQLite 文件含全部用户 VPN 密码(明文,设计 §4.3 妥协),
	// 必须 0600 否则任何同主机 user 都能读 panel.db = 拿到所有用户 VPN 凭证。
	// DataDir 已在 main.go 用 os.MkdirAll(..., 0o700) 保护,但 panel.db 文件本身
	// 沿用 umask 022 时是 0644(world-readable),需要显式 chmod。
	// 先 chmod 既存文件,再 sql.Open(sql.Open 不会对既存文件做 chmod)。
	if info, statErr := os.Stat(dbPath); statErr == nil {
		// 只对既存文件操作;新建文件走 sql.Open 后 chmod(下面)
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(dbPath, 0o600); err != nil {
				return nil, fmt.Errorf("chmod panel.db 0600: %w (path=%s)", err, dbPath)
			}
		}
	}

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

	// 新建文件:sql.Open 后立刻 chmod 0600(此时文件已被创建)。
	// 注意:sql.Open 在 PingContext 之前可能还没真正 open 文件(惰性连接),
	// 这里在 PingContext 之后再 chmod 一次,确保覆盖所有路径。
	if info, statErr := os.Stat(dbPath); statErr == nil {
		if info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(dbPath, 0o600); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("chmod panel.db 0600 (post-open): %w", err)
			}
		}
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

	// v2.86-PR12.21:每用户 mobileconfig 覆盖项。
	// ALTER TABLE 用 IF NOT EXISTS 在 modernc/sqlite 不支持,先查列名再加。
	// 列存 JSON 字符串,空 = 用 cert 默认值。
	// 参考:https://sqlite.org/lang_altertable.html#otheralter
	// 必须在 stmts 循环之后跑:全新 DB 下 users 表还没建,ALTER 会失败
	var hasMobileOpts int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'mobileconfig_opts'`,
	).Scan(&hasMobileOpts); err != nil {
		return fmt.Errorf("check mobileconfig_opts column: %w", err)
	}
	if hasMobileOpts == 0 {
		if _, err := s.DB.ExecContext(ctx,
			`ALTER TABLE users ADD COLUMN mobileconfig_opts TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("add mobileconfig_opts column: %w", err)
		}
	}
	return nil
}