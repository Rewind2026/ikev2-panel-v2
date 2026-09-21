# v2.86-PR18:登录页整页重新设计(visual-first brand split)

> 评审驱动 + 真 bug 修复 + 整页重做,跟整站 Indigo/Inter 设计语言对齐。

## 背景

PR17 加了明文 HTTP 监听端口后,日常会在 `http://192.168.50.63:9090/login` 上做调试登录。
登录页本身已经 4 个 PR 没动过(`login_content.html` 注释停留在 PR12.21),实际渲染有以下问题:

### 评审发现

**1. 真 bug — `body.login-page` class 从来没生效**

- `templates/layout.html` 里 `<body {{if eq .PageKey "login"}}class="login-page"{{end}}>`
- 但 `handleLoginPage` 和 `renderLoginError` 都没设 `PageKey: "login"`,只在 PageMeta 里塞了 `Page: "login", Title: "登录"`
- 后果:浅色主题下 body 没有 `flex` 居中和深色渐变背景,卡片死死贴左上角 (实测 `cardRect.x=38.5`)
- 深色主题下之所以"看起来"正常,是因为深色时浏览器默认 UA body 也有 padding-bottom,意外地把卡片视觉下移了一点,**但 flex 居中实际从未生效**。

**2. 设计语言断裂**

| 现状 | 整站设计系统 |
|---|---|
| 380px 卡片光秃秃,2 个 label + 1 个按钮 | TopBar 56px + Topology + SideStats,品牌色 indigo `#4f46e5` |
| 浏览器默认衬线字 | Inter + JetBrains Mono |
| 圆角 3.75px | `--ikev2-radius-xl` token |
| 按钮纯色实心矩形 | 圆角按钮 + focus ring |
| 无品牌识别 | 整站 TopBar 有 brand-mark |
| 无版本/域名 hint | home 页有"面板版本"meta |

**3. 可访问性 / UX**

- input 缺 `autocomplete`(浏览器无法 autofill)
- 按钮在 hover/active 没视觉反馈
- 没有"忘记密码 / 联系管理员"等辅助信息
- favicon 缺失 → 浏览器标签页空白图标

## 改动清单

### 1. 修 bug:`loginPageData.PageKey`

`internal/web/handlers_auth.go`
- `handleLoginPage` 和 `renderLoginError` 的 `PageMeta` 都补上 `PageKey: "login"`
- `renderLoginError` 签名加 `r *http.Request` 参数(为了 brand 区显示真实 host)
- `loginPageData` 加 3 个字段:`Host` / `Version` / `SessionTTL`

`internal/web/server.go`
- `Server` 加 `Version string` 字段(main.go 注入)

`cmd/ikev2-panel/main.go`
- `srv.Version = panelVersion`

### 2. 重做模板

`web/templates/login_content.html` (整页重写,~140 行)
- 双列 `.login-split`:左品牌区(55%)+右登录卡(460px)
- 品牌区内容:盾形 SVG logo + 产品名 + 一句话定位 + 3 项卖点(绿点/黄点 dot 列表)+ 服务地址/版本号 meta
- 右下角叠 SVG"网络节点"装饰(1 个中心节点 + 4 个外围节点 + 连线),作为视觉镇纸不抢标题
- 登录卡:hgroup + 错误条 + 2 个字段(用户名/密码)+ 主按钮(带 → 箭头)+ 底部 hint
- input 加 leading SVG icon、autocomplete 属性、placeholder
- 移除冗余副标题("管理员控制台" → 删除

### 3. 重做 CSS

`web/static/style.css` 把 `body.login-page` / `.login-card` 整段重写 (+265 行)
- 全屏背景 + flex 容器 + grid `1fr 460px`
- 品牌区:indigo 三色渐变 (#4f46e5 → #6366f1 → #818cf8) + 白色 dot grid 底纹 + 网络节点 SVG 装饰
- 登录卡:圆角 12px、56px 边距、深色下用 `#0f172a` token
- input:leading icon + focus ring (4px `--ikev2-brand-ring`)
- 按钮:indigo 渐变 + 阴影 + hover translate(-1px) + 箭头右移
- 响应式:`@media (max-width: 960px)` 隐藏品牌区退化为单列
- 深色主题微调 (`[data-theme="dark"] .login-card { background: #0f172a }` 等)

## 关键设计决策

| 决策 | 替代 | 理由 |
|---|---|---|
| 品牌区一直 indigo 渐变(浅/深主题都同一色) | 浅色下用极淡灰底 | 内网调试时 admin 一眼看出"这是面板不是别的产品";调试时主题可能临时切 |
| `LoginFieldIcon` 直接 inline SVG | icon font / 第三方图标库 | 零依赖、零额外请求、CSP 友好 |
| 品牌区底部显示 host + version | 不显示 | 帮 admin 在多环境(测试机/生产)区分当前面板实例 |
| 按钮带 → 箭头 | 不带 | 主操作给强 CTA 暗示,跟 SaaS 表单 pattern 一致 |
| input 圆角 12px(跟 token) | 16px / 8px | 跟整站 `--ikev2-radius-xl` 一致 |
| 响应式断点 960px | 768px | 登录页面向 admin(桌面为主),960 隐藏品牌区保留登录卡 460px 仍可用 |

## 测试结果

```bash
$ go build ./...                  # ✓ 无编译错误
$ go test ./internal/web/...       # ✓ PASS (10.6s,30+ 子测试)
$ go test ./internal/config/...    # ✓ PASS (cached)
```

部署到 192.168.50.63:
- 镜像 `ikev2-panel:v2.86-pr18` sha `652284abfd28`,218MB
- `docker compose up -d` 成功,容器状态 `Up 3 seconds`
- `curl http://192.168.50.63:9090/healthz` → 200
- 浏览器实测:`splitCols: 775px 460px`,`brandBg: linear-gradient(135deg, #4f46e5 → #6366f1 → #818cf8)`,`btnH: 50px`,`svgW: 18px`,全部指标符合预期

## 用户升级指南

零迁移。镜像升级后立即生效,无需清浏览器缓存(虽然浏览器可能缓存旧 CSS,强刷一次或重启浏览器即可)。

## 安全警告

无变化。HTTP 明文 cookie 行为跟 PR17 完全一致(`IKEV2_HTTP_LISTEN_ADDR` 不为空时自动 `cfg.CookieSecure=false`)。设计重做**完全没碰 IKEv2 / strongSwan / 隧道证书链路**。

## 没改的东西

- 没改 TopBar / nav(其他页面不变)
- 没改 auth 流程(密码 hash / CSRF / rate limit 全部不变)
- 没改 HTTPS 监听 / strongSwan 配置 / DDNS / mobileconfig 任何代码
- 没改 favicon(下一 PR 顺手做)

## 已知小坑

1. **浏览器 HTTP 缓存**:容器重启后浏览器可能仍渲染旧 CSS。强刷 (Ctrl+Shift+R) 或加 `?v=xxx` query 即可。
2. **favicon**:浏览器标签页仍空白。下次 PR 加。