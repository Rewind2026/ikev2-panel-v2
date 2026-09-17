// / 首页（M3 占位：显示欢迎 + 链接到 /users）。
// 设计见 docs/design.md §3.5
// M4 后会加入 SA 列表 + 用户数。
package web

import (
	"net/http"
)

type homeData struct {
	AdminUsername string
	TotalUsers    int
	CSRFToken     string // 渲染 nav 里的 logout form（§4.1 CSRF）
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	admin, ok := AdminFrom(r.Context())
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sess, _ := SessionFrom(r.Context())

	total, err := s.Store.CountUsers(r.Context())
	if err != nil {
		s.Logger.Error("count users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	_ = s.Templates.ExecuteTemplate(w, "home.html", homeData{
		AdminUsername: admin.Username,
		TotalUsers:    total,
		CSRFToken:     csrfTokenOf(sess),
	})
}