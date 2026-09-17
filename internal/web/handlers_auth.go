// /login / /logout 处理器。
// 设计见 docs/design.md §3.2 + §7.1
package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// loginPageData 登录页模板数据。
type loginPageData struct {
	Error    string // 用户名/密码错误时回显
	Username string // 表单回显（避免重复输入）
}

// handleLoginPage GET /login。已登录则 302 → /。
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// 已登录直接跳首页
	if auth.GetSessionCookie(r) != "" {
		if _, err := s.Store.GetSessionByID(r.Context(), auth.GetSessionCookie(r), time.Now()); err == nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}
	_ = s.Templates.ExecuteTemplate(w, "login.html", loginPageData{})
}

// handleLogin POST /login。校验用户名密码 → 创建 session → 302 → /。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	// 登录端点不需要 CSRF：登录前无 session，无法发 token
	// cookie SameSite=Lax 已阻断跨站 POST
	// 受保护路由的 POST 会强制 CSRF（M4 实现）

	admin, err := s.Store.GetAdminByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 故意返回相同错误，避免用户名枚举
			s.renderLoginError(w, username, "用户名或密码错误")
			return
		}
		s.Logger.Error("get admin", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := auth.VerifyPassword(admin.PasswordHash, password); err != nil {
		s.Logger.Debug("login: password verify failed",
			"username", username,
			"pw_len", len(password),
			"err", err)
		s.renderLoginError(w, username, "用户名或密码错误")
		return
	}

	// 创建 session
	sessID, err := auth.GenerateSessionID()
	if err != nil {
		s.Logger.Error("generate session id", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	csrfToken, err := auth.GenerateCSRFToken()
	if err != nil {
		s.Logger.Error("generate csrf", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	sess := &store.Session{
		ID:        sessID,
		CSRFToken: csrfToken,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(s.SessionTTL).Unix(),
		UserAgent: r.UserAgent(),
	}
	if err := s.Store.CreateSession(r.Context(), sess); err != nil {
		s.Logger.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	auth.SetSessionCookie(w, sessID, s.Secure)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout POST /logout。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sessID := auth.GetSessionCookie(r)
	if sessID != "" {
		_ = s.Store.DeleteSession(r.Context(), sessID)
	}
	auth.ClearSessionCookie(w, s.Secure)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) renderLoginError(w http.ResponseWriter, username, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	_ = s.Templates.ExecuteTemplate(w, "login.html", loginPageData{
		Error:    msg,
		Username: username,
	})
}