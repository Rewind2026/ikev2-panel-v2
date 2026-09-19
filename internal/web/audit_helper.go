// v2.85-PR8(U11):web 层审计 helper。
//
// 所有 handler 调 s.writeAudit(r, "event.name", "details") 一行搞定。
//
// 设计要点:
//   - actor 自动从 ctx 拿 admin username,无 admin → "system"
//   - 写入失败仅 log.Warn,不返回 error(handler 不阻塞业务流)
//   - 时间戳用 time.Now().UnixNano()(单调,可排序)
package web

import (
	"net/http"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// writeAudit 写入一条审计日志。
//
// 调用约定:handler 主体逻辑完成(DB 写成功 + 文件写成功)后调一次,
// 不要在出错分支调 —— 只记成功事件,失败由各自错误日志负责。
func (s *Server) writeAudit(r *http.Request, event, details string) {
	actor := "system"
	if admin, ok := AdminFrom(r.Context()); ok && admin != nil {
		actor = admin.Username
	}
	if err := s.Store.WriteAudit(r.Context(), store.AuditEvent{
		Timestamp: time.Now().UnixNano(),
		Actor:     actor,
		Event:     event,
		Details:   details,
	}); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("audit write failed", "event", event, "err", err)
		}
	}
}
