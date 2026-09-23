// v2.85-PR8(U11):审计日志。
//
// 设计要点:
//   - WriteAudit: 单条 INSERT,失败不返回 error 给 caller(由 handler 决定 log.warn)
//   - ListAudit: 按 id DESC 取最近 N 条
//   - 不做全文搜索 / 导出 CSV / 分页(只读 + LIMIT 即可,见 proposal 不在范围)
//
// actor 字段:
//   - admin username(从 requireSession 中间件注入 ctx 取出)
//   - 'system'(后台任务触发的操作,如 cron 自动续签)
//
// details 字段:
//   - 简单 k=v,k=v 串(避免引入 JSON 编解码依赖,人眼可读)
//   - 例: "username=alice,speed=10" / "user_id=3,username=alice"
package store

import (
	"context"
	"time"
)

// AuditEvent 单条审计记录。
type AuditEvent struct {
	Timestamp int64  // unix nano
	Actor     string // admin username / "system"
	Event     string // e.g. "user.create", "aliyun.save", "ddns.toggle"
	Details   string // 自由格式 k=v 串
}

// WriteAudit 写入一条审计日志。
//
// 失败不返回 error 是有意为之——handler 调 WriteAudit 是 fire-and-forget,
// 审计写失败不应阻塞用户操作(详见 proposal §4.2 不破坏兼容性)。
// 调用方若需要错误处理,可显式 err := s.DB.ExecContext(...) 绕过本函数。
func (s *Store) WriteAudit(ctx context.Context, e AuditEvent) error {
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().UnixNano()
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO audit_log(timestamp, actor, event, details) VALUES(?, ?, ?, ?)`,
		e.Timestamp, e.Actor, e.Event, e.Details,
	)
	return err
}

// ListAudit 取最近 limit 条审计事件(按 id DESC)。
//
// limit ≤ 0 时返回空切片。limit > 1000 自动截断到 1000(防 OOM)。
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT timestamp, actor, event, details FROM audit_log ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.Timestamp, &e.Actor, &e.Event, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
