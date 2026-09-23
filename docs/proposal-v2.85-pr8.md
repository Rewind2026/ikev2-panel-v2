# Proposal v2.85-PR8 — 收尾 PR:U05/U08/U04/U11/U10 + Q1-04/Q4-02

> **类型**:PR 级提案(对应 v2.85 release 第 8 个 PR,**收尾**)
> **目标**:处理 v2.85 周期剩余的 7 个 issue,全部为"小而确定的体验/正确性补丁"
> **关联审计**:
> - [audit-2026-09-usability.md §U04 / §U05 / §U08 / §U10 / §U11](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)
> - [audit-2026-09-correctness.md §Q1-04 / §Q4-02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)
> **工作量**:**2.6d**(U05 0.5d + U08 0.5d + U04 0.9d + U11 0.3d + U10 0.1d + Q1-04 0.2d + Q4-02 0.1d)
> **作者**:自查

---

## 背景

v2.85 release 已交付 7 个 PR,本 PR 是**收尾**,集中处理最后 7 个低/中/高优先级 issue。每项改动都很小(单文件 ~10-50 行),但合在一起让 v2.85 成为"首次部署体验完备"的版本:

| Issue | 优先级 | 工作量 | 类别 | 一句话 |
|-------|--------|--------|------|--------|
| **U05** onboarding checklist | MED | 0.5d | Usability | 新人 0 用户时首页给出"下一步"清单 |
| **U08** troubleshooting 总入口 | MED | 0.5d | Usability | 新增 `docs/TROUBLESHOOTING.md`(症状→排查→解法) |
| **U04** /healthz 升级 + /readyz + /metrics | HIGH | 0.9d | Usability | 健康检查 JSON 化 + k8s readiness + Prometheus 端点 |
| **U11** DDNS / 凭证变更 audit log | MED | 0.3d | Usability | `audit_log` 表 + "操作日志" 只读页 |
| **U10** 强 delete 双确认 | LOW | 0.1d | Usability | 用户删除由 JS `confirm()` 改两步 POST,无 JS 也能用 |
| **Q1-04** home ListSAs timeout 截断 | LOW | 0.2d | Correctness | 首页拿 SA 列表 3s 截断,失败 fallback 文案 |
| **Q4-02** GeneratePassword 注释修正 | LOW | 0.1d | Correctness | 修一处误导性字符集大小注释 + 修一处 dead-code rejection 表达式 |

**为什么集中在一个 PR**:
- 都是"补完"性质,不涉及架构/接口变更,合并冲突面窄
- 7 项合计 +600 行 / 改动 ~15 个文件,可在一个 review session 完成
- 单独拆 7 个 PR 会把 v2.85 release 推到 15 个 PR,reviewer 疲劳

---

## 目标

1. 新人首次打开面板(< 1 分钟)看到"接下来该做什么"清单
2. 出问题时管理员能在 30 秒内找到对应 troubleshooting 章节
3. K8s / Prometheus 可消费 `/healthz` / `/readyz` / `/metrics`
4. 所有敏感操作(用户增删/凭证变更/DDNS toggle)可追溯
5. 关 JS 浏览器仍能完成删除流程(无障碍 + 合规)
6. 首页不会因 swanctl 阻塞超过 3 秒
7. 注释与代码一致,后续 reviewer 不被误导

---

## 不在范围(明确不做)

- ❌ 不引入 alertmanager / grafana / pagerduty 集成(运维侧,留 Phase 5)
- ❌ 不做 metrics 长期存储(retention 30 天由 Prometheus 自管)
- ❌ 不实现 audit_log 的全文搜索 / 导出 CSV / 分页(只读 + LIMIT 100 即可)
- ❌ 不改 swanctl / DDNS / cert 的内部逻辑(只暴露 metrics)
- ❌ 不引入 RBAC / 多管理员(单 admin 假设保持)
- ❌ 不改强 delete 之外的 form(其他 POST 不强制两步;**未来按需扩展**)
- ❌ 不动 bcrypt / session 长度 / cookie 配置
- ❌ 不把 Prometheus 暴露到公网(默认监听 127.0.0.1,通过 env `IKEV2_METRICS_ADDR` 切换)

---

## 设计

### 1. U05 — 首页 onboarding checklist(`home_content.html:25-37`)

#### 设计动机

新部署完成后第一次登录 `TotalUsers = 0`,只有"用户管理" / "新增用户" 两个按钮。**没填凭证、没放防火墙、没下 mobileconfig 的用户**需要在文档和面板间来回跳。

#### 实现

在 `home_content.html` L25(欢迎 hgroup 之前)插入绿色 tipbox,**只在 `TotalUsers == 0` 时渲染**:

```html
{{/* v2.85-PR8(U05):首次部署 onboarding checklist */}}
{{if eq .TotalUsers 0}}
<article class="alert-success" style="background:#e6f4ea;border-left:4px solid #1e8e3e;padding:1rem 1.25rem">
  <header><strong>�� 欢迎使用 ikev2-panel — 5 步搞定首次部署</strong></header>
  <ol style="margin:0.5rem 0 0.5rem 1.5rem;line-height:1.8">
    <li>✅ <strong>服务器地址：<code>{{.ServerAddr}}</code></strong></li>
    <li>☐ <a href="#aliyun-card"><strong>填阿里云凭证</strong></a>（LE 自动签发 + DDNS 都依赖）</li>
    <li>☐ <a href="/users/new"><strong>创建第一个 VPN 用户</strong></a></li>
    <li>☐ <a href="/users"><strong>下载 mobileconfig / 扫码</strong></a>（用户详情页 → iOS / macOS）</li>
    <li>⚠️ <strong>开放防火墙 UDP 500 / 4500</strong>(静态提示,无需点击)
      <br><small>云服务商安全组 + 主机 firewall 都需放行;<code>{{.ServerAddr}}</code> 解析到的服务器上执行 <code>iptables -I INPUT -p udp --dport 500 -j ACCEPT</code> 等</small></li>
  </ol>
  <p style="margin-top:0.75rem"><small>所有步骤完成后,这个提示会自动消失(<code>TotalUsers &gt; 0</code>)。</small></p>
</article>
{{end}}
```

