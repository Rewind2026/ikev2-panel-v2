// v2.85-PR8(U11):审计日志只读页。
//
// 路由: GET /audit
// 数据: 最近 100 条审计事件
package web

import (
	"net/http"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// auditPageData 审计页模板数据。
type auditPageData struct {
	PageMeta
	Events []store.AuditEvent
}

// handleAudit GET /audit
//
// 取最近 100 条审计事件,按时间倒序(id DESC)。
// 失败 → 500;成功 → 渲染 audit 模板。
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	admin, _ := AdminFrom(r.Context())
	sess, _ := SessionFrom(r.Context())

	events, err := s.Store.ListAudit(r.Context(), 100)
	if err != nil {
		s.Logger.Error("list audit", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.RenderPage(w, "audit", auditPageData{
		PageMeta: PageMeta{Page: "audit", Title: "操作日志", PageKey: "audit", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
		Events:   events,
	})
}
