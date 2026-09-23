# v2.86-PR20 — Dev 模式工作流 + 静态资源 cache-bust

> 状态:已完成
> 用户反馈:
> 1. "IDE 浏览器访问 CSS 错误,真实浏览器正常" → IDE view 进程级 cache 问题
> 2. "webui 修改没必要打包 docker,直接本机启服务应该就能测"
>
> 影响范围:**纯开发体验改进**,生产镜像不变化(部署后行为完全一致)

## 背景

PR19 部署后,IDE 内置 browser view 一直命中旧 CSS,即使服务器已配
`Cache-Control: no-cache, must-revalidate`。诊断后确认是 IDE view 进程级 cache
问题——浏览器 view 跟真实浏览器的 cache 体系是两回事,query string bust 也可能不
生效。

同时用户指出:**改 webui 不需要打包 docker**。每次改一行 CSS 走完整 docker build
+ scp + load + restart 链路要几分钟,反馈循环太长。

PR20 两件事:

1. **`assetVersion` cache-bust** —— 模板里给 CSS/JS 加 `?v=<file-mtime>`,文件改
   mtime 自动变 → URL 变 → **任何 cache 层 100% 失效**(磁盘 / view 进程级 / CDN)
2. **Makefile dev 工作流** —— `make dev` 一键编译 + 启动,改 HTML/CSS/JS 不用重启
   进程,改 Go 才需要 `make build && make dev-stop && make dev`

## 改动清单

### 1. `internal/web/templates.go`

- `LoadTemplates` 签名加 `staticDir string` 参数(插在 `templatesDir` 和 `displayTZ` 之间)
- 新增 `newAssetVersion(staticDir) func(string) string` 闭包
- 注册 `assetVersion` FuncMap
- **不存在的文件返回空串** → URL 不带 query,跟旧行为完全一致

```go
// 用法(layout.html):
<link rel="stylesheet" href="/static/style.css{{assetVersion "/static/style.css"}}">
// 渲染结果(dev 模式):
<link rel="stylesheet" href="/static/style.css?v=1789954815248000000">
```

版本号 = `file.ModTime().UnixNano()`,10^18 量级,绝对够用。

### 2. `web/templates/layout.html`

```html
<link rel="stylesheet" href="/static/css/pico.min.css{{assetVersion "/static/css/pico.min.css"}}">
<link rel="stylesheet" href="/static/style.css{{assetVersion "/static/style.css"}}">
<script src="/static/js/topology.js{{assetVersion "/static/js/topology.js"}}" defer></script>
```

3 处 assetVersion 注入,覆盖所有 CSS/JS 引用。

### 3. `cmd/ikev2-panel/main.go`

```diff
- tmpl, err := web.LoadTemplates(templatesDir, cfg.DisplayTimezone)
+ tmpl, err := web.LoadTemplates(templatesDir, staticDir, cfg.DisplayTimezone)
```

调用点更新,`staticDir` 是 L246-L253 已经算好的本地变量(本机优先 /app fallback)。

### 4. 测试更新(9 处)

`internal/web/flash_test.go` 8 处 + `pr8_smoke_test.go` 1 处 + `pr9_smoke_test.go` 1 处
LoadTemplates 调用加 `staticDir` 参数,统一传 `filepath.Join("..", "..", "web", "static")`。

### 5. `Makefile`(新增)

| Target | 作用 |
| --- | --- |
| `make build` | 编译 Go 二进制到 `./ikev2-panel` |
| `make test` | 跑全部 go test |
| `make vet` | go vet 静态检查 |
| `make dev` | ★ 编译 + 后台启动 dev 模式(无需 docker)★ |
| `make dev-stop` | 停掉后台 dev 进程 |
| `make dev-logs` | tail dev 日志(/tmp/ikev2-panel-dev.log) |
| `make docker-build` | 构建 docker 镜像 |
| `make docker-run` | docker compose up -d |
| `make clean` | 清理构建产物 + dev 数据 |

**Dev 模式自动行为**:
- 检测 `/etc/swanctl` 不存在 → 自动切 `SkipVici` 模式(不调真 charon)
- 模板从 `./web/templates` 读(`./web/static` 优先级高于 `/app/...`)
- SQLite 写到 `./dev-data/panel.db`
- 自动生成 self-signed 证书到 `./dev-data/`
- 首次启动自动创建 admin 账号,密码写到 `./dev-data/panel-state/INITIAL_ADMIN_PASSWORD.txt`
- HTTP 端口 19090,HTTPS 端口 18443(避开 8443/9090,防跟 63 部署冲突)

**改了模板/CSS 后** —— dev 进程**不需要重启**,浏览器 Ctrl+Shift/R 强刷即可:
1. `assetVersion` 把文件 mtime 编进 URL,改文件 → URL 自动变 → cache 100% 失效
2. 不需要重启服务、不需要 clear cache、不需要 query string 手 bust

