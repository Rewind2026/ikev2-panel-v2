package web

import (
	"bytes"
	"strings"
	"testing"
)

// TestVerify_BannerEscapesUserInput 是 v2.86-pr23x 反射型 XSS 修复的回归测试。
//
// 背景(2026-10-08 复核确认):
//
//	layout.html 的 banner helper 曾写成 `{{.Msg | safeHTML}}`,而 safeHTML
//	直接返回 template.HTML,绕过 Go 模板的自动转义。由于全部 22 处 banner
//	调用点的 .Msg 都汇入此处,任何进入 .Msg 的用户输入都会被原样输出到
//	HTML —— 即便 handler 侧做了转义,模板层的转义层已被关闭。
//
// 已知可利用路径(修复前):
//
//	GET /admin/mobileconfig-defaults?error=<script>...
//	  → handlers_mobileconfig_admin.go: Error = r.URL.Query().Get("error")
//	  → admin_mobileconfig_defaults_content.html:181: banner(dict "Msg" .Error)
//	  → layout.html:802: {{.Msg | safeHTML}}   ← 注入点
//
// 本测试用真实模板 + 真实 banner helper 渲染,验证 HTML 已被转义。
func TestVerify_BannerEscapesUserInput(t *testing.T) {
	tpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	// 与实际攻击 payload 等价(XSS 向量:未转义的 < > 与属性注入)
	payloads := []string{
		`<script>alert(document.cookie)</script>`,
		`<img src=x onerror=alert(1)>`,
		`"><svg/onload=alert(1)>`,
		`<a href="javascript:alert(1)">x</a>`,
		// 阿里云 API 错误响应体场景(复核报告 #4)
		`{"Code":"InvalidAccessKeyId","Message":"<b>boom</b>"}`,
	}

	for _, payload := range payloads {
		var buf bytes.Buffer
		// banner helper 的 Msg 字段直接喂入 payload,
		// 与 handler 侧 Error/Flash 的真实数据流一致。
		err := tpl.ExecuteTemplate(&buf, "banner", map[string]any{
			"Kind": "error",
			"Msg":  payload,
		})
		if err != nil {
			t.Fatalf("渲染 banner 失败(payload=%q): %v", payload, err)
		}
		out := buf.String()

		// 只取 banner-body 的内容做断言 —— banner 自身含合法的内联 SVG
		// 图标(banner-icon),那是代码常量,不是注入点。
		body := extractBannerBody(out)
		if body == "" {
			t.Fatalf("payload=%q 未找到 banner-body,输出: %s", payload, out)
		}

		// 断言 1:banner-body 内不得出现可执行的原始标签
		for _, dangerous := range []string{"<script", "<img", "<svg", "<b>", "<a "} {
			if strings.Contains(strings.ToLower(body), dangerous) {
				t.Errorf("payload=%q 渲染后 banner-body 仍含危险标签 %q\nbanner-body: %s", payload, dangerous, body)
			}
		}
		// 断言 2:必须被转义为实体
		if !strings.Contains(body, "&lt;") {
			t.Errorf("payload=%q 未见转义实体 &lt;\nbanner-body: %s", payload, body)
		}
		// 断言 3:banner 本身应正常渲染(证明不是把整段吞掉)
		if !strings.Contains(out, `class="banner`) {
			t.Errorf("payload=%q banner 未正常渲染\n输出: %s", payload, out)
		}
	}
}

// extractBannerBody 取出 <div class="banner-body">...</div> 之间的内容。
// 用它把断言范围限定在用户数据上,排除 banner 自身的图标 SVG。
func extractBannerBody(html string) string {
	const open = `<div class="banner-body">`
	i := strings.Index(html, open)
	if i < 0 {
		return ""
	}
	rest := html[i+len(open):]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestVerify_BannerIconStillRenders 确认 $iconSvg(代码常量)仍走 safeHTML。
// 图标是内联 SVG,必须保持渲染 —— 这是本次修复不能误伤的既有功能。
func TestVerify_BannerIconStillRenders(t *testing.T) {
	tpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	// 复用 layout.html 中实际使用的图标常量
	icon := `<svg class="ico" viewBox="0 0 16 16"><path d="M8 1a7 7 0 1 0 0 14A7 7 0 0 0 8 1z"/></svg>`

	var buf bytes.Buffer
	err = tpl.ExecuteTemplate(&buf, "banner", map[string]any{
		"Kind":    "error",
		"Msg":     "普通提示",
		"IconSvg": icon,
	})
	if err != nil {
		t.Fatalf("渲染 banner 失败: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "<svg") {
		t.Errorf("图标 SVG 未渲染 —— safeHTML 被误删会破坏此功能\n输出: %s", out)
	}
	if !strings.Contains(out, "banner-icon") {
		t.Errorf("缺少 banner-icon 包裹层\n输出: %s", out)
	}
	if !strings.Contains(out, "普通提示") {
		t.Errorf("正常文案未渲染\n输出: %s", out)
	}
}

// TestVerify_AndroidFlashFixedTextOnly 回归 #2:android 页的 Flash 只能是固定文案。
//
// handler 曾把 ?flash= 原样透传给模板(再经 banner 的 .Msg 输出)。
// 修复后该 query 参数不再被读取,只有 ServerAddr 为空时才用固定提示。
// 这里直接对 banner helper 断言 —— 它是 android 页 Flash 的最终渲染路径。
func TestVerify_AndroidFlashFixedTextOnly(t *testing.T) {
	tpl, err := LoadTemplates("../../web/templates", "../../web/static", "UTC")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}

	// 修复后 handler 只会传这个固定文案
	const fixedMsg = "ServerAddr 未配置（容器未设置 IKEV2_SERVER_ADDR_V6/V4 或 IKEV2_DOMAIN）。"

	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "banner", map[string]any{
		"Kind": "info",
		"Msg":  fixedMsg,
	}); err != nil {
		t.Fatalf("渲染 banner 失败: %v", err)
	}

	body := extractBannerBody(buf.String())
	if !strings.Contains(body, "ServerAddr") {
		t.Errorf("固定提示文案未渲染\nbanner-body: %s", body)
	}
	// 确认没有任何标签被转义 —— 说明这是纯文本路径,未误伤显示
	if strings.Contains(body, "&lt;") {
		t.Errorf("纯文本提示被意外转义(可能引入了多余转义层)\nbanner-body: %s", body)
	}
}
