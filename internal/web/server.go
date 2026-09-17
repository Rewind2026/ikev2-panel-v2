// HTTP server + middleware + handler 注册。
// 设计见 docs/design.md §3 + §7
package web

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/store"
	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// Server 依赖集合。
type Server struct {
	Store      *store.Store
	Swanctl    *swanctl.Manager
	Limiter    *limit.Limiter
	Templates  *template.Template
	SessionTTL time.Duration

	// HTTPS / 证书
	Cert          *tls.Certificate // 面板 HTTPS 证书（M5+）
	Secure        bool             // cookie Secure 标志 + QR URL 协议
	ServerAddr    string           // mobileconfig RemoteAddress（域名/IP，不带端口）
	PanelHost     string           // 扫码 URL host（域名 + 端口，如 ikev2.rewind2023.cn:8443）
	ServerCN      string // mobileconfig RemoteIdentifier
	CACertPEM     []byte // mobileconfig 内联 CA
	CertIncludeCA bool   // true = 自签模式；false = LE 模式
	// v2-76+：EAP-MSCHAPv2 模式。mobileconfig 的 AuthName/AuthPassword 从 user 对象取（per-user），
	// server-level 不再持有共享密钥。

	// 一次性安装 token（QR 扫码安装 mobileconfig）
	InstallTokens *installtoken.Store

	Logger *slog.Logger
}

// New 构造 HTTP handler。
func New(srv *Server, staticDir string) http.Handler {
	mux := http.NewServeMux()

	// 静态文件
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	// 公共路由
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /login", srv.handleLoginPage)
	mux.HandleFunc("POST /login", srv.handleLogin)
	mux.HandleFunc("GET /ca.cert.pem", srv.handleUserCACert)

	// 公共路由（无需登录）：一次性安装 token 消费
	// 扫码场景下用户没有账号密码也能装配置，跟登录态互斥
	mux.HandleFunc("GET /install/{token}", srv.handleInstallByToken)

	// 受保护路由（用中间件链）
	//   protect       = requireSession（防未登录访问）
	//   protectPOST   = protect + requireCSRF（防 CSRF 攻击）
	// GET 路由只用 protect；POST 路由用 protectPOST。
	// 设计见 docs/design.md §4.1 + §7.3
	protect := srv.requireSession
	protectPOST := func(h http.Handler) http.Handler {
		return protect(srv.requireCSRF(h))
	}
	mux.Handle("GET /{$}", protect(http.HandlerFunc(srv.handleHome)))
	mux.Handle("GET /users", protect(http.HandlerFunc(srv.handleUsersList)))
	mux.Handle("GET /users/new", protect(http.HandlerFunc(srv.handleUserNew)))
	mux.Handle("POST /users", protectPOST(http.HandlerFunc(srv.handleUserCreate)))
	mux.Handle("GET /users/{id}", protect(http.HandlerFunc(srv.handleUserDetail)))
	mux.Handle("GET /users/{id}/mobileconfig", protect(http.HandlerFunc(srv.handleUserMobileconfig)))
	mux.Handle("GET /users/{id}/mobileconfig.qr.png", protect(http.HandlerFunc(srv.handleUserMobileconfigQR)))
	mux.Handle("GET /users/{id}/sswan", protect(http.HandlerFunc(srv.handleUserSSwan)))
	// Android 11+ 原生客户端配置页（不走 sswan 导入，走系统 VPN 设置手动配 EAP-MSCHAPv2）
	mux.Handle("GET /users/{id}/android", protect(http.HandlerFunc(srv.handleUserAndroidConfig)))
	mux.Handle("POST /users/{id}/reset-password", protectPOST(http.HandlerFunc(srv.handleUserResetPassword)))
	mux.Handle("POST /users/{id}/enable", protectPOST(http.HandlerFunc(srv.handleUserEnable)))
	mux.Handle("POST /users/{id}/disable", protectPOST(http.HandlerFunc(srv.handleUserDisable)))
	mux.Handle("POST /users/{id}/delete", protectPOST(http.HandlerFunc(srv.handleUserDelete)))
	mux.Handle("POST /logout", protectPOST(http.HandlerFunc(srv.handleLogout)))

	return logging(srv.Logger)(mux)
}

// logging 简单请求日志中间件。
func logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(ww, r)
			log.Debug("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.status,
				"dur_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// statusRecorder 捕获响应状态码（用于日志）。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// handleHealthz 健康检查。
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// requireSession 中间件：未登录 302 → /login。
// 同时把 session + admin 写入 ctx。
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessID := auth.GetSessionCookie(r)
		if sessID == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		now := time.Now()
		sess, err := s.Store.GetSessionByID(r.Context(), sessID, now)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		admin, err := s.Store.GetDefaultAdmin(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeySession, sess)
		ctx = context.WithValue(ctx, ctxKeyAdmin, admin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF 中间件：受保护 POST 必须携带 CSRF token。
//
//   - token 来源：header X-CSRF-Token（前端 JS）或 form 字段 csrf_token（普通表单提交）
//   - 校验方式：与 session.CSRFToken 常量时间比较（防 timing attack）
//   - 缺失或不匹配：返回 403 Forbidden，不调用 next
//
// 设计见 docs/design.md §4.1 + §7.3
//
// 注意：本中间件必须在 requireSession 之后使用（依赖 session 已写入 ctx）。
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFrom(r.Context())
		if !ok || sess == nil {
			// requireSession 没跑或 session 无效——这属于编程错误
			http.Error(w, "internal error: no session in ctx", http.StatusInternalServerError)
			return
		}

		// ParseForm 让 PostFormValue 可用
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		token := auth.GetCSRFToken(r)
		if token == "" {
			http.Error(w, "csrf token missing", http.StatusForbidden)
			return
		}
		// 常量时间比较（防 timing attack）
		if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
			s.Logger.Warn("csrf token mismatch",
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
				"ua", r.UserAgent(),
			)
			http.Error(w, "csrf token mismatch", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}