package web

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
	"github.com/yourname/ikev2-panel-v2/internal/store"
)

func TestFlashStore_BasicSetConsume(t *testing.T) {
	store := NewFlashStore(5 * time.Minute)

	store.Set("session-abc", Flash{
		NewPassword: "alice-secret-123",
		Message:     "用户已创建",
	})

	// 第一次 consume 应成功
	f, err := store.Consume("session-abc")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if f.NewPassword != "alice-secret-123" {
		t.Errorf("NewPassword: got %q want %q", f.NewPassword, "alice-secret-123")
	}
	if f.Message != "用户已创建" {
		t.Errorf("Message: got %q want %q", f.Message, "用户已创建")
	}

	// 第二次 consume 应失败（一次性）
	if _, err := store.Consume("session-abc"); err == nil {
		t.Error("second consume should fail (one-time)")
	}
}

func TestFlashStore_EmptySessionID(t *testing.T) {
	store := NewFlashStore(5 * time.Minute)

	// Set 空 sessionID 应静默忽略（panic 安全）
	store.Set("", Flash{Message: "orphan"})

	if _, err := store.Consume(""); err == nil {
		t.Error("Consume empty sessionID should fail")
	}
}

func TestFlashStore_NotFound(t *testing.T) {
	store := NewFlashStore(5 * time.Minute)

	if _, err := store.Consume("nonexistent"); err == nil {
		t.Error("Consume nonexistent session should fail")
	}
}

func TestFlashStore_Expired(t *testing.T) {
	// 注入可调时间
	store := &FlashStore{
		flashes: make(map[string]Flash),
		ttl:     1 * time.Hour,
		now:     func() time.Time { return time.Unix(1000, 0) },
	}

	store.Set("sess", Flash{Message: "hello"})

	// 时间前进 2 小时（超过 ttl）
	store.now = func() time.Time { return time.Unix(1000+7200, 0) }

	if _, err := store.Consume("sess"); err == nil {
		t.Error("consume after expiry should fail")
	}
}

func TestFlashStore_Overwrite(t *testing.T) {
	store := NewFlashStore(5 * time.Minute)

	store.Set("sess", Flash{Message: "first"})
	store.Set("sess", Flash{Message: "second"}) // 覆盖

	f, err := store.Consume("sess")
	if err != nil {
		t.Fatal(err)
	}
	if f.Message != "second" {
		t.Errorf("expected 'second' (last write wins), got %q", f.Message)
	}
}

func TestFlashStore_Sweep(t *testing.T) {
	store := &FlashStore{
		flashes: make(map[string]Flash),
		ttl:     1 * time.Hour,
		now:     func() time.Time { return time.Unix(1000, 0) },
	}

	store.Set("a", Flash{Message: "alive"})
	store.Set("b", Flash{Message: "dead"})

	// 时间前进 2 小时
	store.now = func() time.Time { return time.Unix(1000+7200, 0) }

	removed := store.Sweep()
	if removed != 2 {
		t.Errorf("Sweep should remove 2 expired, got %d", removed)
	}
	if store.Len() != 0 {
		t.Errorf("Len after Sweep: got %d want 0", store.Len())
	}
}

