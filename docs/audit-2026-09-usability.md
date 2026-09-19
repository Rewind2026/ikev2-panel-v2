# 易用性(Usability)审计报告 — 2026-09-19

> 维度:**audit-framework 维度 4 — 易用性**(U1-U8)
> 审计员:项目自查
> 范围:`README.md`、`docker-compose.yml`、`.env.example`、`configs/`、`web/templates/*.html`、`web/static/*`、`internal/web/`、`internal/cert/`、`scripts/entrypoint.sh`、`scripts/renew-cert.sh`、`docs/*.md`、`cmd/ikev2-panel/main.go`
> 状态:**草案,等待评审**
> 前置阅读:[audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) + [audit-2026-09-web.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-web.md)(本报告不重复 W 系列已列项)

---

## 范围

| 路径 | 类别 |
|------|------|
| [README.md](file:///opt/ikev2-panel-v2-main/README.md) | 用户入门 + 部署清单 + 10 条 FAQ |
| [docker-compose.yml](file:///opt/ikev2-panel-v2-main/docker-compose.yml) | 容器编排 + 默认值 |
| [.env.example](file:///opt/ikev2-panel-v2-main/.env.example) | 环境变量样例 |
| [configs/swanctl-ipv6-only.conf](file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf) | strongSwan 主配置 |
| [configs/strongswan-filelog.conf](file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf) | strongSwan filelog |
| [web/templates/*.html](file:///opt/ikev2-panel-v2-main/web/templates/) | Pico v2 模板集(7 个) |
| [web/static/style.css](file:///opt/ikev2-panel-v2-main/web/static/style.css) | 品牌色 + 局部覆盖 |
| [internal/web/handlers_*.go](file:///opt/ikev2-panel-v2-main/internal/web/) | 7 个 handler 文件 |
| [internal/web/flash.go](file:///opt/ikev2-panel-v2-main/internal/web/flash.go) | 一次性消息 |
| [internal/web/server.go](file:///opt/ikev2-panel-v2-main/internal/web/server.go) | middleware + 路由 + /healthz |
| [internal/web/templates.go](file:///opt/ikev2-panel-v2-main/internal/web/templates.go) | 模板加载 + FuncMap |
| [internal/cert/mobileconfig.go](file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go) | iOS mobileconfig 模板 |
| [internal/cert/acme.go](file:///opt/ikev2-panel-v2-main/internal/cert/acme.go) | LE 状态读取 |
| [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) | 容器入口(自动探测 + 自愈) |
| [scripts/renew-cert.sh](file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh) | LE 续签 + 回退 |
| [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) | 进程入口(启动日志) |
| [docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md)(1810 行) | 设计文档 |
| [docs/architecture.md](file:///opt/ikev2-panel-v2-main/docs/architecture.md)(1887 行) | 架构理解 + 借鉴指南 |
| [docs/release-notes-v2.84.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.84.md) 等 6 份 | 版本说明 |

## 工具 / 参考

- [Nielsen 10 Usability Heuristics](https://www.nngroup.com/articles/ten-usability-heuristics/)
- [WCAG 2.1 Quick Reference](https://www.w3.org/WAI/WCAG21/quickref/)
- [MDN: ARIA Live Regions](https://developer.mozilla.org/en-US/docs/Web/Accessibility/ARIA/Live_regions)
- [Color Contrast Checker (WebAIM)](https://webaim.org/resources/contrastchecker/)
- [Apple Developer: NEVPNProtocolIKEv2](https://developer.apple.com/documentation/networkextension/nevpnprotocolikev2)
- [Apple Deployment Guide: IKEv2 device management](https://support.apple.com/zh-cn/guide/deployment/dep4ce9487d/web)
- [Mozilla MDN: Format Date with locale](https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Intl/DateTimeFormat)
- [prometheus/client_golang](https://github.com/prometheus/client_golang)

## 发现汇总

| 严重度 | 数量 | 关键问题 |
|--------|------|----------|
| **HIGH** | 4 | docker-compose image tag 与 README 构建命令不一致 → 首启动失败 / 默认管理员密码只打到 stdout,容器停止后查不到 / `formatTime` 用服务器本地时区 + 格式不一致(UTC vs 本地)/ `/healthz` 仅返回 "ok",无任何健康指标(更无法判断 DDNS / 续签 / DB 是否健康) |
| **MED** | 8 | 启动后首页无"下一步该做什么"引导 / 无 `/metrics` 端点(运维盲区)/ acme.sh 失败日志分散在 `/var/log/acme-renew.log` 与 `/var/log/ikev2-renew.log` 两处,文档只说前者 / 文档 1810+1887 行没有 troubleshooting 总入口 / mobileconfig 安装后 iPhone 没有"如何验证 VPN 连上"的指引 / DDNS toggle + 凭证变更缺 audit log(关联 W10)/ 时区显示硬编码 UTC vs 服务器本地无选择 |
| **LOW** | 6 | CSS 类名 `alert-warning` 与 `.alert-warn` 不匹配(黄条未生效)/ QR 图 alt="QR" 信息缺失 / 颜色对比度(绿/红 status)未达 WCAG AA 4.5:1 / 强 delete 用 `onsubmit=confirm(...)` 依赖 JS / `<label>` 不写 `for` 属性 / 表单错误无 `aria-live` |

> 与 `audit-2026-09-web.md` 重复的(不重复列):
> - 错误信息泄漏(W04)、input 校验(W08)、filename injection(W09)、rate limit(W03)、审计日志(W10)、unicode 用户名(W13)、GeneratePassword(W12)、CSRF/W01 等已在 web 报告覆盖。
> - 本报告聚焦**用户感知层面的"易用性"**,代码层的"输入校验缺失"等归 web。

---

## [SEVERITY-HIGH] ISSUE

### ISSUE-U01:docker-compose.yml 的 image tag 与 README 构建命令不一致 → 新人 `docker compose up` 拉取不到镜像

- **位置**:
  - [docker-compose.yml:38](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L38) `image: ikev2-panel:v2-84` + `build: .`
  - [README.md:179](file:///opt/ikev2-panel-v2-main/README.md#L179) `docker build --network=host -t ikev2-panel:v2-80 .`
  - [README.md:469-486](file:///opt/ikev2-panel-v2-main/README.md#L469) "v2-79 简化版" 章节里 `IKEV2_SERVER_CN=vpn.example.com` 是 v2-79 的字段名,缺 v2-83 凭证统一说明
  - README 全篇至少 7 处写 `v2-80`:`libipsec` 行 332 / 自动探测 行 39 / Docker build 行 179 / `./scripts/up.sh` 行 187 / 镜像标记 行 159-160 / etc.
- **问题**:
  1. **首次部署按 README 跑 `docker build -t ikev2-panel:v2-80 .`**(README 写明)→ 镜像 tag 是 `v2-80`
  2. **接着跑 `docker compose up -d`**(README 写明)→ compose 找 `image: ikev2-panel:v2-84` → **找不到**(本地 tag 是 v2-80,registry 没发布 v2-84)→ docker 会 fallback 到 `docker build .`(因为有 `build: .`)→ 镜像重建但 tag 不固定 → 行为不确定(可能打 `latest`,可能用 cache 里的 v2-80)
  3. **或者用户根本没看到 README 里那行 build** → 直接 `docker compose up -d` → 走 `build: .` 重新编译,**镜像本地无名 tag**,后续 `docker exec` / `docker logs` 操作不便
  4. **即使工作流跑通**,README 里写的是 v2-80 部署说明(image tag / default value / up.sh)→ 用户实际跑的是 v2-84 → 字段命名不同(README 没有 `IKEV2_ALIYUN_KEY_ID` 提示,只提示 `Ali_Key`)→ 配置项漏配
- **风险**:
  - **真用户卡住**:首次部署会卡在镜像版本不一致上,需要有一定 docker 经验的人才能 debug
  - **下游连锁**:如果用户看 README 第 467 行"v2-79 简化版"`IKEV2_SERVER_CN=...` 缺 `IKEV2_DOMAIN`,可能在 LE 模式下启动失败
- **参考**:
  - [Docker Compose: build vs image](https://docs.docker.com/compose/compose-file/05-services/#build) — `build` + `image` 同时存在时,build 出来的镜像打 `image` 指定的 tag
  - 内部"README 同步编译产物"原则(同 commit 改代码 + 改 README + 改 image tag)
- **修复方案**:
  1. **统一 tag**:改 docker-compose.yml 为 `image: ikev2-panel:v2-80` 或 `image: ikev2-panel:latest`(配合 Dockerfile 的 ARG `PANEL_VERSION`);改 README 的 `docker build` 命令 tag 同步
  2. **(推荐)**在 Dockerfile 顶部加 `ARG PANEL_VERSION=dev`,在 `docker build` 时传 `--build-arg PANEL_VERSION=v2.84`,写入 `/etc/ikev2-panel-version`,启动时打日志 + 显示在面板
  3. **README 增加"验证部署正确性"小节**:`docker compose ps` 应该看到 `ikev2-panel:v2-XX` 容器在运行,`docker logs ikev2-panel 2>&1 | head -5` 应该看到启动 banner
- **工作量**:0.2d(改 tag + README 同步 + Dockerfile 加 PANEL_VERSION)

---

### ISSUE-U02:默认管理员密码只打印到 stdout,容器销毁后无法找回(也无任何"我现在用什么密码"的 UI 提示)

- **位置**:
  - [cmd/ikev2-panel/main.go:100-107](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L100-L107) 默认密码打印用 `fmt.Println` 到 stdout
  - [README.md:446-448](file:///opt/ikev2-panel-v2-main/README.md#L446-L448) "端到端验收"步骤 2 要求"看启动日志,默认管理员密码打印"——**用户**只有这一个途径
  - 无任何面板 UI 提示"修改默认密码"
  - 无"忘记密码 / 重置 admin"的 recovery flow
- **问题**:
  1. **首次启动,密码打印到 docker compose logs 输出**——**docker 默认日志 driver 是 `json-file`,有轮转(默认 10MB × 3)**——如果日志被轮转/裁剪,密码就找不回了
  2. **重启容器**:默认密码**不会**再次打印(`EnsureDefaultAdmin` 走 created==false 短路),所以 `docker logs ikev2-panel | grep admin` 也找不到
  3. **面板**无"修改 admin 密码"页面(只有重置 VPN 用户密码)
  4. **没有 forgot-password 流程**(无邮件通知系统,这是 design §4.3 的妥协点,但**没有应急流程**)
- **风险**:
  - **真用户卡住**:用户误删 DB、迁移失败、首次启动后没看到密码 → **永久锁定 admin**
  - **回滚路径**:`rm /data/ikev2-panel.db` → 重启容器 → 重新生成默认密码 + **所有 VPN 用户数据全丢**
- **参考**:
  - [12-Factor App: Logs as event streams](https://12factor.net/logs) — 密码不应只走 stdout
  - [NIST SP 800-63B §5.1.1.2 — Memorized Secret Verifiers](https://pages.nist.gov/800-63-3/sp800-63b.html) — 凭据必须有可恢复路径
- **修复方案**:
  1. **(短期 0.2d)** 启动时除了 stdout,同步写到 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`(`chmod 0600`),并在文件存在时**保留**(让管理员可以从容器外读)
  2. **(短期 0.3d)** 面板首页顶部加横幅(7 天后消失):"⚠️ 你还在用默认密码,请立即到 admin 设置页修改"——前提是加 admin 修改密码页面
  3. **(中期 1.0d)** 加 admin 修改密码页 + "忘记密码" CLI 工具(在容器内 `docker exec ikev2-panel ikev2-panel-reset-admin`)
- **工作量**:**HIGH 优先级,但简单修复 0.2d 就够**

---

### ISSUE-U03:`formatTime` 用服务器本地时区(默认 UTC)且时间格式不统一(DDNS UTC vs 表格本地)

- **位置**:
  - [internal/web/templates.go:89-94](file:///opt/ikev2-panel-v2-main/internal/web/templates.go#L89-L94) `formatTime` 用 `time.Unix(unix, 0).Format("2006-01-02 15:04")` —— **`time.Unix` 返回的 time.Time 是 UTC,但 .Format 输出时按服务器本地时区**
  - 容器内默认 `TZ=UTC`(Dockerfile 没显式设置)
  - [internal/web/handlers_ddns.go:58](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L58) DDNS 时间 `last.Time.UTC().Format("2006-01-02T15:04:05Z")` —— **显式 UTC + Z 后缀**
  - [internal/web/handlers_home.go:190](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L190) 同上
- **问题**:
  1. **容器里 `formatTime` 显示的是 UTC 时间**(因为 `TZ=UTC` 容器默认)
  2. **DDNS 同步时间显示 ISO8601 UTC** 带 `Z` 后缀
  3. **`mobileconfig.PayloadDescription`** 用 UTC(RFC3339)——一致
  4. **三处格式不同**:
     | 场景 | 显示 | 时区 |
     |------|------|------|
     | 用户列表/详情 created_at / last_used_at / expires_at | `2026-09-19 08:30` | 服务器本地(UTC) |
     | DDNS 最后同步 | `2026-09-19T08:30:15Z` | UTC 显式 |
     | 移动端 mobileconfig 描述 | `2026-09-19T08:30:15Z` | UTC 显式 |
  5. **admin 在 HKT(UTC+8)看面板**,表格列显示 `08:30`,但实际是"UTC 08:30"——admin 可能误以为是 HKT 16:30
  6. **无配置项** `IKEV2_TZ` / `IKEV2_DISPLAY_TIMEZONE`
- **风险**:
  - **时间显示不一致**:同一时间在 3 个地方显示 3 种格式,**admin 难以关联**
  - **业务风险**:admin 重置某用户密码后看"创建时间",以为是 HKT 时间 → 排查客户端连接问题时记错时间窗口
- **参考**:
  - [Go time package: Unix() + Format](https://pkg.go.dev/time#Unix)
  - [ICU DateTimeFormat patterns](https://unicode-org.github.io/icu/userguide/format_parse/datetime/) — 标准化显示
- **修复方案**:
  1. **加配置项** `IKEV2_DISPLAY_TIMEZONE=Asia/Hong_Kong`(默认 `"UTC"`),`internal/web/templates.go` 读 config 注入 FuncMap
  2. **统一格式**:
     - 表格列: `2026-09-19 16:30 HKT`(显式缩写)
     - DDNS: `2026-09-19 16:30:15 HKT`(带秒 + 缩写)
     - mobileconfig: ISO8601 + 偏移(`+08:00` 或 `Z`)
  3. **(更简单)**面板顶部 nav 加 "时区: UTC ▼" 下拉,选 UTC / 浏览器本地 / 自定义
  4. **(最佳)**用 JS `<time datetime="2026-09-19T08:30:15Z">` 让浏览器按本地时区显示(浏览器侧 Intl.DateTimeFormat)
- **工作量**:0.3d(配 FuncMap + 模板改 `<time>` + nav 加时区选项)

---

### ISSUE-U04:`/healthz` 仅返回 "ok" 字符串——外部健康检查 / 监控系统无法判断实际服务状态

- **位置**:
  - [internal/web/server.go:76](file:///opt/ikev2-panel-v2-main/internal/web/server.go#L76) `mux.HandleFunc("GET /healthz", handleHealthz)`
  - [internal/web/server.go:196-199](file:///opt/ikev2-panel-v2-main/internal/web/server.go#L196-L199) 实现:`w.WriteHeader(200) + fmt.Fprintln(w, "ok")`
- **问题**:
  1. **`/healthz` 不检查任何东西**——DB / charon / DDNS / 续签状态 / swanctl 路径 都没看
  2. **无 `/readyz` / `/health`** 区分 liveness vs readiness
  3. **无 `/metrics`** Prometheus 端点
  4. **运维盲区**:K8s / Docker Swarm / 监控探针只能判断"进程在响应",不能判断"VPN 实际能工作"
- **风险**:
  - **生产环境监控失效**:容器跑着但 DB 损坏 → `/healthz` 仍返回 ok → 监控不报警 → 用户拨号失败才发现
  - **续签失败无人知**:LE 证书过期 5 天 → 只有 admin 登录面板看横幅才知 → 监控没报警
  - **DDNS 静默失败**:DDNS 一直失败但容器正常 → `LastDDNSFailed` 标志位存在 → 监控不知道
- **参考**:
  - [Kubernetes: Liveness vs Readiness Probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
  - [Prometheus: Go client_golang](https://github.com/prometheus/client_golang)
  - 标杆:[authentik health endpoints](https://github.com/goauthentik/authentik/blob/main/authentik/health/)
- **修复方案**:
  1. **`/healthz` 升级**(0.3d):
     - DB ping
     - VICI socket 可达性
     - LE 模式时检查 LAST_RENEW_FAILED 文件
     - 返回 JSON: `{"ok": true, "components": {"db": "ok", "vici": "ok", "le_cert": "ok"}}`
     - 任一失败 → 503
  2. **`/readyz`**(0.1d):在 /healthz 基础上额外要求 "至少有一个 swanctl connection loaded"
  3. **`/metrics`**(0.5d):[prometheus/client_golang](https://github.com/prometheus/client_golang) 暴露:
     - `http_request_duration_seconds{path,method,status}`
     - `vpn_active_sas`(从 swanctl 周期采集)
     - `ddns_last_sync_timestamp_seconds{success}`(Gauges)
     - `le_cert_expiry_timestamp_seconds`(Gauge)
     - `panel_login_attempts_total{result}`(Counter)
- **工作量**:0.9d(0.3 healthz + 0.1 readyz + 0.5 metrics)

---

## [SEVERITY-MED] ISSUE

### ISSUE-U05:启动后首页(`/`)无"下一步该做什么"引导,新管理员面对空面板不知道干啥

- **位置**:
  - [web/templates/home_content.html:25-37](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L25-L37) "欢迎,admin" + 总用户数 + 活跃 SA + 服务器地址 + 2 个按钮
  - 无"onboarding wizard" / 无"checklist"
- **问题**:
  1. **首次登录**(`TotalUsers = 0`):
     - 看到一个"欢迎,admin" + 数字都是 0
     - 只有"用户管理"+"新增用户"两个按钮
     - **没说**:"先去阿里云凭证卡填凭证 / 关掉 self-signed 用 LE / 配置 DNS AAAA / 防火墙开 500/4500"
  2. **活跃 SA = 0** 时:
     - 表格显示"无活跃连接"
     - **没说**:"按用户详情 → 下载 mobileconfig → 扫码安装"
  3. **没有 checklist / progress bar**,管理员不知整体进度
- **风险**:
  - 新 admin 看到空面板 → 误以为"VPN 还没工作" → 联系开发者求助 → 实际只差填个阿里云凭证
- **参考**:
  - [Nielsen Heuristic #6: Recognition rather than recall](https://www.nngroup.com/articles/recognition-and-recall/)
  - [Nielsen Heuristic #10: Help and documentation](https://www.nngroup.com/articles/user-control-and-freedom/)
- **修复方案**:
  1. **`home_content.html` 加 onboarding checklist**(0.5d):
     - ✅ "服务器地址:2408:..."
     - ☐ "阿里云凭证" → "填阿里云凭证" 链接
     - ☐ "创建第一个 VPN 用户" → "新增用户" 链接
     - ☐ "下载 mobileconfig / 扫码"
     - ☐ "开放 UDP 500/4500 防火墙"(静态提示,无 API 检测)
  2. **首次 `TotalUsers = 0` 时**显示一个绿色 tipbox:"还没有用户 — 点这里新增第一个"
- **工作量**:0.5d

---

### ISSUE-U06:无 `/metrics` 端点,Prometheus / Grafana / 告警系统无法接入

- **位置**:见 U04(server.go 全文件 + 整个项目 grep `prometheus` 0 处)
- **问题**:
  - 仅有 `slog` text/json 日志(运维得 grep)
  - 无指标维度(`gauge`, `counter`, `histogram`)
  - 无结构化的"每秒请求数"、"P99 延迟"、"SA 数"指标
- **风险**:
  - **告警无法自动化**:续签失败 → 只能 admin 登录面板看 → 30 天才发现
  - **容量规划缺数据**:不知道 peak SA 数 / 流量趋势 → 不知道何时扩容
  - **SLO 无法度量**:无 uptime / 错误率指标
- **参考**:
  - [Prometheus Go client](https://github.com/prometheus/client_golang)
  - [OpenMetrics spec](https://openmetrics.io/)
  - 标杆:[authentik exposes /metrics](https://github.com/goauthentik/authentik)
- **修复方案**:
  1. (与 U04 同步):加 `prometheus/client_golang` 依赖,注册 `/metrics` 路由
  2. (与 W10 同步):audit log 表数据可用 `audit_log_total{event_type}` Counter 暴露
  3. 关键指标:
     - `vpn_active_sas`(Gauge,周期从 swanctl 采集)
     - `ddns_last_sync_unixtime{family}`(Gauge)
     - `le_cert_expiry_unixtime`(Gauge)
     - `http_requests_total{method,path,status}`(Counter)
     - `http_request_duration_seconds{method,path}`(Histogram)
     - `panel_login_attempts_total{result}`(Counter,success/fail)
- **工作量**:0.5d(与 U04 合并做 0.9d)

---

### ISSUE-U07:acme.sh / renew-cert 失败日志分散在 `/var/log/acme-renew.log` 和 `/var/log/ikev2-renew.log`,文档与代码不一致

- **位置**:
  - [scripts/renew-cert.sh:59](file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh#L59) `> /var/log/acme-renew.log 2>&1` —— **acme.sh 自身日志写到 acme-renew.log**
  - [internal/web/handlers_home.go:142](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L142) LE 续签失败面板横幅提示"请检查 `/var/log/ikev2-renew.log`" —— **告诉用户去看 ikev2-renew.log**
  - [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) 应该在 LE 续签触发 reload 时也会写日志(未确认具体路径,需 `grep "ikev2-renew\|acme-renew\|log" entrypoint.sh`)
- **问题**:
  1. **面板告警指向 `/var/log/ikev2-renew.log`**(handlers_home.go:142)
  2. **renew-cert.sh 实际写到 `/var/log/acme-renew.log`**
  3. **用户按提示去查 ikev2-renew.log** → 找不到错误 → 误以为"问题不存在"
  4. **两处日志都不在 docker logs 输出里**(因为是文件而非 stdout/stderr)→ `docker logs ikev2-panel` 看不到续签细节
- **风险**:
  - **真用户卡住**:LE 续签失败 → 面板红字提示"看 ikev2-renew.log" → 找不到 → 求助开发者 → 实际是 acme.sh 凭据失效
- **参考**:
  - [12-Factor: Logs to stdout](https://12factor.net/logs)
  - [Docker logging best practices](https://docs.docker.com/config/containers/logging/)
- **修复方案**:
  1. **(短期 0.1d)** 改 handlers_home.go 提示路径为正确的 `/var/log/acme-renew.log`,或同时提两个路径
  2. **(中期 0.3d)** renew-cert.sh 把输出用 tee 同时写文件 + stdout(`tee /var/log/acme-renew.log >(cat >&1)`)→ docker logs 能看到续签结果
  3. **(更好)** 在面板加"LE 续签日志最近 100 行"折叠区(读文件渲染),无需 SSH 进容器
- **工作量**:0.1d(短修) / 0.3d(中期)

---

### ISSUE-U08:文档体量 4400+ 行,无统一 troubleshooting 索引,新人 1 小时很难上手

- **位置**:
  - [docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md) **1810 行**
  - [docs/architecture.md](file:///opt/ikev2-panel-v2-main/docs/architecture.md) **1887 行**
  - [README.md](file:///opt/ikev2-panel-v2-main/README.md) **697 行**
  - [docs/plan-v2.84.md](file:///opt/ikev2-panel-v2-main/docs/plan-v2.84.md) 等
  - 6 份 `release-notes-v2.79.2.md ... v2.84.md`
- **问题**:
  1. **README FAQ 10 条**(README line 594-686)只覆盖"启动期经典问题"——缺 mobileconfig 安装失败、证书信任、限速不生效、Android 连不上等
  2. **没有 troubleshooting 总入口**(README §"故障排查"只有 5 条)
  3. **设计文档 vs 架构理解文档的边界模糊**:
     - design.md §19 是"DDNS"——架构 + 流程 + 边界全有
     - architecture.md §18 是 "govici 重构"——也是同一项目
     - 新人不知道先看哪份
  4. **没有"第一小时入门"路径图**——README "快速开始"只到 `docker compose up` 跑通为止,没有"现在打开浏览器 → 登录 → 创建用户 → 装 mobileconfig"的端到端演练
  5. **6 份 release-notes** 是历史快照而非"当前状态"
- **风险**:
  - **新人学习曲线陡**:4400 行文档,无导航,**1 小时找不到 mobileconfig 安装步骤**(只能读 README)
  - **贡献者门槛高**:想改代码的开发者要先读完整 design.md + architecture.md 才能下手
- **参考**:
  - [Diátaxis framework](https://diataxis.fr/) — 把文档分 tutorial / how-to / reference / explanation
  - [Divio documentation system](https://documentation.divio.com/)
- **修复方案**:
  1. **加 `docs/TROUBLESHOOTING.md`**(0.5d):按"症状 → 排查路径 → 解法"组织,覆盖:
     - 容器启动失败(IPv6 / forwarding / 端口)
     - mobileconfig 装不上(iOS 不弹 / 装完连不上)
     - Android 连不上(EAP / CA 证书)
     - 限速不生效
     - DDNS 不工作
     - 续签失败
  2. **README 顶部加 "Where to start" 决策树**(0.1d):
     - "我要部署" → 快速开始
     - "出问题" → TROUBLESHOOTING.md
     - "我要改代码" → architecture.md
     - "我要理解 why" → design.md
  3. **(可选)** 加 `docs/ONBOARDING.md` —— 端到端演练(git clone → 登录 → 创建用户 → 安装 mobileconfig)
- **工作量**:0.6d(0.5 + 0.1)

---

### ISSUE-U09:mobileconfig 安装后,iPhone 端没有"如何验证 VPN 是否连上"的指引

- **位置**:
  - [web/templates/user_detail_content.html:51-53](file:///opt/ikev2-panel-v2-main/web/templates/user_detail_content.html#L51-L53) 有 iOS/Android 安装步骤,但**没说"装完怎么看 VPN 连上了"**
  - [internal/cert/mobileconfig.go:73-77](file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go#L73-L77) PayloadDescription 只写版本号,不含验证指引
  - [README.md](file:///opt/ikev2-panel-v2-main/README.md) 搜索"verify\|验证\|连上" 仅有 §"端到端验收" 中的 docker 端验证步骤(UDP/4500 抓包)
- **问题**:
  1. iPhone 装完 mobileconfig → 设置 → VPN 开关 → 拨
  2. **iOS 不显示任何"已连接"文字** —— 只在状态栏出现"VPN"图标 + 顶部通知
  3. **用户装完拨号** → 看到设置里 VPN 开关 ON → 但**没流量** → 不知道是 iOS 端没拨通,还是拨号成功了但流量没走 VPN
  4. **没有"诊断步骤"**:
     - 测速网站(如 fast.com)→ 看 IP 是否变成服务器 IP
     - 设置 → 通用 → VPN 与设备管理 → 描述文件 → 看上次连接时间(strongSwan 提供)
     - 设置 → Wi-Fi → 看 DNS 是否变成 1.1.1.1/8.8.8.8
  5. **admin 端** 也没"这个用户是否现在连接"的实时光标(SA 列表在 / 首页,不在用户详情页)
- **风险**:
  - **iOS 用户常见"装上了但不能用"** —— 实际是 captive.apple.com 探测或 DNS 配置问题,但用户不知道
  - 反复联系 admin 排查,增加运维负担
- **参考**:
  - [Apple: About iOS VPN](https://support.apple.com/en-us/HT202673)
  - [Apple: How to verify VPN connection](https://support.apple.com/guide/deployment/use-vpn-protocols-dep4ce9487d/web)
- **修复方案**:
  1. **user_detail_content.html** 加"验证 VPN 连接"折叠区(0.1d):
     - 拨号后看设置 → VPN 开关 → 状态栏 VPN 图标
     - 打开 https://ifconfig.me → IP 应变成 {{.ServerAddr}}
     - 设置 → 通用 → VPN 与设备管理 → 描述文件 → PayloadDescription 显示 v2-84
  2. **面板用户详情页**加"当前 SA 状态"卡片(0.5d):实时从 swanctl ListSAs 查该用户的 SA → 显示"已连接 5 分钟 / 未连接"
  3. **(长期)** 增加一个 `/users/{id}/qr.png` 端点 + 描述文件指引
- **工作量**:0.6d

---

### ISSUE-U10:DDNS toggle / 凭证变更 / 用户删除全部缺 audit log(关联 W10,但补 U6 角度)

- **位置**:
  - [internal/web/handlers_ddns.go:106](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L106) `s.Logger.Info("ddns toggled", "enabled", enabled)` —— 只记 enabled / family,**无 admin name / IP / UA**
  - [internal/web/handlers_aliyun.go:90](file:///opt/ikev2-panel-v2-main/internal/web/handlers_aliyun.go#L90) `s.Logger.Info("aliyun creds saved via panel", "key_id_masked", ...)` —— **无 IP / admin**
  - [internal/web/handlers_users.go:213-222](file:///opt/ikev2-panel-v2-main/internal/web/handlers_users.go#L213) 创建用户 flash,但 **Logger.Warn 都没记**
  - [internal/web/handlers_auth.go:60-67](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L60-L67) 登录成功**完全不打日志**(只失败时 Debug)
- **问题**:
  - **事后追溯难**:用户报"我的账号不见了" → admin 看日志找不到谁删的
  - **合规**:审计要求 login / CRUD / 凭证变更全留痕
  - **grep 不到"哪个 IP 在什么时候登录了"**(没有 login success 日志)
- **风险**:
  - **数据无追溯**:被入侵后无法知道攻击者做了什么
  - **误操作无法追责**:"谁把我的 DDNS 关了?"
- **参考**:
  - [OWASP Logging Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
  - [NIST SP 800-92](https://csrc.nist.gov/publications/detail/sp/800-92/final)
- **修复方案**:
  - 完整方案已在 W10(audit_log 表 + UI)覆盖
  - 本 issue 重点:即使是简单加 `slog.Info` 也能大幅提升可观测性(0.1d 起步)
- **工作量**:**与 W10 合并**(共 1.0d)

---

### ISSUE-U11:时区显示硬编码 UTC,无 admin 配置项 / 无浏览器本地时区适配

- **位置**:见 U03
- **问题**:
  1. 容器 `TZ=UTC`(默认)
  2. `formatTime` 无 `TZ` 参数化
  3. 面板 `<html lang="zh-CN">` 强制中文,无 `Accept-Language` fallback
  4. admin username 也是 ASCII-only([web audit W13](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-web.md)),中国 admin 不能用中文名
- **风险**:
  - **多语言 admin 受限**(见 W13)
  - **时区错位**(见 U03)
- **参考**:与 U03 合并修复
- **工作量**:包含在 U03

---

### ISSUE-U12:`renderInstallError` 在 mobileconfig 渲染失败时返回 generic 500,iPhone 看到 "render mobileconfig failed"

- **位置**:
  - [internal/web/handlers_config.go:42](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L42) `http.Error(w, "render mobileconfig failed", http.StatusInternalServerError)`
  - [internal/web/handlers_config.go:149](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go#L149) 同
- **问题**:
  1. `cert.RenderMobileconfig` 失败极罕见(template parse / base64 / UUID),但失败时:
     - iPhone Safari 扫码 → iOS 看到 generic "render mobileconfig failed" → **不知道是 token 过期还是服务端 bug**
     - 不知道是不是 QR 截图过期
  2. `renderInstallError` 只处理 2 种已知情况(token 过期 / user gone),不处理"渲染失败"
- **风险**:
  - **iPhone 端不可达**(token 一次性 + 10 分钟 TTL → 重新生成二维码才能恢复)→ 但用户不知道
- **参考**:与 U9 合并
- **修复方案**:
  - 改用 `renderInstallError(w, "服务器生成配置文件失败,请联系管理员。\n请重新生成二维码再试一次。")`
  - 用户体验:用户知道"重新生成二维码"
- **工作量**:0.1d(改 2 处)

---

## [SEVERITY-LOW] ISSUE

### ISSUE-U13:CSS 类名 `.alert-warn` 与模板用的 `.alert-warning` 不匹配 → 黄条未生效

- **位置**:
  - [web/static/style.css:82-89](file:///opt/ikev2-panel-v2-main/web/static/style.css#L82-L89) CSS 定义 `.alert-warn`(无 `ing`)
  - [web/templates/home_content.html:22](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L22) 模板用 `<div class="alert-warning">`(有 `ing`)
- **问题**:
  - 类名不一致 → 黄条未应用 CSS → 默认浏览器渲染(可能样式丑陋、无 padding、无背景色)
  - 用户看不到"⚠️ 未配置阿里云凭证"提示
- **参考**:
  - [BEM naming](https://getbem.com/) — 类名一致性
- **修复方案**:
  - 二选一:`style.css` 改 `.alert-warning` 或模板改 `.alert-warn`
  - 推荐改 CSS(更标准的命名)
- **工作量**:0.01d

---

### ISSUE-U14:QR 码 `<img alt="QR">` 信息缺失,屏幕阅读器朗读"QR"

- **位置**:
  - [web/templates/user_detail_content.html:36-37](file:///opt/ikev2-panel-v2-main/web/templates/user_detail_content.html#L36-L37) `alt="QR" width="200" height="200"`
- **问题**:
  - 屏幕阅读器读"QR" → 用户不知道这个 QR 是什么
  - 应该是 `alt="QR 码,扫描后在 iOS 上安装 {{.User.Username}} 的 VPN 描述文件"`
- **参考**:[WebAIM: Alt Text](https://webaim.org/techniques/alttext/)
- **修复方案**:改 `alt` 文案
- **工作量**:0.01d

---

### ISSUE-U15:状态颜色对比度未达 WCAG AA 4.5:1(纯绿/纯红)

- **位置**:
  - [web/templates/home_content.html:55](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L55) `<strong style="color: green;">已配置</strong>`
  - [web/templates/home_content.html:87](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L87) `<strong style="color: red;">未配置</strong>`
  - [web/templates/home_content.html:131](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L131) 同样 `style="color: green;"` / `style="color: red;"`
- **问题**:
  - `#008000` 绿 / `#ff0000` 红 在白底上对比度 ~3.4:1 / ~4.0:1(参考 [WebAIM Contrast Checker](https://webaim.org/resources/contrastchecker/))
  - WCAG AA 要求正文 4.5:1 → **未达标**
  - 颜色信息同时无文字(图标或 emoji)→ 屏幕阅读器 + 色盲用户看不到状态
- **参考**:[WCAG 1.4.3 Contrast Minimum](https://www.w3.org/WAI/WCAG21/Understanding/contrast-minimum.html)
- **修复方案**:
  - 加 emoji(✅ / ❌)辅助 + 改用 Pico 的语义色(`--pico-ins-color` / `--pico-del-color`,自带对比度)
  - 或用 `#2e7d32`(深绿,对比度 5.5:1)/ `#c62828`(深红,对比度 6.5:1)
- **工作量**:0.1d

---

### ISSUE-U16:删除 / 停用按钮用 `onsubmit="return confirm('...')"`,仅 JS 启用时生效

- **位置**:
  - [web/templates/users_list_content.html:36](file:///opt/ikev2-panel-v2-main/web/templates/users_list_content.html#L36) `onsubmit="return confirm('确认删除？')"`
  - [web/templates/user_detail_content.html:58, 63, 73](file:///opt/ikev2-panel-v2-main/web/templates/user_detail_content.html#L58-L73) 同
- **问题**:
  - 依赖 JS 启用 → 用户禁用 JS → 误删无法挽回
  - Pico 默认无渐进增强 fallback
- **参考**:
  - [Progressive enhancement](https://developer.mozilla.org/en-US/docs/Glossary/Progressive_Enhancement)
- **修复方案**:
  - 短期:接受现状(管理面板通常 JS 启用)
  - 中期:加 `details/summary` 二次确认表单 → 服务端二次校验
- **工作量**:0.1d(可选)

---

### ISSUE-U17:表单 `<label>` 不写 `for="..."` 属性,屏幕阅读器不一定关联输入框

- **位置**:
  - [web/templates/login_content.html:12-17](file:///opt/ikev2-panel-v2-main/web/templates/login_content.html#L12-L17) `<label>用户名 <input name="username">` —— 隐式 wrap,大多数屏幕阅读器识别,**但不是所有**
  - 全部 5 个表单(login / user_new / user_detail / aliyun / android)
- **问题**:
  - 当前 `<label>...<input>` 嵌套方式(implicit label)是 W3C 合法的,但**显式 `for="id"` 更可靠**
- **参考**:[W3C: Labeling form controls](https://www.w3.org/WAI/tutorials/forms/labels/)
- **修复方案**:
  - 给每个 `<input>` 加 `id="..."`,`<label for="...">` 显式绑定
  - 或在 `<input>` 加 `aria-label="..."`
- **工作量**:0.3d(全 7 个模板)

---

### ISSUE-U18:表单错误无 `aria-live`,错误信息不被屏幕阅读器朗读

- **位置**:
  - [web/templates/login_content.html:10](file:///opt/ikev2-panel-v2-main/web/templates/login_content.html#L10) `<div class="alert-danger">{{.Error}}</div>`
  - 所有 `alert-danger` / `alert-success` 同
- **问题**:
  - 错误出现时屏幕阅读器不会自动朗读
  - 加 `aria-live="polite"` 后,辅助技术会在内容变化时朗读
- **参考**:[MDN: ARIA Live Regions](https://developer.mozilla.org/en-US/docs/Web/Accessibility/ARIA/Live_regions)
- **修复方案**:
  - CSS 类加 `aria-live` 属性:
    ```html
    <div class="alert-danger" role="alert" aria-live="polite">{{.Error}}</div>
    ```
- **工作量**:0.05d

---

## 不在范围内

故意**不审计**:
- ❌ strongSwan 内部 charon / kernel-libipsec 的可用性(已归 strongswan 审计)
- ❌ acme.sh 工具本身的 UX(归 cert 审计)
- ❌ DDNS API 错误分类与重试(归 alidns 审计)

## 借鉴参考(供方案决策用)

- **Onboarding 设计**:
  - [Authentik: First-time setup wizard](https://goauthentik.io/docs/)
  - [Casdoor: Setup wizard](https://casdoor.org/docs/)
- **a11y / i18n**:
  - [MDN: HTML lang attribute](https://developer.mozilla.org/en-US/docs/Web/HTML/Global_attributes/lang)
  - [go-i18n](https://github.com/nicksnyder/go-i18n)
- **时区**:
  - [Go: LoadLocation](https://pkg.go.dev/time#LoadLocation)
  - [IANA TZ database](https://www.iana.org/time-zones)
- **troubleshooting 文档组织**:
  - [CockroachDB troubleshooting](https://www.cockroachlabs.com/docs/stable/operational-faqs.html)
  - [strongSwan Support Hub](https://docs.strongswan.org/docs/)

---

## 建议优先级

### 本周(W1,与安全/正确性配合)

| 优先级 | ISSUE | 工作量 | 与 web audit 重复 |
|--------|-------|--------|-------------------|
| 🔴 | **U01 image tag 修复** | 0.2d | 否 |
| 🔴 | **U02 默认密码写到持久化文件 + 提示改密** | 0.2d | 部分(W06) |
| 🟡 | U04 / U06 `/healthz` + `/metrics` 升级 | 0.9d | 否 |
| 🟡 | U07 日志路径修复 | 0.1d | 否 |
| 🟡 | U12 `renderInstallError` 友好化 | 0.1d | 否 |
| ⚪ | U13 CSS 类名修复 | 0.01d | 否 |
| ⚪ | U18 aria-live 加属性 | 0.05d | 否 |
| **小计** | | **1.56d** | |

### 下周(W2)

| 优先级 | ISSUE | 工作量 |
|--------|-------|--------|
| 🟡 | U03 / U11 时区统一 + admin 配置项 | 0.3d |
| 🟡 | U05 onboarding checklist | 0.5d |
| 🟡 | U08 文档导航 + TROUBLESHOOTING.md | 0.6d |
| 🟡 | U09 iOS 验证 VPN 连接指引 | 0.6d |
| 🟡 | U10 audit log(与 W10 合并) | (含 W10) |
| **小计** | | **2.0d** |

### 后续(>W2)

| 优先级 | ISSUE | 工作量 |
|--------|-------|--------|
| ⚪ | U14 QR alt 文字 | 0.01d |
| ⚪ | U15 颜色对比度修复 | 0.1d |
| ⚪ | U16 渐进增强二次确认 | 0.1d |
| ⚪ | U17 显式 label for | 0.3d |

---

## 完成定义

- [ ] U01 ~ U04 / U07 / U12 / U13 / U18 修复(本周)
- [ ] U03 / U05 / U08 / U09 / U10 修复(下周)
- [ ] 配合 audit-2026-09-web.md 的 W10 一起做 audit log
- [ ] 用户 onboarding 流程真机验证(从 git clone 到 mobileconfig 装上 ≤ 30 分钟)
- [ ] `/healthz` 返回 JSON + curl 验证
- [ ] WCAG 颜色对比度 + a11y 验证
- [ ] release-notes 记录修复内容

---

## 附录:本报告跟其他审计的关系

| 本报告 issue | 其他审计重复 | 差异 |
|--------------|--------------|------|
| U02 默认密码持久化 | W06 部分(session idle) | 本 issue 重点"初始密码只 stdout" |
| U03 / U11 时区 | 无 | **本审计独家** |
| U04 / U06 healthz / metrics | 无 | **本审计独家** |
| U05 onboarding | 无 | **本审计独家** |
| U07 日志路径 | cert 审计部分 | 本 issue 重点"文档路径不一致" |
| U08 文档导航 | 无 | **本审计独家** |
| U09 iOS 验证指引 | 无 | **本审计独家** |
| U10 audit log | W10 | **本审计强调"即使简单 slog.Info 也行"** |
| U13-U18 UI 一致性 / a11y | web 审计 W 系列(部分) | 本审计强调**用户感知**的 UI 问题 |