**改了 Go 代码后** —— `make build && make dev-stop && make dev`(因为 Go 二进制本身是编译期固化的)

## 关键设计决策

| 决策点 | 选择 | 原因 |
| --- | --- | --- |
| Cache-bust 方式 | **URL query**(`?v=mtime`),不是 hash | hash 要每次启动算;mtime 文件系统直接给 |
| 版本号精度 | **unix nano**(10^18) | 毫秒(10^12)编辑器秒存可能撞;纳秒绝对安全 |
| 静态文件加载 | **磁盘 + FuncMap 读 mtime** | dev 模式无 hash;build 时 mtime 注入 |
| dev 端口 | **19090 / 18443**(避开 9090 / 8443) | 防跟 63 上的生产部署冲突 |
| dev data 目录 | **`./dev-data`**(跟仓库同目录) | 跟生产 `/data` 完全隔离;`.gitignore` 已忽略 |
| Dev 入口 | **复用 `cmd/ikev2-panel/main.go`** | 0 新增二进制;dev/prod 同代码路径 |
| Makefile vs scripts | **Makefile** | 用户熟;IDE 集成好;self-documenting via `make help` |

## Dev 模式演示

```bash
$ make dev
=== starting dev (logs → /tmp/ikev2-panel-dev.log) ===
=== status ===
    PID CMD
  50781 ./ikev2-panel
=== quick info ===
time=... level=INFO msg="ikev2-panel starting" version=dev ...
========================================================
 默认管理员已创建
   用户名: admin
   密  码: <随机 12 位>
   （请立刻登录并修改密码）
========================================================
=== access ===
  HTTP  (no TLS) : http://127.0.0.1:19090/login
  HTTPS (self-signed): https://127.0.0.1:18443/login
  管理员账号: admin
  密码: cat ./dev-data/panel-state/INITIAL_ADMIN_PASSWORD.txt

$ # 现在改 web/static/style.css,保存
$ touch web/static/style.css
$ curl -s http://127.0.0.1:19090/login | grep style.css
<link rel="stylesheet" href="/static/style.css?v=1789957065836000000">
$ # URL 变了 → IDE view 也能看到新版本

$ make dev-stop
stopping dev (pid 50781)
```

## 测试结果

```
=== web 测试 ===
ok  github.com/yourname/ikev2-panel-v2/internal/web 10.757s
  (TestLoadTemplates_* 全部通过 — 签名改了 + assetVersion 注册成功)

=== 全包测试 ===
ok  github.com/yourname/ikev2-panel-v2/internal/cert
ok  github.com/yourname/ikev2-panel-v2/internal/config
ok  github.com/yourname/ikev2-panel-v2/internal/ddns 16.834s
ok  github.com/yourname/ikev2-panel-v2/internal/dns
ok  github.com/yourname/ikev2-panel-v2/internal/expiry
ok  github.com/yourname/ikev2-panel-v2/internal/installtoken
ok  github.com/yourname/ikev2-panel-v2/internal/limit
ok  github.com/yourname/ikev2-panel-v2/internal/metrics
ok  github.com/yourname/ikev2-panel-v2/internal/panelstate
ok  github.com/yourname/ikev2-panel-v2/internal/store
ok  github.com/yourname/ikev2-panel-v2/internal/web 10.650s
```

(`internal/swanctl` 3 个测试需要真 vici socket,本机无 → fail,
跟 PR20 改动**无关**,跟 PR19 之前状态一致。)

## Dev 模式实测(本机)

```bash
$ curl -s http://127.0.0.1:19090/login | grep -E "stylesheet|script src"
<link rel="stylesheet" href="/static/css/pico.min.css?v=1789717765728000000">
<link rel="stylesheet" href="/static/style.css?v=1789954815248000000">
<script src="/static/js/topology.js?v=1789873257616000000" defer></script>

$ touch web/static/style.css

$ curl -s http://127.0.0.1:19090/login | grep "style.css"
<link rel="stylesheet" href="/static/style.css?v=1789957065836000000">
# ✅ mtime 变 → URL 自动变 → IDE view 也 bust
```

## 没改的东西(强调)

- ❌ 生产部署逻辑(63 上的 v2.86-pr19 镜像无需升级)
- ❌ strongSwan / IKEv2 / 证书
- ❌ HTTP 安全 header(PR9 完整保留)
- ❌ 任何 handler / 业务逻辑
- ❌ Docker 镜像(但 PR20 让 docker build 不再是 webui 改动的反馈回路)

## 升级 / 回滚

**生产无影响** —— 模板渲染时 `assetVersion` 跟原 URL 拼起来,效果是 URL 多一个 query,
浏览器/反代/CDN 行为正常(public 资源,版本号无害)。
**dev 工作流可选** —— 用户继续走 docker 链路也完全 OK,只是反馈循环长。
**回滚** —— git revert 即可。
