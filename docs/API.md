# HTTP API 参考

面板的 HTTP 接口清单，共 **49 条**。

- **服务端**：[`internal/web/server.go`](../internal/web/server.go)（路由注册）
- **约定**：Go 1.22 `net/http.ServeMux` 模式（`"GET /path/{id}"`），非第三方面板框架
- **渲染**：绝大多数是服务端渲染 HTML；只有 `/api/*` 和少数端点返回 JSON

> 本文只列**接口契约**。内部实现见 [architecture.md](architecture.md)。

---

## 1. 鉴权模型

三条路由带，按敏感度递增：

| 带 | 中间件链 | 用途 |
|---|---|---|
| **公开** | `secureHeaders()` | 登录页、健康检查、CA 证书下载、install token |
| **protect** | `requireSession` | 全部 GET 页面/API。未登录 → **302 → `/login`** |
| **protectPOST** | `requireSession` + `requireCSRF` + 全局限速 | 全部写操作 |

### 各条带的具体规则

**公开**
- `GET /healthz` / `GET /readyz` / `GET /metrics` —— 供监控拉取，不鉴权
- `GET /login` + `POST /login` —— **额外挂登录限流**（防暴力破解）
- `GET /ca.cert.pem` —— CA 证书公开下载（客户端要导入）
- `GET /install/{token}` —— 一次性安装 token，扫码场景用，**与登录态互斥**

**protect**（`requireSession`）
读 session cookie → 查库校验过期 → 注入 ctx。未登录 302 跳登录页，**不返回 401**。

**protectPOST**（`requireSession` → `requireCSRF` → 限速）
在 protect 之上再加两道：

| 检查 | token 来源 | 失败 |
|---|---|---|
| CSRF | header `X-CSRF-Token`（前端 JS）或表单字段 `csrf_token`（普通表单） | **403** |
| 限速 | 任意已登录 POST **30 req/min** | 429 |

**CSRF token 只在登录后签发**，所以 `POST /login` 本身不在 protectPOST 内。

### 响应约定

| 场景 | 状态码 |
|---|---|
| 表单提交成功 | **302** 重定向 + flash 消息（多数端点；`mobileconfig-options` 用 303） |
| 表单校验失败 | **303** 回原页面 + flash 报错 + 已填内容回填（**不再有 `http.Error` 纯文本页**） |
| API 成功 | 200 + JSON |
| API 失败 | 200 + JSON `{error: "..."}`（部分端点）或 4xx/5xx |
| CSRF / 鉴权失败 | 403 |
| 未登录访问 protect | 302 → `/login` |

**为什么校验失败走 303**：`http.Error` 写出的是脱离站点的纯文本页 ——
没有导航、没有返回链接、已填内容全丢。数据流见 architecture §3.11。

---

## 2. 公开端点

### `GET /healthz`

存活探针 + 4 项依赖检查。**任一失败 → 503**。

```json
{
  "ok": true,
  "time": "2026-10-12T09:28:10Z",
  "checks": {
    "db":   {"ok": true, "latency_ms": 1},
    "vici": {"ok": true, "latency_ms": 2},
    "le":   {"ok": true, "detail": "self-signed mode", "latency_ms": 0},
    "ddns": {"ok": true, "detail": "never synced (cold start)", "latency_ms": 0}
  }
}
```

| check | 失败意味着 | dev 模式 |
|---|---|---|
| `db` | SQLite 打不开 | `detail: "store not initialized"` |
| `vici` | charon socket 连不上 —— **最常见的静默半瘫信号**，见 [TROUBLESHOOTING §8.1](TROUBLESHOOTING.md) | `detail: "skipped (dev mode)"` |
| `le` | LE 模式且 `LAST_RENEW_FAILED` 标志存在 | 非 LE 模式时 `detail: "self-signed mode"` |
| `ddns` | 最近一次同步失败 | 冷启动显示 `never synced (cold start)` |

**结果缓存 5 秒 + 单飞**（`healthCacheTTL`）。这不是优化，是防匿名 DoS：
该端点无鉴权，每次调用都要打 DB + 开 VICI socket（占 charon fd）。
不限流的话任何人循环打就能打满 charon 的 fd 和 SQLite 连接池，
重则 charon 拒新连接、正在协商的 IKE 失败。

命中缓存时 `time` 刷新为当前时间（看起来新鲜），各组件 `latency_ms` 保持实测值。

### `GET /readyz`

比 `healthz` 多一条：**charon 至少加载了 1 个 SA**（即 `connections` 非空）。
dev 模式（`SkipVici=true`）跳过这项检查直接视为 ready。

### `GET /metrics`

Prometheus 文本格式（`text/plain; version=0.0.4`），**不引入 prom client 库**。

