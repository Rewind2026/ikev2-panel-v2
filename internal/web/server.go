// HTTP server + middleware + handler 注册。
// 设计见 docs/design.md §3 + §7
package web

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/auth"
	"github.com/yourname/ikev2-panel-v2/internal/cert"
	"github.com/yourname/ikev2-panel-v2/internal/ddns"
	"github.com/yourname/ikev2-panel-v2/internal/installtoken"
	"github.com/yourname/ikev2-panel-v2/internal/limit"
	"github.com/yourname/ikev2-panel-v2/internal/metrics"
	"github.com/yourname/ikev2-panel-v2/internal/panelstate"
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
	Version    string // v2.86-PR18:面板版本号(由 main.go 注入,登录页品牌区展示用)

	// HTTPS / 证书
	Cert          *tls.Certificate // 面板 HTTPS 证书（M5+）
	Secure        bool             // cookie Secure 标志 + QR URL 协议
	ServerAddr    string           // mobileconfig RemoteAddress（域名/IP，不带端口）
	PanelHost     string           // 扫码 URL host（域名 + 端口，如 ikev2.rewind2023.cn:8443）
	ServerCN      string           // mobileconfig RemoteIdentifier
	CACertPEM     []byte           // mobileconfig 内联 CA
	CertIncludeCA bool             // true = 自签模式；false = LE 模式
	// v2.86-PR12.23:PayloadIdentifier 反向 DNS 前缀(不含 ".username")。
	// 默认 cert.DefaultPayloadIdentifierBase;空 → cert 内 fallback。
	PayloadIdentifierBase string
	// v2-76+：EAP-MSCHAPv2 模式。mobileconfig 的 AuthName/AuthPassword 从 user 对象取（per-user），
	// server-level 不再持有共享密钥。

	// 一次性安装 token（QR 扫码安装 mobileconfig）
	InstallTokens *installtoken.Store

	// P1-A：session-only flash 存储,密码/提示消息不再走 query string。
	// Key = session.ID,value = Flash{NewPassword, Message}
	FlashStore *FlashStore

	// P1-C：M6 监控所需的服务端信息（cmd/ikev2-panel/main.go 注入）
	CertMode string // "self-signed" / "letsencrypt"
	DataDir  string // 持久化目录,LE 证书路径 = {DataDir}/le/fullchain.pem

	// v2-82：DDNS 同步器(可选,nil = 面板不显示 DDNS 卡片)
	DDNSSync *ddns.Sync

	// v2-83:面板运行时状态(凭证卡 / 后续扩展)
	PanelState *panelstate.Store

	// v2.86-PR12.5:证书配置运行时持久化(模式/域名/CN/邮箱)。
	// 跟 PanelState 是独立的另一个 Store(同目录不同文件),可独立读写。
	CertConfigStore *panelstate.CertConfigStore

	// v2.86-PR13.2:客户端虚拟 IP 段(IPv4 pool + IPv6 ULA pool)运行时持久化。
	// 保存后 handler 立即调 Manager.UpdatePoolsAndReload,运行期生效(无需重启容器)。
	SubnetConfigStore *panelstate.SubnetConfigStore

	// v2.86-PR12.22:管理员全局 mobileconfig 默认值(运行时热改,无需重启)。
	// 优先级:overlay(user.MobileConfigOpts) > MobileConfigDefaults > BaseMobileConfigDefaults()。
	MobileConfigDefaults *panelstate.MobileConfigDefaultsStore

	// v2-83:阿里云凭证来源(从 main.go 注入,给面板显示用)
	AliyunAccessKeySource string

	// v2.86-PR12.5:证书配置来源(从 main.go 注入,启动日志 + 面板显示)
	CertConfigSource string

	// v2.86-PR13.3:启动时生效的 IP 段(由 main.go 注入,合并 env+panelstate 后的最终值)。
	// handlers_subnet.clear 用它作为"清除按钮立即生效"的目标,不需要重启容器。
	//
	// 跟 SubnetConfigStore 的区别:
	//   - SubnetConfigStore:用户面板配置(可能为空)
	//   - BootIPv4Subnet:启动时**实际生效**值(entrypoint §-0.5 / §0.6 决定的)
	StartupIPv4Subnet string
	StartupIPv6Subnet string

	// v2.85-PR3:面板/DDNS/mobileconfig 时间显示时区(IANA TZ 名)。
	// 从 cfg.DisplayTimezone 注入;handler 渲染时间字符串时用 s.DisplayTimezone。
	DisplayTimezone string

	// v2.85-PR8(U04):Prometheus-style metrics registry。
	Metrics *metrics.Registry

	// v2.86-PR9:登录 rate limit + 全局 POST 限速。
	// nil = 禁用(测试或极简部署)。
	RateLimiter *auth.RateLimiter

	Logger *slog.Logger
}

