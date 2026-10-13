package web

// P1/D3/F1 修复的回归防线(2026-10-13 第二轮审查)。
//
//	P1-1  mc 表单校验失败原本走 http.Error() → 浏览器显示裸文本页,
//	      用户丢失全部已填内容、无导航、英文错误串混进中文界面。
//	      实测可稳定触发(dns_servers 是 textarea,无任何前端兜底):
//	        dns_servers=999.1.1.1 → 400 text/plain
//	      修法:303 回详情页 + flash 报错 + 部分 opts 回填 + details 展开。
//
//	D3    fallbackCopy 吞掉 execCommand 异常并忽略其布尔返回值,
//	      无条件显示"已复制 ✓" —— 失败也报成功,Android 明文密码场景尤其危险。
//
//	P1-2  Android 复制按钮漏了 cert_type,而展示区恰用两行注释强调
//	      "不要选 RSA 证书",漏掉等于让用户靠猜。
//
//	P1-3  .terminal-card 与内部 pre 同时 overflow-x:auto → 嵌套双层滚动。
//
//	F1    .copy-btn 在 style.css 零定义,同一交互两页外观迥异。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
)

const p1TplDir = "../../web/templates"

// ---------- P1-1 ----------

// TestP1_MCOptsNoBareTextError mc 校验失败不得再返回裸文本页。
func TestP1_MCOptsNoBareTextError(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"非法 DNS IP", url.Values{"dns_servers": {"999.1.1.1"}}},
		{"DNS 超上限", url.Values{"dns_servers": {
			"1.1.1.1\n2.2.2.2\n3.3.3.3\n4.4.4.4\n5.5.5.5\n6.6.6.6\n7.7.7.7\n8.8.8.8"}}},
		{"保活间隔越界", url.Values{
			"nat_keepalive_enabled":  {"1"},
			"nat_keepalive_interval": {"9999"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, sess := newTestServerWithSession(t)
			srv.FlashStore = NewFlashStore(5 * time.Minute)
			id := createTestUser(t, srv, "mctest")

			form := c.form
			form.Set("csrf_token", csrfTokenOf(sess))

			handler := srv.requireSession(http.HandlerFunc(srv.handleUserMobileconfigOptionsSave))
			req := httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/users/%d/mobileconfig-options", id),
				strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
			req.SetPathValue("id", fmt.Sprintf("%d", id))

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusSeeOther {
				t.Fatalf("校验失败应返回 303 而非裸文本页,实际 %d;body=%q",
					rr.Code, p1Trunc(rr.Body.String()))
			}
			if loc := rr.Header().Get("Location"); !strings.HasSuffix(loc, fmt.Sprintf("/users/%d", id)) {
				t.Errorf("应重定向回用户详情页,实际 Location=%q", loc)
			}
			if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/plain") {
				t.Errorf("不应返回 text/plain,实际 %q", ct)
			}
		})
	}
}

// TestP1_MCOptsBackfillPreservesUserInput 校验失败必须保留用户已填的其他字段。
func TestP1_MCOptsBackfillPreservesUserInput(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.FlashStore = NewFlashStore(5 * time.Minute)
	id := createTestUser(t, srv, "mcbk")

	form := url.Values{
		"csrf_token":             {csrfTokenOf(sess)},
		"user_defined_name":      {"我的 VPN"},
		"dns_servers":            {"1.1.1.1\n8.8.8.8"},
		"nat_keepalive_enabled":  {"1"},
		"nat_keepalive_interval": {"9999"}, // 越界,触发失败
	}

	handler := srv.requireSession(http.HandlerFunc(srv.handleUserMobileconfigOptionsSave))
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/users/%d/mobileconfig-options", id),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	req.SetPathValue("id", fmt.Sprintf("%d", id))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("应 303,实际 %d", rr.Code)
	}

	f, err := srv.FlashStore.Consume(sess.ID)
	if err != nil {
		t.Fatalf("应写入 flash: %v", err)
	}
	if f.Kind != "error" {
		t.Errorf("flash Kind 应为 error,实际 %q", f.Kind)
	}
	if f.Message == "" {
		t.Error("flash 应带错误文案")
	}
	if f.MCOptsBackfill == "" {
		t.Fatal("校验失败时必须回填草稿,否则用户丢失全部输入")
	}
	if !strings.Contains(f.MCOptsBackfill, "我的 VPN") {
		t.Errorf("草稿应保留 user_defined_name,实际: %s", f.MCOptsBackfill)
	}
	if !strings.Contains(f.MCOptsBackfill, "1.1.1.1") || !strings.Contains(f.MCOptsBackfill, "8.8.8.8") {
		t.Errorf("草稿应保留 dns_servers 原值,实际: %s", f.MCOptsBackfill)
	}
}

