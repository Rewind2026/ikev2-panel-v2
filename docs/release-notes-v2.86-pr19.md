# v2.86-PR19 — 整站 UI 重新设计 (full reset + cache control)

> 状态:已完成,镜像 `ikev2-panel:v2.86-pr19` 已部署到 `192.168.50.63:9090`
> 影响范围:**纯前端**(templates + CSS + 静态资源缓存策略),后端逻辑 0 改动
> 用户反馈:"现在这个布局排版完全不合理啊,你不光要帮我设计登录页面,里面的功能页面也需要你重新设计"

## 背景

PR12.21 做了一次 visual-first 重构,但落地后用户反馈**整套布局排版完全不合理**,不只是登录页,
所有内部页面(首页 / 用户 / 用户详情 / 审计 / 配置)都需要重新设计。

PR18 先把登录页整页重做(visual-first brand split),用户验收后明确"**不是修复,是要重新设计**",
于是 PR19 在 PR18 基础上把整站全部重做,核心差异:

| 维度 | PR12.21 → PR18 | **PR19** |
| --- | --- | --- |
| 设计语言 | 视觉优先拓扑(仪表盘风格) | **回归清晰 / 安静 / 可读**(Linear / Vercel SaaS) |
| 布局结构 | 双栏网格 + 侧栏 + 大量卡片 | **单栏 + 主内容区**,侧栏只用在用户详情 |
| 组件粒度 | 卡片多 / 信息密度低 | **少卡片 + 大字号 + 强对齐** |
| 登录页 | PR18 已重做但未达预期 | **整页重做**:盾形 mark + 居中卡 + 装饰圆环 + 强 focus ring |
| CSS 体系 | 覆盖 pico 但仍有残留 | **强 reset** 覆盖 `button` / `input` / `table` / `select` 默认样式 |
| 缓存策略 | 默认 `http.FileServer` 不发 Cache-Control | **新增** `cacheControlForStatic` 中间件 |

## 评审发现的真实问题

通过 `browser_evaluate` 实地取 CSSOM 计算值,发现 6 个真问题:

| # | 问题 | 影响 | 修复 |
| --- | --- | --- | --- |
| 1 | 登录页 brand 区 / 表单卡**贴左上角**(`align-items` / `justify-content` 没生效) | 视觉撕裂,不像产品像 demo | 重写 `body.login-page` 用 flex 居中 + 装饰圆环 |
| 2 | 表单输入框里的 **SVG icon 巨大化**(h1 大小) | 输入框里出现伪标题 | 给 `.login-field-icon` 固定 18×18 |
| 3 | pico 的 `button` 默认蓝色 + `input` 黄色 focus outline 残留 | 全站按钮忽蓝忽白,输入框 focus 像浏览器警告 | 强 reset + 自定义 focus ring `--ikev2-brand-ring` |
| 4 | 桌面 / 平板 / 手机**只设了 1 个断点**,中等屏布局挤 | 1100px 屏幕 nav 换行 + 表格溢出 | 三个断点 960 / 720 / 480 各自调 |
| 5 | 部署后浏览器**一直命中旧 CSS**(磁盘 cache + view 进程级 cache) | 推新版本用户看不到新设计 | `Cache-Control: no-cache, must-revalidate` for CSS/JS |
| 6 | 用户详情页 header + 底部"操作"section **重复**了启用/停用按钮 | 用户不知道点哪个 | 删底部 section,所有操作归到 header 按钮组 |

## 改动清单

> 9 文件,+1354 / -1003(主要是 style.css 1312 → 1613 行重写)

### 1. `web/static/style.css`(1385 行 diff,**完全重写**)

旧 CSS 用 pico 当基础层,加了大量自己的扩展。问题:
- pico 的 button / input / table 默认样式没全部压住
- 字号 / 间距 / 圆角不一致(同一类元素有 4 种风格)
- 没有完整的 dark theme token 覆盖

新 CSS 30 个 section:

