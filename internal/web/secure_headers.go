// v2.86-PR9:HTTP 安全 header 中间件。
//
// 审计 Top-10 #5 + OWASP Secure Headers Project:
//   - Strict-Transport-Security  强制 HTTPS(1 年 + 子域)
//   - X-Frame-Options             DENY(防 clickjacking)
//   - X-Content-Type-Options      nosniff(防 MIME sniff)
//   - Referrer-Policy             strict-origin-when-cross-origin(防 Referrer 泄漏)
//   - Permissions-Policy          关闭不需要的浏览器 API(geo/camera/mic)
//   - Content-Security-Policy     self-only(防 XSS)。style 允许 'unsafe-inline'
//                                  因为我们有些内联 style;script 用 self
//   - Cross-Origin-Opener-Policy  same-origin(防 XS-Leaks)
//
// CSP 设计权衡:
//   - default-src 'self'                    只允许本站资源
//   - script-src 'self'                     不允许 inline script(我们没用)
//   - style-src 'self' 'unsafe-inline'      允许内联 style(模板里有)
//   - img-src 'self' data:                  允许 base64 图(LE cert QR 等)
//   - frame-ancestors 'none'                防 clickjacking(等同 X-Frame-Options: DENY)
//   - base-uri 'self'                       防 base 标签劫持
//   - form-action 'self'                    防表单劫持到外站
//
// 不设:
//   - Public-Key-Pins (HPKP) 已 deprecated,Chrome 移除支持
//   - Expect-CT 已 deprecated
package web

import "net/http"

// secureHeaders 返回对所有响应注入安全 header 的中间件。
// 必须在 logging 之外(否则 header 写入顺序错乱)。
// 在 recoverPanic 之内(panic 也应该有 header)。
func secureHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			// HSTS:1 年 + 子域。生产推荐 preload(提交到 hstspreload.org),
			// 我们不自动 preload(让管理员决定)。
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; "+
					"font-src 'self'; "+
					"connect-src 'self'; "+
					"frame-ancestors 'none'; "+
					"base-uri 'self'; "+
					"form-action 'self'")
			next.ServeHTTP(w, r)
		})
	}
}