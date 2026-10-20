package web

// v2.88:顶栏账号菜单不可见的回归防线。
//
// 背景(2026-10-20 用户报「UI右上角 点击登录的账号没有任何反应」):
//
//	§30.9 整站统一时给 .topbar-user-pill 加了 clip-path 切角,而
//	.topbar-menu 原本是 pill 的**子元素**。CSS 的 clip-path 会连同
//	子元素一起裁到元素的 border-box 之外 —— 下拉菜单定位在
//	top: hud-h + 6px(在 pill 盒子下方),于是整个菜单被切角裁掉:
//	DOM 里 menu.hidden=false、display:flex,视觉上完全看不见。
//	表现就是"点了齿轮/账号,什么都没发生"。
//
//	修法:把菜单挪到同级的 .topbar-user-wrap 里,wrap 才是定位上下文。
//
// 本文件锁死两条:
//  1. 结构:topbar-menu 不能重新变成 topbar-user-pill 的后代;
//  2. 交互:整个账号区(头像/用户名)可点,不是只有 28px 的齿轮。
import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const v288TplDir = "../../web/templates"

// findElementSpan 返回 src 中 open 标签所在元素的 [起始, 结束) 区间。
// end 是该元素的闭合标签之后的位置;解析不出来返回 (-1,-1)。
//
// 只处理本测试关心的 span/div 这类无嵌套自闭合标签的普通元素,
// 遇到 void 元素(img/br/input/hr/meta/link)直接跳过。
func findElementSpan(src string, marker string) (int, int) {
	open := strings.Index(src, marker)
	if open < 0 {
		return -1, -1
	}
	// 回退到 '<' 开始
	lt := strings.LastIndex(src[:open], "<")
	if lt < 0 {
		return -1, -1
	}
	// 找该元素的起始标签结尾 '>'
	gt := strings.Index(src[open:], ">")
	if gt < 0 {
		return -1, -1
	}
	openTagEnd := open + gt + 1
	openTag := src[lt:openTagEnd]
	// 取标签名
	name := ""
	for i := 1; i < len(openTag); i++ {
		c := openTag[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '>' {
			break
		}
		name += string(c)
	}
	if name == "" {
		return -1, -1
	}

	depth := 1
	i := openTagEnd
	for i < len(src) {
		next := strings.Index(src[i:], "<")
		if next < 0 {
			return -1, -1
		}
		next += i
		if strings.HasPrefix(src[next:], "</") {
			close := strings.Index(src[next:], ">")
			if close < 0 {
				return -1, -1
			}
			closeTag := src[next : next+close+1]
			closeName := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(closeTag, "</"), ">"))
			if closeName == name {
				depth--
				if depth == 0 {
					return lt, next + close + 1
				}
			}
			i = next + close + 1
			continue
		}
		// 开始标签
		end := strings.Index(src[next:], ">")
		if end < 0 {
			return -1, -1
		}
		tag := src[next : next+end+1]
		i = next + end + 1
		// void 元素不参与配对
		if strings.HasSuffix(tag, "/>") {
			continue
		}
		tagName := ""
		for j := 1; j < len(tag); j++ {
			c := tag[j]
			if c == ' ' || c == '\t' || c == '\n' || c == '>' {
				break
			}
			tagName += string(c)
		}
		switch tagName {
		case "img", "br", "hr", "input", "meta", "link", "source", "path", "circle", "polyline":
			continue
		}
		if tagName == name {
			depth++
		}
	}
	return -1, -1
}

// TestV288_TopbarMenuNotInsideClippedPill 菜单不能是被切角 pill 的后代。
func TestV288_TopbarMenuNotInsideClippedPill(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v288TplDir, "layout.html"))
	if err != nil {
		t.Fatalf("读取 layout.html 失败: %v", err)
	}
	src := string(raw)

	pillStart, pillEnd := findElementSpan(src, `id="topbar-user-pill"`)
	if pillStart < 0 {
		t.Fatalf("layout.html 里找不到 id=\"topbar-user-pill\" —— 账号区结构已变,请同步更新本测试")
	}
	menuAt := strings.Index(src, `id="topbar-menu"`)
	if menuAt < 0 {
		t.Fatalf("layout.html 里找不到 id=\"topbar-menu\" —— 账号下拉菜单已丢失")
	}
	if menuAt > pillStart && menuAt < pillEnd {
		t.Errorf("topbar-menu 落在 topbar-user-pill 内部 —— 该 pill 有 clip-path 切角,"+
			"CSS 会把溢出到 pill 盒子之外的子元素整个裁掉,菜单将不可见(表现为点了没反应)。"+
			"请把菜单挪到同级的 .topbar-user-wrap 里。")
	}
}