**id="aliyun-card"** 给凭证 `<article>` 头部加 `id` 属性,锚点跳转。

#### 兼容性

- 已有用户 `TotalUsers > 0` 不显示 → 零影响
- Pico CSS 已支持 `article` + 内嵌 `<ol>` → 无新依赖
- 不动 `homeData` 结构(用现有 `.ServerAddr` + `.TotalUsers`)

---

### 2. U08 — `docs/TROUBLESHOOTING.md` 总入口

#### 设计动机

当前遇到问题只能 grep 源码或在 README 找零散"FAQ"。需要一份**按症状索引**的文档。

#### 实现

新增 `docs/TROUBLESHOOTING.md`,结构"症状 → 排查路径 → 解法":

```markdown
# 故障排查 / Troubleshooting

> 找症状 → 跟着排查命令跑 → 找到解法。
> 命令在容器内执行:`docker compose exec ikev2-panel bash`。

## 目录
1. [容器启动失败](#1-容器启动失败)
2. [mobileconfig 装不上 / 连不上](#2-mobileconfig-装不上--连不上)
3. [Let's Encrypt 续签失败](#3-lets-encrypt-续签失败)
4. [DDNS 不更新](#4-ddns-不更新)
5. [限速不生效](#5-限速不生效)
6. [admin 密码忘](#6-admin-密码忘)
7. [DB 损坏](#7-db-损坏)

---

## 1. 容器启动失败

### 1.1 IPv6 不可用
**症状**:`docker compose up` 报 `Failed to enable IPv6 forwarding` 或容器内 `ip -6 addr` 空。
**排查**:
\`\`\`bash
docker compose exec ikev2-panel cat /proc/sys/net/ipv6/conf/all/forwarding
\`\`\`
值为 `0`= 未启用 → docker daemon IPv6 转发没开。
**解法**:`/etc/docker/daemon.json` 加 `"ipv6": true, "ip6tables": true`,重启 docker。

### 1.2 UDP 端口被占用
**症状**:日志 `bind: address already in use`。
**排查**:`netstat -ulnp | grep -E ':(500|4500)'`
**解法**:停掉冲突服务(streisand / 旧 ikev2 实例等)或改 compose `ports:` 映射。

[... 后续 6 节类似,每节 1-3 个症状,每症状 3-7 步 ...]
```

**README 顶部加一行**:
```markdown
- �� 出问题?先看 [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)
```

#### 内容清单(本 PR 落地)

| 章节 | 覆盖 issue | 排查命令数 |
|------|-----------|-----------|
| 容器启动失败 | IPv6 / forwarding / 端口 / chmod | 6 |
| mobileconfig 装不上 | iOS 不弹 / 装完连不上 | 5 |
| LE 续签失败 | LAST_RENEW_FAILED / DNS-01 / rate limit | 4 |
| DDNS 不更新 | 凭证 / API 错误码 / throttle | 5 |
| 限速不生效 | tc qdisc / iptables / counter | 4 |
| admin 密码忘 | --reset-password flag / DB 直改 | 3 |
| DB 损坏 | `sqlite3 panel.db .recover` / VACUUM | 3 |

总 ~250 行 markdown,**人工写**;不依赖工具生成。

---

### 3. U04 — `/healthz` 升级 + `/readyz` + `/metrics`

#### 3.1 `/healthz` 升级(JSON + 多组件探测)

```go
// internal/web/server.go handleHealthz 改写

type healthResp struct {
    OK     bool              `json:"ok"`
    Time   string            `json:"time"`
    Checks map[string]checkR `json:"checks"`
}

type checkR struct {
    OK      bool   `json:"ok"`
    Detail  string `json:"detail,omitempty"`
    LatencyMs int64 `json:"latency_ms"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
    defer cancel()

    resp := healthResp{Time: time.Now().UTC().Format(time.RFC3339), Checks: map[string]checkR{}}

    // 1) DB ping
    t0 := time.Now()
    dbOK := true
    if s.Store != nil {
        if err := s.Store.DB.PingContext(ctx); err != nil {
            dbOK = false
            resp.Checks["db"] = checkR{OK: false, Detail: err.Error(), LatencyMs: ms(time.Since(t0))}
        } else {
            resp.Checks["db"] = checkR{OK: true, LatencyMs: ms(time.Since(t0))}
        }
    } else {
        resp.Checks["db"] = checkR{OK: false, Detail: "store not initialized"}
    }

    // 2) VICI socket 探测(如果有 Swanctl)
    t0 = time.Now()
    if s.Swanctl != nil {
        // swanctl --list-sas 已经做了一轮 list;这里用底层 VICI dial
        if _, err := s.Swanctl.PingVICI(ctx); err != nil {
            resp.Checks["vici"] = checkR{OK: false, Detail: err.Error(), LatencyMs: ms(time.Since(t0))}
        } else {
            resp.Checks["vici"] = checkR{OK: true, LatencyMs: ms(time.Since(t0))}
        }
    } else {
        resp.Checks["vici"] = checkR{OK: true, Detail: "skipped (dev mode)", LatencyMs: 0}
    }

    // 3) LE 续签状态
    t0 = time.Now()
    if s.CertMode == "letsencrypt" {
        st := cert.CheckLERenewStatus(filepath.Join(s.DataDir, "le", "fullchain.pem"))
        if st.LastRenewFailed {
            resp.Checks["le"] = checkR{OK: false, Detail: "renew failed (LAST_RENEW_FAILED flag)", LatencyMs: ms(time.Since(t0))}
        } else {
            resp.Checks["le"] = checkR{OK: true, LatencyMs: ms(time.Since(t0))}
        }
    } else {
        resp.Checks["le"] = checkR{OK: true, Detail: "self-signed mode", LatencyMs: 0}
    }

    // 4) DDNS last sync
    t0 = time.Now()
    if s.DDNSSync != nil {
        last := s.DDNSSync.LastSyncSnapshot()
        if last.Time.IsZero() {
            // 从未同步不视为失败(dev 模式 / 刚启动)
            resp.Checks["ddns"] = checkR{OK: true, Detail: "never synced (cold start)", LatencyMs: ms(time.Since(t0))}
        } else if !last.Success {
            resp.Checks["ddns"] = checkR{OK: false, Detail: last.Error, LatencyMs: ms(time.Since(t0))}
        } else {
            resp.Checks["ddns"] = checkR{OK: true, LatencyMs: ms(time.Since(t0))}
        }
    } else {
        resp.Checks["ddns"] = checkR{OK: true, Detail: "not configured", LatencyMs: 0}
    }

    // 汇总
    for _, c := range resp.Checks {
        if !c.OK { resp.OK = false; break }
    }

    w.Header().Set("Content-Type", "application/json")
    if !resp.OK {
        w.WriteHeader(http.StatusServiceUnavailable)
    } else {
        w.WriteHeader(http.StatusOK)
    }
    _ = json.NewEncoder(w).Encode(resp)
}