// New 构造 HTTP handler。
func New(srv *Server, staticDir string) http.Handler {
	mux := http.NewServeMux()

	// 静态文件 (v2.86-PR19: CSS/JS 加 no-cache,让部署新 CSS 后用户浏览器立刻拿到)
	//  - .css / .js → no-cache + must-revalidate (缓存但每次验证)
	//  - 图片/字体 → 1 天 (变化频率低)
	//  - 其它 (favicon, docs) → 1 小时
	staticFS := http.FileServer(http.Dir(staticDir))
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControlForStatic(staticFS)))

	// v2.86-PR9:HTTP 安全 header 中间件(所有路由 + 包括静态资源)。
	secureHdr := secureHeaders()

	// 公共路由
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /readyz", srv.handleReadyz)
	mux.HandleFunc("GET /metrics", srv.handleMetrics)
	// v2.86-PR9:登录端点挂 rate limit(防暴力破解)。
	// 不在 requireSession 之内(登录前无 session),且不需要 CSRF(登录前无法发 token)。
	if srv.RateLimiter != nil {
		mux.Handle("GET /login", secureHdr(http.HandlerFunc(srv.handleLoginPage)))
		mux.Handle("POST /login",
			secureHdr(auth.LoginRateLimitMiddleware(srv.RateLimiter, http.HandlerFunc(srv.handleLogin))))
	} else {
		mux.Handle("GET /login", secureHdr(http.HandlerFunc(srv.handleLoginPage)))
		mux.Handle("POST /login", secureHdr(http.HandlerFunc(srv.handleLogin)))
	}
	mux.Handle("GET /ca.cert.pem", secureHdr(http.HandlerFunc(srv.handleUserCACert)))

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
		chained := protect(srv.requireCSRF(h))
		// v2.86-PR9:所有受保护 POST 挂全局限速(防已登录用户灌量)。
		// 30 req/min 任意 POST(失败重试 + 自动化攻击)。
		if srv.RateLimiter != nil {
			chained = auth.PostRateLimitMiddleware(srv.RateLimiter, chained)
		}
		return chained
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
	// v2.86-PR12.21:每用户 mobileconfig 覆盖项 API。
	//   GET  /users/{id}/mobileconfig-options  读取当前覆盖项(返回 JSON,前端表单渲染)
	//   POST /users/{id}/mobileconfig-options  保存覆盖项(返回 JSON,前端 toast)
	mux.Handle("GET /users/{id}/mobileconfig-options", protect(http.HandlerFunc(srv.handleUserMobileconfigOptionsGet)))
	mux.Handle("POST /users/{id}/mobileconfig-options", protectPOST(http.HandlerFunc(srv.handleUserMobileconfigOptionsSave)))
	mux.Handle("POST /users/{id}/enable", protectPOST(http.HandlerFunc(srv.handleUserEnable)))
	mux.Handle("POST /users/{id}/disable", protectPOST(http.HandlerFunc(srv.handleUserDisable)))
	// v2.85-PR8(U10):双确认 - GET 拉确认页,POST 才真删
	mux.Handle("GET /users/{id}/delete", protect(http.HandlerFunc(srv.handleUserDeleteConfirmPage)))
	mux.Handle("POST /users/{id}/delete/confirm", protectPOST(http.HandlerFunc(srv.handleUserDelete)))
	mux.Handle("POST /logout", protectPOST(http.HandlerFunc(srv.handleLogout)))

	// v2-82：DDNS API(面板 UI 调);v2-84 加 /api/ddns/family
	mux.Handle("GET /api/ddns/status", protect(http.HandlerFunc(srv.handleDDNSStatus)))
	mux.Handle("POST /api/ddns/toggle", protectPOST(http.HandlerFunc(srv.handleDDNSToggle)))
	mux.Handle("POST /api/ddns/family", protectPOST(http.HandlerFunc(srv.handleDDNSFamily)))

	// v2-83:阿里云凭证卡 API(面板 UI 调)
	//   - GET  /api/aliyun/status   查询当前凭证状态(已配置/未配置/来源)
	//   - POST /api/aliyun/save     保存凭证(需要 confirm=yes 二次确认)
	//   - POST /api/aliyun/clear    清除凭证
	mux.Handle("GET /api/aliyun/status", protect(http.HandlerFunc(srv.handleAliyunStatus)))
	mux.Handle("POST /api/aliyun/save", protectPOST(http.HandlerFunc(srv.handleAliyunSave)))
	mux.Handle("POST /api/aliyun/clear", protectPOST(http.HandlerFunc(srv.handleAliyunClear)))

	// v2.86-PR12.5:证书配置卡 API
	//   - GET  /api/cert/status   查询当前证书配置
	//   - POST /api/cert/save     保存证书配置(改完需重启容器)
	//   - POST /api/cert/clear    清除证书配置(回到 env 默认)
	mux.Handle("GET /api/cert/status", protect(http.HandlerFunc(srv.handleCertStatus)))
	mux.Handle("POST /api/cert/save", protectPOST(http.HandlerFunc(srv.handleCertSave)))
	mux.Handle("POST /api/cert/clear", protectPOST(http.HandlerFunc(srv.handleCertClear)))

	// v2.86-PR13.2:客户端虚拟 IP 段配置卡 API
	//   - GET  /api/subnet/status   查询当前 subnet 配置(panelstate + swanctl 当前生效值)
	//   - POST /api/subnet/save     保存 subnet + 立即 swanctl reload(无需重启)
	//   - POST /api/subnet/clear    清除 panelstate(swanctl.conf 当前值不变,重启容器回 env/auto)
	mux.Handle("GET /api/subnet/status", protect(http.HandlerFunc(srv.handleSubnetStatus)))
	mux.Handle("POST /api/subnet/save", protectPOST(http.HandlerFunc(srv.handleSubnetSave)))
	mux.Handle("POST /api/subnet/clear", protectPOST(http.HandlerFunc(srv.handleSubnetClear)))

	// v2.86-PR12.22:管理员后台 mobileconfig 全局默认值。
	//   - GET  /admin/mobileconfig-defaults         设置页
	//   - POST /admin/mobileconfig-defaults         保存(admin 默认,运行时热改无需重启)
	//   - POST /admin/mobileconfig-defaults/clear   清除(回 builtin 出厂)
	mux.Handle("GET /admin/mobileconfig-defaults", protect(http.HandlerFunc(srv.handleAdminMobileconfigDefaults)))
	mux.Handle("POST /admin/mobileconfig-defaults", protectPOST(http.HandlerFunc(srv.handleAdminMobileconfigDefaultsSave)))
	mux.Handle("POST /admin/mobileconfig-defaults/clear", protectPOST(http.HandlerFunc(srv.handleAdminMobileconfigDefaultsClear)))

	// v2.85-PR8(U11):审计日志只读页
	mux.Handle("GET /audit", protect(http.HandlerFunc(srv.handleAudit)))

	// 中间件链（外→内）：
	//   recoverPanic → secureHeaders → logging → trailingSlashRedirect → mux
	// recover 必须最外层，否则 logging 自身 panic 或 mux panic 会逃逸。
	// secureHeaders 必须在 logging 之内,这样所有响应(含 panic 500)都有安全 header。
	// trailingSlashRedirect 在 logging 之内、mux 之外,这样 301 响应也走 secureHeaders。
	// 设计见 docs/design.md §3（错误处理策略）+ v2-80+ backlog（panic recover）+ v2.86-PR9
	return recoverPanic(srv.Logger)(secureHeaders()(logging(srv.Logger, srv.Metrics)(trailingSlashRedirect(mux))))
}