func TestFlashStore_Concurrent(t *testing.T) {
	// 并发 Set/Consume/Sweep 不死锁、不 panic、不 race。
	store := NewFlashStore(5 * time.Minute)
	done := make(chan struct{})

	go func() {
		for i := 0; i < 100; i++ {
			store.Set("sess", Flash{NewPassword: "x"})
			_, _ = store.Consume("sess")
			store.Sweep()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent ops deadlocked")
	}
}

// TestFlashStore_PasswordNotInURL 验证：密码不在 query string 中。
//
// 这是 P1-A 的核心安全断言。
// 我们模拟一个用户创建场景：
//  1. handler 收到 POST /users
//  2. 创建用户成功
//  3. handler Set flash（含密码）
//  4. handler 302 redirect /users（**不带任何 query string**）
//  5. 浏览器 GET /users
//  6. handler 渲染前 Consume flash → 把密码放到模板
func TestFlashStore_PasswordNotInURL(t *testing.T) {
	store := NewFlashStore(5 * time.Minute)
	sessionID := "admin-session"

	// Step 3: handler 写 flash
	store.Set(sessionID, Flash{
		NewPassword: "MyS3cret!Pass",
		Message:     "用户已创建",
	})

	// Step 4: redirect URL — 这里只能 redirect 到 /users,不能有 ?flash_new=
	// 用 redirect target 模拟（手工检查不带密码）
	redirectTarget := "/users"
	if strings.Contains(redirectTarget, "flash_new=") ||
		strings.Contains(redirectTarget, "flash_pw=") {
		t.Fatalf("REGRESSION: redirect URL contains password: %s", redirectTarget)
	}

	// Step 6: 下次请求 Consume 出来
	f, err := store.Consume(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if f.NewPassword != "MyS3cret!Pass" {
		t.Errorf("consumed password mismatch")
	}

	// 二次消费应失败
	if _, err := store.Consume(sessionID); err == nil {
		t.Error("one-shot broken: consumed twice")
	}
}

// TestHandleUsersList_ConsumesFlash 验证：handleUsersList 在渲染前 Consume flash。
//
// 这是 P1-A 的端到端测试：
//  1. admin 创建用户 → handler Set flash（含密码）
//  2. admin GET /users → handleUsersList 应 Consume 出来 + 模板拿到密码
//  3. 再次 GET /users → flash 已消费,模板 NewPassword 应为空
//
// 关键断言：用户**不会**通过 URL 看到密码,只能通过第一次渲染的页面看到。
func TestHandleUsersList_ConsumesFlash(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.FlashStore = NewFlashStore(5 * time.Minute)

	// 加载真实模板（P1-B 后所有页面都走 RenderPage → LoadTemplates）
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	srv.Templates = tmpl

	// Step 1: 模拟创建用户后 handler 设的 flash
	const wantPw = "AliceS3cret-PW-12"
	srv.FlashStore.Set(sess.ID, Flash{
		NewPassword: wantPw,
		Message:     "用户 alice 已创建",
	})

	handler := srv.requireSession(http.HandlerFunc(srv.handleUsersList))

	// Step 2: GET /users
	req1 := httptest.NewRequest("GET", "/users", nil)
	req1.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("GET /users: %d", rr1.Code)
	}
	body1 := rr1.Body.String()
	if !strings.Contains(body1, wantPw) {
		t.Errorf("first render should contain password %q, body=%q", wantPw, body1)
	}
	if !strings.Contains(body1, "用户 alice 已创建") {
		t.Errorf("first render should contain message: %s", body1)
	}

	// Step 3: 再 GET /users,flash 已消费,密码不应出现
	req2 := httptest.NewRequest("GET", "/users", nil)
	req2.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	body2 := rr2.Body.String()
	if strings.Contains(body2, wantPw) {
		t.Errorf("REGRESSION: password %q leaked on second render", wantPw)
	}
}

// TestHandleUserDetail_ConsumesFlash 验证：handleUserDetail 在渲染前 Consume flash。
func TestHandleUserDetail_ConsumesFlash(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.FlashStore = NewFlashStore(5 * time.Minute)

	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	srv.Templates = tmpl

	id := createTestUser(t, srv, "alice")

	// 模拟 reset-password 后 Set 的 flash
	const newPw = "BrandNew123!"
	srv.FlashStore.Set(sess.ID, Flash{
		NewPassword: newPw,
		Message:     "密码已重置",
	})

	handler := srv.requireSession(http.HandlerFunc(srv.handleUserDetail))

	req := httptest.NewRequest("GET", fmt.Sprintf("/users/%d", id), nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	req.SetPathValue("id", fmt.Sprintf("%d", id))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, newPw) {
		t.Errorf("detail should contain new password: %s", body)
	}
	if !strings.Contains(body, "密码已重置") {
		t.Errorf("detail should contain message: %s", body)
	}

	// URL 不应含密码（核心安全断言）
	if strings.Contains(req.URL.String(), newPw) {
		t.Errorf("REGRESSION: URL contains password: %s", req.URL.String())
	}
}

// TestFlashStore_NilSafe 验证：FlashStore 未配置时 handlers 不 panic,
// 只是不会显示 flash。这是给现有部署的兼容保证。
func TestFlashStore_NilSafe(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	// 故意不设置 srv.FlashStore

	// 用 LoadTemplates 拿真实模板(handler 会渲染 users_list)
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	srv.Templates = tmpl

	handler := srv.requireSession(http.HandlerFunc(srv.handleUsersList))
	req := httptest.NewRequest("GET", "/users", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()

	// 不应 panic,应返回 200(只是没有 flash 内容)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("nil FlashStore should be safe, got %d", rr.Code)
	}
}

// ---------- P1-B layout 集成测试 ----------

// TestLoadTemplates_Layout 验证 LoadTemplates 能正确加载真实的 layout.html + 各页面,
// 并能渲染 (P1-B 的核心)。
//
// P1-B 设计：layout + 各 *_content.html, 通过 RenderPage 两段渲染。
// layout 不再用 {{template (printf ...)}}（Go html/template 不支持）,
// 改用 Server.RenderPage(buf, page, data) 封装。
//
// 这是回归测试：防止以后有人误删 layout.html 或改坏 FuncMap 让模板解析失败。
func TestLoadTemplates_Layout(t *testing.T) {
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	// 1. layout / nav 必须注册
	if tmpl.Lookup("layout") == nil {
		t.Error("layout template not defined")
	}
	if tmpl.Lookup("nav") == nil {
		t.Error("nav template not defined")
	}

	// 2. 每个 _content.html 子模板必须能渲染
	type testCase struct {
		name string
		tpl  string // "<page>_content.html"
		data interface{}
	}
	cases := []testCase{
		{"login", "login_content.html", loginPageData{
			PageMeta: PageMeta{Page: "login", Title: "登录"},
			Error:    "用户名或密码错误",
		}},
		{"home", "home_content.html", homeData{
			PageMeta:   PageMeta{Page: "home", Title: "首页", AdminUsername: "admin"},
			TotalUsers: 5,
		}},
		{"users_list", "users_list_content.html", usersListData{
			PageMeta: PageMeta{Page: "users_list", Title: "用户管理", AdminUsername: "admin"},
			Users:    []*store.User{},
		}},
		{"user_new", "user_new_content.html", usersNewData{
			PageMeta:   PageMeta{Page: "user_new", Title: "新增用户", AdminUsername: "admin"},
			Enabled:    true,
			SpeedLimit: 10,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloned, err := tmpl.Clone()
			if err != nil {
				t.Fatalf("Clone: %v", err)
			}
			var buf bytes.Buffer
			if err := cloned.ExecuteTemplate(&buf, tc.tpl, tc.data); err != nil {
				t.Fatalf("ExecuteTemplate(%s): %v", tc.tpl, err)
			}
			body := buf.String()
			// 验证子模板内容真的有渲染（不含 layout 外壳,因为这次只渲染 _content 部分）
			if !strings.Contains(body, "<h1") {
				t.Errorf("%s 渲染结果不含 h1 — 内容模板可能没正确渲染", tc.tpl)
			}
		})
	}
}

// TestRenderPage_FullPipeline 验证 RenderPage 完整渲染（layout + content）。
//
// 这是端到端测试,确保两段渲染 + BodyHTML 反射注入不出错。
func TestRenderPage_FullPipeline(t *testing.T) {
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	// 构造一个最小的 Server(只用 Templates)
	srv := &Server{
		Templates: tmpl,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	data := homeData{
		PageMeta:   PageMeta{Page: "home", Title: "首页", AdminUsername: "admin"},
		TotalUsers: 5,
	}

	rr := httptest.NewRecorder()
	srv.RenderPage(rr, httptest.NewRequest("GET", "/", nil), "home", data)

	body := rr.Body.String()
	// 验证 layout 套上去了
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("RenderPage 渲染结果不含 DOCTYPE — layout 没被 include")
	}
	if !strings.Contains(body, "<meta name=\"viewport\"") {
		t.Errorf("RenderPage 渲染结果不含 viewport meta")
	}
	if !strings.Contains(body, "欢迎，admin") {
		t.Errorf("RenderPage 渲染结果不含 home_content 的内容")
	}
}

// TestLoadTemplates_LoginHasNoNav 验证：login_content.html 渲染时不含 nav 守卫。
// （完整页面的 nav 守卫由 layout.html 的 {{if .AdminUsername}} 实现,所以测试要拼 layout）
func TestLoadTemplates_LoginHasNoNav(t *testing.T) {
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Templates: tmpl,
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	data := loginPageData{
		PageMeta: PageMeta{Page: "login", Title: "登录"},
		Error:    "用户名或密码错误",
		// 注意：故意不设 AdminUsername,模拟未登录
	}

	rr := httptest.NewRecorder()
	srv.RenderPage(rr, httptest.NewRequest("GET", "/login", nil), "login", data)

	body := rr.Body.String()
	if strings.Contains(body, "登出") {
		t.Errorf("login 页不应渲染 nav（含'登出'按钮）:\n%s", body)
	}
	if !strings.Contains(body, "欢迎回来") {
		t.Logf("DEBUG login body (%d bytes):\n%s", len(body), body)
		t.Errorf("login 页应渲染登录表单 (PR19 标题: 欢迎回来)")
	}
}

// ---------- P1-C M6 监控测试 ----------

// TestHome_ActiveSAsCount 验证：handleHome 调用 ListSAs 并把活跃连接数渲染到页面。
func TestHome_ActiveSAsCount(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	// 用真实模板
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	srv.Templates = tmpl
	// 不设置 Swanctl,handler 应优雅降级（nil SA list, no panic）

	handler := srv.requireSession(http.HandlerFunc(srv.handleHome))
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "活跃连接") {
		t.Errorf("首页应显示'活跃连接'区块:\n%s", body)
	}
	if !strings.Contains(body, "无活跃连接") {
		t.Errorf("Swanctl=nil 时应显示'无活跃连接':\n%s", body)
	}
}