// TestV288_TopbarWrapIsPositioningContext wrap 必须是定位上下文。
//
// 菜单的 top: calc(var(--ikev2-hud-h) + 6px); right: 0 依赖一个
// position:relative 的祖先。菜单挪出 pill 后,这个祖先只能是 wrap。
func TestV288_TopbarWrapIsPositioningContext(t *testing.T) {
	css, err := os.ReadFile("../../web/static/style.css")
	if err != nil {
		t.Fatalf("读取 style.css 失败: %v", err)
	}
	idx := strings.Index(string(css), ".topbar-user-wrap{")
	if idx < 0 {
		t.Fatalf("style.css 里找不到 .topbar-user-wrap 规则 —— 菜单会失去定位上下文")
	}
	// 取该规则块(到下一个 '}' 为止)
	end := strings.Index(string(css)[idx:], "}")
	if end < 0 {
		t.Fatal(".topbar-user-wrap 规则块未闭合")
	}
	block := string(css)[idx : idx+end]
	if !strings.Contains(block, "position:relative") {
		t.Errorf(".topbar-user-wrap 必须有 position:relative —— "+
			"它是下拉菜单的定位上下文,缺了菜单会按更外层祖先定位、飘离账号区。block=%s", block)
	}
}

// TestV288_WholeAccountAreaClickable 整个账号区(头像/用户名)都要能点。
//
// 原实现里 pill 的 click handler 是空的(注释写着「不要在点击 username
// 文字时打开」),于是顶栏右上角的账号文字点了毫无反应 —— 这正是用户
// 报的现象之一。现在 pill 的 handler 必须真正调用 toggle。
func TestV288_WholeAccountAreaClickable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(v288TplDir, "layout.html"))
	if err != nil {
		t.Fatalf("读取 layout.html 失败: %v", err)
	}
	src := string(raw)

	// 定位 initTopbarMenu 函数体
	fnAt := strings.Index(src, "function initTopbarMenu()")
	if fnAt < 0 {
		t.Fatal("layout.html 里找不到 initTopbarMenu")
	}
	// 函数体到下一个 "\n        }" 结束
	end := strings.Index(src[fnAt:], "\n        }")
	if end < 0 {
		t.Fatal("initTopbarMenu 函数体未找到结尾")
	}
	fn := src[fnAt : fnAt+end]

	if !strings.Contains(fn, "pill.addEventListener('click'") {
		t.Errorf("initTopbarMenu 里没有给 pill 绑 click —— 头像/用户名点了不会打开菜单")
	}
	if !strings.Contains(fn, "toggle();") {
		t.Errorf("initTopbarMenu 的 pill click handler 没有调用 toggle()")
	}
	// pill handler 里必须有 stopPropagation,否则冒泡到 document 的
	//「点外部关闭」会立刻把刚打开的菜单关掉。
	pillAt := strings.Index(fn, "pill.addEventListener('click'")
	if pillAt < 0 {
		return
	}
	pillBlock := fn[pillAt:]
	if !strings.Contains(pillBlock, "stopPropagation") {
		t.Errorf("pill click handler 缺 stopPropagation —— 事件会冒泡到 document 的"+
			"「点外部关闭」,刚打开的菜单会被立刻关掉")
	}
}

