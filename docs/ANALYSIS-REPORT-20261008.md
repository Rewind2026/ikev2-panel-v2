# ikev2-panel-v2 项目综合分析报告

> 分析日期:2026-10-08 | 基线 commit:`d04ce38` (v2.86-pr23p) | 回滚 tag:`baseline-20261008`
> 分析方式:4 个专业 agent 并行静态分析 + 本机工具链实机验证
> 本报告为只读分析产物,未修改任何业务代码

> ⚠️ **复核修订(2026-10-08 第二轮)**:本报告已完成独立复核,逐项重验证据链。
> 复核发现 **1 项误判已修正**(`audit_log` GC)、**1 项范围被低估已更正**(ddns race)、
> **1 项安全机制漏检已修复**(pre-commit XSS 检测)。详见
> [复核报告](file:///opt/ikev2-panel-v2/docs/REVIEW-REPORT-20261008.md)。

---

## 一、项目架构概述

### 1.1 定位

单容器、自托管的 **IKEv2/IPsec VPN 管理面板**,核心场景是面向 IPv6-only 主机的移动端(iOS/Android)远程接入。README 顶部明确标注「⚠️ 仅限自用」,非多租户 SaaS。

**技术底座**:strongSwan 6.0.1(自编译,启用 `kernel-libipsec` 用户态 ESP)+ Go 编写的管理面板,通过 `swanctl` 下发配置。

### 1.2 规模

| 类别 | 文件数 | 行数 |
|---|---|---|
| Go 源码(含测试) | 100 | 25,032 |
| 前端(html/js/css) | 14 | 5,586 |
| 运维脚本 | 7 | 1,713 |
| 文档(md) | 47 | — |
| **Go 包** | **16 个 internal 包** | — |

### 1.3 核心功能

用户管理 · swanctl 配置动态下发与热加载 · 自签/Let's Encrypt 证书体系 · Apple `.mobileconfig` 生成 · 阿里云 DDNS 同步 · `tc` 双栈限速 · SA 生命周期审计 · Prometheus 指标 · 容器内网络自动探测(nftables/路由表 220)

### 1.4 架构分层(拓扑无环)

```
第 0 层  fsutil / metrics / installtoken / publicip / dns / config   ← 叶子
第 1 层  auth→fsutil · cert · store · limit→swanctl · expiry→store
第 2 层  swanctl→{store,fsutil,publicip} · ddns→{dns,swanctl} · panelstate→cert
第 3 层  runtime→{config,panelstate}
第 4 层  web→{auth,cert,ddns,installtoken,limit,metrics,panelstate,store,swanctl}
第 5 层  cmd/ikev2-panel→全部 13 个包
```

**结论:无循环依赖。** `config` 自 v2.86-PR13.0 已与 `panelstate` 完全解耦。

### 1.5 关键架构决策(修改前必须理解)

| 决策 | 内容 | 影响 |
|---|---|---|
| **用户态 ESP** | `--enable-kernel-libipsec`,绕过内核 XFRM,明文经 `/dev/net/tun` 注入 | 不依赖内核 XFRM;代价是单核瓶颈、无硬件 offload、每包 +8~12 字节 |
| **强制 host 网络** | `network_mode: host` 是唯一模式,非 host → FATAL exit 12 | docker bridge 会分配 `fd00::/8` ULA,charon 抓到后客户端握手成功但流量不可路由 |
| **Go 不碰数据通路** | 面板只做配置管理,不做加解密 | 改网络行为改 shell,改面板行为改 Go |
| **VICI 不支持 load 命令** | charon 的 VICI 协议不实现 `load-creds`/`load-all` | 必须「Go 写文件 + shell out 到 swanctl CLI」,这是全项目最核心的技术约束 |
| **配置三层来源** | entrypoint.sh > panelstate(`/data/panel-state/`) > env var | 合并器在 `internal/runtime/runtime.go:45` |
| **零前端框架** | Go `html/template` SSR + 原生 ES5 + 自研 CSS,无 npm/无 node_modules | 改前端无需构建,`make dev` 直接生效 |

---

## 二、技术栈

### 2.1 依赖(仅 4 个直接依赖,攻击面极小)

| 模块 | 版本 | 用途 |
|---|---|---|
| `github.com/strongswan/govici` | v0.8.1 | VICI 协议,驱动 charon(仅本地 unix socket) |
| `modernc.org/sqlite` | v1.59.0 | 纯 Go SQLite 驱动,**零 CGO**,可 `CGO_ENABLED=0` 交叉编译 |
| `golang.org/x/crypto` | v0.57.0 | bcrypt(cost=12) |
| `github.com/skip2/go-qrcode` | v0.0.0-2020... | 二维码生成(2020 快照,无已知 CVE) |

- **Go 版本**:`go 1.26.0`(go.mod 强 pin 到 patch 版本,低版本直接编译失败)
- **无 Web 框架**:仅标准库 `net/http` + Go 1.22+ 增强路由(`mux.Handle("GET /users/{id}", ...)`)
- **无 ORM / 无配置库 / 无日志库**:裸 `database/sql`、`log/slog`

### 2.2 前端

零框架、零构建、零 CDN。`web/templates/` 11 个模板(Go SSR)+ `web/static/style.css`(2257 行自研主题)+ `topology.js`(498 行手写 SVG 拓扑)。

内联 JS 走 CSP nonce(`<script nonce="{{.CSPNonce}}">`),这是 v2.86-PR12.21 的历史教训:CSP `'self'` 会**静默拒绝** inline script,导致页面上所有按钮失灵,且因 CSS 正常显示极易误诊为前端时序问题。

> 遗留:`web/static/css/pico.min.css` 已是死文件,layout.html 未引用。

### 2.3 构建与部署

- **Dockerfile** 4 阶段:strongswan-builder(源码编译 6.0.1 + MD5 强 pin)→ go-builder(`CGO_ENABLED=0 -ldflags="-s -w"`)→ runner-test(`go test -race`)→ runtime
- **暴露端口**:`500/udp`(IKEv2)、`4500/udp`(NAT-T)、`8443/tcp`(面板 HTTPS)
- **容器安全**:`cap_drop: ALL` + 5 项最小 `cap_add`、`no-new-privileges`、`/dev/net/tun`;已移除 `privileged` 与 `read_only`(改用 tmpfs 限制可写面)
- **CI**:`.github/workflows/docker-publish.yml` 单 job 双架构(amd64/arm64)推 GHCR

> ⚠️ **CI 没有任何测试步骤**。虽有 14 个包的测试和 `runner-test` stage,但 workflow 从不调用,测试完全依赖人工本地执行。

---

## 三、关键代码模块与修改注意事项

### 3.1 修改风险最高的文件(改前必读)

| 行数 | 文件 | 风险 | 改错后果 |
|---|---|---|---|
| 1060 | [internal/ddns/sync.go](file:///opt/ikev2-panel-v2/internal/ddns/sync.go) | 极高 | DDNS 全部核心逻辑混在一个文件(并发/节流/状态迁移/错误分类) |
| 947 | [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2/cmd/ikev2-panel/main.go) | 极高 | 唯一入口,21 步启动时序 + 11 个 goroutine 装配,签名变更影响全项目 |
| 709 | [internal/cert/mobileconfig.go](file:///opt/ikev2-panel-v2/internal/cert/mobileconfig.go) | 高 | Apple 专有格式,改错 iOS 客户端无法导入描述文件 |
| 695 | [internal/web/handlers_home.go](file:///opt/ikev2-panel-v2/internal/web/handlers_home.go) | 高 | 首页聚合 5 个包查询,含 3s 超时降级 |
| 678 | [internal/web/server.go](file:///opt/ikev2-panel-v2/internal/web/server.go) | 极高 | **路由注册唯一入口** + 中间件链 + 安全头 |
| 650 | [internal/web/templates.go](file:///opt/ikev2-panel-v2/internal/web/templates.go) | 高 | 模板加载顺序敏感;FuncMap 是隐式契约 |
| 2257 | [web/static/style.css](file:///opt/ikev2-panel-v2/web/static/style.css) | — | 全项目最大文件,改动影响全部页面 |
| 1163 | [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2/scripts/entrypoint.sh) | 极高 | 容器启动全流程,第 8 步才 `exec` Go 进程 |

### 3.2 公开 API 改动波及面

| 包 | 被引用文件数 | 改动风险 |
|---|---|---|
| `store` | 14 | 最高(类型被 web 模板直接复用) |
| `panelstate` | 8 | 高(三层配置合并的中层) |
| `cert` | 7 | 高 |
| `swanctl` | 6 | 高 |
| `auth` | 5 | 中 |
| `web` | 1(仅 main.go) | 外部影响最小,但运行时影响全部 HTTP 请求 |

### 3.3 五条不可触碰的隐式契约

1. **模板 FuncMap 契约** — `templates.go` 的函数名与 `layout.html` 的 `{{define}}` 名称构成隐式契约,**Go 编译器无法校验**,重命名会静默失效。`layout.html:906` 还留了 `nav` 别名做向后兼容,说明历史上出现过此类问题。

2. **entrypoint 与 Go 的职责边界** — 改网络行为必须改 shell,改面板行为才改 Go。易出现「面板显示的 IP 与 strongSwan 实际监听不一致」。

3. **`MobileConfigOpts` 字段的 nil 语义各不相同** — `DisconnectOnSleep` nil=false,而 `NATKeepaliveEnabled` / `OnDemandEnabled` / `IncludeAllNetworks` nil=**true**。这是最容易写错的地方。

4. **panelstate 的 `cert.go` 三个字段不能带 `omitempty`** — 否则 JSON 缺 key 会让 `entrypoint.sh` 的 `grep` 返回 1,配合 `set -euo pipefail` **直接让容器秒死**(v2.86-pr23o 已踩过)。

5. **`tc` rate 单位必须小写 `mbit`** — 写 `Mbit` 会出错。卸载顺序必须先删 filter 再删 class,**根 qdisc 永不删除**。

### 3.4 业务领域要点

- **EAP ≠ 免证书**:EAP-MSCHAPv2 只免除客户端证书,服务器证书依然必需。这是 iOS 配置最易误解的点。
- **iOS 拒绝 ECDSA 证书**,服务端证书固定 RSA;需 `send_cert=always`,否则 iOS 握手卡住。
- **续签只能用 `--load-creds`**,`--load-all` 会重置 active SA 造成全员断流。
- **`audit_log.timestamp` 存 unix nano**,任何时间范围查询都要 ×1e9。
- **panel.db 必须 0600** — 库中含全部 VPN 用户明文密码,且 `sql.Open` 不会对已存在文件 chmod,必须先 chmod 再 open。

---

## 四、代码质量评估

### 4.1 风险汇总(23 项发现)

| # | 级别 | 类别 | 位置 | 问题 |
|---|---|---|---|---|
| 1 | ~~**高**~~ ✅已修复 | 前端安全 | [handlers_mobileconfig_admin.go:104](file:///opt/ikev2-panel-v2/internal/web/handlers_mobileconfig_admin.go#L104) → [layout.html:802](file:///opt/ikev2-panel-v2/web/templates/layout.html#L802) | ~~`?error=` query 参数经 `safeHTML` 未转义输出 → **反射型 XSS**~~ **已修**:sink 改为 `{{.Msg}}` 自动转义 + handler 停止读 query |
| 2 | ~~**高**~~ ✅已修复 | 前端安全 | [handlers_config.go:378](file:///opt/ikev2-panel-v2/internal/web/handlers_config.go#L378) → android_content.html:17 | ~~`?flash=` query 同上(`ServerAddr != ""` 时可利用)~~ **已修**:该 query 读法为死代码,已移除 |
| 3 | ~~**高**~~ ✅已修复 | 并发 | [auth/ratelimit.go:181-183](file:///opt/ikev2-panel-v2/internal/auth/ratelimit.go#L181-L183) | ~~`RecordLoginFail` 未持 `b.mu` 读 `failCount/firstFailAt/lockedUntil` → **data race**~~ **已修**:改为 `snapshot()` 在 `b.mu` 内捕获并随 `RecordFail` 返回 |
| 4 | 中 ⚠️已缓解 | 前端安全 | [dns/aliyun.go:278](file:///opt/ikev2-panel-v2/internal/dns/aliyun.go#L278) → handlers_ddns.go | 阿里云原始响应体 → `err.Error()` → flash → `safeHTML`。**#1 修复后已随 sink 自动转义**,建议后续改为白名单错误码 |
| 5 | ~~中~~ ✅已修复 | 并发 | [ddns/sync.go:661-664,731,736,748,770](file:///opt/ikev2-panel-v2/internal/ddns/sync.go#L661) | ~~`Run()`/`tick()` 无锁读 `s.cfg.*` → data race~~ **已修**:`snapshot()` + `cfg Config` 参数一路传到 5 个下游方法 |
| 6 | 中 | 认证 | [store/users.go:100](file:///opt/ikev2-panel-v2/internal/store/users.go#L100) | VPN 用户密码明文存库(已知妥协,design §4.3) |
| 7 | 中 | 认证 | [handlers_account.go](file:///opt/ikev2-panel-v2/internal/web/handlers_account.go) | 改密后旧 session 不失效,无法踢掉攻击者 |
| 8 | 中 | 认证 | [main.go:121-164](file:///opt/ikev2-panel-v2/cmd/ikev2-panel/main.go#L121-L164) | 初始 admin 密码明文打印 stdout + 落盘 |
| 9 | 中 | 资源 | [store/store.go](file:///opt/ikev2-panel-v2/internal/store/store.go) | 未设 `SetMaxOpenConns`,SQLite 多连接写会 SQLITE_BUSY |
| 10 | ~~中~~ **误报** | 性能 | — | ~~`audit_log` 表无 GC 会无限增长~~ **复核证伪**:entrypoint.sh:1041-1063 已注册 cron,每天 04:00 跑 `audit-retention.sh`(默认保留 90 天,LE/自签两种模式都覆盖)。**无此问题** |
| 11 | 中 | 性能 | [store/users.go:73](file:///opt/ikev2-panel-v2/internal/store/users.go#L73) | `ListUsers` 无 WHERE/LIMIT,全表扫描 |
| 12 | 中 | 错误处理 | [installtoken.go:61](file:///opt/ikev2-panel-v2/internal/installtoken/installtoken.go#L61) | HTTP 请求路径上的 `panic()` |
| 13-14 | 中 | 错误处理 | handlers_auth.go:133 / user_pipeline.go:201 | 登出删 session 失败静默吞掉;删用户后清理失败**连日志都不打** |
| 15-23 | 低 | 混合 | 多处 | 手写 HTML 未转义、`io.ReadAll` 无 LimitReader、UUID 忽略 rand 错误、Content-Disposition 拼接等 |

### 4.2 明确未发现问题的类别(已逐项验证)

| 类别 | 结论 |
|---|---|
| **SQL 注入** | ✅ 全部 `?` 占位符参数化,无任何字符串拼接 SQL |
| **CSRF 防护** | ✅ header + form 双通道,`subtle.ConstantTimeCompare` 常量时间比较,无缺口 |
| **权限校验缺失** | ✅ 34 条业务路由逐条核对,无遗漏中间件;单 admin 模型无 RBAC 场景 |
| **路径遍历** | ✅ `validateUsername` 禁 `/ \ . 空格 tab 换行` + handler 正则 `^[a-z0-9_-]{3,32}$` 双重防护 |
| **Cookie 安全属性** | ✅ HttpOnly + SameSite=Lax + 条件 Secure(明文 HTTP 时强制关闭并打 WARN) |
| **硬编码密钥/默认密码** | ✅ 全部环境变量注入或随机生成,`.env.example` 敏感项全注释 |
| **XFF 伪造** | ✅ `IKEV2_TRUSTED_PROXY` 默认 false,防伪造 `X-Forwarded-For` 绕过登录锁定 |
| **CORS 越权** | ✅ 零 CORS 头,同源策略默认生效 |
| **HTTP body / 文件句柄泄漏** | ✅ 全部 `defer Close()` |
| **goroutine / channel 泄漏** | ✅ 未发现 |
| **已知 CVE 依赖** | ✅ 依赖风险低,直接依赖仅 4 个 |
| **N+1 查询** | ✅ 未发现 |

### 4.3 并发设计良好的部分

- `atomic.Pointer[tls.Certificate]` + SIGHUP 热重载证书,无锁无 race
- `swanctl.reloadMu` 串行化所有改变 charon 状态的操作,解决并发 `load-all` 经典问题
- `startBG` 框架统一管理 11 个后台 goroutine(WaitGroup + panic recover + Debug 日志)
- 关闭序列**刻意不走** `startBG`(main.go:834-837),避免 `bgWg.Wait()` 等待自身造成死锁
- panelstate 全部 5 个 Store 用 `sync.RWMutex` + 内存缓存 + 锁外做 IO
- DDNS per-family 并发用 buffered channel(2) + `wg.Wait()` 后 `close`,模式正确

---

## 五、潜在风险点与规避建议

### 5.1 修改前必须规避的陷阱

| 风险 | 触发条件 | 规避建议 |
|---|---|---|
| **CSP 静默失效** | 内联 script 漏加 `nonce` 属性 | 检查脚本第 5 项已自动拦截;新增内联 script 务必带 `nonce="{{.CSPNonce}}"` |
| **容器秒死** | panelstate JSON 字段加 `omitempty`,`entrypoint.sh` 的 `grep` 返回 1 撞上 `set -euo pipefail` | `cert.go` 的 `ServerCN`/`ACMEEmail`/`Domain` 三个字段禁止 `omitempty` |
| **全员断流** | 证书续签时用 `swanctl --load-all` | 只用 `--load-creds`;`ikev2-reload.sh` 已封装,勿绕过 |
| **拨号成功但上不了网** | IP forwarding / FORWARD ACCEPT / MASQUERADE 三者缺一 | 改网络相关必须同时核对 entrypoint §3、§3.5、§4 |
| **XSS** | ~~banner 消息走 `safeHTML`~~ **已修**(2026-10-08) | 现为 `{{.Msg}}` 自动转义。新增字段默认安全;唯一豁免是代码常量 `$iconSvg`,**禁止**给 `.Msg` 等用户可控字段加 `safeHTML` |
| **模板静默失效** | 重命名 FuncMap 函数或 `{{define}}` 名称 | 改 `templates.go` 后全量 grep 模板确认同步 |
| **data race** | 在 `ddns.Sync` / `RateLimiter` 上新增无锁字段访问 | `Sync` 已改为 `snapshot()` + `cfg` 参数传递(勿直接读 `s.cfg`);`ipBucket` 状态一律走 `snapshot()` |
| **强推破坏 main** | 直接 `git push origin main` | pre-push hook 已拦截,须走功能分支 |

### 5.2 环境相关风险

- `Dockerfile` 的 `ACMESH_COMMIT` 是**占位 SHA**,本地 build 前必须替换(release-notes-v2.86.md 有明确警告)
- `go.mod` 的 module 名是 `github.com/yourname/ikev2-panel-v2` **占位符**,与实际仓库 `Rewind2026/ikev2-panel-v2` 及镜像 `ghcr.io/rewind2026/...` 不一致
- README「已知限制」第 1 条称 IPv6-only 下 tc 只能作用于 IPv4,**此说已过时**(v2.85-PR7 的 flower 方案已修复),README 未同步

### 5.3 建议优先修复(按投入产出比)

1. ~~**修 XSS(#1/#2/#4)**~~ — ✅ **已于 2026-10-08 完成**。`layout.html:802` 改为 `{{.Msg}}` 恢复自动转义(仅 `$iconSvg` 保留 `safeHTML`),两处 query 读法一并移除。**`-race`/单测/模板渲染三重验证通过**,并新增 `xss_banner_test.go` 防回归。
2. ~~**修 data race #3**~~ — ✅ **已完成**。`ipBucket.snapshot()` 在 `b.mu` 内捕获状态,随 `RecordFail` 返回。新增 `ratelimit_race_test.go`(8 goroutine × 300 次)。
3. ~~**修 data race #5**~~ — ✅ **已完成**。`Sync.snapshot()` + `cfg Config` 参数传递至 5 个下游方法。新增 `sync_race_test.go`,`-race` 连跑 8 次 0 DATA RACE。
4. ~~**加 `audit_log` GC**~~ — **复核证伪,取消此项**。cron GC 机制已完整存在。
5. **CI 补测试步骤** — 加 `docker build --target runner-test` 或直接 `go test ./...`,成本极低。当前 CI 零测试。
6. **改密后踢 session(#7)** — 剩余高价值项中成本最低的一个,`store` 层加 `UPDATE sessions SET ... WHERE user_id=?` 即可。

---

## 六、推荐开发环境配置

### 6.1 本机已就绪(实测)

| 工具 | 版本 | 说明 |
|---|---|---|
| Go | **1.26.0** | `/usr/local/go/bin/go`,与 go.mod 要求一致 |
| gcc | 14.2.0 | 支持 `make race`(CGO 检测) |
| make | 已安装 | Makefile target 可用 |
| git | 2.47.3 | 已配置仓库级身份 |

**基线验证结果**:`go build ./...` ✅ · `go vet ./...` 0 问题 ✅ · `go test ./...` **14 个包全部通过** ✅

### 6.2 尚未安装

Docker / Docker Compose —— 本机**无 `/dev/net/tun` 且无 strongSwan 环境**,无法运行完整容器栈。

### 6.3 依赖管理方案

- **Go**:`go.mod` + `go.sum` 严格锁版本。升级依赖后必须跑 `go test ./...`,`modernc.org/sqlite` 升级风险最高(影响 DB 层与交叉编译)
- **前端**:无 `package.json`,**不存在 npm 依赖管理**。这是项目刻意设计,不要引入
- **部署依赖**:Dockerfile 中 strongSwan 6.0.1 用 MD5 强 pin、acme.sh 用 commit SHA pin,均为可复现构建
- **建议**:如需新增 Go 依赖,优先选纯 Go 实现以保持 `CGO_ENABLED=0` 交叉编译能力

### 6.4 开发工作流

```bash
export PATH=$PATH:/usr/local/go/bin

# 改前端(模板/CSS/JS)—— 无需重启,响应头已配 no-cache
make dev            # 127.0.0.1:18443 (HTTPS) / 19090 (HTTP)

# 改 Go 代码
make build && make dev-stop && make dev

# 提交前检查(全量,约 40s)
./.git/hooks/pre-commit-check.sh

# 快速检查(跳过单测,约 3s)
./.git/hooks/pre-commit-check.sh --fast
```

`make dev` 会自动进入 SkipVici 模式(本机无 charon),SQLite 写 `./dev-data/panel.db`,自动建 admin 并把密码写入 `dev-data/panel-state/INITIAL_ADMIN_PASSWORD.txt`。

---

## 七、项目文档索引

| 文档 | 用途 | 优先级 |
|---|---|---|
| [docs/design.md](file:///opt/ikev2-panel-v2/docs/design.md) | 主设计文档,§3 有 18 条架构决策、§4 安全决策、§9 swanctl 模板设计 | **改代码前必读** |
| [docs/architecture.md](file:///opt/ikev2-panel-v2/docs/architecture.md) | §12「已知陷阱」表 30+ 条是全库最有价值的经验汇总 | **改代码前必读** |
| [configs/swanctl-ipv6-only.conf](file:///opt/ikev2-panel-v2/configs/swanctl-ipv6-only.conf) | 协议层配置基线,含 RFC 8247 去弱排序的 proposals | 改协议相关必读 |
| [docs/TROUBLESHOOTING.md](file:///opt/ikev2-panel-v2/docs/TROUBLESHOOTING.md) | 排障手册,含 README 缺失的 bug 报告指引 | 出问题时查 |
| `docs/audit-2026-09-*.md` (14 份) | 按维度拆分的审计报告,147 个 issue | 深入了解历史决策 |
| `docs/release-notes-*.md` (16 份) | 逐 PR 变更历史 | 了解演进脉络 |

**建议阅读顺序**:README → design.md §3/§9 → architecture.md §12 → swanctl-ipv6-only.conf → entrypoint.sh → internal/swanctl + internal/ddns + internal/cert → TROUBLESHOOTING.md

---

*报告生成完毕;复核与修复记录见 [docs/REVIEW-REPORT-20261008.md](file:///opt/ikev2-panel-v2/docs/REVIEW-REPORT-20261008.md)。*
*2026-10-08 更新:风险 #1/#2/#3/#5 已修复并通过故障注入验证。新增文件仅 `scripts/backup-restore.sh`、3 个回归测试文件与 `.git/hooks/` 下的 3 个本地 hook(后者不在版本控制内)。*
