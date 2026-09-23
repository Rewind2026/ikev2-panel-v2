// v2.86-PR9:HTTP 安全 header 中间件。
//
// 审计 Top-10 #5 + OWASP Secure Headers Project:
//   - Strict-Transport-Security  强制 HTTPS(1 年 + 子域)
//   - X-Frame-Options             DENY(防 clickjacking)
//   - X-Content-Type-Options      nosniff(防 MIME sniff)
//   - Referrer-Policy             strict-origin-when-cross-origin(防 Referrer 泄漏)
//   - Permissions-Policy          关闭不需要的浏览器 API(geo/camera/mic)
//   - Content-Security-Policy     nonce-based。script/style 各有针对性策略:
//                                    script-src 'self' 'nonce-...'   每个请求随机 nonce,
//                                                              允许 inline <script nonce="...">
//                                                              (PR12.21 时代 CSP 只 'self',
//                                                              inline 被静默拒绝,所有交互失灵)
//                                    style-src  'self' 'unsafe-inline' 模板里有 inline style
//                                                              (保留旧行为)
//   - Cross-Origin-Opener-Policy  same-origin(防 XS-Leaks)
//
// CSP 设计权衡:
//   - default-src 'self'                    只允许本站资源
//   - script-src 'self' 'nonce-{随机}'      每响应独立 nonce (16B → 22 char base64),
//                                            inline <script nonce="{{.CSPNonce}}"> 允许,
//                                            没有 nonce 的 inline 一律拒绝。这是 XSS 防御核心。
//                                            nonce 由 secureHeaders 生成,通过 r.Context 传给 handler,
//                                            handler 再注入到 data.PageMeta.CSPNonce 给模板。
//   - style-src 'self' 'unsafe-inline'      允许内联 style(模板里有)
//   - img-src 'self' data:                  允许 base64 图(LE cert QR 等)
//   - frame-ancestors 'none'                防 clickjacking(等同 X-Frame-Options: DENY)
//   - base-uri 'self'                       防 base 标签劫持
//   - form-action 'self'                    防表单劫持到外站
//
// 不设:
//   - Public-Key-Pins (HPKP) 已 deprecated,Chrome 移除支持
//   - Expect-CT 已 deprecated
//
// 历史教训 (PR12.21 → PR-Redesign → nonce 化):
//   - PR12.21 layout.html 在 <head> 塞 80 行 inline script → CSP 'self' 静默拒绝 → 所有
//     按钮失灵,看起来"CSS 正常",易误诊为"DOMContentLoaded 时序"。
//   - PR-Redesign 把脚本挪到 /static/js/ui.js 走 <script defer> → CSP 兼容,但 ui.js
//     文件本身不是"自然存在",而是"按约束被迫存在"。
//   - 现在:CSP 加 nonce,JS 重新内联进 layout.html(每个响应一个 nonce),ui.js 可删。
//     这是最严格的 CSP 形式:'unsafe-inline' / 'self' 都不需要全开,nonce 还能防
//     "攻击者复用页面里已存在的合法 inline script"(nonce 不符即拒绝)。
package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

// ctxKeyCSPNonce 在 r.Context 里存 per-request nonce 的 key。
type ctxKeyCSPNonce struct{}

// CSPNonceFromCtx 从 ctx 取出本请求的 nonce,缺省返回空串(防御性)。
//
// 设计原因:nonce 必须在 secureHeaders 里生成、handler 里读出,
// 写小范围 防止跨请求重用(每个响应独立随机)。
func CSPNonceFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyCSPNonce{}).(string); ok {
		return v
	}
	return ""
}

// generateNonce 16 字节 (128 bit) base64 编码,CSP spec 要求 ≥128 bits entropy。
func generateNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

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

			// 每请求独立 nonce。rand.Read 失败 → 退回到 'self' (无 nonce),
			// 仍然是合法响应,inline 脚本此时仍会被拒(等同 CSP 'self' 行为)。
			nonce, err := generateNonce()
			if err != nil {
				nonce = ""
			}
			scriptSrc := "'self'"
			if nonce != "" {
				scriptSrc += " 'nonce-" + nonce + "'"
				// 把 nonce 挂到 ctx,handler 通过 CSPNonceFromCtx 读出后注入 PageMeta.CSPNonce。
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyCSPNonce{}, nonce))
			}
			h.Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src "+scriptSrc+"; "+
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