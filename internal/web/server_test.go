package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// TestRequireCSRF_MissingToken 验证：受保护 POST 缺 csrf token → 403
func TestRequireCSRF_MissingToken(t *testing.T) {
	srv, sess := newTestServerWithSession(t)

	handler := srv.requireSession(srv.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})))

	// POST 不带 csrf_token
	body := url.Values{}
	body.Set("foo", "bar")
	req := httptest.NewRequest("POST", "/anything", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("missing token: got status %d want 403, body=%s", rr.Code, rr.Body.String())
	}
}

// TestRequireCSRF_WrongToken 验证：token 不匹配 → 403
func TestRequireCSRF_WrongToken(t *testing.T) {
	srv, sess := newTestServerWithSession(t)

	handler := srv.requireSession(srv.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should NOT be called when token mismatches")
	})))

	body := url.Values{}
	body.Set("csrf_token", "deadbeef-not-the-real-token")
	req := httptest.NewRequest("POST", "/anything", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("wrong token: got %d want 403", rr.Code)
	}
}

// TestRequireCSRF_HeaderToken 验证：通过 X-CSRF-Token header 也能通过校验
func TestRequireCSRF_HeaderToken(t *testing.T) {
	srv, sess := newTestServerWithSession(t)

	called := false
	handler := srv.requireSession(srv.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	body := url.Values{}
	body.Set("foo", "bar")
	req := httptest.NewRequest("POST", "/anything", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(auth.SessionHeaderCSRF, sess.CSRFToken)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("valid header token: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Error("handler was not called")
	}
}

// TestRequireCSRF_FormToken 验证：通过 csrf_token form 字段也能通过校验
func TestRequireCSRF_FormToken(t *testing.T) {
	srv, sess := newTestServerWithSession(t)

	called := false
	handler := srv.requireSession(srv.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	body := url.Values{}
	body.Set("csrf_token", sess.CSRFToken)
	body.Set("foo", "bar")
	req := httptest.NewRequest("POST", "/anything", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("valid form token: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Error("handler was not called")
	}
}

// TestRequireSession_NoCookie 验证：无 session cookie → 302 → /login
func TestRequireSession_NoCookie(t *testing.T) {
	srv := newTestServer(t)

	handler := srv.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should NOT be called when no session")
	}))

	req := httptest.NewRequest("POST", "/anything", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("no session: got %d want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("redirect target: got %q want /login", loc)
	}
}

// TestMobileconfigQR_InstallURL 验证：QR PNG 是有效的 image/png，
// QR 内容是一次性 install URL（scheme://panelHost/install/{token}）。
//
// 不能直接解 PNG 验证 URL（避免引入 GPL qrcode decoder 库），
// 而是用白盒：直接调 InstallTokens 看是否多了一个 token，
// 然后从 handler 抓 PNG 验证大小（短 URL QR 应该明显比 base64 data URL QR 小）。
func TestMobileconfigQR_InstallURL(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.ServerAddr = "ikev2.rewind2023.cn"
	srv.PanelHost = "ikev2.rewind2023.cn:8443"
	srv.ServerCN = "ikev2.rewind2023.cn"
	srv.InstallTokens = installtoken.New(time.Minute)

	id := createTestUser(t, srv, "testuser-qr")

	before := srv.InstallTokens.Len()

	handler := srv.requireSession(http.HandlerFunc(srv.handleUserMobileconfigQR))
	req := httptest.NewRequest("GET", fmt.Sprintf("/users/%d/mobileconfig.qr.png", id), nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	req.SetPathValue("id", fmt.Sprintf("%d", id))
	// 故意用内网 IP 访问面板，验证 QR 内容已不再依赖 r.Host
	req.Host = "192.168.50.63:8443"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type: got %q want image/png", ct)
	}

	// 短 URL（~70 字节）QR PNG 大概 1-3KB（256x256 Medium 容错）
	pngLen := rr.Body.Len()
	if pngLen < 200 || pngLen > 6000 {
		t.Errorf("QR PNG 大小异常: %d bytes (短 URL 应该在 200-6000 之间)", pngLen)
	}

	// 白盒断言：handler 应该 issue 了一个 token
	after := srv.InstallTokens.Len()
	if after != before+1 {
		t.Errorf("token 未被 issue: before=%d after=%d", before, after)
	}
}

// TestInstallHandler_OneShot 验证：QR → install URL → mobileconfig 全链路
func TestInstallHandler_OneShot(t *testing.T) {
	srv := newTestServer(t)
	srv.ServerAddr = "ikev2.rewind2023.cn"
	srv.ServerCN = "ikev2.rewind2023.cn"
	srv.InstallTokens = installtoken.New(time.Minute)

	id := createTestUser(t, srv, "alice")

	// 1) 模拟 QR handler issue token
	token := srv.InstallTokens.Issue(id)

	// 2) 模拟手机扫码访问 /install/{token}（无 cookie、无 session）
	req := httptest.NewRequest("GET", "/install/"+token, nil)
	req.SetPathValue("token", token)
	rr := httptest.NewRecorder()
	srv.handleInstallByToken(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("install: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}

	// Content-Type 必须是 iOS 认识的，否则不弹安装
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/x-apple-aspen-config") {
		t.Errorf("Content-Type 不对: got %q want application/x-apple-aspen-config", ct)
	}

	// body 必须是合法 mobileconfig XML
	body := rr.Body.String()
	if !strings.Contains(body, "<plist") || !strings.Contains(body, "<key>IKEv2</key>") {
		t.Errorf("body 不是 mobileconfig:\n%s", body)
	}
	if !strings.Contains(body, "alice") {
		t.Errorf("mobileconfig 不包含 username=alice:\n%s", body)
	}
	if !strings.Contains(body, "ikev2.rewind2023.cn") {
		t.Errorf("mobileconfig 不包含 server=域名:\n%s", body)
	}

	// 3) 第二次访问同 token → 404（一次性消费）
	req2 := httptest.NewRequest("GET", "/install/"+token, nil)
	req2.SetPathValue("token", token)
	rr2 := httptest.NewRecorder()
	srv.handleInstallByToken(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("第二次访问 token: got %d want 404", rr2.Code)
	}
}

// TestInstallHandler_InvalidToken 验证：无效 token 返回友好提示页（HTML 不是裸 404）
func TestInstallHandler_InvalidToken(t *testing.T) {
	srv := newTestServer(t)
	srv.InstallTokens = installtoken.New(time.Minute)

	req := httptest.NewRequest("GET", "/install/not-a-real-token", nil)
	req.SetPathValue("token", "not-a-real-token")
	rr := httptest.NewRecorder()
	srv.handleInstallByToken(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("invalid token: got %d want 404", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("失效页 Content-Type: got %q want text/html", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "链接失效") {
		t.Errorf("失效页应含中文提示'链接失效':\n%s", body)
	}
}

// TestMobileconfigXML_ContainsRemoteAddress 验证：mobileconfig XML 包含正确的 RemoteAddress（不带端口）
func TestMobileconfigXML_ContainsRemoteAddress(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.ServerAddr = "ikev2.rewind2023.cn" // LE 模式：域名，不带端口
	srv.PanelHost = "ikev2.rewind2023.cn:8443"
	srv.ServerCN = "ikev2.rewind2023.cn"

	handler := srv.requireSession(http.HandlerFunc(srv.handleUserMobileconfig))

	// 创建测试用户（mobileconfig 需要 username）
	hash, err := auth.BaseHashPassword("testpass")
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.Store.CreateUser(context.Background(), &store.User{
		Username:       "testuser-mc",
		Password:       hash,
		Enabled:        true,
		SpeedLimitMbps: 10,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := srv.Store.GetUserByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/users/%d/mobileconfig", u.ID), nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	req.SetPathValue("id", fmt.Sprintf("%d", u.ID))
	req.Host = "192.168.50.63:8443"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// 关键断言：RemoteAddress 是域名（不带端口）
	wantRemote := "<string>ikev2.rewind2023.cn</string>"
	if !strings.Contains(body, wantRemote) {
		t.Errorf("RemoteAddress 应该是域名（无端口）\n  got body: %s\n  want contains: %s", body, wantRemote)
	}

	// 关键断言：不带端口的域名（不应有 :8443 在 RemoteAddress 附近）
	if strings.Contains(body, "<string>ikev2.rewind2023.cn:8443</string>") {
		t.Errorf("RemoteAddress 不应包含端口:8443（IKEv2 走 UDP 500/4500）\n  body: %s", body)
	}

	// 关键断言：RemoteIdentifier 是 ServerCN
	if !strings.Contains(body, "<string>ikev2.rewind2023.cn</string>") {
		t.Errorf("RemoteIdentifier 应该是 ServerCN\n  body: %s", body)
	}

	// 关键断言：LE 模式不内联 CA（PayloadCertificateFileName 不应在 XML 中）
	if strings.Contains(body, "PayloadCertificateFileName") {
		t.Errorf("LE 模式不应内联 CA 证书\n  body: %s", body)
	}
}

// ---------- panic recover tests ----------

// TestRecoverPanic_HandlerPanic 验证：handler panic → 进程不挂 + 返回 500
func TestRecoverPanic_HandlerPanic(t *testing.T) {
	srv := newTestServer(t)

	// 故意 panic 的 handler
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	handler := recoverPanic(srv.Logger)(panicHandler)
	req := httptest.NewRequest("GET", "/anything", nil)
	rr := httptest.NewRecorder()

	// 关键断言 1：调用本身不 panic（证明 recover 生效）
	handler.ServeHTTP(rr, req)

	// 关键断言 2：返回 500
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("panic 后状态码: got %d want 500, body=%s", rr.Code, rr.Body.String())
	}

	// 关键断言 3：Connection: close（防止 keep-alive 在 panic 连接上复用）
	if got := rr.Header().Get("Connection"); got != "close" {
		t.Errorf("panic 后 Connection header: got %q want close", got)
	}

	// 关键断言 4：响应体简短不泄露内部信息
	body := rr.Body.String()
	if !strings.Contains(body, "internal server error") {
		t.Errorf("panic 后 body: got %q should contain 'internal server error'", body)
	}
	if strings.Contains(body, "boom") {
		t.Errorf("panic 信息泄露到响应: %s", body)
	}
}

// TestRecoverPanic_NextHandlerStillWorks 验证：recover 后后续请求不受影响
func TestRecoverPanic_NextHandlerStillWorks(t *testing.T) {
	srv := newTestServer(t)

	var shouldPanic = true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		if shouldPanic {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	handler := recoverPanic(srv.Logger)(mux)

	// 1) 第一次请求 panic
	req1 := httptest.NewRequest("GET", "/boom", nil)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusInternalServerError {
		t.Errorf("panic 请求: got %d want 500", rr1.Code)
	}

	// 2) 第二次请求正常
	shouldPanic = false
	req2 := httptest.NewRequest("GET", "/boom", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("后续请求: got %d want 200, body=%s", rr2.Code, rr2.Body.String())
	}
	if rr2.Body.String() != "OK" {
		t.Errorf("后续请求 body: got %q want OK", rr2.Body.String())
	}
}

// TestRecoverPanic_LoggedStack 验证：stack 写入日志（白盒：注入 buffer logger）
func TestRecoverPanic_LoggedStack(t *testing.T) {
	var buf strings.Builder
	srv := &Server{
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	handler := recoverPanic(srv.Logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("intentional test panic")
	}))

	req := httptest.NewRequest("GET", "/test/path", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	logged := buf.String()
	// 必须含 panic recovered 标记
	if !strings.Contains(logged, "panic recovered") {
		t.Errorf("日志缺 'panic recovered' 标记:\n%s", logged)
	}
	// 必须含 stack trace
	if !strings.Contains(logged, "intentional test panic") {
		t.Errorf("日志缺 panic 信息:\n%s", logged)
	}
	if !strings.Contains(logged, ".go") {
		t.Errorf("日志缺 stack 路径:\n%s", logged)
	}
	// 必须含请求路径和远程地址（便于排查是哪个请求触发的）
	if !strings.Contains(logged, "/test/path") {
		t.Errorf("日志缺 path:\n%s", logged)
	}
	if !strings.Contains(logged, "1.2.3.4") {
		t.Errorf("日志缺 remote:\n%s", logged)
	}
}

// ---------- trailing slash redirect tests ----------

// TestTrailingSlashRedirect v2.86-PR12.21:验证 /path/ 301 → /path。
//
// 背景:Go 1.22 ServeMux 严格区分 /users 与 /users/,iOS mobileconfig / 用户书签 /
// 客户端 VPN 起来后内部跳转经常带 trailing slash,导致 404。
func TestTrailingSlashRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("users-ok"))
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "user-%s", r.PathValue("id"))
	})
	handler := trailingSlashRedirect(mux)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantLoc    string // expected Location header (for 301)
		wantBody   string // expected body (for 200)
	}{
		{"trailing slash /users/", "/users/", http.StatusMovedPermanently, "/users", ""},
		{"trailing slash /users/4/", "/users/4/", http.StatusMovedPermanently, "/users/4", ""},
		{"no slash /users", "/users", http.StatusOK, "", "users-ok"},
		{"no slash /users/4", "/users/4", http.StatusOK, "", "user-4"},
		{"root /", "/", http.StatusNotFound, "", ""}, // mux 没注册 / → 404,trailingSlashRedirect 跳过不动
		{"query string preserved", "/users/?foo=bar", http.StatusMovedPermanently, "/users?foo=bar", ""},
		{"file ext .pem/ 不重定向", "/ca.cert.pem/", http.StatusNotFound, "", ""}, // .pem 含 .,跳过 redirect → mux 404
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if got := rr.Code; got != tt.wantStatus {
				t.Errorf("status: got %d want %d, body=%s", got, tt.wantStatus, rr.Body.String())
			}
			if tt.wantLoc != "" {
				if got := rr.Header().Get("Location"); got != tt.wantLoc {
					t.Errorf("Location: got %q want %q", got, tt.wantLoc)
				}
			}
			if tt.wantBody != "" {
				if got := rr.Body.String(); got != tt.wantBody {
					t.Errorf("body: got %q want %q", got, tt.wantBody)
				}
			}
		})
	}
}