```
# HELP panel_login_attempts_total Panel login attempts, labeled by result.
# TYPE panel_login_attempts_total counter
panel_login_attempts_total{result="success"} 3

# HELP http_requests_total Total HTTP requests, labeled by method/path/status.
# TYPE http_requests_total counter
http_requests_total{method="GET",path="/users",status="200"} 42

# HELP http_request_duration_seconds HTTP request duration in seconds.
# TYPE http_request_duration_seconds histogram
http_request_duration_seconds_bucket{method="GET",path="/users",le="0.1"} 40
```

三个指标：登录成败计数、请求计数、请求耗时直方图。

label 基数上限 **4096**（`maxLabelSeries`），超限的新 key 归并到
`path="{overflow}"` 桶 —— 防"请求方无限造新 path"导致 map 无界增长
（内存耗尽 DoS）。上界预留 1 个槽位给 overflow 桶本身。

### `GET /ca.cert.pem`

返回 PEM 格式 CA 证书。客户端（尤其 Android）要导入它，否则弹"证书不可信"。

### `GET /install/{token}`

一次性安装 token。校验通过后 stream mobileconfig。

| 响应头 | 值 | 为什么 |
|---|---|---|
| `Content-Type` | `application/x-apple-aspen-config; charset=utf-8` | iOS 看到它 + 用户点"安装" → 自动弹"安装描述文件"对话框 |
| `Content-Disposition` | `attachment; filename="<username>.mobileconfig"` | |
| `Cache-Control` | `no-store` | 防 CDN / 代理缓存过期 mobileconfig |

token 消费后即失效；失效 / 不存在 / 过期都返回 **404 + 中文提示页**
（引导"请在管理面板重新生成二维码"），不是裸 404。

**与登录态互斥**：扫码的人没有账号密码也能装配置。

mobileconfig 渲染是**三层合并**：builtin 出厂值 + admin 全局默认 + 用户覆盖项。
用户覆盖项 JSON 损坏时返回 500 并明确报"mobileconfig 配置损坏"。

---

## 3. 页面路由（HTML）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 首页：概览 + 告警横幅 |
| GET | `/login` | 登录页 |
| POST | `/login` | 提交账号密码 |
| POST | `/logout` | 登出 |
| GET | `/users` | 用户列表 |
| GET | `/users/new` | 新建用户表单 |
| POST | `/users` | 创建用户 → 302 `/users` + flash 显示初始密码 |
| GET | `/users/{id}` | 用户详情 |
| GET | `/users/{id}/delete` | 删除确认页（**双确认第一步**） |
| POST | `/users/{id}/delete/confirm` | 真正删除 |
| GET | `/users/{id}/mobileconfig` | 下载 `.mobileconfig` |
| GET | `/users/{id}/mobileconfig.qr.png` | 二维码 PNG |
| GET | `/users/{id}/sswan` | strongSwan 客户端导入文件（`.sswan`） |
| GET | `/users/{id}/android` | Android 11+ 系统设置配置指引 |
| GET | `/account` | 改自己密码的表单 |
| GET | `/audit` | 审计日志 |
| GET | `/static/` | 静态资源 |
| GET | `/admin/mobileconfig-defaults` | 全局默认值设置页 |

**双确认删除**：GET 只拉确认页，POST 才真删。避免误触或 CSRF 造成不可逆操作。

---

## 4. 用户操作 API

| 方法 | 路径 | 表单字段 | 说明 |
|---|---|---|---|
| POST | `/users/{id}/enable` | — | 启用（重建 `conf.d/<user>.conf`） |
| POST | `/users/{id}/disable` | — | 停用（**删 conf.d**，见 architecture §12.3） |
| POST | `/users/{id}/reset-password` | `password` | 重置密码 → 302 + flash 显示新密码 |
| GET | `/users/{id}/mobileconfig-options` | — | 读该用户的渲染覆盖项，**返回 JSON** |
| POST | `/users/{id}/mobileconfig-options` | 覆盖项字段 | 保存 → **303** `/users/{id}` + flash。**校验失败也是 303**：错误进 flash，本次已填内容带回表单回填（见 architecture §3.11） |
| POST | `/api/account/password` | `old_password` `new_password` | admin 改自己密码 |

**disable 的语义**：不是"标记一下"。`enabled=0` **不参与 IKE 认证** ——
必须真删掉 `conf.d/<user>.conf`，否则用户拿旧密码照样能拨。
启用时重建该文件。

⚠️ **本项目不再有返回 `400` 纯文本页的 POST 表单。**
校验失败一律 303 + flash；回填数据走服务端内存（FlashStore），不进 query string。
详见 architecture §3.11 与 §12.9。

---

## 5. 配置 API

请求体是 `application/x-www-form-urlencoded`（和普通表单一样）。

**响应是双模** —— 看 `Accept` 头：

| `Accept` | 响应 |
|---|---|
| `application/json` | JSON（前端 fetch 用） |
| 其他 / 缺省 | **302 重定向**到 `/` + flash 消息（普通表单提交用） |

