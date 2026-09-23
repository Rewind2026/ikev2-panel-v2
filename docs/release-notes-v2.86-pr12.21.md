# v2.86-PR12.21 Release Notes — Visual-First UI 重构 + Dev 工作流

**日期:** 2026-09-21
**Tag:** `v2.86-pr12.21`
**Commit:** `ecdae45`(包含 PR12.21 main/fix/cont + PR18/19/20 共 6 个 commit)
**镜像:** `ikev2-panel:v2.86-pr12.21`
**类型:** 大版本 — UI/UX 整站重做 + 开发者体验

---

## 背景

v2.86-PR12.20(commit 5da83c0)上线后,真实用户反馈 UI 布局"完全不合理",触发**第二轮 UI 重构**。
PR12.21 → PR18 → PR19 → PR20 连续 4 个迭代,把"VPN = 网络连接产品"作为视觉锚点重做整站,
并顺手补齐本地开发体验。

**设计哲学(贯穿 PR12.21 ~ PR20):**
> VPN 是一个连接产品,不是一个内容产品。
> 视觉锚点 = 服务器↔用户实时拓扑(Ubiquiti UniFi / Cisco vManage 风)。

| Commit | 标题 | 核心交付 |
|---|---|---|
| `1c4c183` | **PR12.21** UI Visual-First 重构 | 拓扑画布 + SideStats + TimelineRow + TerminalCard |
| `84b1c73` | **PR12.21 fix** trailing-slash 301 | Go 1.22 mux 严格区分 `/users` vs `/users/` 的 404 修复 |
| `55363c1` | **PR12.21 cont.** mobileconfig 可配置化 | 三层 overlay + admin 全局默认 + audit 修正 |
| `4325397` | **PR18** 登录页整页重设计 | 品牌区/表单卡双列布局 + PageKey bug 修复 |
| `edb4af0` | **PR19** 整站 UI 重做 | 5 个 content template + style.css 全部重写 |
| `ecdae45` | **PR20** Dev 模式工作流 + cache-bust | `make dev` + asset version URL 参数 |

---

## 改动清单

### 1. PR12.21 (main):UI Visual-First 重构 — 服务器↔用户网络拓扑仪表盘

**新增 5 个组件:**

| 组件 | 用途 | 替代了什么 |
|---|---|---|
| **TopBar** | 56px sticky + brand-mark + breadcrumb + active nav (brand-soft 高亮) | 旧的顶部 nav 链接 |
| **Topology** | SVG 拓扑画布(浅网格 + server/user node + animated data flow) | 0(全新) |
| **SideStats** | 垂直 stat 列表(大数字 + 小标签) | 8 行 kv-list 详情卡 |
| **TimelineRow** | 单列时间线 56px 行高(60% 紧凑 + severity dot 颜色) | audit table 200px 行 |
| **TerminalCard** | 黑底 + mono 字体 + 1px indigo 左 border | 旧的 code block |

**3 个页面迁移:**
- **用户详情页**:8 行 key-value 表 → 拓扑(data-focus-user) + SideStats
- **首页**:status-row 4 列紧凑 → 拓扑(服务器↔所有用户) + 侧栏 SideStats
- **审计页**:table 4 列 → TimelineRow 单列
- **用户删除确认**:4 个副作用 → TerminalCard(代码风)
- **Android 原生配置**:步骤列表 + TerminalCard(可复制)
- **登录页**:dark gradient bg + 居中 card
- **OS Tabs hint**:二行(iOS/macOS 最简 / Android 11+ 无需 app)

**通用:**
- copy-btn 通用 handler(`navigator.clipboard` + `execCommand` fallback)

**品牌色**:`indigo #4f46e5`(网络感,不是 blue-purple)

**后端支撑:**
- `homeData` 加 `TopologyClients` / `RecentEvents` / `NowUnix`
- `userDetailData` 加 `ServerAddr` / `NowUnix` / `Breadcrumb`
- `PageMeta` 加 `Breadcrumb`
- `FuncMap` 加 `jsonTopologyClients` / `jsonOneClient` / `auditSeverity`
- `topology.js` 注入 `data-server-addr` / `data-clients` JSON / `data-focus-user` 渲染

---

### 2. PR12.21 (fix):trailing-slash 301 redirect middleware

**Bug 背景:**
Go 1.22 `net/http.ServeMux` 严格区分 `/users` 与 `/users/`。
iOS mobileconfig 安装后的内部跳转 / 用户书签 / 客户端 VPN 起来后浏览器自动补
trailing slash → 触发 404 "page not found"。

**修复:** 加 `trailingSlashRedirect` 中间件到中间件链(logging 之内,mux 之外):

| 路径 | 处理 |
|---|---|
| `/path/` | → 301 → `/path` |
| `/`(根路径) | 不动 |
| 静态资源(`/static/*`) | 不动 |
| 文件路径(`.pem` `.css` `.png`) | 不动 |
| POST 请求 | 不动(query string 保留) |

