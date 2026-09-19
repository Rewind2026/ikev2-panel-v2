// Session 工具：cookie 名常量 + CSRF token 校验。
package auth

import (
	"net/http"
	"strings"
)

// CookieName session id cookie 名。
const CookieName = "ikev2_session"

// SessionHeaderCSRF 是 CSRF token 在请求中的 header 名（用于前端 JS 提交）。
// 模板里渲染的 CSRF token 也要走 form 字段 + header 二选一。
const SessionHeaderCSRF = "X-CSRF-Token"

// SessionFormCSRF form 字段名。
const SessionFormCSRF = "csrf_token"

// SetSessionCookie 写入 session cookie。
// 注意：Secure 标志需要 HTTPS；HttpOnly 阻止 JS 读取；SameSite=Lax 阻断跨站 POST。
func SetSessionCookie(w http.ResponseWriter, sessionID string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   0, // session cookie，关闭浏览器即失效（实际由 expires_at 决定）
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie 删除 session cookie。
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// GetSessionCookie 从请求读取 session id，未找到返回空串。
func GetSessionCookie(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// GetCSRFToken 从请求提取 CSRF token，优先 header，回退 form。
func GetCSRFToken(r *http.Request) string {
	if h := getHeaderCSRF(r); h != "" {
		return h
	}
	return r.PostFormValue(SessionFormCSRF)
}

func getHeaderCSRF(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(SessionHeaderCSRF))
}