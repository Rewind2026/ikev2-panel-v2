package web

// P0 修复的回归防线。
//
// 背景(2026-10-13 审查):
//
//	P0-1  banner 的 .Msg 走 html/template 默认转义(这是 2026-10-08 XSS 事故后
//	      建立的防线,xss_banner_test.go 锁定)。但 8 处调用方把 HTML 字面量塞进
//	      .Msg,导致 <strong>/<a> 全部显示成可见文本,"前往账号设置"变死链。
//	      修法:新增 .MsgHTML(仅代码字面量)渲染富文本,含变量的调用方拆成
//	      banner(字面量) + 独立段落(默认转义)。
//
//	P0-2  CSP 是 script-src 'self' 'nonce-…',无 'unsafe-inline'/'unsafe-hashes',
//	      onclick/onsubmit 内联事件被浏览器拒绝 → 5 处破坏性操作的二次确认
//	      从未生效。修法:改用 data-confirm + nonce 脚本委托。
//
// 本文件锁死这两条边界,防止后续改动把它们改回去。

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const templatesDirForP0 = "../../web/templates"

// TestP0_NoInlineEventHandlers 模板里不允许再出现 onclick= / onsubmit=。
//
// CSP 不放行内联事件,留着它们等于"以为有确认、实际没有"。
func TestP0_NoInlineEventHandlers(t *testing.T) {
	entries, err := os.ReadDir(templatesDirForP0)
	if err != nil {
		t.Fatalf("读取模板目录失败: %v", err)
	}

	// 只匹配 HTML 标签内的真实属性位置:必须在 '<' 之后,且不在注释里。
	// 形如 <button ... onclick="...">。注释中解释历史用法的 onclick= 不算。
	re := regexp.MustCompile(`<[a-zA-Z][^<>]*?\son(click|submit|change|input|load|error)\s*=\s*["']`)
	// 去掉 HTML 注释后再扫,避免注释里的示例代码误报。
	comment := regexp.MustCompile(`(?s)<!--.*?-->`)

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		path := filepath.Join(templatesDirForP0, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		src := comment.ReplaceAllString(string(raw), "")
		for _, m := range re.FindAllString(src, -1) {
			found++
			t.Errorf("%s 仍有内联事件属性 %q —— CSP 无 'unsafe-inline'/'unsafe-hashes',该处理器不会执行。请改用 data-confirm 或事件委托。",
				e.Name(), strings.TrimSpace(m))
		}
	}
	if found == 0 {
		t.Log("模板中已无内联事件属性(P0-2 已修复)")
	}
}

// TestP0_DataConfirmPresent 确认破坏性操作确实挂上了 data-confirm。
//
// 这些操作一旦点错就直接生效:改密码、改密码、踢线、清覆盖项、恢复出厂。
func TestP0_DataConfirmPresent(t *testing.T) {
	cases := []struct {
		file   string
		needle string
		why    string
	}{
		{"user_detail_content.html", `data-confirm="确认重置`, "重置密码"},
		{"user_detail_content.html", `data-confirm="确认删除用户`, "删除用户"},
		{"user_detail_content.html", `data-confirm="确认清空所有覆盖项`, "清空描述文件覆盖项"},
		{"user_detail_content.html", `data-confirm="确认停用`, "停用账号(踢线)"},
		{"admin_mobileconfig_defaults_content.html", `data-confirm="确认恢复出厂默认值`, "恢复出厂默认值(影响全部用户)"},
	}

	for _, c := range cases {
		path := filepath.Join(templatesDirForP0, c.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		if !strings.Contains(string(raw), c.needle) {
			t.Errorf("%s 的「%s」操作缺少 data-confirm,二次确认会失效", c.file, c.why)
		}
	}
}

// TestP0_ConfirmGuardScriptExists layout 里有 initConfirmGuard 委托实现。
func TestP0_ConfirmGuardScriptExists(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(templatesDirForP0, "layout.html"))
	if err != nil {
		t.Fatalf("读取 layout.html 失败: %v", err)
	}
	src := string(raw)
	for _, needle := range []string{
		"function initConfirmGuard()",
		"initConfirmGuard();",
		"[data-confirm]",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("layout.html 缺少 %q —— data-confirm 不会被拦截", needle)
		}
	}
}

