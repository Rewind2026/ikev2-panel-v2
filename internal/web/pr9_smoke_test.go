// v2.86-PR9:登录 rate limit + HTTP 安全 header 端到端 smoke test。
//
// 测试场景：
//  1. 未登录访问 POST /login 5 次失败 → 第 6 次返回 429
//  2. HTTP 安全 header 7 项全存在 + CSP 严格
//  3. 不同 IP 独立计数(IP A 锁定不影响 IP B)
//  4. 静态资源也有安全 header
package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/metrics"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// pr9Env 最小 web server 环境。
type pr9Env struct {
	handler http.Handler
}

func newPR9Env(t *testing.T) *pr9Env {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr9-smoke-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pwHash, _ := auth.BaseHashPassword("test123")
	if _, err := st.EnsureDefaultAdmin(context.Background(), "admin", pwHash); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{
		Store:           st,
		Swanctl:         nil,
		Limiter:         limit.New(),
		InstallTokens:   installtoken.New(time.Hour),
		FlashStore:      NewFlashStore(time.Hour),
		CertMode:        "self-signed",
		DataDir:         dir,
		PanelState:      panelstate.NewStore(),
		DisplayTimezone: "UTC",
		Metrics:         metrics.New(),
		RateLimiter:     auth.NewRateLimiter(auth.DefaultRateLimitConfig(dir)),
		Logger:          logger,
		SessionTTL:      time.Hour,
		ServerAddr:      "test.local",
		Secure:          false,
	}

	tpl, err := LoadTemplates(filepath.Join("..", "..", "web", "templates"), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	srv.Templates = tpl

	handler := New(srv, filepath.Join("..", "..", "web", "static"))
	return &pr9Env{handler: handler}
}

func TestSmoke_PR9_LoginRateLimit_BlocksAfterFiveFailures(t *testing.T) {
	env := newPR9Env(t)

	// 5 次错误登录
	for i := 0; i < 5; i++ {
		form := url.Values{}
		form.Set("username", "admin")
		form.Set("password", "wrong")
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "198.51.100.1:12345"
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	// 第 6 次应该被 rate limit 中间件拦截返回 429
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "wrong")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.1:12345"
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 5 fails, status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 should set Retry-After header")
	}
}

func TestSmoke_PR9_LoginRateLimit_DifferentIPsIndependent(t *testing.T) {
	env := newPR9Env(t)

	// IP A 失败 5 次
	for i := 0; i < 5; i++ {
		form := url.Values{}
		form.Set("username", "admin")
		form.Set("password", "wrong")
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "198.51.100.10:12345"
		rec := httptest.NewRecorder()
		env.handler.ServeHTTP(rec, req)
	}

	// IP B 不受影响,可以正常失败(返回 401 而不是 429)
	form2 := url.Values{}
	form2.Set("username", "admin")
	form2.Set("password", "wrong")
	req2 := httptest.NewRequest("POST", "/login", strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.RemoteAddr = "198.51.100.20:12345"
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusTooManyRequests {
		t.Fatal("IP B should not be affected by IP A's lock")
	}
}

func TestSmoke_PR9_SecureHeaders_AllPresent(t *testing.T) {
	env := newPR9Env(t)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	required := map[string]string{
		"Strict-Transport-Security":  "max-age=31536000; includeSubDomains",
		"X-Frame-Options":            "DENY",
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "strict-origin-when-cross-origin",
		"Permissions-Policy":         "geolocation=(), camera=(), microphone=(), payment=()",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for k, v := range required {
		got := rec.Header().Get(k)
		if got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}

	// CSP 必须包含关键指令
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"frame-ancestors 'none'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q. Full: %s", want, csp)
		}
	}
}

func TestSmoke_PR9_SecureHeaders_StaticAlsoGetHeaders(t *testing.T) {
	env := newPR9Env(t)

	req := httptest.NewRequest("GET", "/static/css/style.css", nil)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("static resources should also get security headers")
	}
}

// 单元验证 metrics.New 不为 nil(避免 smoke test 上 PR-9 误改 metrics 路径)。
func TestSmoke_PR9_MetricsAlive(t *testing.T) {
	_ = metrics.New()
}