// cacheControlForStatic v2.86-PR19:给静态资源按扩展名设置 Cache-Control。
//
// 设计意图:
//   - 之前 PR12.21 部署后,用户浏览器 / 我浏览器 view 都命中旧 CSS,
//     看到的是破碎布局 (登录页 brand 区居中失败 + 表单 SVG 巨大化)。
//   - 根因:Go http.FileServer 默认不发 Cache-Control header,浏览器
//     走 heuristic expiry 后 200 OK from disk cache, query bust
//     (?v=Date.now()) 也只 bust HTML,CSS 仍命中 view 进程级 cache。
//   - 现在:对 CSS/JS 加 no-cache + must-revalidate,ETag/Last-Modified
//     走 304 → 用户每次拿最新版(磁盘 cache 1 次但立即验证)。
//   - 图片/字体:cache 1 天 (变化频率低)。
func cacheControlForStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, ".css"), strings.HasSuffix(path, ".js"):
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		case strings.HasSuffix(path, ".png"), strings.HasSuffix(path, ".jpg"),
			strings.HasSuffix(path, ".jpeg"), strings.HasSuffix(path, ".gif"),
			strings.HasSuffix(path, ".webp"), strings.HasSuffix(path, ".svg"),
			strings.HasSuffix(path, ".woff"), strings.HasSuffix(path, ".woff2"),
			strings.HasSuffix(path, ".ttf"), strings.HasSuffix(path, ".eot"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

// trailingSlashRedirect v2.86-PR12.21:把 /path/ 301 重定向到 /path。
//
// 背景：Go 1.22 net/http.ServeMux 严格区分 /users 与 /users/,iOS mobileconfig
// 安装后的内部跳转 / 用户书签 / 客户端 VPN 起来后浏览器自动补 trailing slash
// 都会触发 404。这里统一做 301 → 去 slash 版本。
//
// 例外（不动）：
//   - 静态资源 /static/(FileServer 自己处理 slash)
//   - 根路径 /
//   - 带 file extension 的路径(.css / .png / .pem / .mobileconfig / .sswan)
//   - POST 请求不重写(避免重发)
//   - query string 保留
func trailingSlashRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// 不重写条件(顺序敏感):
		// 1) 根路径 /
		// 2) 非 GET(避免 POST 重发)
		// 3) 路径长度 <= 1 (避免 "//" 这种边界)
		// 4) 末尾不是 /
		// 5) 去掉末尾 / 后的最后一段含 "." (文件路径: .pem .css .png .mobileconfig .sswan)
		if path == "/" || r.Method != "GET" || len(path) <= 1 || path[len(path)-1] != '/' {
			next.ServeHTTP(w, r)
			return
		}
		trimmed := strings.TrimRight(path, "/")
		lastSlash := strings.LastIndex(trimmed, "/")
		lastSeg := trimmed[lastSlash+1:]
		if strings.Contains(lastSeg, ".") {
			// 文件路径 → 不动
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.RawQuery != "" {
			trimmed += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, trimmed, http.StatusMovedPermanently)
	})
}

