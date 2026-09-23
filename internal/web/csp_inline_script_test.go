// CSP 与 inline <script> 兼容性回归测试。
//
// 背景 (参见 secure_headers.go 注释):
//   - PR9 的 CSP 用 nonce 模式:script-src 'self' 'nonce-{per-request}'
//   - 浏览器解析到 inline <script>...</script> 块时,只执行 nonce 与 CSP header
//     一致的块,其它一律静默拒绝执行 (无 console 错误)
//   - PR12.21 期间 layout.html 用了 inline script 但没 nonce → CSP 拒绝 → 所有交互失灵
//   - 现在:layout.html 里 inline script 必须带 nonce="{{.CSPNonce}}",CSP 才能放行
//
// 本测试做的事:
//   1. 直接读 layout.html 文件源码(避免启 HTTP server)
//   2. 剥掉 Go template 注释 {{/* ... */}}(不会渲染到响应)
//   3. 扫所有 <script>...</script> 节点,过滤掉有 src 的(<script src="..."> 不需要 nonce)
//   4. 对每个 inline <script nonce="..."> 节点,检查是否带 nonce 属性
//   5. 如果有任何 inline script 缺 nonce — 测试 fail,提示真正根因
package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptNode 描述一个扫描到的 <script> 标签。
type scriptNode struct {
	body  string // 标签体 (空表示自闭合如 <script src="..."></script> 中空)
	hasSrc bool
	hasNonce bool
	attrs string // 整个 attrs 字符串,用于调试输出
}

// scanScriptNodes 返回 HTML 里所有 <script> 标签的扫描结果。
// 大小写不敏感,排除 Go template 注释 {{/* */}} 块里的伪标签。
func scanScriptNodes(html string) []scriptNode {
	stripped := stripGoTemplateComments(html)
	var out []scriptNode
	lower := strings.ToLower(stripped)
	i := 0
	for {
		open := strings.Index(lower[i:], "<script")
		if open < 0 {
			break
		}
		open += i
		end := strings.Index(stripped[open:], ">")
		if end < 0 {
			break
		}
		tagStart := open
		tagEnd := open + end + 1
		attrs := stripped[tagStart+len("<script") : open+end]
		attrsLower := strings.ToLower(attrs)
		hasSrc := strings.Contains(attrsLower, "src=")
		hasNonce := strings.Contains(attrsLower, "nonce=")

		close := strings.Index(lower[tagEnd:], "</script>")
		var body string
		if close < 0 {
			// 自闭合或异常,跳出
			break
		}
		body = stripped[tagEnd : tagEnd+close]
		out = append(out, scriptNode{
			body:     body,
			hasSrc:   hasSrc,
			hasNonce: hasNonce,
			attrs:    attrs,
		})
		i = tagEnd + close + len("</script>")
	}
	return out
}

// stripGoTemplateComments 移除 Go template 注释 {{/* ... */}}。
// 这些是给开发者看的,不会出现在渲染输出里。
func stripGoTemplateComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+4 <= len(s) && s[i:i+4] == "{{/*" {
			rest := s[i+4:]
			if j := strings.Index(rest, "*/}}"); j >= 0 {
				i += 4 + j + len("*/}}")
				continue
			}
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func findLayoutPath(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../web/templates/layout.html",
		"../../../web/templates/layout.html",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	t.Fatalf("layout.html not found; tried %v (cwd=%s)", candidates, mustGetwd())
	return ""
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

// TestLayoutInlineScriptsHaveNonce 验证 layout.html 里所有 inline <script> 都带 nonce 属性。
//
// CSP 模式 (script-src 'self' 'nonce-{per-request}') 要求每个 inline script 必须带
// 与响应 CSP header 一致的 nonce,否则被静默拒绝 → 所有 JS handler 失灵。
//
// 这是 PR12.21 bug 的回归保护:PR12.21 layout.html 有 inline script 但没 nonce,
// 按钮全部失灵,看起来 CSS 正常,易误诊。现在 nonce 是渲染时由 RenderPage 注入,
// layout.html 必须显式写出 nonce="{{.CSPNonce}}",否则测试 fail。
func TestLayoutInlineScriptsHaveNonce(t *testing.T) {
	path := findLayoutPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	html := string(raw)

	nodes := scanScriptNodes(html)
	if len(nodes) == 0 {
		t.Fatalf("layout.html 中没有 <script> 标签,topbar/dismissable/os-tabs/copy-btn 等 handler 都不会绑定")
	}

	badCount := 0
	for i, n := range nodes {
		if n.hasSrc {
			// <script src="..."> 不需要 nonce,合法
			continue
		}
		// inline <script> 必须带 nonce
		if !n.hasNonce {
			badCount++
			preview := strings.TrimSpace(n.body)
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			t.Errorf("inline <script> in layout.html is missing nonce= attribute (CSP script-src 'nonce-...' will silently refuse it, breaking all JS handlers). #%d attrs=%q body=%q", i, strings.TrimSpace(n.attrs), preview)
		}
	}
	if badCount > 0 {
		t.FailNow()
	}
}

// TestServerNoUiJiFile 验证 /static/js/ui.js 已被删除。
//
// nonce 化后,所有交互 JS 内联进 layout.html (带 nonce),ui.js 不再需要。
// 如果 PR 又加了 ui.js,本测试 fail,提示"CSP 已升级,JS 不应该再走外部文件"。
func TestServerNoUiJiFile(t *testing.T) {
	candidates := []string{
		"../../web/static/js/ui.js",
		"../../../web/static/js/ui.js",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("/web/static/js/ui.js still exists at %s. CSP has been upgraded to nonce-based (see secure_headers.go); all interaction JS is now inline in layout.html. Delete this file.", p)
		}
	}
}