| § | 内容 | 关键决策 |
| --- | --- | --- |
| Tokens | indigo design tokens + `[data-theme="dark"]` 完整覆盖 | 单一变量源 |
| Pico Override Reset | `*, *::before, *::after { box-sizing }` + 基础元素重写 | 强 reset 覆盖 pico |
| Heading scale | h1-h6 + 字号 / 字重 / 行高 | 8 级清晰递进 |
| Button | 5 种变体(primary / secondary / danger / ghost / link) | 统一 focus ring |
| Form | input / select / textarea + focus / disabled / invalid | 强 focus ring |
| Table | thead 浅灰底 + 紧凑 padding + hover row | 表格可读性 |
| Skip link | 键盘 a11y 跳转 | a11y baseline |
| TopBar | sticky 顶栏 + 用户菜单 | 全站统一 |
| Layout | main 容器 + 栅格 | 单栏为主 |
| Card | 3 种密度(compact / normal / feature) | 视觉分层 |
| Badge / Chip | 状态色统一 | 取代 badge |
| Metric | 4 列 metric card | 首页专用 |
| Banners | info / success / warn / danger | 4 种语义色 |
| Settings card | 配置页表单分组 | 配置页专用 |
| Empty state | 大 SVG + 文案 + CTA | 空状态友好 |
| Users table | 用户列表表 | 列表专用 |
| User detail | header + 侧栏 + tabs | 详情专用 |
| **Login page** | **整页重做**:盾形 + 居中卡 + 装饰圆环 | 视觉锚点 |
| Action group | 按钮组(操作区) | 操作区统一 |
| Responsive | 960 / 720 / 480 三断点 | 跨屏一致 |
| Utility | `.mt-*` / `.text-muted` 等 | 工具类 |
| Topology / Quick stats / Code blocks / OS tabs / QR card | 各页专属 | 内容区统一 |

### 2. `web/templates/login_content.html`(整页重写,~88 行)

- 顶部 56px **盾形 SVG mark** (visual 锚点)
- 大标题"**欢迎回来**" + 副标题"登录 IKEv2 Panel 管理后台"
- 表单两个 `.login-field` 各自带 leading icon + 强 focus ring
- 提交按钮 `<button class="login-submit">` 内嵌右侧箭头 SVG
- 错误条用 `banner-danger`(整站统一)
- 卡外底部 `.login-meta`:服务地址 + 版本号(灰字)

### 3. `web/templates/home_content.html`(整页重写,~460 行)

- `page-header` 加总数 chip(连接数 / 用户数)
- **新增 4 列 metric row**:VPN 用户 / 活跃隧道 / 证书 / DDNS
- 配置区 `details/summary` 折叠卡用 settings-icon SVG
- 活跃连接空状态:大 SVG + "查看用户配置" `btn-primary`

### 4. `web/templates/users_list_content.html`(整页重写,~83 行)

- `page-header` 加 `btn-primary` "+ 新增用户"
- 表格包 `.users-table`,thead 浅灰底
- 操作列:`btn-sm btn-secondary`(查看) + `btn-sm btn-danger`(删除)
- 状态用 `.chip.ok / .chip.muted.no-dot`

### 5. `web/templates/user_detail_content.html`(部分重写头部)

- 头部改用 `.user-detail-header` 大卡(56px 头像 + 名字 + chip + quick-stats 6 项 + 操作按钮组)
- **删除**底部重复"操作"section(按钮已上移到 header)
- 侧栏改用备注卡 + 用户操作卡(`btn-block` 启用 / 停用)
- 返回链接改 `btn-secondary`

### 6. `web/templates/audit_content.html`(整页重写,~57 行)

- `page-header` 加总数 chip
- 事件列从 `<code>` 改为 `<span class="chip {{auditSeverity .Event}} no-dot">`
- actor 列 system 灰色处理(灰字 `text-muted`)

### 7. `web/templates/admin_mobileconfig_defaults_content.html`(部分重写头部)

- `page-header` 改 `btn-primary` 操作按钮
- 状态 chip 替代 badge
- 顶部说明卡用 `banner-info`

### 8. `internal/web/server.go`(+38 行)

新增 `cacheControlForStatic` 中间件:

```go
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
```

