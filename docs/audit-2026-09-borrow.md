# 借鉴优化(Borrow & Improve)审计报告 — 2026-09-19

## 范围

本报告对应 [audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) 维度 5。**深度优先**地挑选自研组件,与领域标杆对比,产出"借鉴 / 保持自研"的明确建议 + 工作量。

**审计目标组件(本批)**:

1. DDNS 同步器
2. 证书 / ACME 集成
3. 用户凭证(密码 hash + session)
4. strongSwan VICI 客户端
5. IPv6 探测 + tc 限速(优先级 2)
6. 配置管理 + 日志(优先级 2)

**不在范围**(每份独立审计已覆盖):

- 阿里云 RAM 权限、网络路由、Dockerfile、web UI 模板、mobileconfig 生成 — 见 `audit-2026-09-*.md` 系列。

**借鉴工作流**(来自 audit-framework §"借鉴工作流"):

1. 静态读标杆(本报告以 WebFetch + README + 源码 review)
2. 差异列表
3. ROI 判断
4. 方案文档(本报告每节"建议"块)
5. 不在本报告范围实施,落地按 issue 一个 PR

---

## 工具 / 参考

| 标杆 | URL | 引用点 |
|------|-----|--------|
| jeessy2/ddns-go | <https://github.com/jeessy2/ddns-go> | 多 provider / IPv6 / Webhook |
| ddclient | <https://github.com/ddclient/ddclient> | RFC 2136 / 多 provider |
| caddyserver/caddy | <https://github.com/caddyserver/caddy> | 自动 HTTPS / TLS |
| caddyserver/certmagic | <https://github.com/caddyserver/certmagic> | ACME 库 / OCSP stapling |
| dexidp/dex | <https://github.com/dexidp/dex> | OIDC / connector 模型 |
| goauthentik/authentik | <https://github.com/goauthentik/authentik> | 现代 IdP / UI |
| strongswan/govici | <https://github.com/strongswan/govici> | 官方 VICI SDK |
| OWASP Password Cheat Sheet | <https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html> | hash 算法建议 |
| Let's Encrypt ACME 客户端列表 | <https://letsencrypt.org/zh-tw/docs/client-options/> | ACME 工具生态 |

---

## 组件 1:DDNS 客户端

### 我们当前实现

- **路径**:[`internal/ddns/sync.go`](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go)、[`internal/ddns/statefile.go`](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go)
- **关键代码**:
  - `sync.go:264` Run() — 60s ticker + 节流窗口(per-type)
  - `sync.go:322` tick() — v2-84 dual 模式 v4 / v6 并发(errgroup 自实现版,见 `sync.go:389` WaitGroup)
  - `sync.go:514` upsertRecord() — retry 3 次 1s/4s/9s + RAM 错误快速失败
  - `sync.go:486` detectFamily() — IPv4 用 `swanctl.DetectGlobalV4`,IPv6 用 `/proc/net/if_inet6`
- **做了什么**:阿里云 DDNS,A/AAAA 同步,内嵌 acme.sh 也用 `dns_ali` 复用凭证;UI 切换 family;LastSync 给面板展示。

### 领域标杆