所以用 `curl` 测试时要显式加 `Accept: application/json`，否则拿到的是 302。
见 [`internal/web/handlers_ddns.go`](../internal/web/handlers_ddns.go)。

### 5.1 DDNS

#### `GET /api/ddns/status`

```json
{
  "configured": true,
  "enabled": true,
  "family": "dual",
  "last_sync": {
    "time": "2026-10-12 09:28:10 CST",
    "success": true,
    "new_ip": "2408:832e::1cf",
    "old_ip": "",
    "v4_ip": "",
    "v6_ip": "2408:832e::1cf"
  },
  "failed": false
}
```

| 字段 | 含义 |
|---|---|
| `configured` | DDNS 同步器是否存在。`false` = 没启用此功能 |
| `family` | `v4` / `v6` / `dual` |
| `last_sync.time` | **空字符串 = 从未同步过** |
| `last_sync.v4_ip` / `v6_ip` | per-family 独立结果 |
| `failed` | 最近一次失败 → 面板显示告警横幅 |

时间按面板配置的时区渲染（`DisplayTimezone`），不是 UTC。

#### `POST /api/ddns/toggle`

`enabled=true|false`

#### `POST /api/ddns/family`

`family=v4|v6|dual`

> `family` 是**兼容入口**。新的配置模型是两个独立开关
> （见下面的 `config.enable_a` / `enable_aaaa`），`Family()` 只是给
> 状态接口和健康检查用的 getter。

#### `POST /api/ddns/config`

| 字段 | 说明 |
|---|---|
| `rr` | 记录名，如 `vpn`。`@` 或空 = 主域 |
| `domain` | 主域名，如 `example.com` |
| `enable_a` | `true`/`false` — 是否同步 A 记录 |
| `enable_aaaa` | `true`/`false` — 是否同步 AAAA 记录 |
| `period_seconds` | 同步周期。改完**下一轮即生效，不需要重启** |

#### `POST /api/ddns/create-record`

`record_type=A|AAAA` + `value=<IP>`

在阿里云上建一条记录（`AddDomainRecord`），供面板「在阿里云创建」按钮调用 ——
用户想立刻建好、不等下个同步周期时用。RAM 没勾 `alidns:AddDomainRecord` 会如实报错，
不会假装成功。

> 后台同步循环本身也会自动建：查询不到记录时用当前探测到的公网 IP 调
> `AddDomainRecord`（TTL 600s），行为与 ddns-updater 的 aliyun / cloudflare
> provider 一致。创建失败会记为本轮同步失败。

#### `POST /api/ddns/sync-now` / `POST /api/ddns/fetch-remote`

立即触发同步 / 拉取远端当前值。**绕过周期限流**，仅面板 UI 调用。

### 5.2 阿里云凭证

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/aliyun/status` | 当前凭证状态（已配置/来源） |
| POST | `/api/aliyun/save` | 保存 AccessKey ID + Secret |
| POST | `/api/aliyun/clear` | 清除 |

**面板填的凭证优先于 env。** 凭证由 `CredentialGetter` 在**每个同步周期现读**，
所以保存后无需重启 —— 最坏等一个周期生效（不想等就点 `POST /api/ddns/sync-now`）。

> 排查 DDNS 时**判断记录是否存在必须用阿里云 API**，不能用 `dig`。
> 见 [TROUBLESHOOTING §4.4](TROUBLESHOOTING.md)。

### 5.3 证书配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/cert/status` | 当前证书配置 |
| POST | `/api/cert/save` | 保存（**需重启容器生效**） |
| POST | `/api/cert/clear` | 清除，回落到 env |

### 5.4 客户端虚拟 IP 段

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/subnet/status` | panelstate 值 + swanctl 当前生效值 |
| POST | `/api/subnet/save` | 保存 + **立即 reload**（无需重启） |
| POST | `/api/subnet/clear` | 清 panelstate（重启后回 env/自动探测） |

### 5.5 mobileconfig 全局默认值

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/mobileconfig-defaults` | 设置页 |
| POST | `/admin/mobileconfig-defaults` | 保存（运行时热改，无需重启） |
| POST | `/admin/mobileconfig-defaults/clear` | 清除，回 builtin 出厂值 |

---

## 6. 加新端点的清单

1. 在 `server.go` 注册，**选对带**：只读 GET → `protect`，写操作 → `protectPOST`
2. handler 签名 `func (s *Server) handleXxx(w http.ResponseWriter, r *http.Request)`
3. 写操作**必须**校验 `r.ParseForm()` 的错误
4. JSON 端点设 `Content-Type: application/json`
5. 写操作产生敏感变更 → 调 `audit` 记一笔
6. 补测试：新端点至少覆盖"未登录被拦"和"CSRF 缺失被拦"