// TestP0_BannerMsgHTMLEscapesUserInput .MsgHTML 存在时也不能让 .Msg 变直出。
//
// 这是本次修复最容易出错的点:如果有人把 MsgHTML 和 Msg 的判断写反
// (例如无条件 safeHTML),XSS 防线就破了。这里用真实 payload 验证。
func TestP0_BannerMsgHTMLEscapesUserInput(t *testing.T) {
	tpl, err := LoadTemplates(templatesDirForP0, "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	payloads := []string{
		`<script>alert(document.cookie)</script>`,
		`<img src=x onerror=alert(1)>`,
		`"><svg/onload=alert(1)>`,
		`<a href="javascript:alert(1)">x</a>`,
	}

	for _, payload := range payloads {
		var buf bytes.Buffer
		err := tpl.ExecuteTemplate(&buf, "banner", map[string]any{
			"Kind": "error",
			"Msg":  payload,
			// 注意:同时给 MsgHTML 时,MsgHTML 优先。若 MsgHTML 为空,
			// payload 必须仍走 .Msg 的默认转义。
		})
		if err != nil {
			t.Fatalf("渲染 banner 失败(payload=%q): %v", payload, err)
		}
		body := extractBannerBody(buf.String())
		if body == "" {
			t.Fatalf("payload=%q 未找到 banner-body", payload)
		}
		if strings.Contains(body, "<script") || strings.Contains(body, "<img") ||
			strings.Contains(body, "<svg") || strings.Contains(body, "<a ") {
			t.Errorf("payload=%q 未被转义,banner-body 出现裸标签:\n%s", payload, body)
		}
	}
}

// TestP0_BannerMsgHTMLRendersLiteral 验证 MsgHTML 确实按富文本渲染。
//
// 这是 P0-1 的正向断言:改完之后 <strong> 必须是真的标签,不能是文本。
func TestP0_BannerMsgHTMLRendersLiteral(t *testing.T) {
	tpl, err := LoadTemplates(templatesDirForP0, "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	var buf bytes.Buffer
	err = tpl.ExecuteTemplate(&buf, "banner", map[string]any{
		"Kind":    "warn",
		"MsgHTML": `<strong>标题</strong><a href="/account">前往账号设置 →</a>`,
	})
	if err != nil {
		t.Fatalf("渲染 banner 失败: %v", err)
	}
	body := extractBannerBody(buf.String())
	if !strings.Contains(body, "<strong>标题</strong>") {
		t.Errorf("MsgHTML 的 <strong> 应作为标签渲染,实际:\n%s", body)
	}
	if !strings.Contains(body, `href="/account"`) {
		t.Errorf("MsgHTML 的链接 href 应保留,实际:\n%s", body)
	}
	if strings.Contains(body, "&lt;strong&gt;") {
		t.Errorf("MsgHTML 不该被转义,实际:\n%s", body)
	}
}

// TestP0_NoUserVarInBannerMsgHTML 防回归:MsgHTML 不得带模板变量插值。
//
// 安全边界:MsgHTML 只接受代码里的字面量。若有人在其中写 {{.Error}},
// 等于把用户输入 safeHTML。本测试扫描全部模板,把这条规则钉死。
func TestP0_NoUserVarInBannerMsgHTML(t *testing.T) {
	entries, err := os.ReadDir(templatesDirForP0)
	if err != nil {
		t.Fatalf("读取模板目录失败: %v", err)
	}

	re := regexp.MustCompile(`"MsgHTML"\s+"([^"]*)"`)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(templatesDirForP0, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			val := m[1]
			if strings.Contains(val, "{{") || strings.Contains(val, "printf") {
				t.Errorf("%s: MsgHTML 含模板插值 %q —— 必须只传代码字面量,否则等于对用户输入 safeHTML。请把动态部分拆到 banner 外的独立元素。",
					e.Name(), val)
			}
		}
	}
}

// TestP0_NoHTMLLeftInBannerMsg 最后一道:Msg 里不该再残留 HTML 标签。
//
// 这直接对应用户可见的症状(标签显示成文本)。允许的例外是纯文本里
// 不含 '<' 即可 —— 一旦 Msg 里带 '<',就是又踩回坑里。
func TestP0_NoHTMLLeftInBannerMsg(t *testing.T) {
	entries, err := os.ReadDir(templatesDirForP0)
	if err != nil {
		t.Fatalf("读取模板目录失败: %v", err)
	}

	// 抓 "Msg" 后紧跟的 printf/字面量内容
	rePrintf := regexp.MustCompile(`"Msg"\s+\(printf\s+"((?:[^"\\]|\\.)*)`)
	reLit := regexp.MustCompile(`"Msg"\s+"((?:[^"\\]|\\.)*)`)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(templatesDirForP0, e.Name()))
		if err != nil {
			continue
		}
		src := string(raw)
		for _, re := range []*regexp.Regexp{rePrintf, reLit} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				val := m[1]
				if strings.Contains(val, "<strong") || strings.Contains(val, "<small") ||
					strings.Contains(val, "<code") || strings.Contains(val, "<a ") ||
					strings.Contains(val, "<p ") {
					t.Errorf("%s: banner 的 Msg 里残留 HTML(%q) —— 会显示成字面文本。请改用 MsgHTML,或把动态内容拆到独立元素。",
						e.Name(), val)
				}
			}
		}
	}
}