// TestP1_BackfillRenderedInForm 回填草稿必须真的渲染进表单。
func TestP1_BackfillRenderedInForm(t *testing.T) {
	srv, sess := newTestServerWithSession(t)
	srv.FlashStore = NewFlashStore(5 * time.Minute)
	tmpl, err := LoadTemplates(p1TplDir, "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	srv.Templates = tmpl
	id := createTestUser(t, srv, "alice")

	srv.FlashStore.Set(sess.ID, Flash{
		Kind:           "error",
		Message:        "dns_servers 含非法 IP: \"999.1.1.1\"",
		MCOptsBackfill: `{"user_defined_name":"回填的名称","dns_servers":["999.1.1.1"]}`,
	})

	handler := srv.requireSession(http.HandlerFunc(srv.handleUserDetail))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/users/%d", id), nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	req.SetPathValue("id", fmt.Sprintf("%d", id))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "回填的名称") {
		t.Error("用户刚填的连接显示名应回填进表单")
	}
	if !strings.Contains(body, "999.1.1.1") {
		t.Error("用户刚填的 DNS(含非法值)应原样回填,让用户看到并修正")
	}
	if !strings.Contains(body, `data-invalid="1"`) {
		t.Error("校验失败时 details 应标红(data-invalid)")
	}
	if !strings.Contains(body, "<details") ||
		!strings.Contains(body, "open") {
		t.Error("校验失败时 details 应展开")
	}
	// 错误文案不应被转义成标签
	if strings.Contains(body, "&lt;") && strings.Contains(body, "dns_servers") {
		t.Error("错误文案被转义了")
	}
}

// TestP1_ParseOptsReturnsPartialOnError parse 出错时必须返回部分填充的 opts。
func TestP1_ParseOptsReturnsPartialOnError(t *testing.T) {
	form := url.Values{
		"user_defined_name":        {"保留我"},
		"ondemand_ssid":            {"MyWiFi"},
		"dead_peer_detection_rate": {"High"},
		"nat_keepalive_interval":   {"不是数字"},
	}

	opts, err := parseMobileConfigOptsFromForm(&form)
	if err == nil {
		t.Fatal("非法整数应返回 error")
	}
	if opts == nil {
		t.Fatal("出错时必须返回部分填充的 opts,不能是 nil")
	}
	if opts.UserDefinedName == nil || *opts.UserDefinedName != "保留我" {
		t.Errorf("应保留 user_defined_name,实际 %+v", opts.UserDefinedName)
	}
	if opts.OnDemandSSID != "MyWiFi" {
		t.Errorf("应保留 ondemand_ssid,实际 %q", opts.OnDemandSSID)
	}
	if opts.DeadPeerDetectionRate != "High" {
		t.Errorf("应保留 dead_peer_detection_rate,实际 %q", opts.DeadPeerDetectionRate)
	}
}