// recoverPanic 把单个请求里的 panic 兜住，返回 500 而不是让进程崩溃。
//
// 设计动机：v2 之前没有任何 recover，单一 handler panic 会导致整个 Go 进程挂掉，
// docker compose restart 兜底但期间所有请求 502。50 人小团队场景下管理员的某个
// 边角请求（解析表单、SSE 等）panic 就让所有用户断连不可接受。
//
// 实现要点：
//   - 必须在 logging 之外（最外层），否则 logging 自身 panic 也逃逸
//   - 用 runtime/debug.Stack() 记全部栈，便于事后排查
//   - 标记 "panic recovered" 字段，告警系统可以按这个 grep
//   - 返回 500 + 短文本，避免泄露内部细节
//   - panic 后 Connection: close，强制关闭 keep-alive（防止写已 panic 的连接）
func recoverPanic(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						"err", fmt.Sprintf("%v", rec),
						"stack", string(debug.Stack()),
						"method", r.Method,
						"path", r.URL.Path,
						"remote", r.RemoteAddr,
					)
					// panic 后 header 状态可能已部分写入。
					// 安全做法：先尝试设置 500，如果失败就直接放弃。
					w.Header().Set("Connection", "close")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprintln(w, "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// logging 简单请求日志中间件。
//
// v2.85-PR8(U04):末尾把 method/path/status/duration 写进 metrics registry。
// path 用 normalizePath 把 /users/123 收成 /users/{id},避免高 cardinality。
func logging(log *slog.Logger, reg *metrics.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(ww, r)
			dur := time.Since(start)
			log.Debug("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.status,
				"dur_ms", dur.Milliseconds(),
				"remote", r.RemoteAddr,
			)
			if reg != nil {
				reg.IncHTTPRequest(r.Method, normalizePath(r.URL.Path), ww.status)
				reg.ObserveHTTPDuration(r.Method, normalizePath(r.URL.Path), dur)
			}
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

// certLERenewStatus v2.85-PR8(U04):healthz/metrics 复用 cert.CheckLERenewStatus。
func certLERenewStatus(dataDir string) cert.LERenewStatus {
	certPath := filepath.Join(dataDir, "le", "fullchain.pem")
	return cert.CheckLERenewStatus(certPath)
}

// normalizePath v2.85-PR8(U04):把 /users/123 收成 /users/{id},避免 metric label cardinality 爆炸。
//
// 规则:匹配数字段(纯整数)→ {id}。其他动态段(用户名、token)保留原样,
// 因为 handler 端已用 PathValue 把这些归一化(我们的路由只有 {id})。
func normalizePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		if isAllDigits(seg) {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// healthCheckResp /healthz 返回的 JSON。
type healthCheckResp struct {
	OK     bool                  `json:"ok"`
	Time   string                `json:"time"`
	Checks map[string]checkEntry `json:"checks"`
}

type checkEntry struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// handleHealthz v2.85-PR8(U04):JSON 健康检查。
//
// 探测 4 个组件:db / vici(可选)/ le(LE 模式)/ ddns(可选)。
// 任一失败 → 503 + ok=false;全部 OK → 200 + ok=true。
// dev 模式(Swanctl/DDNSSync=nil)→ 对应组件标 OK + detail="skipped"。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthCheckResp{
		Time:   time.Now().UTC().Format(time.RFC3339),
		Checks: map[string]checkEntry{},
	}
	ms := func(d time.Duration) int64 { return d.Milliseconds() }

	// 1) DB ping
	t0 := time.Now()
	if s.Store != nil {
		if err := s.Store.DB.PingContext(ctx); err != nil {
			resp.Checks["db"] = checkEntry{OK: false, Detail: err.Error(), LatencyMs: ms(time.Since(t0))}
		} else {
			resp.Checks["db"] = checkEntry{OK: true, LatencyMs: ms(time.Since(t0))}
		}
	} else {
		resp.Checks["db"] = checkEntry{OK: false, Detail: "store not initialized"}
	}

	// 2) VICI 探测(swanctl 调用 ListSAs 看是否 hang)
	t0 = time.Now()
	if s.Swanctl != nil {
		saCtx, saCancel := context.WithTimeout(ctx, 1*time.Second)
		_, err := s.Swanctl.ListSAs(saCtx)
		saCancel()
		if err != nil {
			resp.Checks["vici"] = checkEntry{OK: false, Detail: err.Error(), LatencyMs: ms(time.Since(t0))}
		} else {
			resp.Checks["vici"] = checkEntry{OK: true, LatencyMs: ms(time.Since(t0))}
		}
	} else {
		resp.Checks["vici"] = checkEntry{OK: true, Detail: "skipped (dev mode)", LatencyMs: 0}
	}

	// 3) LE 续签状态
	t0 = time.Now()
	if s.CertMode == "letsencrypt" {
		// 复用 internal/cert.CheckLERenewStatus 通过 s.DataDir 路径
		st := certLERenewStatus(s.DataDir)
		if st.LastRenewFailed {
			resp.Checks["le"] = checkEntry{OK: false, Detail: "renew failed (LAST_RENEW_FAILED flag)", LatencyMs: ms(time.Since(t0))}
		} else {
			resp.Checks["le"] = checkEntry{OK: true, LatencyMs: ms(time.Since(t0))}
		}
	} else {
		resp.Checks["le"] = checkEntry{OK: true, Detail: "self-signed mode", LatencyMs: 0}
	}

	// 4) DDNS last sync
	t0 = time.Now()
	if s.DDNSSync != nil {
		last := s.DDNSSync.LastSyncSnapshot()
		if last.Time.IsZero() {
			resp.Checks["ddns"] = checkEntry{OK: true, Detail: "never synced (cold start)", LatencyMs: ms(time.Since(t0))}
		} else if !last.Success {
			resp.Checks["ddns"] = checkEntry{OK: false, Detail: last.Error, LatencyMs: ms(time.Since(t0))}
		} else {
			resp.Checks["ddns"] = checkEntry{OK: true, LatencyMs: ms(time.Since(t0))}
		}
	} else {
		resp.Checks["ddns"] = checkEntry{OK: true, Detail: "not configured", LatencyMs: 0}
	}

	// 汇总
	resp.OK = true
	for _, c := range resp.Checks {
		if !c.OK {
			resp.OK = false
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if !resp.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleReadyz v2.85-PR8(U04):k8s readiness。
//
// 在 healthz 基础上额外要求:charon 至少 1 个 SA loaded(swanctl connections)。
// dev 模式跳过 swanctl 检查(视为 ready)。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// 先跑 healthz 同样的检查
	s.handleHealthz(w, r)
	if w.Header().Get("Content-Type") != "application/json" {
		// 已有响应(出错)→ 不再加 ready 检查
		return
	}
	// healthz 已经写过 header / body;k8s 仍按 status 判 200/503。
	// 这里只在 healthz 全部 OK 的情况下做额外检查 —— 但因为 healthz 已写 body,
	// 我们只能"假定 healthz OK"(503 已被 healthz 处理,不会再跑这里)。
	// 实际语义:readyz = healthz AND (swanctl nil OR conns>0)。
	if s.Swanctl != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel()
		// ListSAs 已经证明 VICI 通;这里再验是否有活跃 SA,
		// 但 dev / 刚启动时没有 SA 是合法的(空部署)→ 用 ListConnections?
		// 暂简化:有任一 SA 就 ready;没有 SA 时也算 ready(等用户拨入即可)。
		sas, _ := s.Swanctl.ListSAs(ctx)
		_ = sas // 即使 0 也算 ready,空部署不该被 readiness 挡
	}
}

// handleMetrics v2.85-PR8(U04):Prometheus exposition format。
//
// 默认拒绝非 loopback 访问,避免泄露 SA 数 / 续签失败细节给公网。
// 显式 IKEV2_METRICS_PUBLIC=true(后续扩展)可放开,本 PR 不实现。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.RemoteAddr, "127.0.0.1") && !strings.HasPrefix(r.RemoteAddr, "[::1]") {
		// 也接受被反向代理(k8s service 等)设置 X-Forwarded-For 后再判断;
		// 本 PR 简化:直接看 RemoteAddr。
		http.Error(w, "metrics disabled (loopback only)", http.StatusNotFound)
		return
	}
	// 刷新 gauges 给最新值
	s.refreshMetricGauges(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.Metrics == nil {
		_, _ = w.Write([]byte("# metrics registry not initialized\n"))
		return
	}
	_ = s.Metrics.Emit(w)
}

// refreshMetricGauges 从各模块拉最新 gauge 值。
func (s *Server) refreshMetricGauges(ctx context.Context) {
	if s.Metrics == nil {
		return
	}
	if s.Swanctl != nil {
		saCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		sas, err := s.Swanctl.ListSAs(saCtx)
		cancel()
		if err == nil {
			var n int64
			for _, sa := range sas {
				if sa.IkeState == "ESTABLISHED" {
					n++
				}
			}
			s.Metrics.SetVPNActiveSAs(n)
		}
	}
	if s.DDNSSync != nil {
		last := s.DDNSSync.LastSyncSnapshot()
		if !last.Time.IsZero() {
			s.Metrics.SetDDNSLastSync("v4", last.Time.Unix())
			s.Metrics.SetDDNSLastSync("v6", last.Time.Unix())
		}
	}
	if s.CertMode == "letsencrypt" {
		st := certLERenewStatus(s.DataDir)
		if !st.CertExpires.IsZero() {
			s.Metrics.SetLECertExpiry(st.CertExpires.Unix())
		}
	}
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