// ---------- helpers ----------

// createTestUser 建一个测试用户，返回 userID。重复样板逻辑收敛在这里。
func createTestUser(t *testing.T, srv *Server, username string) int64 {
	t.Helper()
	hash, err := auth.BaseHashPassword("testpass")
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.Store.CreateUser(context.Background(), &store.User{
		Username:       username,
		Password:       hash,
		Enabled:        true,
		SpeedLimitMbps: 10,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	tmpDir := t.TempDir()
	st, err := store.Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Server{
		Store:      st,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Templates:  nil, // 测试不渲染模板
		SessionTTL: time.Hour,
	}
}

func newTestServerWithSession(t *testing.T) (*Server, *store.Session) {
	t.Helper()
	srv := newTestServer(t)

	// 创建默认 admin（requireSession 中间件会查）
	hash, err := auth.BaseHashPassword("testpassword")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store.EnsureDefaultAdmin(context.Background(), "admin", hash); err != nil {
		t.Fatalf("ensure admin: %v", err)
	}

	sessID, err := auth.GenerateSessionID()
	if err != nil {
		t.Fatal(err)
	}
	csrfToken, err := auth.GenerateCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sess := &store.Session{
		ID:        sessID,
		CSRFToken: csrfToken,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
		UserAgent: "test",
	}
	if err := srv.Store.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return srv, sess
}