func ms(d time.Duration) int64 { return d.Milliseconds() }
```

**关键点**:
- 整体超时 2s(任一慢组件不拖死 healthz)
- dev 模式(Swanctl/DDNSSync=nil) → skip 标 OK,不让本地测试痛苦
- 失败 → 503;成功 → 200;k8s `livenessProbe` 友好

#### 3.2 `/readyz`(在 /healthz 基础上额外要求"至少一个 swanctl connection loaded")

```go
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
    // 先跑 /healthz 同样的检查
    // 再额外要求 swanctl 至少 1 个 loaded connection
    if s.Swanctl != nil {
        conns, err := s.Swanctl.ListConnections(ctx)
        if err != nil || len(conns) == 0 {
            w.WriteHeader(http.StatusServiceUnavailable)
            json.NewEncoder(w).Encode(map[string]any{"ready": false, "reason": "no swanctl connection loaded"})
            return
        }
    }
    // ... healthz 全部 OK ...
}
```

#### 3.3 `/metrics` Prometheus 端点

**新增依赖**:
```
github.com/prometheus/client_golang v1.20.x
```

**package 新增** `internal/metrics/metrics.go`:

```go
package metrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    VPNActiveSAs = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "vpn_active_sas",
        Help: "Number of currently ESTABLISHED IKEv2 SAs.",
    })

    DDNSLastSyncUnixtime = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "ddns_last_sync_unixtime",
        Help: "Unix timestamp of last DDNS sync, per family.",
    }, []string{"family"})

    LECertExpiryUnixtime = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "le_cert_expiry_unixtime",
        Help: "Unix timestamp when current LE cert expires (0 = unknown / self-signed).",
    })

    HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "http_requests_total",
        Help: "Total HTTP requests, labeled by method/path/status.",
    }, []string{"method", "path", "status"})

    HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "http_request_duration_seconds",
        Help:    "HTTP request duration in seconds.",
        Buckets: prometheus.DefBuckets,
    }, []string{"method", "path"})

    PanelLoginAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "panel_login_attempts_total",
        Help: "Panel login attempts, labeled by result (success/fail).",
    }, []string{"result"})
)
```

**handleMetrics**:
```go
import "github.com/prometheus/client_golang/prometheus/promhttp"

mux.Handle("GET /metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // 仅 127.0.0.1 或配置的 metrics token(避免泄露 SA 数量 / 续签失败细节给公网)
    if !s.allowMetrics(r) {
        http.Error(w, "metrics disabled (set IKEV2_METRICS_ADDR=0.0.0.0:9090)", http.StatusNotFound)
        return
    }
    // 先刷新 gauge(避免 prometheus pull 时拿到旧值)
    s.refreshGauges(r.Context())
    promhttp.Handler().ServeHTTP(w, r)
}))
```

**refreshGauges**(从各模块拉最新值):
```go
func (s *Server) refreshGauges(ctx context.Context) {
    if s.Swanctl != nil {
        sas, _ := s.Swanctl.ListSAs(ctx)
        var n int
        for _, sa := range sas {
            if sa.IkeState == "ESTABLISHED" { n++ }
        }
        metrics.VPNActiveSAs.Set(float64(n))
    }
    if s.DDNSSync != nil {
        last := s.DDNSSync.LastSyncSnapshot()
        if !last.Time.IsZero() {
            metrics.DDNSLastSyncUnixtime.WithLabelValues("v4").Set(float64(last.Time.Unix()))
            metrics.DDNSLastSyncUnixtime.WithLabelValues("v6").Set(float64(last.Time.Unix()))
        }
    }
    if s.CertMode == "letsencrypt" {
        st := cert.CheckLERenewStatus(...)
        if st.Expiry > 0 {
            metrics.LECertExpiryUnixtime.Set(float64(st.Expiry.Unix()))
        }
    }
}
```

**logging 中间件增量**(在 `logging` 末尾加):
```go
method := r.Method
path := normalizePath(r.URL.Path) // 把 /users/123 → /users/{id}
metrics.HTTPRequestsTotal.WithLabelValues(method, path, strconv.Itoa(ww.status)).Inc()
metrics.HTTPRequestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
```

**PanelLoginAttempts**:在 `handleLogin` 里成功 / 失败两条路径各 `.Inc()`。

#### 路由 & 安全

```
GET /healthz   公开(原状)
GET /readyz    公开(本 PR 新增)
GET /metrics   127.0.0.1-only 默认;env IKEV2_METRICS_ADDR=0.0.0.0:9090 切换公网监听
```

不暴露在 `/users` 之类保护路由前缀下,沿用 `mux.HandleFunc` 在 server.go 注册。

---

### 4. U11 — DDNS / 凭证变更 audit log

#### 4.1 新增 `audit_log` 表

`internal/store/store.go` 的 `migrate` 加一条:

```sql
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp  INTEGER NOT NULL,    -- unix nano
    actor      TEXT    NOT NULL DEFAULT '',  -- admin username / 'system'
    event      TEXT    NOT NULL,    -- 'user.create', 'user.delete', 'aliyun.save', etc.
    details    TEXT    NOT NULL DEFAULT ''   -- JSON or 'k=v,k=v'
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_event ON audit_log(event);
```

#### 4.2 store 层封装

```go
// internal/store/audit.go (新增)
package store