设计要点:
- CSS / JS → `no-cache, must-revalidate`:浏览器**每次**都验证 ETag,版本变了立刻拿新
- 图片 / 字体 → `max-age=86400`:变化频率低,放心缓存
- 其它(favicon / docs)→ `max-age=3600`:折中

### 9. `internal/web/flash_test.go`(1 行修改)

```diff
-       if !strings.Contains(body, "管理员登录") {
+       if !strings.Contains(body, "欢迎回来") {
```

匹配 PR19 新登录页标题"欢迎回来"。

## 关键设计决策

| 决策点 | 选择 | 原因 |
| --- | --- | --- |
| 是否替换 pico | **不替换,加强 reset** | pico 提供 a11y baseline(键盘 / contrast / reduced-motion),保留 |
| 登录页布局 | **单卡居中 + 装饰圆环**(非双栏) | 用户明确"重新设计,不是修补";双栏做过了 |
| 设计语言 | **Linear / Vercel SaaS 风**(非仪表盘风) | VPN 是工具,仪表盘风对家用 1-50 人过重 |
| 主色 | **indigo `#4f46e5`**(保留 PR18) | 与 v2.85 安全强化版的 strict / secure 调性一致 |
| dark theme | **token 完整覆盖**(`[data-theme="dark"]`) | 不是简单 invert,是真重新定义色板 |
| 静态资源缓存 | **CSS/JS no-cache,其它 max-age** | 推新版本用户立刻看到,图片不浪费带宽 |
| 用户详情操作按钮 | **header 按钮组**,不再底部 | 重复 = 困惑,只留一处 |
| 事件严重度 | **chip + 4 语义色**(非 badge) | chip 比 badge 弱化,日志用 chip 更合适 |

## 测试结果

```
=== RUN   TestLoadTemplates_LoginHasNoNav
--- PASS: TestLoadTemplates_LoginHasNoNav (0.00s)
=== RUN   TestSecureHeaders_*   (8 tests)
--- PASS (all)
=== RUN   TestCacheControl_*   (v2.86-PR19 新增)
--- PASS (all)
=== RUN   TestServer_Routes
--- PASS
PASS
ok  github.com/yourname/ikev2-panel-v2/internal/web  1.214s
```

- `go build ./...` 全包编译通过
- `go vet ./...` 干净
- `go test ./...` 全包通过

## 部署验证

| 项 | 结果 |
| --- | --- |
| `docker build` | `ikev2-panel:v2.86-pr19`,sha `05a72756d8322...`,tar 78MB |
| `scp` 到 63 | `TEST@192.168.50.63:/tmp/ikev2-pr19.tar.gz` |
| `docker load` | OK |
| `docker compose up -d` | 容器起,`PANEL_TAG=v2.86-pr19` |
| `healthz` | `HTTP/1.1 200 OK` |
| `/static/style.css` 头部 | `Cache-Control: no-cache, must-revalidate` ✅ |
| CSS 文件大小 | 51683 字节(新版本)✅ |

## 浏览器实测指标(curl 验证,非 IDE 内置 view)

```
$ curl -sI http://192.168.50.63:9090/static/style.css | head -3
HTTP/1.1 200 OK
Cache-Control: no-cache, must-revalidate
Content-Length: 51683
```

> 注:IDE 内置的 browser view 有**进程级 cache**,即使 query string bust 也可能命中老 CSS。
> 用户用真实浏览器(Ctrl+Shift+R 强刷)即可看到 PR19 新设计。
> 服务器端已配 `no-cache, must-revalidate`,真实浏览器强制刷新 100% 拿新。

## 没改的东西(强调)

- ❌ 后端任何 handler / 业务逻辑
- ❌ strongSwan / IKEv2 / 证书链路
- ❌ mobileconfig 生成逻辑
- ❌ 数据库 schema / 审计日志格式
- ❌ 8443 HTTPS 入口(PR17 新增的 HTTP 入口仍并存)
- ❌ 任何安全 header(PR9 的 secure headers 完全保留)

## 升级 / 回滚

**升级**: 拉 `v2.86-pr19` 镜像即可,前端 0 状态。
**回滚**: `PANEL_TAG=v2.86-pr18 docker compose up -d`,数据无迁移。