**测试:** `TestTrailingSlashRedirect` 7 个 case 全过

---

### 3. PR12.21 (cont.):mobileconfig 可配置化 + 管理员全局默认

**三层优先级:**
> 用户 overlay > 管理员 defaults > builtin 出厂值

`/data/panel-state/mobileconfig.defaults.json` 运行时热改无需重启。

**新文件:**
- `internal/cert/mobileconfig_opts_test.go` — 字段 + 渲染测试
- `internal/panelstate/mobileconfig_default.go` — admin defaults store
- `internal/panelstate/mobileconfig_default_test.go`
- `internal/store/mobileconfig_opts_test.go` — roundtrip + migration
- `internal/web/handlers_mobileconfig_admin.go` — `/admin/mobileconfig-defaults`
- `internal/web/handlers_mobileconfig_options.go` — `/users/{id}/mobileconfig-options`
- `web/templates/admin_mobileconfig_defaults_content.html`

**修改:**
- `internal/cert/mobileconfig.go`:抽出 `MobileConfigOpts` + 5 个 OnDemand preset + `effectiveOpts` 三层合并
- `internal/store/{types,store,users}.go`:`users.mobileconfig_opts` TEXT 列 + `UpdateUserMobileConfigOpts` + 幂等 migration
- `internal/web/{handlers_config,server,templates}.go`:渲染路径接 overlay + admin defaults,新增路由,`boolPtrVal` / `intPtrVal` FuncMap

**audit 修正(对照 Apple developer docs):**
- 删除 `AuthPasswordRetries` 字段(Apple 不承认)

---

### 4. PR18:登录页整页重新设计 + 修一个真 bug

**Bug 修复:**
`handleLoginPage` / `renderLoginError` 没设 `PageKey="login"`,导致 `body.login-page` class
永远加不上,浅色主题下卡片贴左上角(实测 `x=38.5`,深色下视觉看着居中是浏览器
UA body padding 撞出来的假象)。

**设计改动(跟整站 Indigo/Inter 设计语言对齐):**
- 双列布局:左 55% indigo 渐变品牌区(盾形 SVG logo + 一句话定位 + 3 项卖点 + host/version meta)+ 右 460px 登录卡
- 品牌区右下叠网络节点 SVG(1 中心 + 4 外围 + 连线)作为视觉镇纸
- input 加 leading SVG icon + autocomplete + 圆角 12px + focus ring
- 按钮:indigo 渐变 + 阴影 + hover `translate(-1px)` + 箭头右移
- 移除冗余副标题 "管理员控制台"
- 响应式 `<960px` 隐藏品牌区退化为单列

---

### 5. PR19:整站 UI 重新设计(full reset + cache control)

**用户原话:**
> "现在这个布局排版完全不合理啊,里面的功能页面也需要你重新设计"

**背景:**
- PR12.21 visual-first 重构后用户验收不通过
- PR18 只重做了登录页,内部页面未触
- PR19 整站重做:**5 个 content templates + 1 个 style.css 完全重写**

**评审发现的 6 个真实问题**(通过 `browser_evaluate` 取 CSSOM 实地诊断):
1. 登录页 brand 区 / 表单卡贴左上角(flex 没生效)
2. 表单输入框 SVG icon 巨大化(h1 大小)
3. pico button/input 默认样式残留(蓝按钮 + 黄 focus outline)
4. 只 1 个断点,中等屏布局挤
5. 部署后浏览器一直命中旧 CSS(磁盘 cache + view 进程级 cache)
6. audit TimelineRow 在窄屏溢出

---

### 6. PR20:Dev 模式工作流 + 静态资源 cache-bust

**用户两个问题:**
1. IDE 内置 browser view 一直命中旧 CSS(`Cache-Control: no-cache` 都不生效)
2. webui 改动没必要打包 docker,本机启服务就能测

**两件事一起做:**

#### 6.1 `assetVersion` cache-bust(根治 IDE view 进程级 cache)
- `internal/web/templates.go`:`LoadTemplates` 加 `staticDir` 参数
- 新增 `newAssetVersion` 闭包,返回 `?v=<file-mtime-unix-nano>`
- `web/templates/layout.html`:3 处 `assetVersion` 注入
- 文件改 mtime → URL 自动变 → 任何 cache 层 100% 失效
- dev 模式不用重启服务就能看到新 CSS

#### 6.2 `Makefile` dev 工作流

| 命令 | 作用 |
|---|---|
| `make build` | 编译 Go 程序 |
| `make dev` | 编译 + 后台启动 dev 模式(pid 写入 `/tmp/ikev2-panel-dev.pid`) |
| `make dev-stop` | 停 dev 模式 |
| `make dev-logs` | tail 日志(`/tmp/ikev2-panel-dev.log`) |
| `make test` / `make vet` | 测试 / vet |
| `make docker-build` / `docker-run` | docker 工作流 |

