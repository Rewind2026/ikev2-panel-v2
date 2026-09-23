// v2.85-PR8:端到端 e2e 烟测。
package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
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

// TestPR8_E2E_HealthzMetrics 端到端测 /healthz / /readyz / /metrics。
func TestPR8_E2E_HealthzMetrics(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("healthz body missing ok=true:\n%s", string(body))
	}
	if !strings.Contains(string(body), `"checks"`) {
		t.Errorf("healthz body missing checks field")
	}

	resp, _ = http.Get(ts.URL + "/readyz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("readyz: got %d, want 200", resp.StatusCode)
	}

	resp, _ = http.Get(ts.URL + "/metrics")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("metrics: got %d, want 200", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	out := string(body)
	for _, want := range []string{
		"vpn_active_sas",
		"ddns_last_sync_unixtime",
		"le_cert_expiry_unixtime",
		"http_requests_total",
		"http_request_duration_seconds",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// TestPR8_E2E_LoginMetrics 验证登录成功 / 失败都计数。
func TestPR8_E2E_LoginMetrics(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: noRedirect}

	// 1. 登录失败
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	resp, _ := client.PostForm(ts.URL+"/login", form)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("login fail: got %d, want 401", resp.StatusCode)
	}

	// 2. 登录成功
	form.Set("password", "test123")
	resp, _ = client.PostForm(ts.URL+"/login", form)
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("login success: got %d, want 302", resp.StatusCode)
	}

	// 3. metrics 含 success=1 + fail=1
	resp, _ = http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(body)
	if !strings.Contains(out, `panel_login_attempts_total{result="success"} 1`) {
		t.Errorf("login success counter missing:\n%s", out)
	}
	if !strings.Contains(out, `panel_login_attempts_total{result="fail"} 1`) {
		t.Errorf("login fail counter missing:\n%s", out)
	}
}

// TestPR8_E2E_OnboardingChecklist 验证 U05:TotalUsers=0 时显示 onboarding。
func TestPR8_E2E_OnboardingChecklist(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	form := url.Values{"username": {"admin"}, "password": {"test123"}}
	resp, _ := client.PostForm(ts.URL+"/login", form)
	resp.Body.Close()

	resp, _ = client.Get(ts.URL + "/")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, "首次部署引导") {
		t.Errorf("U05: onboarding checklist missing (TotalUsers=0):\n%s", firstLines(html, 100))
	}
	if !strings.Contains(html, `id="aliyun-card"`) {
		t.Errorf("U05: aliyun-card anchor missing")
	}
}

// TestPR8_E2E_DeleteConfirmPage 验证 U10:GET /users/{id}/delete 渲染确认页不真删。
func TestPR8_E2E_DeleteConfirmPage(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	// 直接通过 store 插入用户(跳过 Swanctl 依赖)
	id, err := e.store.CreateUser(context.Background(), &store.User{
		Username:       "bob",
		Password:       "test",
		Enabled:        true,
		SpeedLimitMbps: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	form := url.Values{"username": {"admin"}, "password": {"test123"}}
	resp, _ := client.PostForm(ts.URL+"/login", form)
	resp.Body.Close()

	// GET /users/{id}/delete → 渲染确认页
	resp, _ = client.Get(ts.URL + "/users/" + intToStr(id) + "/delete")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "确认删除用户") {
		t.Errorf("U10: confirm page not rendered:\n%s", string(body))
	}

	// DB 校验:bob 仍在(GET 不能删)
	u, err := e.store.GetUserByID(context.Background(), id)
	if err != nil {
		t.Errorf("U10: bob should still exist after GET confirm page, got err=%v", err)
	}
	if u != nil && u.Username != "bob" {
		t.Errorf("U10: bob username = %q, want bob", u.Username)
	}
}

func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestPR8_E2E_AuditPage 验证 U11:/audit 渲染 + audit_log 可读。
func TestPR8_E2E_AuditPage(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 预写一条 audit
	_ = e.store.WriteAudit(context.Background(), store.AuditEvent{
		Actor: "test", Event: "test.event", Details: "key=value",
	})

	form := url.Values{"username": {"admin"}, "password": {"test123"}}
	resp, _ := client.PostForm(ts.URL+"/login", form)
	resp.Body.Close()

	resp, _ = client.Get(ts.URL + "/audit")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	if !strings.Contains(html, "操作日志") {
		t.Errorf("U11: /audit page missing title")
	}
	if !strings.Contains(html, "test.event") {
		t.Errorf("U11: /audit missing test.event row")
	}
}

// TestPR8_E2E_HealthzJSON 验证 /healthz JSON 结构。
func TestPR8_E2E_HealthzJSON(t *testing.T) {
	e := newPR8Env(t)
	ts := httptest.NewServer(e.httpHandler)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/healthz")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// 检查 4 个 check 名都存在(db / vici / le / ddns)
	for _, key := range []string{`"db"`, `"vici"`, `"le"`, `"ddns"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("healthz body missing %s:\n%s", key, string(body))
		}
	}
}

// TestPR8_E2E_NormalizePath 验证 path normalization 防 metric cardinality 爆炸。
func TestPR8_E2E_NormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/users/123", "/users/{id}"},
		{"/users/3/delete", "/users/{id}/delete"},
		{"/users/3/delete/confirm", "/users/{id}/delete/confirm"},
		{"/", "/"},
		{"/audit", "/audit"},
		{"/users/123/mobileconfig", "/users/{id}/mobileconfig"},
	}
	for _, c := range cases {
		got := normalizePath(c.in)
		if got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// helpers

type pr8Env struct {
	store       *store.Store
	httpHandler http.Handler
}

func newPR8Env(t *testing.T) *pr8Env {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr8-smoke-*")
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

	tpl, err := LoadTemplates(filepath.Join("..", "..", "web", "templates"), filepath.Join("..", "..", "web", "static"), "UTC")
	if err != nil {
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
		Logger:          logger,
		SessionTTL:      time.Hour,
		ServerAddr:      "test.local",
		Secure:          false,
	}
	srv.Templates = tpl

	handler := New(srv, filepath.Join("..", "..", "web", "static"))
	return &pr8Env{store: st, httpHandler: handler}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n..."
}

// getCSRF 从 jar cookie 拿 csrf token。Cookie 名见 auth 包。
func getCSRF(jar *cookiejar.Jar, baseURL string) string {
	u, _ := url.Parse(baseURL)
	for _, c := range jar.Cookies(u) {
		if c.Name == "csrf_token" {
			return c.Value
		}
	}
	return ""
}

// noRedirect 阻止 http.Client 自动跟 redirect,以便测试 302 状态码。
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