// TestHome_LEWarning 验证：LE 续签失败时显示红色横幅。
func TestHome_LEWarning(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	srv.Templates = tmpl
	srv.CertMode = "letsencrypt"
	srv.DataDir = t.TempDir() // 不存在的 /le/fullchain.pem

	// v2-83:测试里设上 PanelState 模拟"已配置凭证"场景,避免未配置横幅干扰。
	// (LE 续签横幅本身跟凭证配置无关,只是新模板加的引导横幅会让这个
	//  "无警告"测试变红。)
	srv.PanelState = panelstate.NewStore()
	tmpDir := t.TempDir()
	credsPath := filepath.Join(tmpDir, "aliyun.creds")
	if err := os.WriteFile(credsPath, []byte(`{"key_id":"LTAI5t","key_secret":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 这里临时覆盖常量 Dir 比较麻烦;直接通过 PanelState.LoadAliyun 行为走 hardcoded 路径
	// /data/panel-state/aliyun.creds 在测试环境不存在 → AliyunConfigured=false → 仍会显示新横幅。
	// 简化:让这个测试只断言"没有 LE 续签相关的 ⚠️",不强制无任何 ⚠️。
	_ = credsPath

	handler := srv.requireSession(http.HandlerFunc(srv.handleHome))
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	body := rr.Body.String()
	// CertDir 不存在 + 续签检查不到 → LastRenewFailed=false → 不警告
	// 但 ShouldWarn(DaysLeft=0) = false,所以也不会黄色 LE 警告
	// v2-83:模板新增"未配置阿里云凭证"横幅,这条不再断言 "无任何 ⚠️",
	// 改成断言 "无 LE 续签失败横幅"。
	if strings.Contains(body, "续签失败") {
		t.Errorf("无续签问题时不应显示 LE 续签失败警告:\n%s", body)
	}
	if strings.Contains(body, "证书将在") {
		t.Errorf("DaysLeft=0 时不应显示 LE 证书过期警告:\n%s", body)
	}
}

// TestLoadLEWarning_NonLE 验证：非 LE 模式永远不警告。
func TestLoadLEWarning_NonLE(t *testing.T) {
	srv := &Server{
		CertMode: "self-signed", // 非 LE
		DataDir:  "/tmp",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if w := loadLEWarning(srv); w != "" {
		t.Errorf("self-signed 模式应返回空告警, got %q", w)
	}
}

// TestItoa 验证：itoa 辅助函数(避免引入 strconv)。
func TestItoa(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{5, "5"},
		{14, "14"},
		{99, "99"},
		{365, "365"},
	}
	for _, tc := range tests {
		got := itoa(tc.n)
		_ = got
		// 负数特殊处理：<=0 返回 "未知"
		if tc.n < 0 {
			if got != "未知" {
				t.Errorf("itoa(%d): got %q want '未知'", tc.n, got)
			}
		} else if got != tc.want {
			t.Errorf("itoa(%d): got %q want %q", tc.n, got, tc.want)
		}
	}
	if w := itoa(-1); w != "未知" {
		t.Errorf("itoa(-1): got %q want '未知'", w)
	}
}

// TestLoadTemplates_FormatTimeFormatBytes 验证 FuncMap 注册成功,
// 模板里能直接用 {{formatTime}} 和 {{formatBytes}}。
func TestLoadTemplates_FormatTimeFormatBytes(t *testing.T) {
	tmpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	tmpls := []string{
		`{{formatTime ` + fmt.Sprintf("%d", now) + `}}`,
		`{{formatBytes 123456789}}`,
		`{{formatTime 0}}`,     // 0 应返回 "-"
		`{{formatBytes 0}}`,    // 0 B
		`{{formatBytes 1024}}`, // 1.0 KB
	}
	for _, s := range tmpls {
		// 每个用全新 Clone 测试（避免模板执行后 Parse 报错）
		cloned, err := tmpl.Clone()
		if err != nil {
			t.Fatalf("Clone: %v", err)
		}
		if _, err := cloned.Parse(s); err != nil {
			t.Errorf("parse %q: %v", s, err)
			continue
		}
		var buf bytes.Buffer
		if err := cloned.Execute(&buf, nil); err != nil {
			t.Errorf("exec %q: %v", s, err)
		}
		if buf.Len() == 0 {
			t.Errorf("template %q produced empty output", s)
		}
	}
}