// TestP1_ParseOptsAcceptsGoodInput 正常输入不受重构影响。
func TestP1_ParseOptsAcceptsGoodInput(t *testing.T) {
	form := url.Values{
		"user_defined_name":        {"My VPN"},
		"dns_servers":              {"1.1.1.1\n8.8.8.8\n2606:4700:4700::1111"},
		"nat_keepalive_enabled":    {"1"},
		"nat_keepalive_interval":   {"60"},
		"disconnect_on_sleep":      {"0"},
		"include_all_networks":     {"1"},
		"exclude_local_networks":   {"1"},
		"on_demand_enabled":        {"1"},
		"dead_peer_detection_rate": {"High"},
	}

	opts, err := parseMobileConfigOptsFromForm(&form)
	if err != nil {
		t.Fatalf("正常输入不应报错: %v", err)
	}
	if opts.UserDefinedName == nil || *opts.UserDefinedName != "My VPN" {
		t.Errorf("user_defined_name 解析错误: %+v", opts.UserDefinedName)
	}
	if len(opts.DNSServers) != 3 {
		t.Errorf("dns_servers 应解析出 3 条,实际 %d: %v", len(opts.DNSServers), opts.DNSServers)
	}
	if opts.NATKeepaliveInterval == nil || *opts.NATKeepaliveInterval != 60 {
		t.Errorf("nat_keepalive_interval 应为 60,实际 %+v", opts.NATKeepaliveInterval)
	}
	if opts.DisconnectOnSleep == nil || *opts.DisconnectOnSleep {
		t.Errorf("disconnect_on_sleep 应解析为 false(指针非 nil),实际 %+v", opts.DisconnectOnSleep)
	}
	if opts.ExcludeLocalNetworks == nil || !*opts.ExcludeLocalNetworks {
		t.Errorf("exclude_local_networks 应为 true,实际 %+v", opts.ExcludeLocalNetworks)
	}
}

// ---------- D3 ----------

func TestD3_FallbackCopyReportsFailure(t *testing.T) {
	src := p1Read(t, filepath.Join(p1TplDir, "layout.html"))

	if strings.Contains(src, `try { document.execCommand('copy'); } catch (e) {}`) {
		t.Error("fallbackCopy 仍是吞异常的旧实现(D3 未修复)")
	}
	if !strings.Contains(src, "复制失败") {
		t.Error("fallbackCopy 缺少复制失败提示")
	}
	if !strings.Contains(src, "var origHTML = btn.innerHTML;") ||
		!strings.Contains(src, "btn.innerHTML = origHTML;") {
		t.Error("复制按钮文案还原应保留 innerHTML,否则子元素(如图标)会丢失")
	}
	if !strings.Contains(src, "t.style.position = 'fixed';") ||
		!strings.Contains(src, "t.style.opacity = '0';") {
		t.Error("临时 textarea 应 position:fixed + opacity:0,否则页面会闪动")
	}
	if !regexp.MustCompile(`ok\s*=\s*document\.execCommand\('copy'\)`).MatchString(src) {
		t.Error("fallbackCopy 应把 execCommand 返回值赋给变量再判断")
	}
}

// ---------- P1-2 ----------

