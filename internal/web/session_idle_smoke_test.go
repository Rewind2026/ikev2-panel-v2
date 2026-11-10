// v2.103-PR(session-idle):滑动空闲超时 + 改密吊销其他会话 smoke test。
//
// 场景:
//  1. 两个浏览器(A/B)同时登录
//  2. 会话闲置超过空闲窗口 → 下一个请求 302 回登录页(而不是继续 200)
//  3. 重新登录 A 后改密 → B 的会话立即被吊销,A 保持登录
//  4. 改密成功后当前会话(A)仍可用 —— 踢自己只会丢 flash,没有安全增益
//
// 空闲窗口用 1s(测试内直改 Srv.IdleTTL,settingsRT 未接管时 idleTTL() 直落字段)。
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// getWithSession 带 session cookie GET,返回 recorder。
func getWithSession(env *pr9Env, path, sessionID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sessionID})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func TestSmoke_SessionIdleTimeout(t *testing.T) {
	env := newPR9Env(t)
	env.Srv.IdleTTL = time.Second // 空闲窗口 1s

	sid := loginAsAdmin(t, env, "admin", "test123")

	// 刚登录就访问:命中(last_seen = created_at)
	if rec := getWithSession(env, "/account", sid); rec.Code != http.StatusOK {
		t.Fatalf("刚登录访问 /account: status = %d, want 200", rec.Code)
	}

	// 闲置 1.5s > 窗口 1s → 302 回登录页
	time.Sleep(1500 * time.Millisecond)
	rec := getWithSession(env, "/account", sid)
	if rec.Code != http.StatusFound {
		t.Fatalf("空闲超窗后访问 /account: status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("重定向目标 = %q, want /login", loc)
	}

	// 关闭空闲(idleTTL=0)后同一会话立即恢复可用
	// —— 验证"窗口关掉就不会误伤",同时证明超窗拒绝没有销毁 session 行
	env.Srv.IdleTTL = 0
	if rec := getWithSession(env, "/account", sid); rec.Code != http.StatusOK {
		t.Fatalf("关闭空闲窗口后同一会话: status = %d, want 200", rec.Code)
	}
}

func TestSmoke_ChangePasswordRevokesOtherSessions(t *testing.T) {
	env := newPR9Env(t)

	// 浏览器 A(执行改密)与 B(只挂着不动)
	sidA := loginAsAdmin(t, env, "admin", "test123")
	sidB := loginAsAdmin(t, env, "admin", "test123")
	if sidA == sidB {
		t.Fatal("两个登录应产生不同 session")
	}

	// 改密前 B 正常
	if rec := getWithSession(env, "/account", sidB); rec.Code != http.StatusOK {
		t.Fatalf("改密前 B: status = %d, want 200", rec.Code)
	}

	// A 改密
	rec := postAccountPassword(t, env, sidA, "test123", "newsecret9", "newsecret9")
	if rec.Code != http.StatusFound {
		t.Fatalf("改密: status = %d, want 302", rec.Code)
	}

	// B 的会话必须已被吊销 → 302 回登录页
	if rec := getWithSession(env, "/account", sidB); rec.Code != http.StatusFound {
		t.Fatalf("改密后 B: status = %d, want 302 (其他会话应被吊销)", rec.Code)
	}
	// B 重新登录要用新密码:旧密码失效是改密的直接效果,顺带验证
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "test123")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.99:55555"
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("旧密码登录: status = %d, want 401", rec2.Code)
	}

	// A(当前会话)保持登录 —— 踢自己只会丢 flash 提示,没有安全增益
	if rec := getWithSession(env, "/account", sidA); rec.Code != http.StatusOK {
		t.Fatalf("改密后 A(当前会话): status = %d, want 200", rec.Code)
	}

	// 审计:改密与吊销都要留痕
	events, err := env.Store.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	foundChange, foundRevoke := false, false
	for _, e := range events {
		if e.Event == "admin.password.change" {
			foundChange = true
		}
		if e.Event == "admin.password.sessions_revoked" {
			foundRevoke = true
		}
	}
	if !foundChange {
		t.Error("审计里没有 admin.password.change 记录")
	}
	if !foundRevoke {
		t.Error("审计里没有 admin.password.sessions_revoked 记录")
	}
}
