// v2.86-pr21:账号自助改密 smoke test。
//
// 测试场景：
//  1. GET /account 渲染表单
//  2. POST /api/account/password 缺字段 → 422 + 错误文案
//  3. POST 旧密码错 → 422 + "旧密码错误"
//  4. POST 新密码 <8 位 → 422
//  5. POST 两次新密码不一致 → 422
//  6. POST 正确流程 → 302 → /account,DB password_hash 更新,审计记录
//
// 这些测试需要在登录态下进行；为了让 handler 拿到 AdminFrom(ctx),
// 我们走 middleware 链：先 /login 拿 session cookie,再带 cookie 访问 /api/account/password。
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
)

// loginAsAdmin 模拟登录,返回 session cookie 字符串(可直接 set 到 req.Cookies)。
func loginAsAdmin(t *testing.T, env *pr9Env, username, password string) string {
	t.Helper()

	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.99:55555"
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: status = %d, want 302", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ikev2_session" {
			return c.Value
		}
	}
	t.Fatalf("login: no ikev2_session cookie set")
	return ""
}

// postAccountPassword 模拟提交改密表单,返回 response recorder。
//
// 自动带 session cookie + CSRF token(从 cookie 同步)。
func postAccountPassword(t *testing.T, env *pr9Env, sessionID, oldPw, newPw, confirmPw string) *httptest.ResponseRecorder {
	t.Helper()

	form := url.Values{}
	form.Set("old_password", oldPw)
	form.Set("new_password", newPw)
	form.Set("confirm_password", confirmPw)

	// CSRF:登录响应里返回的 session 对象的 CSRF token 通过 GetSession 拿不到明文,
	// 所以先 /account 渲染一次,从 HTML 里抓 csrf_token hidden input。
	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sessionID})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account: status = %d", rec.Code)
	}
	body := rec.Body.String()
	idx := strings.Index(body, `name="csrf_token" value="`)
	if idx < 0 {
		t.Fatalf("GET /account: no csrf_token input found in body")
	}
	rest := body[idx+len(`name="csrf_token" value="`):]
	end := strings.Index(rest, `"`)
	csrf := rest[:end]
	form.Set("csrf_token", csrf)

	req2 := httptest.NewRequest("POST", "/api/account/password", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "203.0.113.99:55555"
	req2.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sessionID})
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req2)
	return rec2
}

func TestSmoke_PR21_AccountPageRenders(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "账号设置") {
		t.Errorf("body missing 账号设置 heading")
	}
	if !strings.Contains(body, `name="old_password"`) {
		t.Errorf("body missing old_password input")
	}
	if !strings.Contains(body, `name="new_password"`) {
		t.Errorf("body missing new_password input")
	}
	if !strings.Contains(body, `name="confirm_password"`) {
		t.Errorf("body missing confirm_password input")
	}
}

func TestSmoke_PR21_ChangePassword_OldWrong(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	rec := postAccountPassword(t, env, sid, "WRONG", "newsecret", "newsecret")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "旧密码错误") {
		t.Errorf("body missing 旧密码错误 message: %s", rec.Body.String())
	}
}

func TestSmoke_PR21_ChangePassword_TooShort(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	rec := postAccountPassword(t, env, sid, "test123", "short", "short")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "至少 8 位") {
		t.Errorf("body missing 至少 8 位 message")
	}
}

func TestSmoke_PR21_ChangePassword_Mismatch(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	rec := postAccountPassword(t, env, sid, "test123", "newsecret", "different")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不一致") {
		t.Errorf("body missing 不一致 message")
	}
}

func TestSmoke_PR21_ChangePassword_SameAsOld(t *testing.T) {
	env := newPR9Env(t)
	// 改密前先把 admin 密码设成 ≥8 字符(默认 test123 是 7 字符),用新密码路径走完整。
	const oldPw = "oldpass1234" // 11 字符
	pwHash, _ := auth.BaseHashPassword(oldPw)
	env.Store.UpdateAdminPassword(context.Background(), "admin", pwHash)

	sid := loginAsAdmin(t, env, "admin", oldPw)

	rec := postAccountPassword(t, env, sid, oldPw, oldPw, oldPw)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "不能与旧密码相同") {
		t.Errorf("body missing 不能与旧密码相同 message; body = %s", rec.Body.String())
	}
}

func TestSmoke_PR21_ChangePassword_SuccessAndReLogin(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	rec := postAccountPassword(t, env, sid, "test123", "newsecret123", "newsecret123")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/account" {
		t.Errorf("Location = %q, want /account", loc)
	}

	// 旧密码应失败
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "test123")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.99:55555"
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("old password login: status = %d, want 401", rec2.Code)
	}

	// 新密码应成功
	form2 := url.Values{}
	form2.Set("username", "admin")
	form2.Set("password", "newsecret123")
	req3 := httptest.NewRequest("POST", "/login", strings.NewReader(form2.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.RemoteAddr = "203.0.113.99:55555"
	rec3 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusFound {
		t.Fatalf("new password login: status = %d, want 302", rec3.Code)
	}

	// 审计应记 admin.password.change
	if env.Store == nil {
		t.Fatal("env.Store nil — pr9Env 没暴露 store")
	}
	events, err := env.Store.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Event == "admin.password.change" && e.Actor == "admin" {
			found = true
			if !strings.Contains(e.Details, "username=admin") {
				t.Errorf("audit details = %q, want contain username=admin", e.Details)
			}
			break
		}
	}
	if !found {
		t.Errorf("admin.password.change audit event not found among %d events", len(events))
	}
}

// 防 unused import
var _ = context.Background