func TestP1_AndroidCopyMatchesDisplay(t *testing.T) {
	src := p1Read(t, filepath.Join(p1TplDir, "android_content.html"))

	m := regexp.MustCompile(`data-copy-text="([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("未找到 data-copy-text")
	}
	content := strings.ReplaceAll(m[1], "&#10;", "\n")
	content = strings.ReplaceAll(content, "&#34;", `"`)

	for _, f := range []string{"name", "server", "ipsec_id", "username",
		"password", "ca_cert", "cert_type", "server_id"} {
		if !strings.Contains(content, f+" =") {
			t.Errorf("复制内容缺少字段 %q(P1-2 未修复)。当前内容:\n%s", f, content)
		}
	}
	if strings.Contains(content, "cert_type =") &&
		!strings.Contains(content, "不要选 RSA") {
		t.Error("cert_type 应保留「不要选 RSA 证书」提示,否则用户可能选错证书类型")
	}
	if regexp.MustCompile(`(?m)^\w+: `).MatchString(content) {
		t.Errorf("复制内容分隔符应统一为 \" = \",与展示区一致:\n%s", content)
	}
}

// ---------- P1-3 + F1 ----------

func TestP1_TerminalCardSingleScroller(t *testing.T) {
	src := p1Read(t, "../../web/static/style.css")

	block := regexp.MustCompile(`\.terminal-card pre\s*\{[^}]*\}`).FindString(src)
	if block == "" {
		t.Fatal("未找到 .terminal-card pre 规则")
	}
	if strings.Contains(block, "overflow-x:auto") {
		t.Error(".terminal-card pre 不应再有 overflow-x:auto —— 与卡片构成嵌套双层滚动(P1-3 未修复)")
	}

	card := regexp.MustCompile(`\.terminal-card\s*\{[^}]*\}`).FindString(src)
	if !strings.Contains(card, "overflow-x:auto") {
		t.Error(".terminal-card 应保留 overflow-x:auto,否则长命令会被裁掉")
	}
}

func TestF1_CopyBtnIsDefined(t *testing.T) {
	src := p1Read(t, "../../web/static/style.css")

	if !regexp.MustCompile(`(?m)^\.copy-btn\s*\{[^}]*\}`).MatchString(src) {
		t.Error(".copy-btn 在 style.css 中无定义 —— 同一交互两页外观不一致(F1 未修复)")
	}
	if !strings.Contains(src, ".copy-btn-block") {
		t.Error("缺少 .copy-btn-block(Android 全宽复制按钮需要)")
	}
	// F1 附带:confirm 按钮不得再用不生效的 .btn-secondary:not([class]) 兜底
	// (该选择器语义是"没有 class 属性",永远匹配不到裸 <a class=...>)。
	// 先剥掉 CSS 注释,否则本文件里解释历史的那段注释会自己撞上断言。
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	if strings.Contains(stripped, ".btn-secondary:not([class])") {
		t.Error("`.btn-secondary:not([class])` 恒不匹配(元素本身就有 class),应删掉这个误导性兜底")
	}
}

// ---------- 模板层 ----------

func TestP1_MCOptsDetailsOpenOnInvalid(t *testing.T) {
	src := p1Read(t, filepath.Join(p1TplDir, "user_detail_content.html"))

	if !strings.Contains(src, `{{if .MCOptsInvalid}}open data-invalid="1"{{end}}`) {
		t.Error("mc 表单 <details> 应在校验失败时自动展开,否则用户看不到错误与回填值")
	}
	if !strings.Contains(src, "mc-opts-hint") {
		t.Error("缺少校验失败后的引导文案")
	}
}

// TestP1_MCDraftNotInListHandler mcDraft 不能污染列表页 handler。
//
// 修复过程中 mcDraft 曾被误加到 handleUsersList —— 那会让用户详情页
// 拿不到草稿(P1-1 静默失效),而列表页多一个用不到的变量。
func TestP1_MCDraftNotInListHandler(t *testing.T) {
	src := p1Read(t, "handlers_users.go")

	if blk := regexp.MustCompile(`(?s)func \(s \*Server\) handleUsersList.*?\n}\n`).FindString(src); blk != "" {
		if strings.Contains(blk, "mcDraft") {
			t.Error("handleUsersList 不应涉及 mcDraft(列表页没有 mc 表单)")
		}
	}
	if blk := regexp.MustCompile(`(?s)func \(s \*Server\) handleUserDetail.*?\n}\n`).FindString(src); blk != "" {
		if !strings.Contains(blk, "mcDraft") {
			t.Error("handleUserDetail 应读取 mcDraft 回填表单")
		}
	}
}

// ---------- 小工具 ----------

func p1Read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(raw)
}

func p1Trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