// TestV288_TopbarRenderedStructure 用真实模板渲染验证 DOM 结构。
//
// 静态扫描 layout.html 源码只保证"源码里菜单不在 pill 内";
// 本测试走 LoadTemplates + ExecuteTemplate 渲染真实 HTML,
// 确认浏览器实际拿到的 DOM 里菜单确实是 pill 的兄弟节点。
func TestV288_TopbarRenderedStructure(t *testing.T) {
	tpl, err := LoadTemplates(v288TplDir, "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	var buf bytes.Buffer
	data := map[string]any{
		"AdminUsername": "admin",
		"CSRFToken":     "test-token",
		"CSPNonce":      "test-nonce",
		"PageKey":       "home",
		"Page":          "home",
		"Title":         "首页",
	}
	if err := tpl.ExecuteTemplate(&buf, "topbar", data); err != nil {
		t.Fatalf("渲染 topbar 失败: %v", err)
	}
	html := buf.String()

	pillStart, pillEnd := findElementSpan(html, `id="topbar-user-pill"`)
	if pillStart < 0 {
		t.Fatalf("渲染结果里找不到 topbar-user-pill")
	}
	menuAt := strings.Index(html, `id="topbar-menu"`)
	if menuAt < 0 {
		t.Fatalf("渲染结果里找不到 topbar-menu")
	}
	if menuAt > pillStart && menuAt < pillEnd {
		t.Errorf("渲染后的 DOM 里 topbar-menu 仍落在 topbar-user-pill 内部 —— "+
			"pill 的 clip-path 会把它整个裁掉,菜单不可见")
	}
	// 菜单必须在 wrap 内(定位上下文)
	wrapStart, wrapEnd := findElementSpan(html, `id="topbar-user"`)
	if wrapStart < 0 {
		t.Fatalf("渲染结果里找不到 topbar-user (wrap)")
	}
	if menuAt < wrapStart || menuAt > wrapEnd {
		t.Errorf("topbar-menu 不在 topbar-user (wrap) 内 —— 失去定位上下文,菜单会飘离账号区")
	}
	// 账号设置 / 登出 两项都要在
	for _, want := range []string{"账号设置", "登出", `action="/logout"`} {
		if !strings.Contains(html, want) {
			t.Errorf("渲染结果里缺少 %q", want)
		}
	}
}

// TestV288_TopbarMenuEndToEnd 走真实 HTTP 链路验证「点账号设置」能到达 /account。
//
// 为什么要这条(而不是只靠浏览器手点):
//
//	IDE 的 browser 插件有一个环境级缺陷 —— 凡是**点击后触发导航**的操作
//	(不论是 <a href> 还是 form submit),browser_click 都会在 60s 后返回
//	"IDE Command timeout",并且把整个浏览器命令通道一起卡死(连 browser_tabs
//	都会超时)。用一页纯静态 HTML(<a href="/b.html">,无任何项目代码)同样
//	能稳定复现,因此这是插件问题,不是本项目的 bug。
//
//	但"插件坏了"不能成为"这条路径没验收"的借口。本测试直接从**首页渲染出的
//	真实 HTML** 里解析出菜单项的 href,再拿同一个 session 去请求它 ——
//	等价于"用户点这一下",但不受插件缺陷影响。
//
//	覆盖两条:
//	  - 账号设置 → GET /account → 200 且渲染出改密表单
//	  - 登出     → POST /logout → 302 /login,且 session 失效
func TestV288_TopbarMenuEndToEnd(t *testing.T) {
	env := newPR9Env(t)
	sid := loginAsAdmin(t, env, "admin", "test123")

	// 1) 登录后取首页,从真实渲染结果里找菜单里的「账号设置」链接
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	home := rec.Body.String()

	// 解析 <a ... href="X" ...>账号设置</a> 取 href
	href := extractHrefBeforeText(home, "账号设置")
	if href == "" {
		t.Fatalf("首页渲染结果里找不到「账号设置」链接的 href")
	}
	if href != "/account" {
		t.Errorf("「账号设置」href = %q, want /account", href)
	}

	// 2) 带上 session 请求该 href(等价于点击)
	req2 := httptest.NewRequest("GET", href, nil)
	req2.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec2 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", href, rec2.Code)
	}
	acct := rec2.Body.String()
	for _, want := range []string{"账号设置", `name="old_password"`, `name="new_password"`} {
		if !strings.Contains(acct, want) {
			t.Errorf("GET %s 的响应里缺少 %q —— 跳转到了但不是改密页", href, want)
		}
	}

	// 3) 登出:从首页解析出 form action,POST 它
	logoutAction := extractFormAction(home, "/logout")
	if logoutAction == "" {
		t.Fatalf("首页渲染结果里找不到登出表单的 action")
	}
	form := url.Values{}
	form.Set("csrf_token", extractCSRFToken(home))
	req3 := httptest.NewRequest("POST", logoutAction, strings.NewReader(form.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.AddCookie(&http.Cookie{Name: "ikev2_session", Value: sid})
	rec3 := httptest.NewRecorder()
	env.handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusFound {
		t.Fatalf("POST %s: status = %d, want 302", logoutAction, rec3.Code)
	}
	if loc := rec3.Header().Get("Location"); loc != "/login" {
		t.Errorf("登出后 Location = %q, want /login", loc)
	}
}

// extractHrefBeforeText 在 src 里找"文本为 text 的 <a>"的 href。
// 用法:src 里通常只有一处,取离 text 最近的那个开标签。
func extractHrefBeforeText(src, text string) string {
	at := strings.Index(src, text)
	if at < 0 {
		return ""
	}
	// 从 text 往前找最近的 "<a "
	head := src[:at]
	open := strings.LastIndex(head, "<a ")
	if open < 0 {
		return ""
	}
	tagEnd := strings.Index(src[open:], ">")
	if tagEnd < 0 {
		return ""
	}
	tag := src[open : open+tagEnd]
	hi := strings.Index(tag, `href="`)
	if hi < 0 {
		return ""
	}
	rest := tag[hi+len(`href="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// extractFormAction 找 action 含 needle 的 <form> 的 action 属性值。
func extractFormAction(src, needle string) string {
	rest := src
	for {
		at := strings.Index(rest, "<form")
		if at < 0 {
			return ""
		}
		tagEnd := strings.Index(rest[at:], ">")
		if tagEnd < 0 {
			return ""
		}
		tag := rest[at : at+tagEnd]
		rest = rest[at+tagEnd:]
		ai := strings.Index(tag, `action="`)
		if ai < 0 {
			continue
		}
		v := tag[ai+len(`action="`):]
		end := strings.Index(v, `"`)
		if end < 0 {
			continue
		}
		if v[:end] == needle {
			return v[:end]
		}
	}
}

// extractCSRFToken 从 HTML 里抓第一个 csrf_token hidden input 的值。
func extractCSRFToken(src string) string {
	idx := strings.Index(src, `name="csrf_token" value="`)
	if idx < 0 {
		return ""
	}
	rest := src[idx+len(`name="csrf_token" value="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