**dev 模式自动:**
- **SkipVici**(`/etc/swanctl` 不存在 → 不调 charon,本地无 strongSwan 也能跑)
- 本地 `web/` 模板路径
- 数据写到 `./dev-data/panel.db`(SQLite)
- self-signed cert(自动签 CA + server cert + panel cert)
- 端口 `127.0.0.1:19090` (HTTP) + `127.0.0.1:18443` (HTTPS)
  - 故意避开 `9090` / `8443` 防止与 `192.168.50.63` 生产部署冲突
- 首次启动自动创建 admin 账号,密码写 `panel-state/INITIAL_ADMIN_PASSWORD.txt`

**测试调用点同步更新(9 处 `LoadTemplates` 加 `staticDir` 参数)。**

---

## Dev 模式验证

```bash
$ touch web/static/style.css   # 改样式文件
# 不需要重启 dev 服务,浏览器 Ctrl+Shift+R 即可看到新 CSS
# (assetVersion 自动 + mtime 变化 → URL 变了)
```

```bash
$ make dev
... admin 账号:admin  密码:2hjyeqmd8xx9 ...
HTTP(明文,推荐本地调试) : http://127.0.0.1:19090/login
HTTPS(self-signed)        : https://127.0.0.1:18443/login
```

---

## 关键设计决策

| 决策点 | 选择 | 原因 |
|---|---|---|
| 视觉锚点 | 服务器↔用户实时拓扑 | VPN 是连接产品,不是内容产品 |
| 主色 | indigo `#4f46e5` | network feel(避开 blue-purple) |
| Trailing slash | 301 redirect(去尾) | Go 1.22 mux 严格,客户端 / 书签会自动补 `/` |
| mobileconfig 优先级 | 用户 overlay > 管理员 defaults > builtin | 个性化覆盖 + 全局策略分层,运维可热改 |
| Dev 端口 | 19090 / 18443(避开 9090/8443) | 跟 192.168.50.63 生产 0 冲突 |
| asset version | `?v=<file-mtime-unix-nano>` | IDE 内置 view 进程级 cache 都打穿 |
| Dev 数据隔离 | `./dev-data/` + 启动清空 | 避免脏数据污染,每次干净起 |

---

## 测试结果

- ✅ `web` 测试全过:`LoadTemplates_Layout` / `RenderPage_FullPipeline` / `LoginHasNoNav` / `TestTrailingSlashRedirect` (7 cases)
- ✅ `entrypoint_static_test.sh` 13/13 通过
- ✅ `go build ./...` 全包编译通过
- ✅ `go test ./internal/auth/...` / `./internal/cert/...` / `./internal/store/...` 全部通过
- ⚠️ `swanctl` 3 个测试失败(`TestReloadAllViciNoSocket` / `TestReloadAll_BinaryNotExists` / `TestLoadCreds_BinaryNotExists`)是开发机环境问题(`/usr/sbin/swanctl` 存在,测试假设"PATH 空就找不到二进制"不成立),**与本 PR 无关**
- ⚠️ `vet` 警告(`pr8_smoke_test`, `swanctl writer_test`)pre-existing

---

## 用户升级指南

### 标准升级路径

```bash
cd /opt/ikev2-panel   # 或你部署的目录
git fetch origin
git checkout v2.86-pr12.21
docker compose pull
docker compose up -d
```

### 行为变化
- **登录页 UI 完全变了**(indigo 双列布局)
- **首页** → 拓扑图 + SideStats(替代 status-row)
- **用户详情** → 拓扑(focus 单用户) + SideStats
- **审计页** → TimelineRow(替代 table)
- 浏览器**强刷一次**(`Ctrl+Shift+R`)才能看到新 CSS(部分浏览器命中旧 cache)

### 配置/数据
- **无需任何 .env / docker-compose 修改**
- mobileconfig 三层 overlay 自动启用,**现有用户的 mobileconfig 行为不变**(overlay 为空 = 走 admin defaults = 走 builtin)
- SQLite 自动 migration 新增 `mobileconfig_opts` 列(幂等)

### 想用 Dev 模式(开发者)

```bash
make dev    # 启 web/http/xx/login   admin / 2hjyeqmd8xx9
```

---

## 没改的东西(强调)

- ❌ strongSwan / charon 任何配置
- ❌ IKEv2 隧道证书(LE / 自签 / mobileconfig 已签发的)
- ❌ 8443 HTTPS 入口(默认行为)
- ❌ IPv6 / NFT / 重载逻辑(拓扑数据流沿用 PR12.21 之前)
- ❌ 后端 API 路径(URL 兼容性保持)

---

## 已知小坑

1. **IDE 内置 browser view 命中旧 CSS** — 已通过 `assetVersion` 根治,改文件后**不需要重启 dev 服务**
2. **第三方 cache(CDN / 反代)** — 如果你前面挂了 nginx / CDN,需要 purge 或加 `Cache-Control: no-cache`(PR20 已用)
4. **dev 模式 HTTP 明文 cookie** — 跟 v2.86-PR17 一样,`Secure` 自动 disable,只绑 `127.0.0.1` 安全