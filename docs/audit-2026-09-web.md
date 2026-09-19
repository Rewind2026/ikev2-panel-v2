# Web 框架 + 认证 / 鉴权专项审计 — 2026-09

> 维度:**Web 路由 / Handler / Session / 输入校验**(审计框架第 2 阶段 — Web 专项)
> 审计员:项目组自查
> 范围:`/opt/ikev2-panel-v2-main/internal/web/` + `internal/auth/` + `web/templates/`
> 时间:2026-09-19
> 状态:**草案,等待评审**
> 前置阅读:[audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) — 本报告**覆盖并扩展**其中的 S05 / S07 / S08 / S10 / S14,所有新增 Web 维度单独编号为 W 系列

---

## 范围

| 路径 | 类别 |
|------|------|
| [internal/web/server.go](file:///opt/ikev2-panel-v2-main/internal/web/server.go) | 路由 + middleware + panic recover |
| [internal/web/handlers_auth.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go) | 登录 / 登出 / 错误信息 |
| [internal/web/handlers_users.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go) | 用户 CRUD + 输入校验 |
| [internal/web/handlers_ddns.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go) | DDNS API(form + JSON 混用) |
| [internal/web/handlers_config.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go) | mobileconfig / sswan / QR / 一次性 token |
| [internal/web/handlers_home.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go) | 首页渲染 / DDNS / 阿里云卡片 |
| [internal/web/handlers_aliyun.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_aliyun.go) | 阿里云凭证 CRUD |
| [internal/auth/password.go](file:///opt/ikev2-panel-v2-main/internal/auth/password.go) | 密码 hash / 随机密码 / session ID |
| [internal/auth/session.go](file:///opt/ikev2-panel-v2-main/internal/auth/session.go) | Cookie 配置 + CSRF 提取 |
| [internal/web/flash.go](file:///opt/ikev2-panel-v2-main/internal/web/flash.go) | 一次性 flash 消息 |
| [internal/web/templates.go](file:///opt/ikev2-panel-v2-main/internal/web/templates.go) | 模板加载 + 反射注入 |
| [web/templates/*.html](file:///opt/ikev2-panel-v2-main/web/templates/) | 模板渲染 |
| [internal/store/sessions.go](file:///opt/ikev2-panel-v2-main/internal/store/sessions.go) | session DB CRUD |
| [internal/store/admins.go](file:///opt/ikev2-panel-v2-main/internal/store/admins.go) | admin CRUD(id=1 CHECK) |
| [internal/installtoken/installtoken.go](file:///opt/ikev2-panel-v2-main/internal/installtoken/installtoken.go) | 一次性 install token |
| [internal/limit/limiter.go](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go) | 限速文件 path 拼接 |

## 工具 / 参考

- [OWASP Cheat Sheet Series](https://cheatsheetseries.owasp.org/) — 总入口
- [OWASP Top 10 (2021)](https://owasp.org/www-project-top-ten/)
- [Go html/template 安全](https://pkg.go.dev/html/template) — 上下文自动转义
- [gorilla/csrf](https://github.com/gorilla/csrf) — 工业级 CSRF 中间件
- [gorilla/sessions](https://github.com/gorilla/sessions) — server-side session 存储
- [chi middleware](https://github.com/go-chi/chi/tree/master/middleware) — middleware 最佳实践
- [gin csrf example](https://github.com/gin-gonic/gin#contrib) — 对照实现
- [dex (OIDC provider)](https://github.com/dexidp/dex) — 工业级 OIDC / session
- [authentik](https://github.com/goauthentik/authentik) — 现代 Web auth 实现
- [casdoor](https://github.com/casdoor/casdoor) — OIDC + session + audit
- [Let’s Encrypt Rate Limits](https://letsencrypt.org/docs/rate-limits/)

---

## 发现汇总

按严重度分组:

| 严重度 | 数量 | 关键问题 |
|--------|------|----------|
| **HIGH** | 3 | 缺少 HSTS / CSP / X-Frame-Options(0 个 HTTP 安全 header)/ 缺少 CSRF 双提交加固 / 仅 login 无限速,其他 POST 端点完全裸奔 |
| **MED** | 8 | 错误信息泄漏 / 路径无 max-age 校验 / 错误响应泄漏 DB 错误 / 无安全审计日志 / 安装 token 不绑 IP+UA 风险 / 阿里云 save 失败回显 raw err / cookie Secure 行为依赖配置 / username 长度上限不强制 |
| **LOW** | 6 | generatePassword 注释 misleading / usernameRe 无 unicode / mobileconfig Content-Disposition filename injection / 同 admin 多端登录但 user_agent 仅记录 / Session 无 idle timeout / 模板 BodyHTML 反射注入存在未来风险 |

> 与 `audit-2026-09-security.md` 重复的 issue:
> - **S01 登录无 rate limit** → W03 + W04 重新覆盖
> - **S07 HSTS 缺失** → W01 重新覆盖
> - **S08 Cookie Secure 行为** → W07 重新覆盖
> - **S10 敏感错误信息泄漏** → W05 + W06 重新覆盖
> - **S14 用户名 / 备注输入长度未限制** → W08 重新覆盖
> 重复内容用**链接**代替,不复述细节。

---

## [SEVERITY-HIGH] ISSUE

### ISSUE-W01:HTTP 安全 header 全缺失(CSP / HSTS / X-Frame-Options / X-Content-Type-Options / Referrer-Policy / Permissions-Policy)

- **位置**:
  - [internal/web/server.go:127](file:///opt/ikev2-panel-v2-main/internal/web/server.go#L127)(`recoverPanic → logging → mux`,**无 security header middleware**)
  - 已确认 grep:全仓库 `Strict-Transport-Security / Content-Security-Policy / X-Frame-Options / X-Content-Type-Options / Referrer-Policy / Permissions-Policy` **0 处设置**
- **问题**:完整缺失 OWASP 推荐的 6 个安全 header。
- **影响**(逐项):
  1. **HSTS 缺失** → SSL stripping 攻击(重复 S07)
  2. **CSP 缺失** → 模板里万一有 XSS 漏洞(用户输入 note / username 透传)→ 攻击者注入 `<script>` 后无 CSP 阻挡,可窃取 admin session
  3. **X-Frame-Options 缺失** → 面板被 `<iframe>` 嵌入,钓鱼页面假装是"内嵌面板",诱导输入密码
  4. **X-Content-Type-Options=nosniff 缺失** → MIME confusion 攻击(浏览器把 user-controlled 文件猜成 HTML 执行)
  5. **Referrer-Policy 缺失** → 从面板跳到阿里云 / GitHub 链接时,把 admin URL 泄露给第三方
  6. **Permissions-Policy 缺失** → 浏览器权限(摄像头 / 麦克风 / 地理位置)默认全开,即使面板不用
- **参考**:
  - [OWASP Secure Headers Project](https://owasp.org/www-project-secure-headers/)
  - [OWASP REST Security Cheatsheet §2.5](https://cheatsheetseries.owasp.org/cheatsheets/REST_Security_Cheat_Sheet.html)
  - [Mozilla Observatory](https://observatory.mozilla.org/) — 跑分工具
  - 标杆:[authentik web/middleware.py](https://github.com/goauthentik/authentik/blob/main/authentik/core/middleware.py) — 全 header 都设
  - 标杆:[casdoor object/middleware.go](https://github.com/casdoor/casdoor/blob/master/object/middleware.go) — CSP + HSTS 全套
  - 标杆:[gin-contrib/security](https://github.com/gin-contrib/) — 现成 middleware
- **修复方案**:
  ```go
  // 在 server.go 加一个 securityHeaders middleware
  func securityHeaders(next http.Handler) http.Handler {
      return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          h := w.Header()
          h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
          h.Set("X-Frame-Options", "DENY")
          h.Set("X-Content-Type-Options", "nosniff")
          h.Set("Referrer-Policy", "no-referrer")
          h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
          // CSP:面板不用 inline JS(只有 theme toggle),全用 nonce
          // 注意 layout.html 的 inline script 需要 'unsafe-inline' 或者改成外置
          h.Set("Content-Security-Policy",
              "default-src 'self'; "+
              "script-src 'self'; "+
              "style-src 'self' 'unsafe-inline'; "+ // Pico CSS 里有 inline style
              "img-src 'self' data:; "+
              "object-src 'none'; "+
              "base-uri 'self'; "+
              "frame-ancestors 'none'")
          next.ServeHTTP(w, r)
      })
  }
  // 加到中间件链:recoverPanic → securityHeaders → logging → mux
  ```
- **工作量**:0.5d(加 middleware + 调整 inline script 外置 + 测试 CSP 不破样式)
- **优先级**:**本周修**

---

### ISSUE-W02:CSRF 实现正确但缺少 belt-and-suspenders 加固(Origin 校验 / SameSite 失效场景)

- **位置**:
  - [internal/web/server.go:242-273](file:///opt/ikev2-panel-v2-main/internal/web/server.go#L242-L273) `requireCSRF`(token 实现本身正确)
  - [internal/auth/session.go:29](file:///opt/ikev2-panel-v2-main/internal/auth/session.go#L29) `SameSite: http.SameSiteLaxMode`
  - [internal/web/handlers_auth.go:44-46](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L44-L46) 注释说"cookie SameSite=Lax 已阻断跨站 POST"
- **问题**:
  - **token 实现本身正确**(constant-time compare + form+header 双通道 + 缺失 403),**这部分没问题**
  - 但**仅靠 SameSite=Lax 不够**:
    1. **老浏览器**(Chrome < 80, Safari < 12, IE)**不支持 SameSite**(→ cookie 跟旧版行为一致,跨站 GET 表单提交可触发 CSRF)
    2. **GET 路由受保护但不发敏感操作** → 实际上没 CSRF 风险
    3. **真正的漏洞**:面板是 server-rendered HTML,**没有 XHR/fetch**,`X-CSRF-Token` header 通道**根本没用到**(只 form 提交) → CSRF token 在 cookie + form 双 cookie 模式下需要"读取 cookie",**但 cookie 是 HttpOnly**(cookie JS 读不到)
  - **因此现在攻击场景是**:
    1. 攻击者页面 `<form method="POST" action="https://panel/users/1/delete">` 提交(无 CSRF token)
    2. 浏览器自动带 cookie(因为 Lax 跨站 POST 在某些浏览器的 [2-minute window](https://blog.chromium.org/2020/02/samesite-cookies-by-default-more-precise.html) 之外允许)
    3. 服务端 `requireCSRF` 收不到 form token → 403 ✓ 防御成功
  - **也就是当前实现是 OK 的**,但是:
    - **没有 Origin/Referer 校验**(CSRF token 缺失 + Origin 不匹配 → 200 OK 的话攻击者就知道"我是合法请求但 token 丢了" = 信息泄露)
    - **没有 anti-XSSI headers**(对 `/api/*` JSON 端点)
    - **`-2min Lax bypass`**:Chrome 跨站 POST 时如果 cookie 是 top-level navigation(2 分钟内)Lax 不阻挡 → 但有 CSRF token 还是挡
- **参考**:
  - [OWASP CSRF Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)
  - [RFC 6265 bis (SameSite)](https://datatracker.ietf.org/doc/html/draft-ietf-httpbis-rfc6265bis)
  - [gorilla/csrf 实现](https://github.com/gorilla/csrf/blob/master/csrf.go) — 用了 Origin 校验 + double submit cookie
  - 标杆:[authentik uses double submit cookie + Origin check](https://github.com/goauthentik/authentik)
  - 标杆:[casdoor double submit cookie + custom header](https://github.com/casdoor/casdoor)
- **修复方案**(双保险):
  1. **加 Origin 校验**:CSRF middleware 入口检查 `r.Header.Get("Origin")` 必须匹配 `s.PanelHost`
     ```go
     origin := r.Header.Get("Origin")
     if origin != "" && !strings.HasSuffix(origin, "://"+s.PanelHost) {
         http.Error(w, "origin mismatch", http.StatusForbidden)
         return
     }
     ```
  2. **把 `X-CSRF-Token` 改成必要通道**(不只 form):让前端 JS 主动加 header,防止 pure form CSRF(虽然现在 form 已经能挡)
  3. **JSON 端点(`/api/*`)加 `Accept: application/json` 校验**:防止 `<form>` 提交到 `/api/*`
  4. **测试**:跨站 POST + 无 token → 403,跨站 POST + 有 token 但 Origin 错 → 403
- **工作量**:0.5d
- **优先级**:**本周修**(安全基础项)

---

### ISSUE-W03:登录端点 + 所有 POST 端点完全无 rate limit(S01 升级版)

- **位置**:
  - [internal/web/handlers_auth.go:36-98](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L36-L98) `handleLogin`
  - [internal/web/handlers_users.go:94, 263, 307, 325, 345](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go#L94) 全部 CRUD 无节流
  - [internal/web/handlers_ddns.go:86](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L86) DDNS toggle / family 无节流
  - [internal/web/handlers_aliyun.go:62](file:///opt/ikev2-panel-v2-main/internal/web/handlers_aliyun.go#L62) 凭证保存 / 清除无节流
- **问题**:S01 提了"登录无 rate limit",但**实际上整个 POST 面都没限速**:
  - `POST /users/{id}/delete` —— 攻击者可遍历 ID 全删
  - `POST /users/{id}/reset-password` —— 攻击者可让 admin 反复生成密码(无业务影响但污染日志)
  - `POST /api/aliyun/save` —— 反复覆盖 AccessKey 文件(DDoS 磁盘 + log flood)
  - `POST /api/ddns/toggle` —— 反复开关 DDNS(触发阿里云 API rate limit,见 [alidns rate limit 1000/天](https://help.aliyun.com/zh/dns/developer-reference/request-rate-limit))
  - `POST /install/{token}` —— 猜测 256 bit token 不可行,但 invalid token 数量无限 → 内存泄漏风险(`InstallTokens` map 无限增长?—— 现在已 Sweep,但 Sweep goroutine 间隔未知)
- **参考**:
  - [OWASP API Security Top 10 — API4:2023 Unrestricted Resource Consumption](https://owasp.org/API-security/editions/2023/en/0xa4-unrestricted-resource-consumption/)
  - [OWASP DoS Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Denial_of_Service_Cheat_Sheet.html)
  - 标杆:[dex middleware/ratelimit.go](https://github.com/dexidp/dex/tree/master/server) — token bucket + per-route
  - 标杆:[chi-ratelimit](https://github.com/go-chi/httprate) — IP+path 维度滑动窗口
- **修复方案**:
  1. **引入全局 limiter**(`internal/middleware/ratelimit.go`):
     - 内存 token bucket,key = `client_ip + path_bucket`
     - 不同 endpoint 不同配额:
       | Endpoint | Rate |
       |----------|------|
       | POST /login | 5/min per IP(锁定 5 min) |
       | POST /api/* | 30/min per IP |
       | POST /users/* CRUD | 20/min per IP |
       | GET /install/{token} | 60/min per IP(扫码场景允许高) |
       | GET /healthz | 不限 |
     - 配置化:`IKEV2_RATE_LIMIT_LOGIN=5` 等环境变量
  2. **加测试**:`TestRateLimiter_Lockout`、`TestRateLimiter_PerIP`
  3. **可观测**:每次被限速触发 → `slog.Warn("rate limit hit", "path", ..., "ip", ...)`
- **工作量**:1.0d(基于 chi-ratelimit 或 [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate))
- **优先级**:**本周修**

---

## [SEVERITY-MED] ISSUE

### ISSUE-W04:错误响应泄漏 DB / 内部细节(覆盖 S10)

- **位置**:
  - [internal/web/handlers_users.go:190, 203](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go#L190) `data.Error = fmt.Sprintf("写入 swanctl 配置失败：%v（请检查容器内 /etc/swanctl/conf.d 权限）", err)`
  - [internal/web/handlers_ddns.go:102, 146](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L102) `http.Error(w, "failed to update state: "+err.Error(), http.StatusInternalServerError)`
  - [internal/web/handlers_aliyun.go:86, 117](file:///opt/ikev2-panel-v2-main/internal/web/handlers_aliyun.go#L86) `http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)`
- **问题**:用户(已登录 admin)看到 `err.Error()`,可能含:
  - DB constraint 名(`UNIQUE constraint failed: users.username` → 暴露 schema)
  - 文件系统权限(`/etc/swanctl/conf.d` → 暴露架构)
  - alidns API 调用细节
- **影响**:
  - 信息收集(攻击者枚举能力增强)
  - 已知 `data.Error` 用模板渲染,**go template 自动转义 → 无 XSS**(✓)
- **参考**:S10([OWASP Error Handling Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Error_Handling_Cheat_Sheet.html))
- **修复方案**:
  1. **统一错误处理 helper**:
     ```go
     func (s *Server) renderUserFacingError(w http.ResponseWriter, w2 io.Writer, tmpl string, data any, err error, defaultMsg string) {
         // 1. log 详情
         s.Logger.Error(defaultMsg, "err", err)
         // 2. 给用户只看 defaultMsg + 可操作建议
         // (可以加白名单:某些已知安全错误(如 "username exists")可以回显)
     }
     ```
  2. **白名单**:用户名冲突 / 格式错误等"已知用户错误"可以详细;**5xx 类错误一律 sanitized**
- **工作量**:0.3d
- **优先级**:本周

---

### ISSUE-W05:`/users/{id}/android` handler 把 `flash` 走 query string(回归)

- **位置**:[internal/web/handlers_config.go:308](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L308) `flash := r.URL.Query().Get("flash")`
- **问题**:P1-A 修复目标是"密码/状态消息走 session-only flash,不走 query string"。**但 `handleUserAndroidConfig` 仍然从 query string 读 `flash` 字段**。
  - 实际上看 context:**该 handler 内部没有 Set flash,只是读取**。
  - **真正的问题**:目前没有 producer(没人 redirect 到 `/users/{id}/android?flash=...`),所以这个 query 参数**永远不会被设置**
  - 但**留有这个接收口**意味着未来开发者一不小心会 redirect 过去 → 泄密回归
- **参考**:
  - [P1-A 设计动机 in flash.go:6-15](file:///opt/ikev2-panel-v2-main/internal/web/flash.go#L6-L15)
  - [OWASP Sensitive Data Exposure](https://owasp.org/www-project-top-ten/OWASP_Top_Ten_2021/3-Sensitive_Data_Exposure_Web_Application_Security_Risk/)
- **修复方案**:
  1. **删除 `r.URL.Query().Get("flash")`** → 改用 `s.FlashStore.Consume(sess.ID)`,跟其他 handler 一致
  2. 加注释:禁止 query string 传 flash
  3. 测试:从 query string 传 `?flash=secret` → 不应该显示在 UI
- **工作量**:0.1d
- **优先级**:本周

---

### ISSUE-W06:`session.user_agent` 仅记录,不做校验 / 绑定

- **位置**:
  - [internal/store/sessions.go:14-23](file:///opt/ikev2-panel-v2-main/internal/store/sessions.go#L14-L23) `CreateSession` 存 user_agent
  - [internal/web/handlers_auth.go:88](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L88) `UserAgent: r.UserAgent()`
- **问题**:
  - 只**记录** user_agent(将来用于日志分析),**不绑定**
  - session 固定攻击(session fixation):
    1. 攻击者诱导用户用**预先设置的 cookie** 登录(虽然现在登录用 `GenerateSessionID` 新生成,看不到明确漏洞,**但如果 SetSessionCookie 改成接收外部值就 fixable**)
    2. 当前代码 `GenerateSessionID` **永远生成新值**,所以 fixation 实际不可行 ✓
- **当前实现 OK 的部分**:
  - Login 后 `SetSessionCookie(w, sessID, s.Secure)` 用新生成的 sessID
  - 旧 cookie 即使存在也被覆盖 → fixation 攻击**结构性防御成功**
- **但还有个小问题**:
  - **同 admin 多端登录会创建多个 session**,如果一个 session cookie 被偷,管理员**没法"撤销"所有其他设备的 session**(无"踢出其他设备"功能)
  - session 无 **idle timeout**(只 absolute expiry = `SessionTTL`),**长生命周期会话** = 偷到 cookie 可用很久
- **参考**:
  - [OWASP Session Management Cheatsheet — Session Expiration](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html#session-expiration)
  - [NIST SP 800-63B §7.1 Reauthentication](https://pages.nist.gov/800-63-3/sp800-63b.html)
- **修复方案**:
  1. **加 idle timeout**:session 表加 `last_active_at INT` 列,requireSession 里 update 当前时间,如果 `now - last_active_at > IDLE_TIMEOUT` 则视为过期
  2. **"登出所有设备"**:管理面板加一个 "强制下线所有 admin session" 按钮 → `DELETE FROM sessions` where admin_id=1
  3. **session 列表 UI**:在 admin 个人设置里展示"当前活跃 session"(用户 / IP / UA / last active),允许单独踢出
- **工作量**:0.5d(idle timeout 改 DB schema + middleware;踢出功能 0.3d)
- **优先级**:本周

---

### ISSUE-W07:Cookie Secure / SameSite 行为依赖配置项,可被误关(覆盖 S08)

- **位置**:
  - [internal/auth/session.go:28](file:///opt/ikev2-panel-v2-main/internal/auth/session.go#L28) `Secure: secure`(参数由 `s.Secure` 传入)
  - [internal/web/handlers_auth.go:96](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L96) `auth.SetSessionCookie(w, sessID, s.Secure)`
- **问题**:
  - `s.Secure` 来源:`internal/config/config.go` 中 `CookieSecure: getbool("IKEV2_COOKIE_SECURE", true)` 默认 `true`
  - **如果用户误设 `IKEV2_COOKIE_SECURE=false`**:
    - 8443 走 HTTPS → cookie 在 HTTPS 上不强制 Secure → 浏览器在 HTTP 也能发 → session 泄漏
  - **没有"如果检测到 HTTP 访问则强制跳 HTTPS"** → 用户从 `http://` 进,登录失败(cookie 不发送)
- **参考**:
  - S08(原始 issue)
  - [OWASP Cookie Security Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html#cookies)
  - [chi middleware.RealIP + Secure](https://github.com/go-chi/chi/tree/master/middleware)
- **修复方案**:
  1. **启动时校验**:如果 `CookieSecure=false` 但 `r.TLS` 始终为 nil(容器配 host 网络),启动警告 / 拒绝启动
  2. **HTTPS 强制跳**:middleware 里检测 `r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https"`,302 → `https://` + 301
  3. **去掉配置项**:Cookie Secure 永远 true(只允许 trusted proxy + TLS termination)
- **工作量**:0.3d
- **优先级**:本周

---

### ISSUE-W08:`username` / `note` 输入校验有 max 但 DB 层无 check / handler 顺序可绕过(覆盖 S14)

- **位置**:
  - [internal/web/handlers_users.go:120-137](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go#L120-L137)
    ```go
    if !usernameRe.MatchString(data.Username) { ... }
    if data.SpeedLimit < 0 || data.SpeedLimit > 1000 { ... }
    if len(data.Note) > 200 { ... }
    ```
- **问题**:
  - **`usernameRe`**:`^[a-z0-9_-]{3,32}$` → **长度上限 32 位** ✓
  - **但**:`data.Username = r.PostFormValue("username")` **没先 trim / 没 max-bytes 上限**。如果 user 提交一个 **5MB 字符串**,`r.ParseForm()` 接收(Go default 限 10MB) → 然后 usernameRe 拒绝 → 但内存已经浪费
  - **`data.Note` 长度校验在 usernameRe 之后** → username 失败时 note 也被解析但不报错,产生**短生命周期内存浪费**(无实际安全影响)
  - **没有 `enabled` 字段的真值校验**:`r.PostFormValue("enabled") == "1"` → 用户发 `"enabled=evil"` 也通过,但 SQLite bool 转换忽略 → **无安全影响**
  - **没有 `expires_at` 合理性校验**:`time.Parse` 通过后,可以直接传 `1970-01-01`(unix=0 是"永不过期"特殊值)→ 攻击者创建 user 让其**永不过期**
- **参考**:
  - [OWASP Input Validation Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html)
  - S14(原始 issue)
- **修复方案**:
  1. **在 ParseForm 前 / 后立刻 normalize**:
     ```go
     data.Username = strings.TrimSpace(r.PostFormValue("username"))
     if len(data.Username) > 32 {  // 提前 max check
         http.Error(w, "username too long", http.StatusBadRequest)
         return
     }
     ```
  2. **`expires_at` 校验**:解析后,如果 `t.Unix() < now` → 报错"过期时间不能是过去"
  3. **`speed_limit_mbps`**:用 `strconv.Atoi` 后立刻 range check(已经有了 ✓),但 `0` = 不限速,`负数` 应该也拒绝 ✓
  4. **DB 层也加 CHECK constraint**:防止代码绕过
- **工作量**:0.1d
- **优先级**:本周

---

### ISSUE-W09:handlers_config.go 中 `Content-Disposition` filename 直接拼 username(注入风险低但应 sanitized)

- **位置**:
  - [internal/web/handlers_config.go:47](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L47) `filename="%s.mobileconfig"` + `u.Username`
  - [internal/web/handlers_config.go:156](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L156) 同
  - [internal/web/handlers_config.go:252](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L252) sswan
- **问题**:
  - `usernameRe` 已限制 username 为 `[a-z0-9_-]`,**所以实际不可注入 CRLF / quote**
  - 但 **handler 没显式断言** username 经过 usernameRe 校验
  - 如果未来 usernameRe 放宽(支持更多字符),代码就破
- **参考**:
  - [OWASP HTTP Response Splitting](https://owasp.org/www-community/attacks/HTTP_Response_Splitting)
- **修复方案**:
  1. **把 usernameRe 校验在所有 handler 入口**(或用 helper 函数 `s.requireValidUser(username)`)
  2. **Content-Disposition 的 filename 强制转义**:`strings.ReplaceAll(filename, "\"", "")`,避免双引号注入
  3. **加测试**:username 含 `"` / `\n` → 拒绝(虽然 regex 会拒绝,测试证明意图)
- **工作量**:0.1d
- **优先级**:下批

---

### ISSUE-W10:admin session 列表不可见 / 无审计日志(SOC 基础要求)

- **位置**:全 `internal/web/`(已 grep:无 `audit_log` 表 / 字段)
- **问题**:
  - **登录成功** → `s.Logger.Debug` 在 `handleLogin` 里 log 了 `pw_len`(log 字段 ✓)但**不记成功日志**
  - **登录失败** → `s.Logger.Debug` 但 log 字段不带 IP(只有 username / pw_len)
  - **CRUD**(create / delete / reset-password / enable / disable)→ **完全无 audit log**
  - **凭证变更** → `s.Logger.Info("aliyun creds saved via panel")` 只有 1 行,**无 IP / admin name / key_id_masked 在主日志**(其实有,key_id_masked 在 save handler)
  - **CSRF 失败** → `s.Logger.Warn("csrf token mismatch")` 有,但 **不记 username / admin_id**(因为没解析)
- **影响**:
  - **事后追溯困难**:用户报告"我的账号不见了",admin 看日志**找不到谁删的**
  - **合规要求**:`audit-2026-09-security.md` 没提这个,但 SOC2 / ISO 27001 / GDPR 都要求 audit trail
- **参考**:
  - [OWASP Logging Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
  - [NIST SP 800-92 — Guide to Computer Security Log Management](https://csrc.nist.gov/publications/detail/sp/800-92/final)
  - 标杆:[authentik events/](https://github.com/goauthentik/authentik/tree/main/authentik/events) — 全审计事件
  - 标杆:[casdoor object/log.go](https://github.com/casdoor/casdoor/blob/master/object/log.go) — login / CRUD 全记录
- **修复方案**:
  1. **建 `audit_log` 表**:
     ```sql
     CREATE TABLE audit_log (
         id INTEGER PRIMARY KEY AUTOINCREMENT,
         timestamp INTEGER NOT NULL,
         admin_id INTEGER,           -- nullable, login 失败时为 NULL
         event_type TEXT NOT NULL,   -- 'login_success' / 'login_fail' / 'user_create' ...
         target TEXT,                -- 关联对象 username / user_id
         ip TEXT,                    -- client IP
         user_agent TEXT,
         details TEXT                -- JSON,不含密码
     );
     ```
  2. **加 `Store.AuditLog(ctx, entry)` helper**
  3. **关键事件打 audit log**:
     - `LOGIN_SUCCESS` / `LOGIN_FAIL`(用户名 / IP / UA / pw_len)
     - `USER_CREATE` / `USER_DELETE` / `USER_RESET_PW` / `USER_ENABLE` / `USER_DISABLE`
     - `ALIYUN_SAVE` / `ALIYUN_CLEAR`
     - `DDNS_TOGGLE` / `DDNS_FAMILY`
     - `CSRF_FAIL`(带 IP / path / UA)
  4. **管理面板 UI**:加"审计日志"页(只读,展示最近 1000 条)
- **工作量**:1.0d(DB + middleware + UI)
- **优先级**:本周(用户可见的安全基线)

---

### ISSUE-W11:install token 不绑定 IP / UA,但绑定 SSH-VPN 客户端机(中风险,设计妥协)

- **位置**:
  - [internal/installtoken/installtoken.go:58-73](file:///opt/ikev2-panel-v2-main/internal/installtoken/installtoken.go#L58-L73) `Issue` 只绑 `UserID`
  - [internal/installtoken/installtoken.go:79-94](file:///opt/ikev2-panel-v2-main/internal/installtoken/installtoken.go#L79-L94) `Consume` 也不绑 IP / UA
- **问题**:
  - **设计选择**(注释承认):"移动网络 IP / UA 易变 → 不绑"
  - **trade-off**:256 bit 随机熵 + 10 min TTL + 一次性消费 → 即使不绑 IP,**猜测成本 2^256**(实际不可行)
  - **但仍有边界 case**:
    - QR 截图泄露给攻击者 → 10 分钟内可被**任何人**(任何 IP / UA)消费 → 即使 token 一次性,**首次扫码的就吃了**
    - QR 截图本身的安全性依赖用户不上传网盘 / 不截图发给别人(用户教育)
- **影响**:低,但**比绑 IP+UA 多 1 个攻击面**(截图泄露)
- **参考**:
  - [OWASP Authentication Cheatsheet — Out-of-Band](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
  - [Magic link security](https://blog.postmarkapp.com/3-ways-to-avoid-the-pitfalls-of-magic-links)
- **修复方案**(可选加固):
  1. **绑 "扫码 IP 段"**:record 第一个 consume 的 IP,后续从不同 IP 消费时要求额外验证(此场景复杂,放弃)
  2. **缩短 TTL**:从 10 min 改 3 min,降低泄露窗口
  3. **加 audit log**:每次 issue / consume 都 log 详细信息(IP / UA 即使不强制校验,记日志用于追溯)
  4. **加 1-click revoke**:admin 可让所有未消费的 install token 失效(`InstallTokens.Purge(userID)`)
- **工作量**:0.3d
- **优先级**:下批

---

## [SEVERITY-LOW] ISSUE

### ISSUE-W12:`GeneratePassword` 注释 misleading + `int(b)` 比较签名陷阱

- **位置**:[internal/auth/password.go:25-52](file:///opt/ikev2-panel-v2-main/internal/auth/password.go#L25-L52)
  ```go
  const n = byte(len(alphabet))  // n = 32, byte 类型
  for _, b := range buf {
      if int(b) < int(n)*256/int(n) { // 永远成立
          if b < n {
              out = append(out, alphabet[b])
              ...
  ```
- **问题**:
  - **注释 misleading**:代码里 if 条件 `int(b) < int(n)*256/int(n)` 永远是 true(恒等式),意图其实是 rejection sampling,但**实现有 bug**
  - 实际:
    - `n` 是 `byte = 32`
    - `int(n)*256/int(n)` = `32*256/32` = `256`
    - `int(b) < 256` → 永远成立(`b` 是 byte,0-255)
    - 所以**实际上没做 rejection sampling**,直接 `b < n`(b 在 0-31 范围)才接受 → **模偏(modulo bias)**!
    - alphabet index 0-31 平均出现(实际 256/32 = 8 倍概率),所以**均匀** → 巧合
  - **但如果 alphabet 大小不是 256 的因子**(比如改成 50 字符)就有偏 → **潜在 bug**
- **参考**:
  - [crypto/rand 模块偏误](https://stackoverflow.com/questions/25417467/generating-a-random-byte-with-bias)
  - [NIST SP 800-90A §7.3 — Rejection Sampling](https://csrc.nist.gov/publications/detail/sp/800-90a/rev-1/final)
- **修复方案**:
  ```go
  func GeneratePassword(length int) (string, error) {
      if length < 6 || length > 64 {
          return "", fmt.Errorf("password length must be in [6, 64], got %d", length)
      }
      const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
      const n = byte(len(alphabet))  // 32
      out := make([]byte, 0, length)
      buf := make([]byte, length*2)
      for {
          if _, err := rand.Read(buf); err != nil {
              return "", fmt.Errorf("rand.Read: %w", err)
          }
          for _, b := range buf {
              // Rejection sampling:256/32 = 8 整除,所以 byte 永远 in range,无需 reject
              // 但代码意图应是 uniform,所以加显式阈值保护
              if b < n {  // 32 整除 256,均匀
                  out = append(out, alphabet[b])
                  if len(out) == length {
                      return string(out), nil
                  }
              }
          }
      }
  }
  ```
  - 简化:注释删除,直接 `if b < n`(32 是 256 的因子,均匀)
  - 或更通用:`limit := byte(256 / int(n) * int(n))` 然后 `if b < limit`(正确 rejection)
  - 测试:加 statistical test(1000 次生成,卡方检验)
- **工作量**:0.1d
- **优先级**:下批

---

### ISSUE-W13:无 unicode / IDN 支持,中文 admin / 用户名不支持

- **位置**:
  - [internal/web/handlers_users.go:18](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go#L18) `usernameRe = regexp.MustCompile(\`^[a-z0-9_-]{3,32}$\`)`
- **问题**:**强制 ASCII**,真实用户场景(admin 中文名)不能用
- **参考**:
  - [OWASP Authentication Cheatsheet — Username](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#username)
  - [Unicode Security Mechanisms (UTS #39)](https://www.unicode.org/reports/tr39/)
- **修复方案**:
  1. **放宽 regex**:`^[\p{L}\p{N}_-]{3,32}$`(Unicode 字母数字)
  2. **加 normalization**:`golang.org/x/text/unicode/norm.NFC.String(username)` 防 unicode 归一化攻击
  3. **Reject confusables**:用 [UTS #39 confusables data](https://www.unicode.org/Public/security/latest/) 防同形字符攻击
- **工作量**:0.5d
- **优先级**:下批

---

### ISSUE-W14:`renderInstallError` 内联 HTML 字符串,msg 由 `fmt.Sprintf` 拼(无 XSS 但脆)

- **位置**:[internal/web/handlers_config.go:163-182](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L163-L182)
  ```go
  body := fmt.Sprintf(`<!DOCTYPE html>...<p>%s</p>...`, msg)
  ```
- **问题**:
  - `msg` 当前是常量字符串(`"链接已失效或不存在..."` / `"用户不存在或已被删除。"`),**没用户输入** → 当前无 XSS
  - 但**模式脆**:未来如果 `msg` 来源变成 DB 错误 / 用户输入 → 立即 XSS
- **参考**:
  - [Go html/template 安全 vs fmt.Sprintf](https://pkg.go.dev/html/template)
- **修复方案**:改成 `text/template` 渲染(`TPL := template.Must(template.New("err").Parse(htmlTemplate))`)
- **工作量**:0.05d
- **优先级**:下批

---

### ISSUE-W15:`handleInstallByToken` 错误回显暴露内部信息

- **位置**:[internal/web/handlers_config.go:131](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L131) `s.Logger.Error("install: user gone", "user_id", userID, "err", err)`(仅服务端)
- **问题**:`s.renderInstallError(w, "用户不存在或已被删除。")` **本身 OK**(无信息泄露)。但注释也 OK
- **实际不需要修**,记入本报告作为"已审计,无问题"的证据
- **优先级**:N/A

---

### ISSUE-W16:`BodyHTML` 用反射注入 `template.HTML`,理论上可被恶意 handler 注入未转义内容

- **位置**:[internal/web/templates.go:200-238](file:///opt/ikev2-panel-v2-main/internal/web/templates.go#L200-L238)
- **问题**:
  - 流程:handler → `RenderPage(w, page, data)` → 子模板先渲染到 buffer → buffer 包成 `template.HTML` 注入 `data.BodyHTML` → layout 渲染 `{{.BodyHTML}}`
  - **风险**:子模板渲染时如果用了 `template.HTML(...)` 直接转义(safe wrapper),就把未转义内容塞进 BodyHTML → layout 也按 template.HTML 不转义渲染
  - **当前所有模板没用 safe wrapper**(grep 0 处) → **当前无 XSS**
  - 但**反射机制**留了"未来不安全代码可能"的窗口
- **参考**:
  - [Go html/template 自动转义](https://pkg.go.dev/html/template#hdr-Contexts)
- **修复方案**:
  1. **加 audit**:每次 `RenderPage` 后,grep 整个 `web/templates/` 确认没有 `template.HTML(...)` 直接用
  2. **加注释**:禁止模板里用 `safe` / `template.HTML`
  3. **(可选)改 `BodyHTML` 类型为 `string`**:但 html/template 默认会转义 string → layout 渲染时双层转义会破(需要 confirm)
- **工作量**:0.05d
- **优先级**:下批

---

## 不在范围内

故意**不审计**:

- ❌ Go stdlib `net/http` 内部实现
- ❌ `modernc.org/sqlite` 加密扩展(只是 driver)
- ❌ strongSwan 内部 charon

## 借鉴参考(供方案决策用)

- **认证 / Session**:
  - [OWASP Authentication Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
  - [OWASP Session Management Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
  - [OWASP Logging Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
- **CSRF**:
  - [OWASP CSRF Prevention Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)
  - [gorilla/csrf](https://github.com/gorilla/csrf)
  - [RFC 6265 bis — SameSite cookies](https://datatracker.ietf.org/doc/html/draft-ietf-httpbis-rfc6265bis)
- **HTTP 安全 header**:
  - [OWASP Secure Headers Project](https://owasp.org/www-project-secure-headers/)
  - [Mozilla Observatory](https://observatory.mozilla.org/)
  - [HSTS RFC 6797](https://datatracker.ietf.org/doc/html/rfc6797)
- **输入校验**:
  - [OWASP Input Validation Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html)
  - [Unicode Security Mechanisms (UTS #39)](https://www.unicode.org/reports/tr39/)
- **Go 模板**:
  - [html/template docs](https://pkg.go.dev/html/template)
  - [text/template vs html/template](https://pkg.go.dev/text/template)
- **Rate limiting**:
  - [golang.org/x/time/rate](https://pkg.go.dev/golang.org/x/time/rate)
  - [chi-ratelimit](https://github.com/go-chi/httprate)
  - [fail2ban](https://github.com/fail2ban/fail2ban)
- **标杆项目**:
  - [authentik](https://github.com/goauthentik/authentik) — modern Python + Go 混合,OIDC + audit + 全 middleware
  - [casdoor](https://github.com/casdoor/casdoor) — Go 全栈,OIDC + RBAC + audit
  - [dex](https://github.com/dexidp/dex) — Go OIDC provider,工业级 session
  - [grafana](https://github.com/grafana/grafana) — Go 大型 web 应用的 session / cookie 参考

---

## 建议优先级

### 本周(W1,与 audit-2026-09-security.md 配合)

| 优先级 | ISSUE | 工作量 | 与 security audit 重复 |
|--------|-------|--------|------------------------|
| 🔴 | **W01 HTTP 安全 header 全缺失** | 0.5d | 部分(S07 HSTS) |
| 🔴 | **W02 CSRF 加固(Origin 校验)** | 0.5d | 否 |
| 🔴 | **W03 全 POST 端点 rate limit** | 1.0d | 是(S01) |
| 🟡 | **W04 错误信息脱敏** | 0.3d | 是(S10) |
| 🟡 | **W05 删 android handler flash query string** | 0.1d | 否 |
| 🟡 | **W06 session idle timeout + 强制下线** | 0.5d | 否 |
| 🟡 | **W07 Cookie Secure / HTTPS 强制跳** | 0.3d | 是(S08) |
| 🟡 | **W08 输入校验:trim / range / DB CHECK** | 0.1d | 是(S14) |
| 🟡 | **W10 审计日志表 + UI** | 1.0d | 否 |
| ⚪ | W09 filename sanitized | 0.1d | |
| **小计** | | **4.4d** | |

### 下周(W2 - 跟 Phase 2 正确性一起做)

- W11 install token 加固(绑 IP / revoke)
- W12 GeneratePassword rejection sampling + 注释
- W13 Unicode / IDN 支持
- W14 renderInstallError 用 text/template
- W16 BodyHTML 反射机制 audit

### P2(后续版本)

- W15 install token 错误信息审查(记入即可)

---

## 完成定义

- [ ] W01 / W02 / W03 修复(本周)
- [ ] W04 / W05 / W06 / W07 / W08 / W10 修复(本周)
- [ ] 每个修复有对应 test 覆盖
- [ ] Mozilla Observatory 跑分 ≥ A
- [ ] OWASP ZAP 基础扫描无 HIGH
- [ ] release-notes 记录修复内容
- [ ] 用户可见的安全警告加进 README

---

## 下一步

1. 你评审本报告,挑要修的 issue
2. 我**不批量改** — 一个 issue 一个 PR
3. 修完再回头看**问题是否解决**(再跑测试 + 真机验证)
4. 跟 `audit-2026-09-security.md` W1(W1 = S01-S15)一起 review

## 附录:本报告跟 audit-2026-09-security.md 重复 issue 对照表

| 本报告 issue | security audit 重复 | 差异 |
|--------------|---------------------|------|
| W03 全 POST 端点 rate limit | S01 登录无 rate limit | **W03 范围更大**:覆盖 login + 所有 CRUD + /api/* + /install |
| W01 HTTP 安全 header 全缺失 | S07 HSTS 缺失 | **W01 范围更大**:CSP / X-Frame-Options / X-Content-Type-Options / Referrer-Policy / Permissions-Policy 全缺 |
| W04 错误响应泄漏 DB / 内部细节 | S10 敏感错误信息可能泄漏 | **W04 新增**:handleUserAndroidConfig 错误回显 |
| W07 Cookie Secure / SameSite 行为依赖配置项 | S08 Cookie Secure 行为 | **W07 新增**:HTTPS 强制跳转建议 |
| W05 android handler flash query string | S14 输入长度未限制 | **W05 新增**:具体到一个 handler 的回归 |
| W08 输入校验 trim / range / DB CHECK | S14 输入长度未限制 | **W08 更细**:expire 范围 / DB CHECK |