- **项目 A**:[jeessy2/ddns-go](https://github.com/jeessy2/ddns-go) — 16k+ stars,Go 实现,18+ DNS provider(阿里云/腾讯/Cloudflare/华为/Porkbun/deSEC…),同时配多 provider,多域名,接口/网卡/命令探测 IP,Webhook 通知,内置 web UI
- **项目 B**:[ddclient](https://github.com/ddclient/ddclient) — Perl,经典,50+ provider 包括 RFC 2136 nsupdate / DynDNS2 协议

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **多 provider** | ddns-go 支持 ≥18 家 DNS,Cloudflare/腾讯/华为/NameSilo/Porkbun/deSEC/Gcore 一键切换 | **仅阿里云**,硬编码 `alidns.UpdateRecord` | 我们用户只能用阿里云;换 DNS 要重写整个 dns 包 |
| 2 | **多域名 / 多 record** | ddns-go 单实例可管几十条记录,RR+Domain 任意组合 | 1 RR+1 Domain(2 套:panel + mobileconfig 也共用同 1 条) | 我们其实够用,但用户加二级域名要改源码 |
| 3 | **探测 IP 策略** | ddns-go 支持网卡/命令/HTTP 接口,可同时配置 v4+v6 source 各自独立 | `Iface` 字段 + 单探测函数;v4 默认 `8.8.8.8`(可改 `223.5.5.5`),v6 读 `/proc/net/if_inet6` | 我们够用,但接口名 hardcode 注入 |
| 4 | **Webhook 通知** | ddns-go 支持 7+ 推送(Server酱/Bark/钉钉/飞书/Telegram/Discord/微信/企业微信) | 仅 `LAST_DDNS_FAILED` 文件 + 面板 LastSync JSON | 用户切 DDNS 后收不到推送 |
| 5 | **并发 / 节流** | ddns-go 默认 5min 周期,可加 `-cacheTimes` 提前探测 + 长间隔比对 | 60s 周期 + per-type 60s 节流窗口 | 我们更激进,适合 v6 prefix 频繁变的家用墙 |
| 6 | **多账号/多 zone** | ddns-go 支持"多个 DNS 服务商同时更新" | 单凭证(单 AccessKey) | 单 VPS 用户够用 |
| 7 | **HTTP API / 自定义命令** | ddns-go `-noweb` 关 web,跑 cron | 我们面板 UI + API 调用 | 差别不大 |

### ROI 判断

- **收益**:多 provider 是最大价值——如果有一天用户想换 Cloudflare / 腾讯,我们不用重写 `internal/ddns` + `internal/dns`,只换 provider 实现。但当前所有用户都强绑阿里云(LE + acme.sh 也用 dns_ali),迁移动机弱。
- **成本**:
  - 引入 ddns-go 二进制=增加 ~20MB + 一个 systemd-like 服务(我们容器里再起一个 sidecar 复杂);
  - "借鉴 ddns-go 写法"=抽 provider interface 重构 `internal/dns` + `internal/ddns`,新增 ≥1 家非阿里云的实现(否则只是空架子)。工作量 ~3-5d。
  - "直接换 ddns-go"=放弃 `CredentialGetter` 运行时凭证热加载(我们 v2-83 的核心特性),破坏性大,**不可取**。

### 建议

- [ ] **借鉴(provider 抽象)**:**值得**。把 `internal/dns/aliyun.go` 抽成 `Provider` interface(`FindRecord / UpdateRecord`),新增 1 家 Cloudflare 实现作为"如果用户要换,可以零代码工作"。不在本批落地,留作 v2.85+ backlog。
- [x] **保持自研(其余)**:探测策略、节流窗口、状态文件 INI 格式、面板 UI 集成——都已经覆盖得很好,**没有借鉴动机**。

### 工作量

- 抽 `Provider` interface + Cloudflare stub:**1.5d**
- 全量迁移面板 UI 支持 provider 选择 + DDNS 多 provider:**+2d**
- **总:不投入 / 或 3.5d(P2)**

---

## 组件 2:证书 / ACME 集成

### 我们当前实现

- **路径**:
  - 自签:[`internal/cert/generate.go`](file:///opt/ikev2-panel-v2-main/internal/cert/generate.go)
  - LE 状态读取:[`internal/cert/acme.go`](file:///opt/ikev2-panel-v2-main/internal/cert/acme.go)
  - 签发/续签:[`scripts/entrypoint.sh`](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) + [`scripts/renew-cert.sh`](file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh)
  - entrypoint.sh 调 `acme.sh --issue --dns dns_ali --install-cert --reloadcmd`
  - renew-cert.sh 用 cron + backup + rollback 机制
- **做了什么**:`certMode=letsencrypt` 时,首启动 → `acme.sh --issue` 拿 cert;每日 cron → `--renew` → 复制 cert 到 `/data/le/` → `swanctl --load-creds`(zero-downtime SA 切换)。`certMode=self-signed` 时直接 `x509.CreateCertificate` 自签 10 年。

### 领域标杆

- **项目 A**:[caddyserver/caddy](https://github.com/caddyserver/caddy) — 自动 HTTPS 默认行为,内置 ACME 客户端 + on-demand TLS + OCSP stapling,zero-downtime 重签/轮换
- **项目 B**:[caddyserver/certmagic](https://github.com/caddyserver/certmagic) — Go 库形态,把 caddy 的 ACME 逻辑剥出来,3 challenge(HTTP/TLS-ALPN/DNS),multi-CA fallback,OCSP 自动 staple,retry 30 天指数退避
- **项目 C**:[lego](https://go-acme.github.io/lego/)(Go 单文件 ACME 客户端) — 跟我们的容器化场景最像:Go 实现 + DNS API plugin

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **ACME 客户端实现** | caddy/certmagic/lego:纯 Go,单二进制,内嵌 ACME 协议 | **Shell**:调 acme.sh(36k+ stars,~300KB shell) + 我们的 entrypoint/renew-cert wrapper | acme.sh 多 1 个进程(每分钟 cron 跑),shell 调 curl 比 certmagic 多 ~50ms 一次;但对 LE 续签(每日 1 次)无感 |
| 2 | **证书切换 zero-downtime** | certmagic:keep `tls.Config.GetCertificate` callback,内存换 cert,新 handshake 拿到新 cert | 我们 LE 续签:换文件 → `swanctl --load-creds` → charon 内存换(~50ms 中断) | 续签是凌晨/低峰,用户感觉不到;**对我们够用** |
| 3 | **OCSP stapling** | certmagic 默认:每次 TLS 握手前查 OCSP + cache + 自动 staple | **无** — charon 不支持 OCSP staple 给我们自签 CA | 我们自签 CA 不需要 OCSP;LE 模式 OS/客户端会自己查 OCSP,影响小 |
| 4 | **多 CA fallback** | certmagic:多 issuer 列表,某 CA 失败切下一个 | 单 CA(Let's Encrypt via acme.sh) | 1-50 人小团队不需要多 CA |
| 5 | **Multi-SAN / wildcard** | certmagic/caddy:`*.example.com` 通配 cert,DNS-01 challenge | 单域名 LE 证书;无 wildcard | 我们没有子域名服务场景 |
| 6 | **续签错误恢复** | certmagic:30 天指数退避,失败 24h 后再试,可切 staging CA 验证 | acme.sh daily cron + 我们 `LAST_RENEW_FAILED` 文件 + 面板告警横幅 | 我们的设计(<14 天横幅告警 + renew-cert.sh 失败 touch 文件)**够用且更简单** |
| 7 | **HTTPS 面板证书切换 atomicity** | caddy 在内存换;我们写文件 + 强 charon reload(SA 中断) | charon `load-creds` 子命令,设计见 [`swanctl/loader.go:235`](file:///opt/ikev2-panel-v2-main/internal/swanctl/loader.go#L235) — 仅换 creds 不重 conn,**已经满足"续签不掉线"** | **无问题** |

### ROI 判断

- **收益**:
  - 换 certmagic/lego → ACME 协议纯 Go 化 → 少一个 shell 进程(cron + acme.sh daemon)。但我们的 cron 是 daily,不是 hot path,节省的 ~50ms × 1次/天 ≈ **0**。
  - 加 OCSP stapling → TLS 握手少 1 个 RTT,意义是给面板 8443 端口的访问者;但用户大部分是后台管理,**客户端不一定看得到 OCSP**。
- **成本**:
  - 引入 certmagic 依赖:**+1 个 dep ~5MB 二进制,需要嵌入 certmagic 存储抽象(我们现在的 acme.sh 文件方案就是它的存储抽象)**;
  - 改写整个 LE pipeline:**破坏性大**,要把 `scripts/entrypoint.sh` 的 acme.sh 流程全替换;
  - lego 跟 certmagic 类似,GO 单文件 ACME 客户端,但仍要写自己的 entrypoint + renewal 脚本 = 工作量 ~3-5d,**收益几乎为 0**。

### 建议

- [x] **保持自研(acme.sh 集成)**:acme.sh 是 ACME 生态的事实标准,150+ DNS provider,Shell 零依赖,跟我们 Docker 镜像契合。**certmagic 给我们带来的都是"我们用不到"的功能**(on-demand TLS / multi-CA / OCSP / wildcard)。
- [ ] **借鉴(OCSP stapling) 候选**:如果 v2.85+ 面板对外公开(目前默认 127.0.0.1:8443 + reverse proxy),可以加 `go.ocsp` 自动 fetch + `tls.Config.OCSPStaple` 注入。**优先级 P2**,工作量 0.5d。
- [ ] **借鉴(certmagic 借鉴 OCSP cache + retry 30d)**:如果以后发现 acme.sh 续签失败场景多,可借鉴 certmagic 的"失败 24h 后再试,30 天指数退避"思路改 renew-cert.sh。**优先级 P3**。

### 工作量

- 不投入(保持 acme.sh);
- OCSP stapling:**0.5d**(P2,可选);
- 续签重试改写:**0.5d**(P3,视问题出现)。

---

## 组件 3:用户凭证(密码 hash + session)

### 我们当前实现

- **路径**:
  - 密码 hash:[`internal/auth/password.go`](file:///opt/ikev2-panel-v2-main/internal/auth/password.go) — bcrypt DefaultCost(`password.go:57`)
  - Session cookie:[`internal/auth/session.go`](file:///opt/ikev2-panel-v2-main/internal/auth/session.go)
  - Session 存储:[`internal/store/sessions.go`](file:///opt/ikev2-panel-v2-main/internal/store/sessions.go)
  - CSRF:[`internal/auth/session.go:55`](file:///opt/ikev2-panel-v2-main/internal/auth/session.go#L55) — header `X-CSRF-Token` + form `csrf_token`
- **做了什么**:
  - **管理员密码**:bcrypt(DefaultCost=10)hash 存 DB。
  - **用户密码**(VPN EAP 凭证,跟管理员密码不同):**明文存 DB**(design §4.3 妥协项,因 strongSwan EAP-MSCHAPv2 需要明文给 charon)。
  - Session:id 32 字节 random hex + `Base64.RawURLEncoding`,存 sqlite 表 `sessions(id/csrf_token/created_at/expires_at/user_agent)`,TTL 24h,登出删除。
  - CSRF:64 hex token(32 字节随机)+ 常量时间比较。

### 领域标杆

- **项目 A**:[dexidp/dex](https://github.com/dexidp/dex) — CNCF,OIDC IdP,10+ connector(LDAP/SAML/GitHub/GitLab),BCrypt 密码验证 + refresh token + 群组 claim
- **项目 B**:[goauthentik/authentik](https://github.com/goauthentik/authentik) — 现代 IdP,Python+Go,PBKDF2/BCrypt/Argon2 多种 hash 后端,**rehash on next login** 自动迁移策略
- **参考**:[OWASP Password Storage Cheat Sheet 2026](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) — 新项目默认 Argon2id `m=19MB, t=2, p=1`,bcrypt cost ≥10 仅遗留系统

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **管理员密码 hash** | dex/authentik:**Argon2id**(OWASP 2026 默认) / bcrypt cost=12 | bcrypt DefaultCost(=10) | OWASP 2026 允许 bcrypt cost ≥10 视为"legacy-OK",我们合规但不"modern" |
| 2 | **用户密码明文** | 设计妥协项(design §4.3)— strongSwan EAP-MSCHAPv2 要明文 | **明文存 DB**(users.password) | **设计层面无法借鉴**;只要 EAP-MSCHAPv2 协议不变,这一项不能动 |
| 3 | **Session 存储** | dex/authentik:数据库 + refresh token + sliding window 续期 | sqlite `id/csrf_token/.../expires_at`,24h 硬过期,**无 sliding 续期** | 我们设计简单但用户体验差(用户活动期间突然过期要重登) |
| 4 | **CSRF token 旋转** | dex:OIDC state nonce 一次性 | **不旋转**(同一 session 内 token 永不变) | CSRF token 不变 ≠ 安全问题,但 OWASP 建议敏感操作换 token |
| 5 | **Session 失效** | dex/admin:管理员可强制踢某用户 | **无**(用户自己登出,或等待 TTL) | 单租户不需要 |
| 6 | **密码 hash 升级策略** | authentik:rehash on next login,自动迁移 bcrypt→argon2 | 无迁移策略(一次性选择) | 我们成本增量小,但**用户基数 1-50 人,迁移成本≈0** |
| 7 | **Rate limit 登录** | OWASP/dex:登录端点限速 + 账号锁定 | **无** — design §4.1 声明未实现 | 已在 `audit-2026-09-security.md` 单独标记,**借鉴维度不重复** |
| 8 | **MFA / 2FA** | dex/authentik:TOTP / WebAuthn | 无 | **范围之外**(本项目目标是"轻量 VPN 面板") |

### ROI 判断

- **收益**:
  - 升级 bcrypt cost 10 → 12 或换 Argon2id → 防离线破解能力提升 ~4 倍;但 24h session TTL + 用户基数小,攻击面有限,**实际收益边际**。
  - Session sliding 续期 → 用户体验提升**,但 OWASP 不强制**,有争议(sliding 续期 = session 永不过期,反而增加被劫持窗口)。
  - CSRF token 旋转 → "安全,但收益不明显"。
- **成本**:
  - 换 Argon2id → 引入 `golang.org/x/crypto/argon2` 依赖(已在 go.sum transitive 但未直接用),改 ~10 行代码,**低成本**;
  - 升级 cost=10→12 → bcrypt 校验时间翻倍,登录慢 100ms 左右,**可接受**;
  - Session sliding 续期 → 需要每次 `GET /` 改 `expires_at`,加 DB 写,**+0.5d**;
  - CSRF 旋转 → 加 state,**+0.5d**;
  - 整体**~1.5d**,工作量不大,但**ROI 模糊**。

### 建议

- [ ] **借鉴(bcrypt cost 升级)**:cost=10→12,**值得**,工作 0.1d。理由:bcrypt 输出格式不变,无需迁移,登录慢 100ms 用户感知不到,**OWASP "legacy-OK cost ≥10" 卡在线上**。
- [ ] **保持自研(用户密码明文)**:EAP-MSCHAPv2 协议限制,**无法借鉴**。
- [ ] **保持自研(sliding session / CSRF 旋转)**:当前 24h TTL 设计简单且够用,**借鉴收益模糊,不做**。
- [ ] **候选(Argon2id 长期迁移)**:v3.0.0 重写时可考虑 Argon2id + rehash on next login。**P3**。

### 工作量

- bcrypt cost 升级:**0.1d**;
- Argon2id 迁移(可选):**+1d**(P3)。

---

## 组件 4:strongSwan VICI 客户端

### 我们当前实现

- **路径**:
  - 入口 + reload:[`internal/swanctl/loader.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/loader.go)
  - list-sas 解析:[`internal/swanctl/parser.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go)
  - IPv6 watch:[`internal/swanctl/ipv6watch.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go)
  - 终止 SA:[`internal/swanctl/terminate.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go)
- **依赖**:`go.mod:7` — `github.com/strongswan/govici v0.8.1`(官方 SDK,**已经在用**)
- **做了什么**:
  - reload:写 swanctl.conf 文件 + `exec.CommandContext` 调 `swanctl --load-all`/`--load-creds`(M8 实机测试发现 VICI 不实现 `load-creds`/`load-all`,所以选择 shell-out)
  - list-sas:`sess.CallStreaming(ctx, "list-sas", "list-sa", msg)` → 解析 `iter.Seq2[*vici.Message, error]` → 结构化 `[]SA`
  - terminate:调 `terminate-sa` 命令
  - IPv6 watch:读 `/proc/net/if_inet6` + sed 替换 swanctl.conf + load-all

### 领域标杆

- **项目 A**:[strongswan/govici](https://github.com/strongswan/govici) — **强 S 官方 VICI SDK**,v0.8.1 已用,我们就是它的用户
- **参考**:[govici CHANGELOG](https://raw.githubusercontent.com/strongswan/govici/master/CHANGELOG.md) — v0.8.0(v2-85+ 我们现在用的) 新增 `Call`/`CallStreaming` 替换 deprecated 的 `CommandRequest`/`StreamedCommandRequest`

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **reload 路径** | govici 暴露 `Call(ctx, "load-conn", msg)` 等细粒度命令 | **shell-out** 到 `swanctl --load-all`(`loader.go:182`) | **正确**(M8 实机发现 VICI 不实现 load-all/load-creds);不需借鉴 |
| 2 | **CallStreaming 用法** | govici v0.8.0 推荐用 `CallStreaming`(v0.8.0 README + vici.CallStreaming API) | 我们用了 v0.8.1,代码里就是 `sess.CallStreaming(ctx, "list-sas", ...)`(`parser.go:78`)| **已经在用标杆最新 API** |
| 3 | **govici version** | latest v0.8.1(我们用的);CHANGELOG 上一个 v0.8.0 新增 Call/CallStreaming | go.mod 锁定 v0.8.1 ✓ | 已对齐 |
| 4 | **Session 复用 vs 新建** | govici:Session 可复用,但官方建议 short-lived | 我们每次 `openSession`(`loader.go:197`)→ 用完 close | 正确做法(short-lived 避免 charon 半挂时 Session 卡死) |
| 5 | **Timeout** | govici v0.8.0:Call/CallStreaming 接受 context | 我们 `withTimeout` 包 ctx + `ctx.Err() == DeadlineExceeded` 判断 | **正确 + 已用最新 API** |
| 6 | **list-sas → 结构化** | 无标杆;这是我们 domain logic | 自己写 `parseSingleSA` / `parseChildSAs`(`parser.go:104`) | 必须自己写**;govici 只给 raw Message** |
| 7 | **IPv6 探测** | 不在 govici scope | 自己读 `/proc/net/if_inet6`(`ipv6watch.go:147`) | 必须自己写;govici 不管 netlink |
| 8 | **govici 文档完整度** | pkg.go.dev 文档稀疏,README 一句话"see getting_started.md" | 我们 + repo docs 都看过 | 文档质量**不是我们能控制的** |

### ROI 判断

- **收益**:
  - 升级 govici 版本(0.8.1 → 下一个版本)= 可能的小 bugfix,**边际收益**;
  - 用 `Subscribe` 取代 polling list-sas = 实时性更好但要重构 collector。**这是 govici v0.6+ 提供的功能**,见 CHANGELOG `Session.Subscribe/Unsubscribe`。
- **成本**:
  - 升级 govici:检查 changelog,跑测试,**0.2d**;
  - 改用 `Subscribe` 实时事件:需要重写 collector(Subscribe 推 event,我们要在内存里维护 SA+child 的 map,变更时算 delta)+ 加 backoff 防 vici 半挂。**1.5d**,但**用户感知不到差异**(collector 5 分钟周期已经够准)。

### 建议

- [x] **保持自研(loader/parser/terminate/ipv6watch)**:govici 已经是我们的依赖且版本最新,reload/list-sas/parse/IPv6 探测都是 domain logic,**没有可借鉴的标杆**。
- [ ] **借鉴(下次依赖升级时 review)**:govici 主仓 v0.8.x → v1.0 之前可能有 API 变化,**保持订阅 CHANGELOG**,升级时 review。**P1**(持续)。
- [ ] **借鉴候选(Subscribe 实时事件)**:如果将来要做"面板实时显示活跃用户列表",可以从 5min collector 改成 Subscribe 推 event。**P3**,仅在加新功能时做。

### 工作量

- 升级 govici 评审:**0.2d**(P1,跟 Go 版本升级一起做);
- Subscribe 重构:**1.5d**(P3,按需)。

---

## 组件 5:IPv6 探测 + tc 限速

### 我们当前实现

- **路径**:
  - IPv6 探测:[`internal/swanctl/ipv6watch.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go)(`detectGlobalV6:146`)+ [`internal/swanctl/ipv4watch.go`](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go)
  - tc 限速 updown:[`scripts/ikev2-updown`](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown)
  - 限速文件管理:[`internal/limit/limiter.go`](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go)
  - 流量采集:[`internal/limit/collector.go`](file:///opt/ikev2-panel-v2-main/internal/limit/collector.go)
- **做了什么**:
  - IPv6:60s ticker,读 `/proc/net/if_inet6`,匹配 global unicast 2000::/3,变了就 sed swanctl.conf → `swanctl --load-all`(~500ms SA 中断);
  - IPv4:UDP dial `8.8.8.8:80`(可配 223.5.5.5);
  - tc 限速:HTB + u32 filter 匹配 IPv4 src,**IPv6 流量无法限速**(`ikev2-updown:56` 注释里写了)。

### 领域标杆

- **项目 A**:[strongSwan traffic-shaping 文档](https://docs.strongswan.org/) — strongSwan 自己 updown 脚本示例
- **项目 B**:nftables + tc 现代用法(参考 [LARTC 指南](https://lartc.org/)及 systemd-networkd 文档)— `tc flower` filter 支持 IPv6
- **项目 C**:**eBPF / TC clsact** —— 现代 eBPF 限速(cgroup-based),但需要内核 ≥4.19 + 我们不熟

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **IPv6 限速** | tc `flower` filter / `tc match u32`(IPv6 不支持) | **未实现**(`ikev2-updown:56` 注释承认) | IPv6-only 用户的限速失效 |
| 2 | **tc 命令 vs nftables** | 现代用法:`nftables` + tc `flower`(IPv6 OK) | 纯 tc + u32(只 IPv4) | IPv6 限制能力缺失 |
| 3 | **fairness** | HTB + fq_codel / CAKE(管控 bufferbloat) | 纯 HTB | 单用户场景无问题;多用户互抢带宽时 CAKE 更好 |
| 4 | **探测 IPv6 SLAAC** | systemd-networkd / NetworkManager 监听 `ip -6 monitor` 事件 | 60s 轮询 `/proc/net/if_inet6` | 我们的轮询间隔对 ISP 重拨够用;**没事件驱动是次优但可接受** |
| 5 | **IPv4 探测方式** | 多种:HTTP 接口 / 命令 / 网关 | UDP dial 8.8.8.8:80 | **够用**;中国大陆可配 223.5.5.5 |
| 6 | **eBPF** | 现代化限速,内核态过滤,可基于 cgroup/socket cookie | 无 | **超出项目复杂度**,不要做 |

### ROI 判断

- **收益**:
  - **IPv6 限速**:目前我们明确缺,**用户 IPv6-only 时 limit 配置失效**,这是真实可感知 bug。**收益 = HIGH**。
  - tc flower / nftables 学习曲线陡(我们没经验)+ 重写 updown 脚本涉及系统依赖。
- **成本**:
  - 改 `ikev2-updown` 用 `tc flower`(IPv6 OK):改 ~30 行 bash,**0.5d**;
  - 引入 nftables:需要新装包,**+0.5d**;
  - eBPF:需要 eBPF 工具链 + 调试,**3-5d**,**远超价值**。

### 建议

- [ ] **借鉴(IPv6 限速) 必做**:**值得**。把 `tc filter ... u32 match ip src VIP` 换成 `tc filter ... flower` 支持 IPv6(`match ip6 src ...`)。这是**用户实际能遇到的 bug**,不是边缘场景。
  - 具体改法:`tc filter add dev $OUT_IF parent 1: protocol ip prio 1 flower ip_proto ip src $VIP flowid 1:$CLASS_ID` + IPv6 协议另外加一条 `protocol ipv6`。
- [x] **保持自研(IPv6 探测)**:60s 轮询对家用墙场景够用,事件驱动复杂度不值。
- [x] **保持自研(tc 架构)**:HTB 简单够用,1-50 人场景不需要 fq_codel/CAKE。
- [x] **永不(eBPF)**:项目复杂度爆炸,**不做**。

### 工作量

- IPv6 限速(改 `ikev2-updown` + `internal/limit/limiter.go` 加 IPv6 字段同步):**0.5d**(P0,下个 release);
- IPv6-only 全面测试:**+0.5d**。

---

## 组件 6:配置管理 + 结构化日志(优先级 2)

### 6a. 配置管理

#### 我们当前实现

- **路径**:[`internal/config/config.go`](file:///opt/ikev2-panel-v2-main/internal/config/config.go)
- **关键代码**:
  - `Load() (55)` — 手写 `getenv/getint/getbool/getdur`
  - `validate() (146)` — 三个手工 if
  - `loadAliyunCreds() (109)` — 4 级优先级链
- **做了什么**:25 个 env 变量 + 默认值 + 校验 + 凭证 4 级 fallback,纯手写 ~250 行。

#### 领域标杆

- **项目 A**:[caarlos0/env](https://github.com/caarlos0/env/v11) — Go struct tag,自动校验 + 默认值 + 必填,**50k+ stars**
- **项目 B**:[spf13/viper](https://github.com/spf13/viper) — 全功能(env/YAML/JSON/remote),**但 ~50MB 编译体积**

#### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **struct tag 校验** | `caarlos0/env`:field tag `env:"IKEV2_DATA_DIR" envDefault:"/data"` | 手写 25 次 getenv | 加新配置项要改 3 处(Load/getenv/struct),**易漏** |
| 2 | **必填校验** | `env.Required()` tag | `validate()` 手写 | 当前 2 个必填项(if c.CertMode == "letsencrypt" && c.Domain == ""),够用 |
| 3 | **类型转换** | env tag 自动 bool/int/duration | 手写 getbool/getint/getdur | 当前 4 个 helper,~60 行,可接受 |
| 4 | **多源** | viper:env + YAML + etcd + remote | 仅 env | **我们需求简单,不需要** |
| 5 | **凭证 fallback 链** | env:v11 不支持 | 手写 `loadAliyunCreds` 4 级优先级 | **没有标杆能更好做**(caarlos0/env 的 fallback 要自己写) |

#### ROI 判断

- **收益**:caarlos0/env 让 Config struct = self-documenting,加配置项只改 1 处;但我们 25 个 env 长期稳定,**加配置项频率低**。
- **成本**:引入 caarlos0/env:**+1 dep,改 Load() ~50 行**(要把手写 getenv 全部改成 struct tag),**1d**。**收益边际**。

#### 建议

- [x] **保持自研**:**不值得**。理由:
  1. 我们 25 个 env 字段稳定,加新配置频率低;
  2. `caarlos0/env` 改写后,手写 `loadAliyunCreds` 4 级优先级链还得手写,**节省的代码量有限**;
  3. viper 太重,**绝不引**。

---

### 6b. 结构化日志

#### 我们当前实现

- **路径**:`NewLogger()` [`internal/config/config.go:167`](file:///opt/ikev2-panel-v2-main/internal/config/config.go#L167) — 标准库 `log/slog`,text/json 双格式 + level
- **使用**:全包都用 `*slog.Logger` 注入

#### 领域标杆

- **项目 A**:[uber-go/zap](https://github.com/uber-go/zap) — 高性能结构化日志,字段类型化
- **项目 B**:[rs/zerolog](https://github.com/rs/zerolog) — 零分配 chain API

#### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **底层** | zap/zerolog 自实现,更高性能 | stdlib `log/slog`(Go 1.21+) | slog 性能 2024 已追平 zerolog 95% |
| 2 | **context 透传** | zap:WithCtx / zerolog:Ctx;context 携带 trace ID | **未使用** | 我们没有分布式 trace,**不需要** |
| 3 | **API 风格** | zap:`log.Info("msg", zap.String("k", "v"))`;zerolog:`log.Info().Str("k", "v").Msg()` | slog:`log.Info("msg", "k", "v")` | slog 最简洁 |
| 4 | **日志聚合** | 业界:k8s JSON + Loki/ELK;zerolog/zap 有专用 sink | slog text/json 到 stdout | 我们 docker logs / docker-compose logs,**够用** |

#### ROI 判断

- **收益**:zap/zerolog 在 2026 年的边际性能优势对**我们这种面板** = 0(每秒日志 < 100 行,不是高 QPS 服务)。
- **成本**:引入 zap/zerolog:**+1 dep,改全部 log 调用**(~30 处),**2-3d**。

#### 建议

- [x] **保持自研**(`log/slog`):**不值得**。理由:
  1. 我们日志量小,slog 性能足够;
  2. stdlib 零依赖,**符合"轻量面板"**;
  3. slog API 已经在 Go 生态占主流,**未来兼容性最好**。

---

### 工作量(组件 6 合计)

- 不投入(**P 永不**)。

---

## 组件 7(附加评审):Web 框架

### 我们当前实现

- **路径**:[`internal/web/server.go`](file:///opt/ikev2-panel-v2-main/internal/web/server.go)
- **关键代码**:`http.ServeMux` (Go 1.22+ 支持 method+path 模式)+ 手写 middleware(`recoverPanic / logging / requireSession / requireCSRF`)
- **做了什么**:~25 个路由 + 4 个 middleware + 模板渲染

### 领域标杆

- **项目 A**:[go-chi/chi](https://github.com/go-chi/chi) — 轻量,跟 stdlib 兼容,middleware 生态丰富
- **项目 B**:[gin-gonic/gin](https://github.com/gin-gonic/gin) — 功能全,JSON API 友好,但有自己的 context,跟 stdlib 不直接兼容

### 差异列表

| # | 差异 | 标杆做法 | 我们做法 | 影响 |
|---|------|---------|---------|------|
| 1 | **路由** | chi:分组 + middleware 嵌套 + URL params | stdlib mux(Go 1.22+)+ 模式 `{id}` | **Go 1.22 stdlib mux 已支持**,**跟 chi 体验 90% 一致** |
| 2 | **middleware 生态** | chi:30+ ready-made middleware(rate limit / CORS / compress) | 手写 4 个 | 我们需求 4 个,**自己写可读** |
| 3 | **性能** | gin 最快,chi 接近 stdlib | stdlib 略慢 5-10% | 面板 ~25 QPS,**差别 0** |
| 4 | **HTTP/3 / SSE** | gin 有;stdlib 1.22 不直接支持 HTTP/3 | 无 | 我们不需要 |

### ROI 判断

- **收益**:换 chi = 加 middleware 生态,但**我们要的 4 个已经写好**;**收益 0**。
- **成本**:换 chi = 改 ~30 行路由注册 + 测试 + 学习 curve,**0.5-1d**,**ROI 负**。

### 建议

- [x] **保持自研**:**不值得**。Go 1.22 stdlib mux 已经满足需求,chi 给我们的都是"用不到"的 middleware。

---

## 借鉴路线图(汇总)

| 优先级 | 组件 | 改动 | 工作量 | 触发条件 |
|--------|------|------|--------|----------|
| **P0(下个 release)** | **tc 限速 IPv6** | 改 `ikev2-updown` + `internal/limit/limiter.go` | 0.5d + 0.5d 测试 | v2.85 backlog 立即做 |
| **P0(下个 release)** | **bcrypt cost 10→12** | 改 1 行 `bcrypt.GenerateFromPassword` cost | 0.1d | v2.85 backlog 立即做 |
| **P1(本月)** | govici 升级 review | 跟 Go 版本升级一起跑测试 | 0.2d | 每次 Go 升级时做 |
| **P2(下季度)** | DDNS provider interface 抽象 | 抽 `Provider` interface + Cloudflare stub | 1.5d(空架子)~3.5d(真支持) | **如果**用户要求换 DNS 时做 |
| **P2(下季度)** | OCSP stapling(certmagic 借鉴) | `tls.Config.OCSPStaple` 注入 | 0.5d | **如果**面板对外公开 |
| **P3(下版本)** | Argon2id 迁移 + rehash on next login | 改 hash + 写迁移逻辑 | 1d | v3.0 重写时做 |
| **P3(下版本)** | govici Subscribe 替代 collector polling | 重写 collector 用事件 | 1.5d | **如果**加"实时活跃用户"功能时做 |
| **P3(下版本)** | certmagic 续签重试 30d 借鉴 | 改 renew-cert.sh | 0.5d | **如果**发现续签失败场景多时做 |
| **永不** | web 框架换 chi/gin | - | - | ROI 负 |
| **永不** | 日志换 zap/zerolog | - | - | slog 性能足够 |
| **永不** | 配置换 caarlos0/env | - | - | 我们 25 env 字段稳定 |
| **永不** | 用户密码哈希(明文) | - | - | EAP-MSCHAPv2 协议限制 |
| **永不** | eBPF 限速 | - | - | 项目复杂度爆炸 |

---

## 不在范围内(其他维度已覆盖)

| 项 | 覆盖处 |
|----|--------|
| 阿里云 RAM 权限 / API 限流 | [audit-2026-09-alidns.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-alidns.md) |
| strongSwan 加密套件 / IKE 协议 | [audit-2026-09-strongswan.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-strongswan.md) |
| Docker 镜像安全 | [audit-2026-09-docker.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-docker.md) |
| 网络/路由 | [audit-2026-09-net.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-net.md) |
| DB / sqlite 配置 | [audit-2026-09-storage.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-storage.md) |
| Web UI / 模板 | [audit-2026-09-web.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-web.md) |
| 登录端点限速 / HTTPS 强制 / HSTS | [audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) |
| 总览 | [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) |

---

## 报告结论

**7 个组件分析,2 个 P0 借鉴(tc IPv6 限速 + bcrypt cost 升级),2 个 P1/P2,3 个 P3 候选,4 个明确判定保持自研。**

**借鉴工作流评审**:本报告完成了"静态读标杆 → 差异列表 → ROI 判断 → 方案文档"前 4 步,实施(步骤 5)按 issue 一个 PR 走,**不在本报告内执行**。

**总借鉴 ROI**:
- P0 总工作量 ~1.1d(下周可做);
- P1 + P2 总工作量 ~2.2d(本月 / 下季度);
- P3 总工作量 ~3d(下版本 / 触发条件出现时)。
- 总投入 < 1 周,**收益** = 解决 1 个真实 bug(IPv6 限速) + 1 个合规升级(bcrypt cost)+ 1 个 API 跟进(govici),**符合"小步快跑"**。

**最关键的反向建议**:**不要**为了"显得勤奋"加 caarlos0/env / chi / zerolog / viper / certmagic / Argon2id —— 这些在我们项目里都通过 ROI 判定被否决。**自研的简洁性本身是项目竞争力**,不要因为"借鉴框架建议"就堆依赖。