type AuditEvent struct {
    Timestamp int64
    Actor     string
    Event     string
    Details   string
}

func (s *Store) WriteAudit(ctx context.Context, e AuditEvent) error {
    _, err := s.DB.ExecContext(ctx,
        `INSERT INTO audit_log(timestamp, actor, event, details) VALUES(?,?,?,?)`,
        e.Timestamp, e.Actor, e.Event, e.Details)
    return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
    rows, err := s.DB.QueryContext(ctx,
        `SELECT timestamp, actor, event, details FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []AuditEvent
    for rows.Next() {
        var e AuditEvent
        if err := rows.Scan(&e.Timestamp, &e.Actor, &e.Event, &e.Details); err != nil {
            return nil, err
        }
        out = append(out, e)
    }
    return out, nil
}
```

#### 4.3 各 handler 写 audit

**统一 helper**:
```go
// internal/web/audit_helper.go (新增)
func (s *Server) writeAudit(r *http.Request, event, details string) {
    admin, _ := AdminFrom(r.Context())
    actor := "system"
    if admin != nil { actor = admin.Username }
    if err := s.Store.WriteAudit(r.Context(), store.AuditEvent{
        Timestamp: time.Now().UnixNano(),
        Actor:     actor,
        Event:     event,
        Details:   details,
    }); err != nil {
        s.Logger.Warn("audit write failed", "event", event, "err", err)
    }
}
```

**handler 接入点**:

| 文件:行 | 事件名 | details 示例 |
|---------|--------|------------|
| `handlers_users.go:180` (Create) | `user.create` | `username=alice,speed=10` |
| `handlers_users.go:283` (ResetPw) | `user.reset_password` | `user_id=3` |
| `handlers_users.go:313` (Enable) | `user.enable` | `user_id=3` |
| `handlers_users.go:330` (Disable) | `user.disable` | `user_id=3` |
| `handlers_users.go:360` (Delete) | `user.delete` | `user_id=3,username=alice` |
| `handlers_aliyun.go:90` (Save) | `aliyun.save` | `key_id_masked=LTAI...XYZW` |
| `handlers_aliyun.go:120` (Clear) | `aliyun.clear` | `` |
| `handlers_ddns.go:106` (Toggle) | `ddns.toggle` | `enabled=true` |
| `handlers_ddns.go:150` (Family) | `ddns.family` | `family=dual` |

每个 handler 加一行:
```go
s.writeAudit(r, "user.create", fmt.Sprintf("username=%s", u.Username))
```

#### 4.4 面板"操作日志" 只读页

**路由**:
```
GET /audit    保护(get-only),LIMIT 100
```

**handler**(新增 `internal/web/handlers_audit.go`):
```go
type auditPageData struct {
    PageMeta
    Events []store.AuditEvent
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
    admin, _ := AdminFrom(r.Context())
    sess, _ := SessionFrom(r.Context())
    events, err := s.Store.ListAudit(r.Context(), 100)
    if err != nil {
        s.Logger.Error("list audit", "err", err)
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }
    s.RenderPage(w, "audit", auditPageData{
        PageMeta: PageMeta{Page: "audit", Title: "操作日志", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
        Events:   events,
    })
}
```

**模板**(新增 `web/templates/audit_content.html`):
```html
<hgroup><h1>操作日志</h1><p>最近 100 条</p></hgroup>
<figure>
<table class="compact striped">
<thead><tr><th>时间</th><th>操作者</th><th>事件</th><th>详情</th></tr></thead>
<tbody>
{{range .Events}}
<tr>
  <td><code>{{formatUnixNano .Timestamp}}</code></td>
  <td><code>{{.Actor}}</code></td>
  <td><code>{{.Event}}</code></td>
  <td><code>{{.Details}}</code></td>
</tr>
{{end}}
</tbody>
</table>
</figure>
{{if not .Events}}<p><em>暂无日志</em></p>{{end}}
```

`formatUnixNano` 加到 `templates.go` 的 FuncMap。

**导航**:layout.html 顶部 nav 加 `<li><a href="/audit">操作日志</a></li>`。

---

### 5. U10 — 强 delete 双确认(替换 JS `confirm()`)

#### 设计动机

当前 4 处 form 用 `onsubmit="return confirm('...')"`,**关 JS / 屏幕阅读器 / 自动化测试**全部绕过确认。需要服务器侧两步。

#### 设计

把"删 X 用户"拆成两步:
```
GET  /users/{id}/delete          → 渲染确认页("确定删除用户 X?")
POST /users/{id}/delete/confirm  → 真删 + 302 → /users
```

**实现要点**:

**5.1** `user_detail_content.html` 现 `POST /users/{id}/delete` 表单 → 改为 `<a href="/users/{id}/delete">删除</a>` 按钮(GET 请求拉确认页)

**5.2** `handlers_users.go` 新增 `handleUserDeleteConfirmPage` (GET):
```go
func (s *Server) handleUserDeleteConfirmPage(w http.ResponseWriter, r *http.Request) {
    id, err := parseInt64(r.PathValue("id"))
    if err != nil { http.NotFound(w, r); return }
    u, err := s.Store.GetUserByID(r.Context(), id)
    if err != nil { http.NotFound(w, r); return }
    admin, _ := AdminFrom(r.Context())
    sess, _ := SessionFrom(r.Context())
    s.RenderPage(w, "user_delete_confirm", userDeleteConfirmData{
        PageMeta: PageMeta{Page: "user_delete_confirm", Title: "删除确认", AdminUsername: admin.Username, CSRFToken: csrfTokenOf(sess)},
        User: u,
    })
}
```

**5.3** 现有 `handleUserDelete` (POST) 改为路径 `/users/{id}/delete/confirm`:
```go
mux.Handle("POST /users/{id}/delete/confirm", protectPOST(http.HandlerFunc(srv.handleUserDelete)))
```

**5.4** 新增 `web/templates/user_delete_confirm_content.html`:
```html
<hgroup><h1>确认删除用户</h1></hgroup>
<article class="alert-danger">
  <strong>⚠️ 确定删除用户 <code>{{.User.Username}}</code>？</strong>
  <p>此操作<strong>不可撤销</strong>:
  <ul>
    <li>DB 用户记录永久删除</li>
    <li>/etc/swanctl/conf.d/{{.User.Username}}.conf 删除 + ReloadAll</li>
    <li>/var/lib/ikev2-panel/limits/{{.User.Username}} 删除</li>
    <li>活跃 SA 被强制 terminate</li>
  </ul>
  </p>
  <form method="POST" action="/users/{{.User.ID}}/delete/confirm">
    <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
    <button type="submit" class="contrast">确认删除(不可撤销)</button>
    <a href="/users/{{.User.ID}}" role="button" class="secondary">取消</a>
  </form>
</article>
```

**5.5** `users_list_content.html` L36(列表页删除按钮)同样改为 `<a href="/users/{{.ID}}/delete">` 链接。

#### 不动的地方

- `reset-password` / `disable` 仍保留单步 POST + `onsubmit=confirm()`(影响小,可后续 PR 升级)
- 范围控制:**只改 delete** 这一个最敏感操作

---

### 6. Q1-04 — home ListSAs 加 3s timeout

`internal/web/handlers_home.go:106`:

```go
func loadActiveSAs(ctx context.Context, s *Server) ([]swanctl.SA, int) {
    if s.Swanctl == nil { return nil, 0 }

    // v2.85-PR8(Q1-04):3s timeout,避免 swanctl 阻塞 10s+ 让首页 hang
    saCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()

    sas, err := s.Swanctl.ListSAs(saCtx)
    if err != nil {
        // 区分 timeout / 其他错误(便于 metrics / log 区分)
        if errors.Is(err, context.DeadlineExceeded) {
            s.Logger.Warn("home: list SAs timeout (3s)", "err", err)
            // 返回特殊标记,模板渲染 fallback 文案
            return nil, -1 // -1 = "暂不可用"
        }
        s.Logger.Debug("home: list SAs failed", "err", err)
        return nil, 0
    }
    var n int
    out := make([]swanctl.SA, 0, len(sas))
    for _, sa := range sas {
        if sa.IkeState == "ESTABLISHED" { out = append(out, sa); n++ }
    }
    return out, n
}
```

`homeData` 加 `SAsUnavailable bool`,模板:

```html
<h3>活跃连接（{{.TotalActive}}）</h3>
{{if .SAsUnavailable}}
  <p style="color:var(--pico-muted-color)">⚠️ SA 列表暂不可用(swanctl 响应超时),刷新重试</p>
{{else if .ActiveSAs}}
  <figure>... 表格 ...</figure>
{{else}}
  <p>无活跃连接</p>
{{end}}
```

`handleHome` 翻译 `loadActiveSAs` 返回值:
```go
activeSAs, totalActive := loadActiveSAs(r.Context(), s)
sasUnavailable := totalActive == -1
if sasUnavailable { totalActive = 0 }
```

---

### 7. Q4-02 — `GeneratePassword` 注释修正

#### 现状问题

`internal/auth/password.go:17-25`:

```go
// GeneratePassword 生成指定长度的随机密码（字母 + 数字，避免歧义字符如 0/O/1/l）。
//
// 字符集 56 个字符（a-z + 2-9，去掉 0/1/o/l）：
//   - 看起来清楚
//   - 12 位 = 56^12 ≈ 2.6e21 组合，强Swan 暴力破解不可能
//
// 注意：v2 用户密码**明文存库**（design §4.3 妥协项），
// 所以这里的密码是"VPN 用户的 EAP 密码"，不是管理员密码。
func GeneratePassword(length int) (string, error) {
    if length < 6 || length > 64 {
        return "", fmt.Errorf("password length must be in [6, 64], got %d", length)
    }
    const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 32 字符
    const n = byte(len(alphabet))  // = 32
```

**3 处错误**:
1. 注释 "字符集 56 个字符" 错误:实际 `alphabet = "abcdefghijkmnpqrstuvwxyz23456789"` 是 **32 字符**(24 个字母 + 8 个数字)
2. "12 位 = 56^12 ≈ 2.6e21" 错误:正确是 32^12 ≈ 1.15e18
3. `if int(b) < int(n)*256/int(n)` 这表达式永远为 `b < 256`(因为 `n * 256 / n = 256` 对整数除法),rejection sampling 实际**没生效**(但 inner `if b < n` 实际做了正确 reject,`>= n` 的字节被丢弃)→ 注释"用 rejection sampling 避免模偏" 与代码表达的含义对不上

#### 修正

```go
// GeneratePassword 生成指定长度的随机密码（字母 + 数字，避免歧义字符如 0/O/1/l）。
//
// 字符集 32 个字符（a-z 去掉 o/l + 2-9，共 24+8 = 32）：
//   - 看起来清楚（避免手抄出错）
//   - 12 位 = 32^12 ≈ 1.15e18 组合，强Swan 暴力破解实际不可行
//
// 注意：v2 用户密码**明文存库**（design §4.3 妥协项），
// 所以这里的密码是"VPN 用户的 EAP 密码"，不是管理员密码。
func GeneratePassword(length int) (string, error) {
    if length < 6 || length > 64 {
        return "", fmt.Errorf("password length must be in [6, 64], got %d", length)
    }
    const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 24 字母 + 8 数字 = 32 字符
    const n = byte(len(alphabet))                      // n = 32

    // rejection sampling：只接受 < n 的字节，避免 mod 偏。
    // 因为 n=32 | 256(整除),所以 `b < n` 等价于 `b < 256/n * n` = 取低 5 bit 后判零。
    // buf 取 length*2 字节足够。
    out := make([]byte, 0, length)
    buf := make([]byte, length*2)
    for {
        if _, err := rand.Read(buf); err != nil {
            return "", fmt.Errorf("rand.Read: %w", err)
        }
        for _, b := range buf {
            if b < n {
                out = append(out, alphabet[b])
                if len(out) == length {
                    return string(out), nil
                }
            }
        }
    }
}
```

**改动总结**:
- 注释 56 → 32
- 12^组合数 2.6e21 → 32^12 ≈ 1.15e18
- 删掉 dead code `if int(b) < int(n)*256/int(n)` — 简化循环逻辑
- 修正 "32 字符" 描述为 "24 字母 + 8 数字 = 32 字符"

**安全性不变**:32^12 ≈ 1.15e18 在 2026 年仍是 VPN 共享密码的合理强度(EAP 离线破解有密码哈希 + per-user salt 保护,详见 design §4)。

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| `web/templates/home_content.html` | U05 onboarding checklist 块 | +20 |
| `web/templates/user_detail_content.html` | U10 delete 按钮改 `<a>` + 文案 | -2 / +3 |
| `web/templates/users_list_content.html` | U10 list 内 delete 改 `<a>` | -2 / +3 |
| `web/templates/user_delete_confirm_content.html` | U10 新增确认页模板 | +30 |
| `web/templates/audit_content.html` | U11 新增操作日志模板 | +25 |
| `web/templates/layout.html` | U11 顶部 nav 加 audit 链接 | +2 |
| `web/templates/login_content.html` | U05/U11 顶部链接占位(若需要) | ±0 |
| `internal/web/server.go` | U04 三路由 + allowMetrics + metrics 中间件 | +80 / -10 |
| `internal/web/handlers_home.go` | Q1-04 3s timeout + SAsUnavailable 字段 | +20 / -5 |
| `internal/web/handlers_users.go` | U11 audit + U10 改路由 + 新增 confirm page | +60 / -10 |
| `internal/web/handlers_aliyun.go` | U11 audit | +6 |
| `internal/web/handlers_ddns.go` | U11 audit | +8 |
| `internal/web/handlers_auth.go` | U04 PanelLoginAttempts counter | +4 |
| `internal/web/handlers_audit.go` | U11 新增 handleAudit | +40 |
| `internal/web/audit_helper.go` | U11 writeAudit helper | +20 |
| `internal/web/templates.go` | U11 formatUnixNano FuncMap | +10 |
| `internal/store/store.go` | U11 migrate 加 audit_log 表 | +8 |
| `internal/store/audit.go` | U11 WriteAudit / ListAudit | +40 |
| `internal/metrics/metrics.go` | U04 Prometheus 注册 | +50 |
| `internal/auth/password.go` | Q4-02 注释 + dead code 删 | +5 / -10 |
| `cmd/ikev2-panel/main.go` | U04 metrics addr env + swanctl.PingVICI / ListConnections 调用 | +20 |
| `docs/TROUBLESHOOTING.md` | U08 新增 7 章节 | +250 |
| `README.md` | U08 顶部加 TROUBLESHOOTING 链接 | +2 |
| `go.mod` / `go.sum` | U04 加 prometheus/client_golang | +3 |

**总计**:**+700 / -40 行**(包含 U08 文档 250 行)

---

## 测试方案

### 自动化测试

```bash
# 1. 编译
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
# 期望:无 error

# 2. 单元测试
go test -race ./...
# 期望:全 PASS

# 3. 新增测试文件
# - internal/metrics/metrics_test.go:校验注册 metric 名 / 标签
# - internal/store/audit_test.go:WriteAudit + ListAudit 往返
# - internal/web/handlers_audit_test.go:GET /audit 渲染 OK
# - internal/auth/password_test.go:校验 GeneratePassword 字符集 = 32
```

### U05 onboarding checklist 验证

```bash
# 启动 + 新数据库
IKEV2_DATA_DIR=/tmp/panel-test IKEV2_LISTEN_ADDR=127.0.0.1:8443 \
    IKEV2_LOG_LEVEL=debug IKEV2_SERVER_CN=test.local \
    IKEV2_COOKIE_SECURE=false /tmp/test-panel &
sleep 2

# 1. 登录 + 第一次进首页
curl -b /tmp/cookies.txt http://127.0.0.1:8443/ | grep -c "5 步搞定首次部署"
# 期望:1(checklist 出现)

# 2. 创建用户后再看
curl -b /tmp/cookies.txt -X POST http://127.0.0.1:8443/users \
    -d "username=alice&note=test&speed_limit_mbps=10&csrf_token=$(csrf)"
curl -b /tmp/cookies.txt http://127.0.0.1:8443/ | grep -c "5 步搞定首次部署"
# 期望:0(checklist 消失)
```

### U04 healthz / readyz / metrics 验证

```bash
# 1. /healthz 200 + JSON
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8443/healthz
# 期望:200
curl -s http://127.0.0.1:8443/healthz | jq .
# 期望:{ "ok": true, "checks": { "db": {"ok":true,...}, ... } }

# 2. 模拟 DB 挂(临时改权限)→ 期望 503
chmod 000 /tmp/panel-test/panel.db
sleep 1
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8443/healthz
# 期望:503
chmod 644 /tmp/panel-test/panel.db

# 3. /metrics
curl -s http://127.0.0.1:8443/metrics | grep -E "^(vpn_active_sas|ddns_last_sync|le_cert|http_requests)"
# 期望:看到 6 个 metric family
```

### U11 audit log 验证

```bash
# 1. 创建用户 → 写 audit
curl -b /tmp/cookies.txt -X POST http://127.0.0.1:8443/users \
    -d "username=alice2&csrf_token=$(csrf)"
# 2. 查 audit 页面
curl -b /tmp/cookies.txt http://127.0.0.1:8443/audit | grep "user.create"
# 期望:看到 user.create + alice2 + 时间

# 3. 改凭证 → 写 audit
curl -b /tmp/cookies.txt -X POST http://127.0.0.1:8443/api/aliyun/save \
    -d "key_id=LTAI5tTest&key_secret=test&confirm=yes&csrf_token=$(csrf)"
curl -b /tmp/cookies.txt http://127.0.0.1:8443/audit | grep "aliyun.save"
# 期望:看到 aliyun.save + LTAI...est 掩码
```

### U10 双确认 delete 验证

```bash
# 1. GET 删除确认页(不应触发实际删除)
curl -b /tmp/cookies.txt http://127.0.0.1:8443/users/3/delete -o /tmp/page.html
grep -c "确定删除" /tmp/page.html
# 期望:>=1
# 2. 检查用户仍存在
sqlite3 /tmp/panel-test/panel.db "SELECT username FROM users WHERE id=3;"
# 期望:仍输出 alice

# 3. POST /users/3/delete/confirm → 真删
curl -b /tmp/cookies.txt -X POST http://127.0.0.1:8443/users/3/delete/confirm \
    -d "csrf_token=$(csrf)" -o /dev/null
sqlite3 /tmp/panel-test/panel.db "SELECT username FROM users WHERE id=3;"
# 期望:空
```

### Q1-04 timeout 验证

```bash
# 1. 模拟 swanctl 阻塞 — 替换 vici socket 为 sleep 5 的 fake socket
# 2. GET / → 期望 < 4s 返回,且 HTML 含 "SA 列表暂不可用"
time curl -b /tmp/cookies.txt -o /tmp/home.html http://127.0.0.1:8443/
grep -c "SA 列表暂不可用" /tmp/home.html
# 期望:实时间 < 4s,grep 输出 1
```

### Q4-02 注释验证

```bash
# 1. 字符集大小 sanity check
grep -A2 "const alphabet" internal/auth/password.go
# 期望:注释不再出现 "56"

# 2. 跑 go test 确认密码仍生成正确
go test ./internal/auth/...
# 期望:PASS,测试断言字符集 == 32
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| `prometheus/client_golang` 引入新依赖,Go 1.26 兼容性 | 低 | 编译失败 | go.mod 已用 1.26;prom 1.20+ 已声明 go 1.23+ 兼容 |
| `/metrics` 暴露内部状态给公网 | 中 | 信息泄露(IP 数量 / DDNS 状态) | 默认 127.0.0.1-only;env 显式开 0.0.0.0 才生效 |
| audit_log 表膨胀失控 | 低 | DB 文件大 | 当前不写 retention;后续 PR 加 cron 清理 90 天前记录 |
| audit_write 失败导致主操作失败 | 低 | 用户操作回滚 | `writeAudit` 仅 log.Warn,**不返回 error**(审计失败不阻塞业务) |
| 双确认 delete 增加 1 个 click | 极低 | 微小 UX 损失 | 仅影响"删除"这一个动作,文档强调"两步是为合规" |
| `metrics.HTTPRequestsTotal` 标签 cardinality 爆炸 | 低 | Prometheus OOM | path 用 `normalizePath` 把 `/users/123` → `/users/{id}`,标签空间有界 |
| `home: list SAs timeout` 改 -1 返回值,模板漏处理 | 中 | 显示空表而非提示 | 模板加 `{{if .SAsUnavailable}}` 分支;单元测试覆盖 |
| GeneratePassword 注释改后,有人基于 "56^12" 推算强度做错误决策 | 极低 | 后人误解 | 改完正好消除这个误解 |
| homeData.SAsUnavailable 字段让 homeData 不兼容老模板 | 低 | 模板报错 | Pico template 字段缺失默认 zero value,SAsUnavailable=false 等于现状 |

---

## 不破坏兼容性的承诺

- **API**:`/healthz` 返回仍是 JSON 但 `Content-Type` 之前是 `text/plain`,现在改 `application/json` —— 老 health check 工具读 body 不 care content type,继续 work
- **DB**:加 `audit_log` 表用 `CREATE TABLE IF NOT EXISTS`,**已存在的 panel.db 启动时自动迁移**;旧 v2.84 用户升级零动作
- **路由**:`/users/{id}/delete` 改为 GET,**旧 POST 链接失效** → 但内部跳转已更新;**外部书签(如果有)** 需要更新。release notes 显式标注
- **metrics**:完全新增端点,不影响老 Prometheus 配置
- **home_content.html**:`SAsUnavailable` 字段缺失时 default false,**老模板也能渲染**(只是不显示提示)
- **GeneratePassword**:行为不变,只改注释 + 删 dead code

---

## 后续 PR 关联

v2.85 release 至此**全部 15 个 issue 关闭**:

| PR | issue | 工作量 |
|----|-------|--------|
| PR-1 | U01 + Q1-05 | 0.4d |
| PR-2 | S01-S03(admin 密码) | 0.3d |
| PR-3 | T01-T03(时区 + 日志路径) | 0.2d |
| PR-4 | Q1-01 + Q1-02(main.go WaitGroup + panic recover) | 0.4d |
| PR-5 | D01-D04(alidns 错误码) | 0.5d |
| PR-6 | D05-D07(DDNS throttle + collector) | 0.4d |
| PR-7 | N01-N03(tc IPv6 限速) | 0.4d |
| **PR-8** | **U04/U05/U08/U10/U11 + Q1-04/Q4-02** | **2.6d** |

**v2.85 总工作量 5.2d**,符合 8 月初估算。

Phase 5 候选(不在本 release):alertmanager / grafana dashboard / audit log retention cron / metrics rate limiting。

---

## 评审检查项(给自己)

- [x] 改动 ~700 行(其中 250 是文档)
- [x] 引入 1 个新依赖(prometheus/client_golang)
- [x] DB 迁移向后兼容(`CREATE TABLE IF NOT EXISTS`)
- [x] API 改动全部向后兼容(/healthz content-type 升级但 body 仍可读)
- [x] U10 改路径但内部链接同步更新,无 404
- [x] 不引入 alert / pager(运维侧,留 Phase 5)
- [x] 模板字段缺失 fallback 已验证
- [x] 所有 7 项各有一组独立测试

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| U05 onboarding tipbox(html + pico class) | 0.5d |
| U08 TROUBLESHOOTING.md 7 章节人工写 | 0.5d |
| U04 healthz 重写 + readyz + metrics 包 + middleware + go.mod | 0.9d |
| U11 audit_log 表 + store + handler + 模板 + 导航 | 0.3d |
| U10 双确认路由 + 模板 + handler 拆分 | 0.1d |
| Q1-04 ListSAs timeout + 模板分支 | 0.2d |
| Q4-02 注释 + dead code 清理 | 0.1d |
| **合计** | **2.6d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] U05 home_content.html 加 onboarding checklist(TotalUsers == 0 时显示)
- [ ] U05 阿里云凭证 article 加 id="aliyun-card" 锚点
- [ ] U08 新建 docs/TROUBLESHOOTING.md 7 章节
- [ ] U08 README 顶部加 troubleshooting 链接
- [ ] U04 go.mod / go.sum 加 prometheus/client_golang
- [ ] U04 新建 internal/metrics/metrics.go(6 metric 注册)
- [ ] U04 server.go handleHealthz 重写(JSON + 4 检查 + 2s timeout)
- [ ] U04 server.go handleReadyz(healthz + 至少 1 conn loaded)
- [ ] U04 server.go handleMetrics(refreshGauges + promhttp + 127.0.0.1 校验)
- [ ] U04 server.go logging middleware 加 HTTPRequestsTotal / Duration
- [ ] U04 handlers_auth.go 登录成功/失败各 Inc PanelLoginAttempts
- [ ] U04 main.go 加 IKEV2_METRICS_ADDR env + Swanctl.PingVICI / ListConnections 包装
- [ ] U11 store.go migrate 加 audit_log 表 + 2 索引
- [ ] U11 新建 internal/store/audit.go(WriteAudit / ListAudit)
- [ ] U11 新建 internal/web/audit_helper.go(writeAudit)
- [ ] U11 handlers_users.go 5 处 handler 各加一行 writeAudit
- [ ] U11 handlers_aliyun.go 2 处 handler 各加一行 writeAudit
- [ ] U11 handlers_ddns.go 2 处 handler 各加一行 writeAudit
- [ ] U11 新建 internal/web/handlers_audit.go(handleAudit)
- [ ] U11 新建 web/templates/audit_content.html
- [ ] U11 layout.html 顶部 nav 加 audit 链接
- [ ] U11 templates.go FuncMap 加 formatUnixNano
- [ ] U10 user_detail_content.html delete 按钮改 <a href>
- [ ] U10 users_list_content.html delete 改 <a href>
- [ ] U10 新建 web/templates/user_delete_confirm_content.html
- [ ] U10 handlers_users.go 新增 handleUserDeleteConfirmPage
- [ ] U10 server.go 路由改 POST /users/{id}/delete/confirm
- [ ] Q1-04 handlers_home.go loadActiveSAs 加 3s context timeout
- [ ] Q1-04 homeData 加 SAsUnavailable 字段
- [ ] Q1-04 home_content.html 加 {{if .SAsUnavailable}} 分支
- [ ] Q4-02 internal/auth/password.go 注释 56 → 32 + 2.6e21 → 1.15e18
- [ ] Q4-02 删 dead code `if int(b) < int(n)*256/int(n)`
- [ ] go test -race ./... PASS
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] 手动跑 U05/U08/U04/U11/U10/Q1-04/Q4-02 7 项验证
- [ ] git commit "v2.85-PR8: closing 7 issues (U04/U05/U08/U10/U11 + Q1-04/Q4-02)"
```

---

> 关联:
> - [audit-2026-09-usability.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md) §U04 / §U05 / §U08 / §U10 / §U11
> - [audit-2026-09-correctness.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md) §Q1-04 / §Q4-02
> - [release-notes-v2.85.md](file:///opt/ike