# IKEv2 VPN 面板 v2 · 架构文档

本文档描述**代码当前状态**，不是设计意图。若发现与代码不符，**以代码为准并顺手改本文档**。
版本历史只在 [CHANGELOG.md](CHANGELOG.md) 里维护，不要往这里加"某版本改成了什么"。

配套文档：[API.md](API.md)（49 条端点）· [TROUBLESHOOTING.md](TROUBLESHOOTING.md)（排障）·
[CHANGELOG.md](CHANGELOG.md)（版本变更）· [README.md](README.md)（文档索引）

---

## 0. 一句话总结

**这是一个 Go 单进程 + strongSwan 官方镜像的 Docker Compose 项目。**

Go 进程负责：
1. 跑一个 HTTPS web 服务器（管理面板）
2. 写 swanctl 子配置文件（每用户一个）
3. 调用 `swanctl --load-all` 让配置生效
4. 解析 `swanctl --list-sas` JSON 显示在线用户

strongSwan 负责：
- 所有 IKEv2/IPsec 协议处理

Go 进程不直接参与 VPN 数据通路，只做配置管理。

---

## 1. 项目结构

以下是**当前实际文件树**（不含 `_test.go`；测试另有 14295 行）。
行数为 `wc -l` 实测，非估算。

```
ikev2-panel-v2/
├── cmd/                                  # 可执行入口（2 个）
│   ├── ikev2-panel/                      947 行
│   │   ├── main.go                       # 解析 flag、wire 各模块、起 HTTP serve、起后台 goroutine
│   │   └── mcaddr_test.go
│   └── dnsq/                             147 行
│       └── main.go                       # 排查工具：直连阿里云 DNS API 查记录（见下方说明）
│
├── internal/                             # 16 个包，共 ~15.3k 行
│   ├── web/                5562 行       # HTTP 层：路由、中间件、handler、模板加载
│   │   ├── server.go                      # 49 条路由注册 + 依赖装配
│   │   ├── secure_headers.go              # CSP / X-Frame-Options 等响应头
│   │   ├── context.go                     # 请求上下文取值（当前管理员、flash）
│   │   ├── flash.go                       # 一次性提示消息
│   │   ├── user_pipeline.go               # 用户增删改的事务化流水线（审查 C1）
│   │   ├── audit_helper.go                # 审计日志写入的统一入口
│   │   ├── templates.go                   # html/template 解析与 embed
│   │   └── handlers_*.go                  # 按资源域拆分，见 API.md §1
│   ├── swanctl/            1785 行       # 与 strongSwan 的唯一边界：CLI 下发 + VICI 查询
│   │   ├── writer.go                      # 用户子配置（swanctl.conf.d/）生成
│   │   ├── loader.go                      # 配置下发：--load-conn ikev2-rw / --load-all
│   │   ├── parser.go                      # VICI list-sas streaming 解析
│   │   ├── terminate.go                   # VICI terminate-IKE / -all
│   │   ├── listener.go                    # VICI event 订阅（IKE SA lifecycle 审计）
│   │   ├── ipv4watch.go / ipv6watch.go    # 公网地址变化感知 → 触发重载
│   │   └── atomic.go                      # 已转调 fsutil，保留薄壳兼容
│   ├── panelstate/         1441 行       # 面板可改 + 重启后需保留的状态（统一读写一个目录）
│   │   ├── state.go                       # 目录定位、通用读写、mode 0600
│   │   ├── cert.go                        # 面板 TLS / ACME 账号等证书配置
│   │   ├── subnet.go                      # 客户端 IPv4 pool + IPv6 ULA pool
│   │   ├── ddns.go                        # DDNS 同步配置
│   │   └── mobileconfig_default.go        # 管理员全局 mobileconfig 默认值
│   ├── ddns/               1419 行       # 公网 IP → DNS A/AAAA 记录同步
│   │   ├── sync.go                        # 同步主循环、节流、结果分类
│   │   └── statefile.go                   # 上次同步结果落盘（供面板展示/避免抖动）
│   ├── cert/               1074 行       # 证书生成与安装配置渲染
│   │   ├── generate.go                    # 自签模式：CA + server cert + 面板 cert
│   │   ├── acme.go                        # LE 模式：解析续签失败标志 LAST_RENEW_FAILED
│   │   └── mobileconfig.go                # .mobileconfig XML 渲染
│   ├── store/               773 行       # SQLite 持久层
│   │   ├── store.go                       # 打开、迁移、连接池（SetMaxOpenConns(8)）
│   │   ├── users.go / admins.go / sessions.go / audit.go
│   │   └── types.go                       # 各表行结构
│   ├── auth/                575 行       # 口令、session、登录限速
│   │   ├── password.go                    # 随机口令生成 + bcrypt
│   │   ├── session.go                     # cookie session + CSRF token
│   │   └── ratelimit.go                   # 登录尝试限速（内存 + 清理）
│   ├── dns/                 512 行       # 阿里云 DNS API 客户端
│   │   ├── aliyun.go                      # 签名、Describe/DescribeSubDomainRecords
│   │   └── errors.go                      # 错误分类（决定 DDNS 该不该重试）
│   ├── limit/               415 行       # 客户端限速
│   │   ├── limiter.go                     # tc/nft 规则下发
│   │   └── collector.go                   # SA 流量累计 → 数据库
│   ├── publicip/            311 行       # 探测本机对外公网地址
│   ├── metrics/             277 行       # Prometheus exposition（手写，不用 client_golang）
│   ├── config/              268 行       # 环境变量解析（只认 env，不 import panelstate）
│   ├── runtime/             125 行       # 配置合并层：config 的 env ⊕ panelstate 的文件
│   ├── installtoken/        120 行       # 一次性安装 token（扫码 → /install/{token}）
│   ├── expiry/               93 行       # 定时扫描过期用户 → 禁用 + 强制下线
│   └── fsutil/               90 行       # 原子写（临时文件 + rename）
│
├── web/                                   # 静态与模板资源
│   ├── templates/                         # 9 个 *_content.html + 1 个 layout.html
│   └── static/
│       ├── style.css                      # 面板样式
│       ├── css/pico.min.css               # 第三方基础样式
│       └── js/topology.js                 # 拓扑图渲染
│
├── configs/                               # 挂进容器的强Swan 侧配置
│   ├── swanctl-ipv6-only.conf             # 默认主配置（IPv6-only）
│   ├── strongswan-filelog.conf            # charon 落 /var/log/charon.log（否则日志进黑洞）
│   └── logrotate-ikev2-charon             # 7 天轮转 + gzip
│
├── scripts/                               # 8 个
│   ├── entrypoint.sh                      # 容器入口：证书 → charon → Go
│   ├── up.sh                              # 交互式安装引导
│   ├── backup-restore.sh                  # backup / backup-data / restore <tarball>
│   ├── renew-cert.sh                      # LE 续签（backup + renew + rollback）
│   ├── audit-retention.sh                 # 审计日志保留期清理
│   ├── ikev2-reload.sh                    # 配置重载
│   ├── ikev2-updown                       # strongSwan updown 脚本
│   └── ikev2.nft.template                 # nftables 限速模板
│
├── Dockerfile                            # 单 stage
├── docker-compose.yml                    # 单 service
├── Makefile                              # build/test/race/vet/fmt/dev/docker-*
├── .env.example
├── go.mod / go.sum
├── docs/                                 # 见 docs/README.md 索引
└── README.md
```

**规模**：非测试代码 15934 行（Go）+ 约 3k 行 shell/conf；测试代码 14295 行。

### 为什么有 `cmd/dnsq`

排查 DNS 问题时会踩一个坑：`dig` 查不到记录，**不能**推出"记录不存在" —— 本地
递归 DNS 可能只是没缓存/被上游拦。必须用权威 API 直接问阿里云。

真机上因此误判过一次——把「DNS 记录不存在 → DDNS 没生效 → LE 与 DDNS 循环依赖死锁」
三条结论全下错了，而同一时刻阿里云 API 上记录一直好着（见
[TROUBLESHOOTING.md「排查方法论」](TROUBLESHOOTING.md#排查方法论)）。
`cmd/dnsq` 就是这条方法论的可执行版本：

```bash
docker compose exec ikev2-panel /usr/local/bin/dnsq <domain> <rr>
```

它与面板读同一份凭证（`/data/panel-state/aliyun.creds`），因此结果与面板所见一致。

---

## 2. 模块依赖图

```
                        ┌──────────────────────┐
                        │  cmd/ikev2-panel     │
                        │  main.go（只 wire）   │
                        └───────────┬──────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              │                     │                     │
              ▼                     ▼                     ▼
      ┌──────────────┐     ┌──────────────┐      ┌──────────────┐
      │   config     │     │    web       │      │    cert      │
      │  （只读 env） │     │  49 条路由    │      │ 证书/mobile-  │
      │              │     │  中间件链      │      │   config     │
      └──────┬───────┘     └───┬───┬───┬──┘      └──────┬───────┘
             │                 │   │   │                │
             │            ┌────┘   │   └────┐           │
             ▼            ▼        ▼        ▼           │
      ┌─────────────┐ ┌──────┐ ┌───────┐ ┌────────┐      │
      │  runtime    │ │ auth │ │store  │ │swanctl │      │
      │  合并 env ⊕ │ │      │ │       │ │        │      │
      │  panelstate │ └──┬───┘ └───┬───┘ └───┬────┘      │
      └──────┬──────┘    │         │         │           │
             │           │         │         │           │
             ▼           │         │         │           │
      ┌─────────────┐    │         │         │           │
      │ panelstate  │    │         │         │           │
      │ cert/ddns/  │    │         │         │           │
      │ subnet.conf │    │         │         │           │
      └─────────────┘    │         │         │           │
                        │         │         │           │
      ┌─────────────────┴─────────┴─────────┴───────────┴──┐
      │                    后台 goroutine                    │
      │  expiry（60s）  limit/collector（5min）  ddns（周期） │
      │  swanctl/ipv4watch + ipv6watch（60s）              │
      └───────────────────────┬─────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
        ┌──────────┐   ┌────────────┐   ┌────────────┐
        │  publicip│   │    dns     │   │   limit    │
        │          │──►│ 阿里云 API │   │  tc/nft    │
        └──────────┘   └─────┬──────┘   └────────────┘
                             │
                        ┌────▼─────┐
                        │   ddns   │
                        └──────────┘

  独立工具：cmd/dnsq（直连 dns 包，不经面板）
  横切工具：fsutil（原子写）、metrics、installtoken
```

**依赖规则（硬约束）**：

| 规则 | 为什么 |
|------|-------|
| `cmd/` 只 wire，不含业务逻辑 | 便于换入口（如 `dnsq`）复用同一批包 |
| `config` **不 import** `panelstate` | 否则形成 `config → panelstate → config` 循环；合并交给 `runtime` |
| `store` 只依赖 `database/sql` + `modernc.org/sqlite` | 不掺业务 |
| `swanctl` 只依赖 `os/exec` + `govici` | 与 strongSwan 的唯一边界（见 [§4.4](#44-swanctlmanager与-strongswan-的唯一边界)） |
| `cert` 只依赖标准库 + `crypto/x509` | 证书逻辑不该知道 HTTP |
| `web` 通过注入拿 `store`/`swanctl`/`cert`，**不直接 new** | 可测试性 |
| **没有循环依赖** | `go build ./...` 可作为 CI 门禁 |

⚠️ 依赖方向是**单向**的：后台 goroutine 所在的包（`expiry` / `limit` / `ddns`）
只被 `cmd/main.go` 拉起，不被 `web` 反向依赖。

## 3. 数据流

### 3.1 启动数据流（v2-79 含 §0 自动探测链）

```
entrypoint.sh                                    # 容器入口（PID 1）
  │
  ├─► §0 自动探测链（v2-79 设计）
  │     ├─► §0.1 detect_out_if()        # 网卡：env > ip -4 route default > ip -6 > 首个非 lo UP（并 export IKEV2_OUT_IF）
  │     ├─► §0.2 detect_public_ipv6()   # IPv6：env > ip -6 addr show (2000::/3 且 DAD 已收敛)
  │     ├─► §0.3 detect_public_ipv4()   # IPv4：env > OUT_IF 上非 RFC1918 首个（仅 IPv6_ONLY=false）
  │     ├─► §0.4 detect_v4_gateway()    # IPv4 GW：ip -4 route default
  │     ├─► §0.5 detect_lan_subnets()   # LAN 段：ip -4 addr 聚合 /24
  │     └─► §0.6 detect_vpn_subnet()    # VPN 子网：从候选（10.10/13/17/42/66.0.0/24）选不冲突
  │
  ├─► §0.0 网络模式归一化 + 校验（v2.86-PR20，失败 FATAL 退出 12）
  │     └─► source network-mode.sh 规格表 → net_mode_normalize / net_mode_validate
  │
  ├─► §1 IPv6 公网地址硬校验（IPv6_ONLY=true 时）
  │     └─► 还没探测到 → FATAL 退出 10
  │           （L2 模式下 6 次 × 3s 重试，等 RA 周期 + DAD 完成）
  │
  ├─► §3 IP forwarding 校验（三层兜底：L1 compose sysctls → L2 自愈 → L3 FATAL）
  │     ├─► L1 docker-compose.yml sysctls: 让 docker daemon 起容器前改宿主内核
  │     ├─► L2 entrypoint §3 enable_forwarding() 自愈：echo 1 到 /proc/sys
  │     │     └─► v2.86-PR19 实测：容器内 /proc/sys 是 ro，此步必然失败
  │     └─► L3 全失败 = FATAL 退出 11（提示宿主 sysctl -w；勿再提示 CAP_SYS_ADMIN）
  │           都没开 = FATAL 退出 11
  │
  ├─► §3.2 SLAAC 双 sysctl 运行时回读复核（v2.86-PR20，仅 L2 模式）
  │     └─► disable_ipv6≠0 或 accept_ra≠2 → FATAL 退出 10
  │
  ├─► §3.5 修出向路由 + 转发放行 + NAT（v2-78 + v2-79 + v2.86-PR19(A12)）
  │     ├─► table 220 default via $V4_GW dev $OUT_IF
  │     ├─► nft 自定义表 ikev2 / ikev26（scripts/ikev2.nft.template）
  │     │     ├─► postrouting: MASQUERADE（$IKEV2_VPN_SUBNET → $OUT_IF + fib !local）
  │     │     ├─► mangle_forward / mangle_output: TCPMSS clamp
  │     │     └─► forward: iifname/oifname ipsec0 accept（见下方⚠️）
  │     └─► **DOCKER-USER: iptables -I ... -j ACCEPT（真正放行，见下方 ⚠️⚠️）**
  │
  ├─► §4 MASQUERADE（v2.86-PR12.16起已并入 §3.5 的 nft 自定义表）
  │
  ├─► §5 占位符替换 swanctl.conf
  │     ├─► %IKEV2_LOCAL_ADDRS_DIRECTIVE%   ←  $IKEV2_SERVER_ADDR_V6（v6 → v4 → 留空）
  │     ├─► %IKEV2_LOCAL_TS%                ←  ::/0 或 0.0.0.0/0, ::/0
  │     ├─► %IKEV2_VPN_SUBNET%              ←  $IKEV2_VPN_SUBNET（v2-79 新增）
  │     ├─► %IKEV2_SERVER_CN%               ←  LE 模式下强制取 $IKEV2_DOMAIN（v2.86-PR19）
  │     └─► %IKEV2_SERVER_CERT_FILE%        ←  自签: server.cert.pem / LE: $DOMAIN.pem
  │
  ├─► §6 启动 charon（strongSwan IKE 守护）
  │     └─► ipsec start --nofork >/var/log/charon.log 2>&1 &
  │
  ├─► §7 加载 swanctl 连接配置
  │     └─► swanctl --load-all（多次重试，最多 10 秒）
  │
  ├─► §7.5 LE 模式证书准备（IKEV2_CERT_MODE=letsencrypt）
  │     ├─► 注入 Ali_Key / Ali_Secret 到 acme.sh account.conf
  │     ├─► 首次签发：acme.sh --issue --dns dns_ali -d $DOMAIN --keylength 2048
  │     ├─► 安装证书：acme.sh --install-cert → /data/le/fullchain.pem + privkey.pem
  │     ├─► 复制到 /etc/swanctl/x509/$DOMAIN.pem + private/$DOMAIN.key
  │     ├─► 注册 cron 任务：/etc/cron.d/ikev2-le-renew（每天 03:30 跑 renew-cert.sh）
  │     └─► 启动 cron 守护
  │
  ├─► §7.6 证书装好后再 load 一次 swanctl
  │
  └─► §8 exec /usr/local/bin/ikev2-panel             # Go 进程接管 PID 1

ikev2-panel (main.go)
  │
  ├─► config.Load()                    # 读环境变量（v2-79：所有网络参数都可选）
  │
  ├─► store.Open(dbPath)               # 打开 SQLite
  │     │
  │     └─► store.Migrate()            # 跑迁移（建表）
  │
  ├─► cert.EnsureServerCert(dataDir)   # 根据 IKEV2_CERT_MODE 选模式
  │     │
  │     ├─► if mode == self-signed:
  │     │     ├─► Ensure CA 存在（缺失则生成）
  │     │     ├─► Ensure ServerCert 存在（缺失则生成）
  │     │     └─► Ensure PanelCert 存在（缺失则生成）
  │     │
  │     └─► if mode == letsencrypt:
  │           ├─► 检 LE 证书是否存在
  │           ├─► 不存在 → 调用 ACME HTTP-01 申请
  │           └─► 存在但 < 30 天 → 续期
  │
  ├─► store.EnsureDefaultAdmin()       # 检测默认管理员，不存在则创建并返回随机密码
  │
  ├─► swanctl.ReloadAll()              # 首次配置加载（VICI）
  │
  ├─► go limit.Collector.Run(ctx)      # 启动流量采集后台 goroutine
  │
  ├─► go expiry.Checker.Run(ctx)       # 启动过期检查后台 goroutine
  │
  ├─► go le.CheckLERenewStatus.Run(ctx)  # LE 模式才有：续签健康检查
  │
  ├─► go ipv6watch.Run(ctx.Run(ctx)      # IPv6-only 模式才有：IPv6 前缀变化感知
  │     └─► 每 60s 扫 /proc/net/if_inet6，变化 → sed + swanctl --load-all
  │
  ├─► web.NewServer(deps)             # 创建 HTTP 服务
  │
  └─► http.ListenAndServeTLS(...)      # 开始监听
```

### 3.2 用户登录数据流

```
浏览器 POST /login {username, password, csrf_token}
       │
       ▼
web/middleware.authRequired(false)      # 允许未登录访问
       │
       ▼
web/handlers_auth.handleLogin
       │
       ├─► auth.VerifyCSRF(session, csrf_token)  # CSRF 校验
       │
       ├─► store.GetAdminByUsername(username)
       │     │
       │     └─► SELECT * FROM admins WHERE username = ?
       │
       ├─► auth.VerifyPassword(admin.password_hash, password)
       │     │
       │     └─► bcrypt.CompareHashAndPassword
       │
       ├─► store.CreateSession(admin_id)
       │     │
       │     └─► INSERT INTO sessions VALUES (...)
       │
       └─► http.SetCookie(session_id, secure, httpOnly)
              │
              └─► 302 / 
```

### 3.3 新增用户数据流

```
浏览器 POST /users {username, note, speed_limit_mbps, expires_at, enabled, csrf_token}
       │
       ▼
web/middleware.authRequired(true)      # 强制登录
web/middleware.csrf()                 # CSRF 校验
       │
       ▼
web/handlers_users.handleCreate
       │
       ├─► validate.Username(username)         # 校验格式
       │
       ├─► auth.GeneratePassword(12)            # 生成 12 位随机密码
       │
       ├─► BEGIN TRANSACTION
       │     │
       │     ├─► store.CreateUser(user, password, enabled, note, speed_limit_mbps, expires_at)
       │     │
       │     ├─► swanctl.WriteUserConf(username, password)
       │     │     │
       │     │     └─► 写 /etc/swanctl/conf.d/<username>.conf
       │     │
       │     └─► limit.WriteLimitFile(username, speed_limit_mbps)
       │           │
       │           └─► 写 /var/lib/ikev2-panel/limits/<username>
       │
       ├─► COMMIT TRANSACTION
       │
       ├─► swanctl.ReloadAll()
       │     │
       │     └─► exec.Command("swanctl", "--load-all")
       │
       └─► 302 /users/<id>  + 一次性显示密码
```

**事务边界**：数据库插入 + 文件写入必须在同一逻辑事务里。
- 如果 DB 写成功但文件写失败：回滚 DB INSERT
- 如果 DB 写成功 + 文件写成功但 `swanctl --load-all` 失败：删除刚写的文件 + 删除限速文件 + 回滚 DB

### 3.4 删除用户数据流

```
浏览器 POST /users/<id>/delete {csrf_token}
       │
       ▼
middleware × 2
       │
       ▼
handlers_users.handleDelete
       │
       ├─► store.GetUser(id)
       │
       ├─► BEGIN
       │     ├─► store.DeleteUser(id)
       │     └─► swanctl.RemoveUserConf(username)
       │
       ├─► COMMIT
       │
       ├─► swanctl.ReloadAll()
       │
       └─► 302 /users
```

### 3.5 查询当前 SAs 数据流

```
浏览器 GET /
       │
       ▼
middleware.authRequired(true)
       │
       ▼
handlers_home.handleHome
       │
       ├─► swanctl.ListSAs()
       │     │
       │     └─► exec.Command("swanctl", "--list-sas", "--json")
       │           │
       │           └─► 解析 JSON → []SA
       │
       ├─► store.CountUsers()          # SELECT COUNT(*) FROM users
       │
       ├─► store.CountActiveSAs()      # 在 swanctl 输出里数已建立 IKE_SA 的用户
       │
       └─► render "home.html" {sas, total_users, active_sas}
```

### 3.6 流量采集数据流（后台 goroutine，每 5 分钟）

```
limit.collector.Run()  // 后台启动的 ticker
       │
       ├─► swanctl.ListSAs()           # 取所有活跃 SA
       │     │
       │     └─► 解析 JSON → []SA
       │
       ├─► for each SA:
       │     │
       │     ├─► store.IncrementBytes(username, bytes_in, bytes_out)
       │     │     │
       │     │     └─► UPDATE users SET bytes_in_total = bytes_in_total + ?, ...
       │     │
       │     └─► store.UpdateLastUsedAt(username)
       │
       └─► log "collector: collected stats for N sessions"
```

### 3.7 过期检查数据流（后台 goroutine，每 60 秒）

```
expiry.Run()  // 后台启动的 ticker
       │
       ├─► store.GetExpired(now)
       │     │
       │     └─► SELECT * FROM users WHERE expires_at > 0 AND expires_at < ?
       │
       ├─► for each expired user:
       │     │
       │     ├─► swanctl.Terminate(username)
       │     │     │
       │     │     └─► exec.Command("swanctl", "--terminate", "--ike-id", username)
       │     │
       │     ├─► store.MarkExpired(username)   # 标记 enabled = 0
       │     │
       │     └─► log "expiry: user %s expired at %s, terminated"
       │
       └─► log "expiry: scanned N users, M expired"
```

### 3.8 限速生效数据流（SA 建立时）

```
客户端连接成功 → charon 调用 updown 脚本
       │
       ▼
/usr/local/bin/ikev2-updown up
       │
       ├─► 读 PLUTO_PEER_ID (= username) 和 PLUTO_PEER_SOURCEIP (= VIP)
       │
       ├─► 读 /var/lib/ikev2-panel/limits/<username>
       │     │
       │     └─► LIMIT_MBPS = 文件内容（数字）
       │
       ├─► if LIMIT_MBPS > 0:
       │     │
       │     └─► tc qdisc + tc class + tc filter
       │           │
       │           └─► 限制该 VIP 的上下行速率
       │
       └─► log "Applied ${LIMIT}Mbps for user ${username}"
```

### 3.9 SA 拆除数据流

```
客户端断开 → charon 调用 updown 脚本 down
       │
       ▼
/usr/local/bin/ikev2-updown down
       │
       ├─► 清理 tc 规则（**只删该用户的 class**，**不删根 qdisc**——避免影响其他用户）
       │
       └─► collector 下一轮采集时 SA 已不在 swanctl 输出中，bytes_in_total 也不会再加
```

### 3.10 证书生命周期数据流（LE 模式）

**首次启动（LE 模式）**：

```
entrypoint.sh
  │
  ├─► 检测 /etc/ikev2-panel/certs-backup/ 是否存在 → 缺失
  │     │
  │     ├─► 调用 acme.sh --issue -d $IKEV2_DOMAIN --keylength 2048 --standalone
  │     │   （RSA 必须，ECDSA 在 iOS IKEv2 上有兼容问题）
  │     │
  │     ├─► 调用 acme.sh --install-cert -d $IKEV2_DOMAIN \
  │     │        --fullchain-file /etc/swanctl/x509/$DOMAIN.pem \
  │     │        --key-file /etc/swanctl/private/$DOMAIN.key \
  │     │        --reloadcmd "swanctl --load-creds"
  │     │
  │     └─► 启动 cron 守护（容器内 apt-get install cron + cron -f &）
  │
  └─► exec /usr/local/bin/ikev2-panel
```

**每日 02:00 续签（cron 触发）**：

```
crond → /etc/ikev2-panel/scripts/renew-cert.sh $IKEV2_DOMAIN
  │
  ├─► openssl x509 -checkend 2592000（检查是否 < 30 天）
  │     │
  │     └─► 否 → exit 0（跳过）
  │
  ├─► 备份当前证书
  │     └─► cp 到 /etc/ikev2-panel/certs-backup/$TIMESTAMP/
  │
  ├─► acme.sh --renew -d $DOMAIN --keylength 2048 --force
  │
  ├─► 验证新证书：
  │     │
  │     ├─► 有效期 ≥ 30 天 → swanctl --load-creds + swanctl --load-all
  │     │                  → rm LAST_RENEW_FAILED
  │     │                  → exit 0
  │     │
  │     └─► 续签失败 / 有效期不足
  │           ├─► cp 备份证书回原位
  │           ├─► swanctl --load-creds + swanctl --load-all（让旧证书继续工作）
  │           ├─► touch LAST_RENEW_FAILED
  │           └─► exit 1
  │
  └─► Go 端 health.certcheck 每 60s 检测 LAST_RENEW_FAILED → 首页红字横幅
```

**Go 端 health.certcheck 流程**：

```
certcheck.Run()  // 每 60s ticker
  │
  ├─► 检测 /etc/ikev2-panel/certs-backup/LAST_RENEW_FAILED 文件
  │     │
  │     └─► 存在 → 设置 atomic.Bool flag，handler_home 渲染时显示红字横幅
  │
  ├─► 解析当前证书有效期
  │     │
  │     ├─► < 14 天 → 日志 WARN + 横幅告警
  │     ├─► < 30 天 → 日志 INFO（仅记录，不告警）
  │     └─► ≥ 30 天 → 正常
  │
  └─► 不发邮件 / 通知（v2 不做通知系统）
```

### 3.11 表单校验失败的数据流（PRG + Flash 回填）

**通用原则：任何 POST 表单校验失败都不许返回 `http.Error`。**

`http.Error` 写出的是一个脱离站点的纯文本页 —— 没有导航、没有返回链接，
用户只能按浏览器后退键。表单越长，丢掉的输入越多。

正确做法是 Post/Redirect/Get，失败也走 303：

```
POST 表单（部分非法）
   │
   ├─► 解析出**部分填充**的结构体（能填的都填上）
   │
   ├─► FlashStore.Set(session, Flash{
   │       Kind:    "error",
   │       Message: 错误文案,          // ← 进 .Msg，受转义
   │       Backfill: 结构体 JSON,      // ← 回填用，不写 URL
   │   })
   │
   ├─► HTTP 303 → 同一页面的 GET
   │
   └─► GET 渲染：
         Consume(session)  // 取一次即消费
           ├─► 有 Backfill → 草稿**覆盖**已保存值，非法字段原样回显
           └─► 对应 <details> 自动 open + 标红
```

两个容易做错的点：

| 点 | 做法 | 原因 |
|----|------|------|
| 解析函数出错时的返回值 | 返回**部分填充**的结构体，不是 `nil` | 返回 `nil` 等于告诉调用方"用户什么都没填"，回填就变成清空 |
| 回填数据的存放 | 走 FlashStore 内存，**不拼 query string** | query string 会进浏览器历史、可被分享、且是反射型 XSS 的经典入口 |

具体实现见 `internal/web/handlers_mobileconfig_options.go` 的 `failMCOpts()`，
Flash 的回填字段是 `Flash.MCOptsBackfill`。

---

## 4. 关键类型与模块边界

本项目**以具体类型为主，不预先定义接口**。只在消费侧（`limit` / `expiry`）声明窄接口，
以便单测注入假实现。`store` 与 `swanctl` 都是直接被注入的具体类型。

### 4.1 `store`：持久层（具体类型）

`internal/store` 不导出任何接口，直接提供包级函数，由调用方持有 `*sql.DB` 上下文：

| 文件 | 职责 |
|------|------|
| `store.go` | 打开 DSN、建表、迁移 |
| `users.go` | `users` 表 CRUD，含 `GetExpiredUsers` 的过滤语义 |
| `admins.go` | `admins` 表（单行，`CHECK (id = 1)`） |
| `sessions.go` | `sessions` 表 |
| `audit.go` | `audit_log` 表写入与查询 |
| `types.go` | 各表行结构 |

DSN 上挂三条 pragma（`internal/store/store.go`）：

```
file:<dataDir>/panel.db
  ?_pragma=foreign_keys(1)
  &_pragma=busy_timeout(5000)
  &_pragma=journal_mode(WAL)
```

`busy_timeout(5000)` 是必需的：流量采集 goroutine 与用户操作可能并发写库。

### 4.2 `config` / `runtime` / `panelstate`：配置的三层

```
env (.env / compose)  ──►  config.Load()      只解析环境变量，不 import 任何项目包
                                   │
面板 UI 写入 ──►  panelstate        │  运行时可改的状态，落 /data/panel-state/*.conf
              (cert/ddns/subnet)   │
                                   ▼
                          runtime.Merge()      合并成一份最终配置
                                   │
                                   ▼
                     ddns / cert / swanctl / web
```

分层的理由：`internal/config` 解析 env，`internal/panelstate` 读文件。若让 `config` 直接
import `panelstate` 就形成 `config → panelstate → config` 的反向依赖，所以中间加一层
`runtime` 做合并。**`config` 包不 import `panelstate`，这是硬约束。**

`panelstate` 管理的文件（`/data/panel-state/`，mode 0600）：

| 文件 | 内容 |
|------|------|
| `cert.conf` | 面板 TLS / ACME 账号等证书配置 |
| `subnet.conf` | 客户端 IPv4 pool + IPv6 ULA pool |
| `ddns.conf` | DDNS 同步配置（panelstate 侧） |
| `aliyun.creds` | 阿里云 API 凭证（JSON） |

⚠️ **DDNS 运行时状态文件不在上面这张表里**：`ddns` 包自己写
`/data/panel-state/ddns-runtime.conf`（INI-style `key=value`），文件名刻意区别于
panelstate 的 `ddns.conf`（JSON）——同名会两边互相覆盖、把对方的文件写坏。
v2.86-pr24 起这个路径不再位于 `/etc/ikev2/`（那里没挂载，容器重建就丢）。

### 4.3 `web`：HTTP 层

路由注册、中间件链与端点清单见 [§7](#7-http-层路由与中间件链) 与 [API.md](API.md)。

### 4.4 `swanctl.Manager`：与 strongSwan 的唯一边界

`swanctl.Manager` 是**具体类型**（`internal/swanctl/loader.go`），不是 interface。
它同时持有两条**互相独立**的通道 —— 这是本项目最容易误解的地方：

```
                     ┌─────────────────────────────────────────┐
   配置下发  ───────► │ 通道 A：CLI   swanctl --load-conn ...   │ ──► 写 conf 文件
   （改配置）         └─────────────────────────────────────────┘     + fork-exec
                                                                        swanctl 二进制

                     ┌─────────────────────────────────────────┐
   状态查询  ───────► │ 通道 B：VICI   govici → charon.vici      │ ──► Unix socket
   （问现状）         └─────────────────────────────────────────┘     结构化，无字符串解析
```

**为什么不能统一成一条**：

| | 通道 A（CLI） | 通道 B（VICI） |
|---|---|---|
| 干什么 | 改配置：写 `swanctl.conf.d/` 后让 charon 重载 | 查状态 / 踢线：列 SA、终止 IKE_SA |
| 实现 | `os/exec` 调 `swanctl` 二进制 | `github.com/strongswan/govici` |
| 为何不用对方 | VICI 协议**不实现** `load-conn` / `load-all`（见 `loader.go` 顶部实测记录） | CLI 的 `--list-sas` 输出要写状态机解析，格式随版本漂移 |

所以"配置下发走 CLI、状态查询走 VICI"是**协议能力决定的**，不是历史遗留。

方法清单（真实签名见 `internal/swanctl/`）：

| 方法 | 通道 | 说明 |
|------|------|------|
| `WriteUserConf` / `RemoveUserConf` | — | 只写文件，**不**触发重载 |
| `WriteUserConfAndReload` / `RemoveUserConfAndReload` | A | 写文件 + 加锁重载，失败回滚 |
| `ReloadAll` | A | 重载全部连接 |
| `LoadCreds` | A | 重载凭据 |
| `UpdatePoolsAndReload` | A | 改客户端地址池并重载 |
| `ListSAs` | B | 列出当前 SA |
| `Terminate` / `TerminateAll` | B | 强制下线 |

构造用链式选项：`WithConfDir` / `WithViciSocketPath` / `WithSkipVici` / `WithSwanctlConfPath`。

**`WithSkipVici()` 是 dev 模式的逃生门**：跳过一切 VICI 调用，只写文件，
让本机没有 charon 时也能跑面板（判定依据是 `/etc/swanctl` 是否存在，不是 VICI 连通性）。

⚠️ **VICI socket 权限**：strongSwan 的 `vici` 插件把 socket 权限**硬编码为
`0770 root:root`，没有任何配置项**。当前面板以 root 运行所以不受影响；
若将来把面板改成非 root，只能从 socket 属组入手，改 `charon.conf` 无效。

### 4.5 `ddns` / `dns` / `publicip`：公网地址与 DNS

| 包 | 职责 | 关键约束 |
|----|------|---------|
| `publicip` | 探测本机对外公网地址 | 不能用"连一个 UDP 服务读本地地址"这种取巧写法，出口选择会失真 |
| `dns` | 阿里云 DNS API 客户端（签名 + 查询） | `errors.go` 做**错误分类**，决定 DDNS 该不该重试 |
| `ddns` | 同步主循环 + 节流 + 结果落盘 | `statefile.go` 记录上次结果，避免每轮都调 API |

⚠️ **同步策略是"查-比-改-建"**：记录不存在时自动调 `AddDomainRecord` 建一条（值 = 当前
探测到的公网 IP，TTL 600s），与 ddns-updater 的 aliyun / cloudflare provider 一致。
前提是面板里已填好 RR / 域名 / 凭证 —— 意图已声明，且记录被误删后能自愈。

⚠️ **同步周期每轮重读**：主循环用一次性 `time.Timer` 而非 `time.Ticker`，每轮跑完重新
arm 并重读 `Period`，面板改完下一轮即生效，不需要重启容器。手动「同步一次」**刻意不
Reset timer**，否则狂点手动同步会把定时同步一直往后推。

### 4.6 `cert`：证书生成与安装配置渲染

`internal/cert` 分三个文件：

| 文件 | 职责 |
|------|------|
| `generate.go` | 自签模式：确保 CA、服务器证书、面板证书存在且自洽 |
| `acme.go` | LE 模式：读取续签失败标志 `LAST_RENEW_FAILED` |
| `mobileconfig.go` | 渲染 `.mobileconfig` XML 模板 |

⚠️ 续签不是 Go 进程在做，而是容器内 cron 跑 [`scripts/renew-cert.sh`](../scripts/renew-cert.sh)
（backup → renew → rollback 三步）。Go 侧只**读状态标志**并在面板顶部渲染告警横幅。

渲染 mobileconfig 的三个易错点见 [§12.5](#125-mobileconfig-易错点)。

### 4.7 `auth`：口令、会话与登录限速

| 文件 | 职责 |
|------|------|
| `password.go` | 随机口令生成 + bcrypt 校验 |
| `session.go` | cookie session + CSRF token |
| `ratelimit.go` | 登录尝试限速（内存计数 + 定期清理） |

⚠️ **CSRF token 只在登录成功后签发**，所以 `POST /login` 不在 CSRF 保护范围内
（详见 [API.md §1](API.md)）。

用户 VPN 口令是**明文存库**的 —— 这是 EAP-MSCHAPv2 的协议取舍，不是缺陷；
补偿措施是数据库文件权限 `0600` + 面板 HTTPS-only + cookie `HttpOnly`。

### 4.8 消费侧窄接口：`limit` 与 `expiry`

全项目只有两处定义了 interface，都是"消费者声明依赖"的窄接口，
目的是让单测能注入假实现（`internal/limit/collector.go`、`internal/expiry/expiry.go`）：

| 位置 | 接口 | 用途 |
|------|------|------|
| `limit/collector.go` | `SwanctlManager`、`UserStore` | 流量采集需要列 SA 与读用户 |
| `expiry/expiry.go` | `SwanctlManager`、`UserStore` | 过期检查需要踢线与改用户 |

`swanctl.Manager` 与 `store` 都满足这两个窄接口。

---

## 5. 数据模型

### 5.1 SQLite

单文件数据库 `${IKEV2_DATA_DIR}/panel.db`，四张表。建表语句在
[`internal/store/store.go`](../internal/store/store.go)，启动时 `CREATE TABLE IF NOT EXISTS` 执行。

**`users`** —— VPN 用户

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | INTEGER PK | |
| `username` | TEXT UNIQUE | 同时是 EAP username 与子配置文件名 |
| `password` | TEXT | **明文**（EAP-MSCHAPv2 取舍），不是 bcrypt |
| `enabled` | INTEGER | ⚠️ **不参与 IKE 认证**，真正的开关是子配置是否存在 |
| `note` | TEXT | 备注 |
| `speed_limit_mbps` | INTEGER | `0` = 不限速 |
| `expires_at` | INTEGER | `0` = 永不过期；Unix 秒 |
| `bytes_in_total` / `bytes_out_total` | INTEGER | 由流量采集 goroutine 累加 |
| `created_at` / `updated_at` / `last_used_at` | INTEGER | Unix 秒 |

索引：`idx_users_enabled(enabled)`、`idx_users_expires(expires_at)`。

另有 `mobileconfig_opts TEXT NOT NULL DEFAULT ''` 这一列（后加的迁移列，存 JSON），
存该用户对安装配置的覆盖项，空串表示用管理员全局默认值。
⚠️ 它是 `users` 的**列**而不是独立表 —— 迁移时先查 `pragma_table_info` 再 `ALTER`。

**`sessions`** —— 面板登录会话（与 VPN 用户无关）

| 列 | 说明 |
|----|------|
| `id` TEXT PK | 随机 session id |
| `csrf_token` TEXT | 登录后签发 |
| `created_at` / `expires_at` | Unix 秒 |
| `user_agent` | 审计用 |

**`admins`** —— 面板管理员。用 `CHECK (id = 1)` 强制单行；口令列是 `password_hash`（bcrypt），
与 `users.password` 不同。

**`audit_log`** —— 敏感操作审计

| 列 | 说明 |
|----|------|
| `timestamp` | Unix 秒 |
| `actor` | 管理员用户名，或 `system` |
| `event` | `user.*` / `aliyun.*` / `ddns.*` 等 |
| `details` | `k=v` 串（刻意不引 JSON 依赖） |

保留期由 `scripts/audit-retention.sh` 清理。

### 5.2 panelstate 文件

`/data/panel-state/` 下的运行时状态文件见 [§4.2](#42-config--runtime--panelstate配置的三层)。
它们**不是数据库表**，是各自独立的文件、mode 0600，由 `internal/panelstate` 读写。

---

## 6. 配置管理

### 6.1 环境变量

**唯一权威清单是 [`.env.example`](../.env.example)** —— 本节只写"哪些必填、什么时候生效"，
不复制变量表。复制就会漂移。

| 变量 | 必填 | 说明 |
|------|------|------|
| `IKEV2_SERVER_CN` | ✅ **唯一必填** | 证书 CN，也是 mobileconfig 的 `RemoteIdentifier` |
| `IKEV2_DOMAIN` | LE 模式必填 | ACME DNS-01 用的域名 |
| `IKEV2_ALIYUN_KEY_ID` / `IKEV2_ALIYUN_KEY_SECRET` | LE / DDNS 必填 | 阿里云凭证，推荐命名 |

其余全部可选：不传就由 [`scripts/entrypoint.sh`](../scripts/entrypoint.sh) 的自动探测链补齐
（出口网卡、公网 IPv4/IPv6、VPN 虚拟网段）。

**阿里云凭证优先级链**（高 → 低）：

```
/data/panel-state/aliyun.creds      面板 UI 填的（每轮同步现读，无需重启）
  ↓ IKEV2_ALIYUN_KEY_ID / _SECRET
  ↓ ALIYUN_ACCESS_KEY_ID / _SECRET
  ↓ Ali_Key / Ali_Secret             acme.sh 原生命名
```

⚠️ **`IKEV2_NETWORK_MODE`**：每个 compose 文件里**硬编码**为自身模式，
**不接受 `.env` 覆盖**。网络由**文件**决定，变量只用于 entrypoint 的行为分支，
必须服从文件。写成 `${IKEV2_NETWORK_MODE:-macvlan}` 会有洞：`.env` 里残留的
`IKEV2_NETWORK_MODE=host` 会覆盖掉默认值，造成「容器在 macvlan netns 里、
entrypoint 却按 host 分支跑」。`bridge` 已被移除（v2.86-PR14），传它会被
FATAL 拒绝 —— 不是静默降级。换模式请换文件（见 [§11.1](#111-网络模式host--macvlan--ipvlanv286-pr20)）。

⚠️ **`IKEV2_TRUSTED_PROXY` 默认 false**：不信任 `X-Forwarded-For`。
默认信任的话，攻击者伪造 `X-Forwarded-For: 127.0.0.1` 就能绕过登录锁定。
只有真的在前面挂了反代才设 true，且必须同时用防火墙保证只有反代能访问面板端口。

### 6.2 启动时校验

| 检查 | 失败行为 |
|------|---------|
| `config.Load()` 解析失败 | 退出码 1 + 错误信息 |
| `IKEV2_SERVER_CN` 缺失 | 退出码 1 |
| 数据目录不存在 / 不可写 | 退出码 1 |
| 端口无法监听 | 退出码 1 |
| TLS 证书加载失败 | 退出码 1，**不带病运行** |
| LE 模式缺阿里云凭证 | entrypoint `exit 18`（否则 acme.sh 会报误导性的 `Error adding TXT record`） |
| IP forwarding 自愈失败（v4 或 v6） | entrypoint `exit 11` |
| 网络模式值非法 / L2 变量缺失或非法 | entrypoint `exit 12`（校验逻辑见 [§11.1.1](#1111-网络模式规格表scriptsnetwork-modesh)） |
| L2 模式下 `IKEV2_OUT_IF` 在容器内不存在 | entrypoint `exit 14` |
| L2 模式下 SLAAC 双 sysctl 状态不符 | entrypoint `exit 10` |

**不做**：JSON / YAML / TOML 配置文件。环境变量 + 镜像默认值 + panelstate 文件已经够用。

---

## 7. HTTP 层：路由与中间件链

### 7.1 中间件链

所有请求依次穿过（外 → 内）：

```
recoverPanic
  └─ secureHeaders
       └─ logging（含 metrics 计时）
            └─ trailingSlashRedirect
                 └─ mux（49 条路由）
```

顺序是刻意的：

| 约束 | 原因 |
|------|------|
| `recoverPanic` 必须最外层 | 否则 logging 自身 panic 或 mux panic 会逃逸出 HTTP server |
| `secureHeaders` 在 logging **之内** | 这样 panic 产生的 500 响应也带安全头 |
| `trailingSlashRedirect` 在 logging 之内、mux 之外 | 这样 301 响应也计入 metrics、也带安全头 |

### 7.2 鉴权三带

| 带 | 检查 |
|----|------|
| `secureHeaders` | 只加响应头 |
| `protect` | 需要已登录 session，否则 **302 跳 `/login`**（不是 401） |
| `protectPOST` | `protect` + CSRF token + 全局限速 30 req/min |

端点清单与请求/响应体见 [API.md](API.md)。

### 7.3 配置类 API 的双模响应

同一端点根据 `Accept` 头返回不同东西 —— 用 `curl` 测**必须**显式加
`Accept: application/json`，否则会拿到 302 重定向而不是 JSON：

| `Accept` | 响应 |
|----------|------|
| `application/json` | JSON |
| 其他 / 缺省 | 302 + flash 消息（给浏览器用） |

### 7.4 静态资源缓存

`cacheControlForStatic` 按扩展名设置 `Cache-Control`（`internal/web/server.go`）。
这是为了解决"浏览器和我的浏览器看到的 CSS 不一样" —— 那是进程级缓存，不是代码 bug。

---

## 8. 日志策略

### 8.1 Go 进程日志

- 用 Go 标准库 `log/slog`
- 输出到 stdout，由 Docker 收集
- 格式由 `IKEV2_LOG_FORMAT` 控制（`text` / `json`）

**审计日志不走 slog**：`audit_log` 表 + `./logs/audit.log` 是另一条线，记录敏感操作
（`user.*` / `aliyun.*` / `ddns.*`），保留期由 `scripts/audit-retention.sh` 管。
charon 自身的日志落在 `./logs/charon.log`（见 `configs/strongswan-filelog.conf`）。

### 8.2 日志级别

| 级别 | 何时用 |
|---|---|
| Debug | 请求处理详情、SQL 参数、swanctl 命令完整输出 |
| Info | 启动、停止、用户 CRUD、配置重载结果 |
| Warn | 重载失败但已自动回滚；证书即将到期 |
| Error | 数据库错误、文件 IO 错误、未处理异常 |

### 8.3 错误处理

三类错误：用户输入错误（4xx + 回显表单）、预期系统错误（5xx + 详细进日志、
简短消息给 UI）、意外错误（`recoverPanic` 兜住 + 栈进日志）。

关键路径必须处理错误：DB 写入失败必须返回；写用户子配置失败必须回滚 DB；
swanctl 重载失败必须回滚配置文件；TLS 证书加载失败 = 启动失败。

刻意不做：错误上报服务（Sentry 等）、自动重试（用户重复点击即可）、
独立错误码体系（HTTP 状态码够用）、i18n（中文硬编码）。

### 8.4 不做的

- 日志聚合 / ELK（几十人规模用不上）
- 错误上报服务
- 用户行为日志（审计日志已覆盖敏感操作）

## 9. 测试策略

### 9.1 必须写的测试

| 模块 | 测试类型 |
|---|---|
| `auth/password.go` | 单元测试：GeneratePassword 随机性、VerifyPassword 正反例 |
| `auth/session.go` | 单元测试：CSRF token 生成与校验 |
| `cert/generate.go` | 单元测试：CA/Server/Panel 证书生成可加载；mobileconfig 渲染可解析 |
| `store/*` | 集成测试：用 in-memory SQLite 跑 CRUD |
| `swanctl/parser.go` | 单元测试：**用真机 `list-sas` 的真实 key 名**做 fixture（见 §12.4） |
| `web/` 模板安全 | 文本断言：模板里不许出现 inline 事件属性；`MsgHTML` 不许含模板插值；XSS payload 进 `.Msg` 必须仍被转义 |

最后一类值得单说。前端缺陷（CSP 拦掉确认、banner 显示成标签文本、复制内容缺字段）
**单测本来测不出来** —— 它们既不 panic 也不返回错值。现在的做法是把断言写在
**模板文本和 DOM 结构**上：读 `web/templates/*.html` 原文做正则/字符串断言，
锁住"不许退回 `onclick`""`MsgHTML` 里不许插变量"。

这类测试的坑：正则会匹配到**解释这次修复的注释**。扫描前必须先剥掉 HTML 注释。

### 9.2 不写的测试

- 性能/压力测试（几十人规模无意义）
- E2E 浏览器测试（管理员一人手测即可）

### 9.3 裁定"不做"时要连测试一起删

断言一个**决定不实现的行为**，等于给 CI 埋编译炸弹 —— 测试引用的符号不存在，
`go test ./...` 和 `go vet ./...` 直接失败，而 CI 的 `runner-test` stage 跑的就是这个。

本项目踩过一次：34 个新测试文件里有 5 个断言的是当时裁定不落地的行为（NIST 密码策略、
`RateLimiter.Sweep`、DB 权限收紧、schema 版本化）。已连同裁定记录一起删除，
见 [CHANGELOG](CHANGELOG.md)。**断言写得再严谨，断言一个不存在的 API 就是错的。**

### 9.4 必做的真机验收

每次合并到 main 分支前：

1. `docker compose up -d` 启动
2. 浏览器登录面板
3. 新增 1 个用户
4. Linux strongSwan 客户端配置 → swanctl --load-all → swanctl --initiate
5. `swanctl --list-sas` 看到 SA 已建立
6. 从客户端 `ping` 服务器内网 IP
7. 删除用户 → 客户端重连失败
8. 重置密码 → 用户能用新密码连接

**基础 8 步全过 = 通过。**

**额外 6 步（限速/过期/流量）：**

9. **限速**：新建用户 `bob` 指定 5 Mbps → 连接 → `iperf3 -c <server-ip> -t 30` → 上行 ≤ 6 Mbps
10. **限速**：`bob` 限速改 0 → `iperf3` 重测 → 达到物理上限
11. **多用户限速**：新建 `charlie` 限速 10 Mbps → 同时连接 bob (5M) + charlie (10M) → 各自速度符合限速
12. **过期**：新建 `dave` 过期时间设 1 分钟后 → 等待 → 客户端连接被拒（看 swanctl 日志）
13. **过期恢复**：`dave` 过期时间改回 30 天后 → 客户端能继续连接
14. **流量统计**：`bob` 连接后下载 1GB 文件 → 面板 `bytes_in_total` 增加约 1GB

**共 14 步 = 全部功能验收完成。**

## 10. 自动探测链：零硬编码部署

`scripts/entrypoint.sh` 的 §0 段负责把所有"网络相关参数"探测出来，
所以绝大多数部署**只需要填一个域名**。优先级统一是 **环境变量 > 运行时文件 > 自动探测**。

| 步骤 | 探测什么 | 怎么探 | 覆盖它的环境变量 |
|------|---------|--------|------------------|
| §0.1 | 对外网卡 | `ip -4 route show default` → 首个非 `lo` 且 UP 的网卡 | `IKEV2_OUT_IF` |
| §0.2 | 公网 IPv6 | `ip -6 addr show` 里取 `2000::/3` 段、且 DAD 已收敛（避开 ULA / link-local / tentative） | `IKEV2_SERVER_ADDR_V6` |
| §0.3 | 公网 IPv4 | 出口网卡上首个非 RFC1918 地址（仅双栈模式需要） | `IKEV2_SERVER_ADDR_V4` |
| §0.4 | IPv4 默认网关 | `ip -4 route show default` | — |
| §0.5 / §0.5b | 宿主 LAN 段 | IPv4 `/24` 聚合 + IPv6 ULA 段 | — |
| §0.6 | 客户端 IPv4 pool | 从候选段里挑一个与 LAN 不冲突的 | `IKEV2_VPN_SUBNET` |
| §0.6b | 客户端 IPv6 ULA pool | 挑一个不冲突的 `fd00::/64` | — |
| §0.7 | 网络模式记录 | 写 `/run/ikev2-panel/netmode.env`，并打印模式 / parent / subnet / container iface | `IKEV2_NETWORK_MODE` |

### 为什么 §0.5 要探测 LAN 段

这是最容易踩的坑：VPN 客户端拿到的是 `10.x.x.x` 的虚拟地址。
如果宿主 LAN 也是 `10.0.0.0/24`，客户端回包时路由会走错接口，表现为
"VPN 连上了但访问不了内网设备"。所以选 pool 段之前必须先知道 LAN 占了哪些段。

`§0.6` 的候选池是 `10.10 / 10.13 / 10.17 / 10.42 / 10.66` 这些 `.0.0/24`。

### §0.2 为什么要按 DAD 状态过滤（v2.86-PR20）

L2 模式（macvlan / ipvlan）的 IPv6 来自 RA + SLAAC，而内核对新配的地址
一律先做 DAD（Duplicate Address Detection）。**DAD 没结束前地址带
`tentative` 标记，此时它还不能收发报文。** 如果把这样一个地址写进
`swanctl.conf` 的 `local_addrs`，charon 会绑定失败或客户端握手异常。

原实现读 `/proc/net/if_inet6`，实测字段布局是：

```
$1 addr(32hex)  $2 ifindex(dec)  $3 prefixlen(hex)  $4 scope(hex)  $5 flags(hex)  $6 devname
```

它有**两个**独立缺陷：

| # | 缺陷 | 后果 |
|---|------|------|
| 1 | 过滤条件写的是 `$4=="01" \|\| $4=="00"`，把 **scope** 当成 **flags**。`scope=00` 恰好等价于 global scope 所以「看起来能用」，但 `01` 不是合法 scope 值，真正的状态位在 `$5` | `tentative` / `dadfailed` 地址从未被排除 |
| 2 | 外层 `awk '{print $6}'` 逐行打印**网卡名**，内层 `awk '$6==d ... {print $1; exit}'` 的 `exit` 只取该网卡上的**第一条**地址 | macvlan/ipvlan 容器 eth0 上 Docker 分的 ULA（`fd48:...`）必然先于 SLAAC 的 GUA 出现，拿到 ULA 后 `case 2xxx:` 匹配不上就跳过，而外层下一轮喂的还是同一张网卡名 → **永远走不到 GUA，探测返回空** |

缺陷 2 的实测证据：同一份 `if_inet6`，只把 ULA 行挪到 GUA 前面，
原实现输出从 `2408:832e:...` 变成**空字符串**。这意味着 L2 模式下
`IKEV2_SERVER_ADDR_V6` 经常探测不到，用户只能靠 `.env` 手工兜底。

改用 `ip -6 addr show` 后两个问题一并消失：

- 按 `tentative` / `dadfailed` / `deprecated` **文字标签**过滤，而不是硬编码
  flags 位 —— 位含义随内核版本演进，文字标签是稳定契约；
- `ip -6 addr show` 按地址逐行输出，不做「每网卡取第一条」，ULA 不可能遮蔽 GUA。

回归测试见 `tests/network_mode_test.sh` 的 NM-41 ~ NM-47 与 NM-52。

> ⚠️ **同一类错误在 `internal/swanctl/ipv6watch.go` 里还有第二处**（v2.86-PR20 已修）。
> `detectGlobalV6` 也读 `/proc/net/if_inet6`，也曾把 `fields[3]`（scope）
> 当成 flags，导致 `tentative` / `dadfailed` 地址从未被排除 —— 而这条路径是
> **运行时**的：ISP 重拨前缀后，`ipv6watch` 可能把一个尚未 DAD 收敛的地址
> 写进 `local_addrs`，DDNS 也可能把不可用地址同步到 DNS。
> 现已抽成纯函数 `parseIfInet6Line`（可脱机测试），按位掩码过滤
> `DADFAILED 0x08` / `DEPRECATED 0x10` / `TENTATIVE 0x40`，
> 并**保留** `TEMPORARY 0x20`（隐私地址是稳定态，应当接受）。
> 测试见 `TestParseIfInet6Line_DADFlags`（12 条）。
>
> **教训**：`/proc/net/if_inet6` 的 flags 是**第 5 个**字段，第 4 个是 scope。
> 两处代码都曾写成第 4 个。凡是碰这个文件的，先看一眼
> `docs/architecture.md` 这里的字段表再动手。

### 探测结果怎么用

探测值经过 `sed` 替换进镜像里的 `configs/swanctl-ipv6-only.conf` 模板占位符
（`%IKEV2_LOCAL_ADDRS_DIRECTIVE%`、`%IKEV2_VPN_SUBNET%`、`%IKEV2_SERVER_CN%` 等），
生成容器内实际的 `/etc/swanctl/swanctl.conf`。

⚠️ **`IKEV2_OUT_IF` 不走模板占位符**（v2.86-PR20）。模板里从来没有过
`%IKEV2_OUT_IF%`，entrypoint 里那句对应的 `sed` 是个从不生效的空转，已删除。
`OUT_IF` 通过 §0.1 的 `export IKEV2_OUT_IF` 以**环境变量**下发给
`ikev2-updown`（tc 限速）、DDNS、`ipv6watch` 三处消费者。

⚠️ **因此这些值只在容器启动时生效**。改环境变量必须重启容器；
改面板里的配置走 `panelstate`，有各自的生效时机（见 [README](../README.md)）。

### 探测失败怎么办

探测失败不是静默兜底 —— 拿不到必需值就直接 FATAL 退出并说明缺什么。
IPv6 探测失败还会再走一次主动探测作为兜底。

---

## 11. 部署架构

### 11.1 网络模式：host / macvlan / ipvlan（v2.86-PR20）

**三个模式 = 三个独立完整的 compose 文件**，各管各的，不做叠加：

| 模式 | compose 文件 | netns | 地址来源 | 何时用 |
|------|-------------|-------|---------|-------|
| `host` | `docker-compose.yml` | 与宿主共享 | 宿主网卡 | **默认**。零改动兼容，宿主已有公网 v6 时直接用 |
| `macvlan` | `docker-compose.macvlan.yml` | 独立 | IPAM 分配 v4 + 上游 RA 下发 v6 | 宿主本身不需要跑这个服务，但要在同一二层里占一个独立 IP |
| `ipvlan` | `docker-compose.ipvlan.yml` | 独立 | 同上，但与宿主共享 MAC | 同 macvlan，但上游（云厂商 / 交换机）不允许第二个源 MAC |

**为什么 host 模式用默认文件名 `docker-compose.yml`**：老用户直接
`docker compose up -d` 零改动可用，这是向后兼容的硬要求。

启动方式（两种等价，`up.sh` 只是多了建 `./logs` 目录这一步）：

```bash
docker compose up -d                                    # host
docker compose -f docker-compose.macvlan.yml up -d      # macvlan
docker compose -f docker-compose.ipvlan.yml up -d       # ipvlan

./scripts/up.sh                                         # 读 IKEV2_NETWORK_MODE 自动选文件
```

| 事实 | 说明 |
|------|------|
| **三个文件都是完整的** | 不用 `!reset` / `!override`，所以**任何 Docker Compose 版本都能用**，不设版本下限 |
| **host 模式没有 `ports:`** | 端口是容器直接在宿主上监听的；L2 模式容器有自己的 IP，同样不需要 `ports:` |
| **host 模式没有 `sysctls:`** | runc 会以 `not allowed in host network namespace` 拒绝（host 模式下容器与宿主共享 netns）；L2 模式容器有自己的 netns，service 段 `sysctls:` 完全可用 |
| **L2 模式的 sysctl 分两级：netns 级走 service 段 `sysctls:`，接口级走 service 段 networks 块的 `driver_opts`** | 见 §12.8.2。`networks.<name>.options` 不是合法 Compose 字段，`docker compose config` 会直接报错；endpoint sysctls（`com.docker.network.endpoint.sysctls`）放在**网络级** `networks.<name>.driver_opts` 会被静默忽略，必须放在 **service 段 `networks.<net>.driver_opts`**（端点级）才生效 |
| **L2 文件里没有 `network_mode:`** | 与 `networks` 互斥，只能二选一 |
| **`bridge` 已被删除** | v2.86-PR14 起移除。传 `bridge` 会被 FATAL 拒绝；`IKEV2_NETWORK_MODE` 合法值只有 `host` / `macvlan` / `ipvlan` |
| **切换模式必须重建容器** | compose 文件决定网络，不看环境变量。改了 `-f` 就会 recreate；`up.sh` 在 L2 分支会显式提示 |
| **`IKEV2_NETWORK_MODE` 不决定用哪个文件** | 文件由 `-f` 决定。该变量在每份文件里**硬编码**为自身模式，只用于 entrypoint 做模式相关的行为分支 |

⚠️ **三份服务定义必须保持一致**（image / capabilities / volumes / tmpfs / environment）。
这是三独立文件方案唯一的结构性风险 —— 改一处漏两处的后果不是报错，而是
「某个模式下静默新建空卷 → panel.db / LE 证书全丢」或「某个模式下静默缺
FOWNER → chmod 失败 → 无限重启」（v2-72 与 PR19 已各真实踩过一次卷名坑）。
所以 [tests/network_mode_test.sh](../tests/network_mode_test.sh) 的
**NM-29~NM-36 强制校验三者一致**：image / container_name / capabilities /
tmpfs / 顶层卷名 / 三条挂载 / environment 键集合 / L2 文件的模式默认值。
改 compose 后必须跑这个测试。

**为什么 host 是默认**：bridge 模式下 Docker 从 `fd00::/8` 分 ULA，公网 IPv6
不可路由，而 IKEv2 的 SA 协商需要真实公网地址。host 模式是唯一开箱即用拿到
公网 v6 的方式，所以保持默认；macvlan / ipvlan 解决的是"端口冲突 / 宿主已有
同端口服务 / 多实例"这类 host 覆盖不到的场景。

**L2 模式的前置条件**：

| 条件 | 说明 |
|------|------|
| Linux + kernel ≥ 3.9（macvlan）/ ≥ 4.2（ipvlan） | macvlan/ipvlan 都是内核特性，Docker Desktop 的 Linux 容器虽可用但宿主互通限制不同 |
| 非 rootless 模式 | rootless 下 macvlan/ipvlan 支持不完整 |
| 宿主 ↔ 容器需要互通 | macvlan/ipvlan 在内核层隔离宿主与容器，需额外建 **macvlan shim**（见 [README](../README.md#三网络模式与前置条件)） |
| 云厂商可能拦新 MAC | 一些云主机的安全组 / 反欺骗策略只放行原有 MAC，此时 macvlan 拿不到地址；ipvlan 共享 MAC 可绕过 |
| `IKEV2_L2_SUBNET` **只接受 IPv4** | 传 v6 子网会被 FATAL 拒绝。L2 模式的 IPv6 一律走 RA/SLAAC |
| `IKEV2_L2_SUBNET` 每段严格 0-255 且**无前导 0** | 见下方「为什么拒绝前导 0」 |

**为什么拒绝前导 0**（v2.86-PR20 补）：POSIX sh 的 `$((010))` 按**八进制**求值得 8，
而 Docker 侧按**十进制**得 10。同一份 `IKEV2_L2_SUBNET=192.168.050.0/24`
在两处算出**不同的网络**，且**任何一边都不报错** —— 只表现为
「网关莫名其妙不在子网内」，极难定位。

之所以要专门拒：`net_mode_validate` 里的网段/网关比对是用 `$(( ))` 算的，
一旦允许前导 0，它算出来的值就不再等于 Docker 的值。
Go 1.17+ 的 `net.ParseIP` 同样拒绝前导 0，所以这里与 Docker 的判定一致 ——
早拒总好过让用户拿到一条语焉不详的 docker 报错。

实现见 `_nm_valid_octet` / `_nm_valid_ipv4`；回归测试 NM-53 ~ NM-56。

**L2 模式下的行为差异**（entrypoint 按模式分支，见 §11.4 退出码）：

| 关注点 | host | macvlan / ipvlan |
|--------|------|------------------|
| `IKEV2_OUT_IF` 语义 | 宿主网卡名（如 `ens18`） | **容器网卡名**（通常 `eth0`）。沿用宿主网卡名会 FATAL `exit 14` |
| DDoS 规则链 | `DOCKER-USER` | 容器私有 netns 内的 `INPUT` |
| forwarding 持久化 | 写宿主 `/host-sysctl.d/99-ikev2.conf` | **不写** —— `net.ipv4.ip_forward` 是 per-netns 的，写宿主对本容器无效 |
| IPv6 地址收敛 | 单次探测 | 6 次 × 3s 重试（RA 周期 ~10s + DAD ~1s） |
| 容器能看到 `docker0` | 是（预期） | 否。若看到说明声明与实际不符 → WARN |

⚠️ **`IKEV2_OUT_IF` 有两道关，缺一不可**（v2.86-PR20 修正了此前不准确的表述）：

1. **存在性校验**（entrypoint §0.1）：探测不到就 `FATAL` / `exit 14`。L2 模式下
   若沿用宿主网卡名，nft 模板会生成一条永不匹配的 `oifname` 规则 →
   MASQUERADE 失效 → 客户端能连 VPN 但出不了网。
2. **回写环境变量**（v2.86-PR20 新增）：探测成功后必须
   `export IKEV2_OUT_IF="$OUT_IF"`。此前 entrypoint 只把值写进自己的局部变量
   就往下走，**从未 export**，导致三个下游消费者全部退回各自的默认值：
   `ikev2-updown` 的 `tc` 限速套到 `eth0`、DDNS 的 `Iface`、ipv6watch 的
   `Iface`（见 [§6.1](#61-环境变量) 的变量表）。这是"启动成功、面板全绿、
   但功能悄悄不生效"的静默故障，比第 1 条更难发现 —— 日志里没有任何异常。

   `export` 之后 `IKEV2_OUT_IF` 才真正是这条链路上唯一的网卡名真源，
   容器内外读到的是同一个值。

⚠️ **不要改 `/etc/docker/daemon.json`**：host 模式不走 docker 的 iptables /
sysctl 管理，改了只会在宿主留下无关配置。

### 11.1.1 网络模式规格表（`scripts/network-mode.sh`）

镜像内路径 `/etc/ikev2-panel/scripts/network-mode.sh`，由 entrypoint source。
**只定义常量与函数、无副作用**；文件缺失时 entrypoint `exit 12`（镜像构建不完整）。

```sh
net_mode_has_own_ip()      # host 模式为 0，其余为 1
net_mode_procsys_rw()      # 同上（v2.86-PR19 结论：容器内 /proc/sys 是 ro）
net_mode_has_docker_user() # 仅 host 模式为 1
net_mode_ddos_chain()      # host → DOCKER-USER；L2 → INPUT
net_mode_normalize()       # 小写 + trim；空值默认 host
net_mode_driver_desc()     # 人类可读的驱动描述（日志用）
net_mode_sysctls()         # L2 容器 netns 的 sysctl 真源（compose 的 sysctls 块同值）
net_mode_sysctl_checks()   # 运行时回读清单：<sysctl 路径>|<期望值>
net_mode_validate()        # 全部配置校验，返回非 0 表示失败

net_shared_network_name_valid()  # 共享网络名合法性（纯 POSIX sh，见 §11.1.2）
```

共享网络名的默认值常量是 `NET_SHARED_NETWORK_NAME_DEFAULT_IPVLAN="ipvlan"` /
`NET_SHARED_NETWORK_NAME_DEFAULT_MACVLAN="macvlan"`
（定义在本文件里，宿主侧 `scripts/up.sh` source 后使用；
按模式取值走 `net_shared_network_default_name`）。

> **v2.86-PR20**：原表里的 `net_mode_out_if_is_host()` 已删除 —— 它零生产调用，
> 且语义与 `net_mode_has_docker_user()` 完全相同。`IKEV2_OUT_IF` 的模式差异
> 现在由 entrypoint §0.1 的存在性校验 + `export` 统一处理，不再需要一个
> 无人调用的判定函数（详见 §0.1 关于 `export IKEV2_OUT_IF` 的说明）。

**`scripts/up.sh` 也 source 这个文件**（v2.86-PR20）。此前 `up.sh` 自带一份
`require_l2_env()`，与 `net_mode_validate()` 是同一件事的两份实现，规则一改
就漂移 —— `net_mode_validate` 后加的 `PARENT != lo`、`GATEWAY` 必须落在
`SUBNET` 内、`macvlan_mode=passthru` 拦截，`up.sh` 全都不知道，只能等容器起来
才 `exit 12`。现在 `up.sh` 在 `docker compose` 之前就会报出与容器内完全一致的
错误。

能力开关用**返回码**而非全局变量实现 —— entrypoint 是
`set -euo pipefail`，全局变量 + 条件判断容易踩 `-u` / `-e` 的坑。

`net_mode_validate()` 的校验点：

| 模式 | 校验 |
|------|------|
| `host` | 6 个 L2 变量非空时打 WARN（说明会被忽略），返回 0 |
| `macvlan` / `ipvlan` | PARENT / SUBNET / GATEWAY 三项必填（聚合缺失清单）；`PARENT != lo`；SUBNET 必须是合法 IPv4 CIDR 且 GATEWAY 在其中；驱动枚举 |
| `macvlan` 专属 | `passthru` 与 IPAM **互斥** → FATAL（建议 `bridge`）；枚举 `bridge \| private \| vepa` |
| `ipvlan` 专属 | 枚举 `l2 \| l3 \| l3s`；`l2` 以外打 WARN；flag 枚举 `bridge \| private \| vepa` |
| `bridge` | FATAL：已于 v2.86-PR14 移除 |
| 其他 | FATAL + 列出合法值 |

L2 容器 netns 需要的 IPv6/转发 sysctl 分**两条通道**注入（层级与放置位置的实测
见 §12.8.2）：

netns 级 —— service 段 `sysctls:` 块（`net_mode_sysctls()` 的返回值）：

```
net.ipv4.conf.all.forwarding=1
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.accept_ra=2
```

接口级 —— service 段 `networks.<net>.driver_opts` 的 endpoint sysctls：

```
com.docker.network.endpoint.sysctls: "net.ipv6.conf.IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2"
```

三条 netns 级 sysctl 必须写在 service 段 `sysctls:`：`networks.<name>.driver_opts`
是**网络级**配置，写接口级 sysctl 会被静默忽略；而 service 段 `sysctls:` 里写
`IFNAME`/`eth0` 又会被 Docker 拒绝 —— 接口级只能走 endpoint sysctls。

`accept_ra=2` 必须是 `2`：内核功能性默认是"forwarding 关闭时接受 RA、开启时不接受"，
而本项目必开 forwarding —— 不显式设 `2` 就会静默丢掉所有 RA。
`disable_ipv6` 不在 service 段 sysctl 清单里：本项目的 L2 网络不带 v6 子网
（`EnableIPv6=false`），Docker（≥26.0.2）建端点时会**显式**把端口的
`disable_ipv6` 置 1，这一步只发生在端点上，service 段 sysctls 够不着，
必须用 endpoint sysctls（接口级、建端点后回写）把它翻回 0。

四条期望值（前三条 + `disable_ipv6=0`）都在 L2 compose 文件里**静态注入**，
entrypoint 启动后按 `net_mode_sysctl_checks()` 逐条回读复核（§12.8.1），
任一条不符即 FATAL `exit 10`。

### 11.1.2 共享 L2 网络（v2.95 / v2.96）

**L2 模式的网络在宿主机上本来就只能有一条这种网络**，所以「各 project 各建各的」
这条路走不通 —— 撞的是 Docker 的两条硬限制：

| 限制 | 报错 | 触发条件 |
|------|------|---------|
| Docker IPAM 全局 | `invalid pool request: Pool overlaps with other one on this address space` | 同子网再建一条 L2 网络 |
| ipvlan parent 独占 | `network di-xxx is already using parent interface ens18` | 同一张网卡上再建一条 ipvlan 网络（**换子网也不行**） |

v2.95 的做法是**把这条唯一性显式化**：ipvlan 网络改用**固定名**（默认 `ipvlan`），
所有 project 加入**同一条**，由第一个部署的人建、后面的人复用。
v2.96 把这套**推广到 macvlan**（默认名 `macvlan`）。

**默认网络名 = 驱动名**，不带任何 project 标识：

| 模式 | 默认网络名 | 覆盖变量 |
|------|-----------|---------|
| `macvlan` | `macvlan` | `IKEV2_L2_NETWORK_NAME` |
| `ipvlan` | `ipvlan` | `IKEV2_L2_NETWORK_NAME` |

**两种驱动都能跨 project 共享同一条网络**（63 实测，Docker 28.5.2）：

| 驱动 | 跨 project 共享同一条网络 | 同一 parent 上多条网络 | parent 独占 |
|------|--------------------------|---------------------|-----------|
| `ipvlan` | ✅ 可以 | ❌ 不行 | **是** |
| `macvlan` | ✅ 可以 | ✅ 行（子网不重叠即可） | 否 |

- **ipvlan**：子设备**共享 parent 的 MAC**，同 parent 上多个子设备共存无碍。
  实测同一 ipvlan 网络上挂 2 个容器（不同 project），各拿 `192.168.50.92` / `.93`，
  互 ping 0% loss。
- **macvlan**：每个容器拿**独立 MAC**，同一 parent 上也可以并存多条 macvlan 网络。
  实测同一条 macvlan 网络上挂 3 个容器（不同 project），各拿 `.2` / `.3` / `.4`，
  MAC 各不相同。

> ⚠️ **曾有一版结论写成「macvlan 不可共享」，那是误判。**
> 当时在 63 上看到 `failed to create the macvlan port: device or resource busy`，
> 实际原因是**宿主 `ens18` 本身不支持 macvlan 设备**（手工
> `ip link add link ens18 type macvlan` 同样报 `Device or resource busy`），
> 与共享无关。换个能建 macvlan 设备的 parent 后，同一条网络挂 3 个容器完全正常。
> **判据要看清楚是「驱动限制」还是「宿主限制」。**

**四条处置路径**（`scripts/up.sh` 的 `_l2_shared_network_ensure`，两种驱动一致）：

| 情况 | 判定 | 动作 |
|------|------|------|
| ① 同名网络已存在 | `docker network inspect <name>` 成功 | **复用**，并逐项比对参数；不符 → 报错列差异 |
| ② 同名不存在，但宿主机上有参数兼容的**同驱动**网络 | parent + 子网 + 网关 + mode(+flag) 全等 | **自动接管**那条（仅限用户没显式指定名字时） |
| ③ parent 上已有别的同驱动网络 | 存在同 parent 的网络且不是② | **仅 ipvlan**：明确报冲突（parent 独占）；**macvlan**：允许共存 |
| ④ 什么都没有 | —— | **创建**：`docker network create --driver <驱动> --opt parent=… --subnet=… --gateway=… <name>`（不带 `--ipv6`，理由见下方「建网时必须显式带 `--subnet`」） |

**② 只在用户没显式指定名字时才自动接管**。显式指定（`IKEV2_L2_NETWORK_NAME`
或 compose 顶部的 `x-l2-netname` 写成非默认值）= 用户明确要求用那一条，
此时若那条不存在而另有兼容的，报错让用户选，不悄悄换。

**参数不符 = 报错并列出差异，绝不自动删改**。共享网络的 IPAM 归**第一个建它
的人**所有，另一条网络很可能正在被别的 project 使用；自动改 subnet 或删网络
都会打断那个 project。差异清单逐项列出（驱动 / 父接口 / 子网 / 网关 /
mode / flag / IPv6 开关），并给出两个处置选项。

**`external` 的语义**（compose networks 段，两份 L2 compose 都有，v2.97 起是插值开关）：

```yaml
external: "${IKEV2_L2_EXTERNAL:-false}"
```

| 取值 | 场景 | 行为 |
|------|------|------|
| `false`（默认） | **纯 compose 路径**（Docker 管理器 GUI / `docker compose up -d`） | compose **自己**建这条网络：不存在就建（名字固定，无 project 前缀），已存在就复用（带正确 label 时） |
| `true` | **up.sh 路径** | compose **只加入**，不创建也不删除（网络必须已由 up.sh 建好并逐项校验过） |

63 实测（Docker 28.5.2 / Compose v2.40.3）：

| 行为 | `external: false` | `external: true` |
|------|------------------|------------------|
| 网络不存在 | compose 建成，名字就是 `ipvlan`（有 `name:`，无 project 前缀） | 直接报 `declared as external, but could not be found` |
| 网络已存在（**带** `com.docker.compose.network=ikev2-l2` label） | 复用，只出 warning | 复用，无 warning |
| 网络已存在（**无** label） | **报错** `network ipvlan was found but has incorrect label com.docker.compose.network set to "" (expected: "ikev2-l2")` | 复用（**跳过 label 校验**） |
| `docker compose down` | 网络是 compose 建的 → 会删（但别的容器还挂着时保留） | **不删**（external 网络不属于 compose 生命周期） |

**为什么 label 这么关键**：compose 复用一条**已存在**的同名网络时会校验它上面的
label `com.docker.compose.network` 是否等于 networks 段的 **key**（`ikev2-l2`）。
所以：

- 纯 compose 路径下 compose 自己建的网络，label 天然正确 → 别的 project 能复用；
- `scripts/up.sh` 建的网络必须**手动带** `--label com.docker.compose.network=ikev2-l2`
  → 才能被纯 compose 路径复用（up.sh 建的网不带 compose 的 project label，
  compose 会打 `a network with name ipvlan exists but was not created by compose`
  的 warning，但**能正常启动**）；
- 两条路径建出来的网因此**互相可用**。

**v2.97 的方向修正**：v2.95/v2.96 的设计前提是「用户跑 `up.sh` 建网，compose 以
`external: true` 加入」。真实部署场景是**只跑 compose**（Docker 管理器 GUI），
那个前提不成立 —— 写死 `external: true` 会让纯 compose 路径直接失败
（`declared as external, but could not be found`）。所以建网责任交还给 compose，
`up.sh` 降级为**可选增强**（参数校验 / 冲突预检 / 固定 IPv4 与宿主地址冲突检查 /
`./logs` 目录准备 / 老网络自动接管），并在建网时补上 compose label 让两条路径互通。

**up.sh 路径强制 `external=true`**：up.sh 建好并校验完网络后，会把
`IKEV2_L2_EXTERNAL` 强制为 `true` 并 export。否则 compose 会再查一遍网络
（打 warning）或在「自动接管来的老网络」上直接报 label 不匹配。

**建网时必须显式带 `--subnet`**：不带的话 Docker 会塞默认池 `172.18.0.0/16`，
之后任何 join 方想指定 `ipv4_address` 都会被拒
（`user specified IP address is supported only when connecting to networks with
user configured subnets`）。反过来**不能再带 `--ipv6`**：开了 `enable_ipv6`
却不给 v6 子网时，Docker 会从内置 ULA 池自动分一段 fd00::/8，容器拿到不路由的
ULA + 一条指向不存在网关的假默认路由，污染选路 —— v6 地址改由上游 RA 下发，
接口的 `disable_ipv6`/`accept_ra` 由 service networks 块的 endpoint sysctls
打开（§12.8.1），建网阶段不需要任何 v6 配置。

**up.sh 与 compose 必须指向同一条网络**。compose 的网络名来自 networks 段的
`${IKEV2_L2_NETWORK_NAME:-<驱动名>}` 插值，所以：

1. `IKEV2_L2_NETWORK_NAME` 在 up.sh 里**必须 export**（否则 up.sh 与 compose
   各自算出不同的名字，两边加入**不同的网络**，且没有任何报错）；
2. 走 compose 顶部 anchor 回落时（用户在文件里写 `x-l2-netname: &l2_netname "mynet"`）
   也要 export，否则 up.sh 读到 `mynet`、compose 仍用默认的驱动名；
3. 自动接管（情况②）时要把 `IKEV2_L2_NETWORK_NAME` 指向实际接管的那一条。

**判定「显式指定」的时机**：在 anchor 回落**之后**算 —— 否则用户在 compose 顶部
写死名字时会被误判成「用了默认值」，从而触发不该触发的自动接管。
比较的基准是**当前模式的默认名**（`net_shared_network_default_name`），
不是某个全局常量 —— 因为默认名随驱动而变。

**网络名合法性**：Docker 网络名不允许空格 / 特殊字符，
`scripts/network-mode.sh` 的 `net_shared_network_name_valid()` 用纯 POSIX sh
校验（该文件会被容器内 entrypoint source，不能用 bash 语法）。

### 11.2 Capabilities

```
cap_drop: ALL
cap_add:  NET_ADMIN  NET_RAW  NET_BIND_SERVICE  CHOWN  DAC_OVERRIDE  FOWNER
security_opt: no-new-privileges:true
devices: /dev/net/tun
```

| Capability | 用途 |
|------------|------|
| `NET_ADMIN` | nftables 规则、路由表、XFRM、forwarding 自愈 |
| `NET_RAW` | `tc` qdisc / class / filter（限速） |
| `NET_BIND_SERVICE` | 监听 500 / 4500 特权端口 |
| `CHOWN` | entrypoint 调整 `/etc/swanctl` 权限 |
| `DAC_OVERRIDE` | 读 `/proc/sys/net` 下的转发开关 |
| `FOWNER` | chown/写入非属主文件（LE 证书签发、`/data` 下 panelstate 的属主切换） |

不要加 `SYS_ADMIN` 或 `privileged: true` —— 这个镜像不需要。
`/dev/net/tun` 是 `kernel-libipsec` 的 ESP 注入点，必须挂。

三种网络模式共用同一套 `cap_add`，无需按模式调整。

### 11.3 数据卷

| 挂载 | 类型 | 宿主可见 |
|------|------|---------|
| `ikev2-panel-data:/data` | 命名卷 | ❌ `{DockerRootDir}/volumes/ikev2-panel-data/_data` |
| `./logs:/var/log:rw` | bind | ✅ |
| `/etc/sysctl.d:/host-sysctl.d:rw` | bind | ✅ |

`/data` 是**命名卷而非 bind mount**，数据不在项目目录下 —— 部署后"看不到"
数据库和证书是预期行为。排查用 `docker exec ikev2-panel ls -la /data`。

⚠️ **`{DockerRootDir}` 不等于 `/var/lib/docker`。** 192.168.50.63 实测
`Docker Root Dir: /vol1/docker`，卷路径是 `/vol1/docker/volumes/...`。
硬编码 `/var/lib/docker/volumes/...` 在这类宿主上会指向不存在的目录：

```bash
docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}'   # 唯一可靠取法
```

卷名由 compose 顶层 `name: ikev2-panel-data` 显式锁定，与项目目录名解耦：

```yaml
services.ikev2-panel.volumes:
  - type: volume
    source: ikev2-data      # 顶层 key
    target: /data
volumes:
  ikev2-data:
    name: ikev2-panel-data  # 实际卷名，恒定
```

⚠️ 早先用短名 `{项目目录名}_ikev2-data` 有个静默数据丢失的坑：目录改名后
compose 找不到旧卷会**自动建一个同名空卷**，`panel.db`、LE 证书、acme account
全部"消失"，容器照常启动、面板可访问、零报错（192.168.50.63 实测）。
从旧命名迁移：

```bash
docker volume rename ikev2-panel-v2_ikev2-data ikev2-panel-data
```

`/data` 下持久化（192.168.50.63 实测清单）：`panel.db`（+ `-wal` / `-shm`）、
`panel-state/`（`cert.conf`、`aliyun.creds`、`ddns-runtime.conf`、`ratelimit.json`、
`INITIAL_ADMIN_PASSWORD.txt`）、`le/`、`acme-sh/`、`ca/`（自签 CA 证书与私钥）、
`server/`（IKEv2 服务端证书与私钥）、`panel-tls/`。

**带宽限速文件（tc）不在 `/data`**：`main.go` 只在 `/var/lib/ikev2-panel/limits`
**不存在**时才回退到 `/data/limits`（dev 分支）。生产环境该目录由镜像预置，
走容器可写层，容器重建即清空 —— 但限速配置在 `panel.db`，重启后重新下发，无影响。
注意别和上表的 `panel-state/ratelimit.json` 混淆：那是**登录**限流计数，
确实持久化在 `/data`。

`scripts/backup-restore.sh` 只备份 `panel.db`、`panel-state/`、`le/` 三项
（`ca/`、`server/`、`acme-sh/`、`panel-tls/` 都不在内），
`DATA_DIR` 默认 `/data` 是容器内路径 —— 在宿主上跑必须覆盖，否则静默跳过：

```bash
DATA_DIR="$(docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}')" \
  ./scripts/backup-restore.sh backup
```

### 11.4 启动链与失败退出码

`scripts/entrypoint.sh` 是 PID 1，顺序是：自动探测 → 前置校验 → charon → Go 面板。

| 退出码 | 含义 |
|--------|------|
| `1` | 通用启动失败（配置、目录、端口、证书） |
| `10` | **IPv6 SLAAC 状态复核失败**（仅 L2 模式）：`disable_ipv6` / `accept_ra` 与期望不符 |
| `11` | **IP forwarding 自愈失败（IPv4 / IPv6 任一）** |
| `12` | **网络模式配置错误**：模式值非法、L2 变量缺失 / 非法、`network-mode.sh` 缺失，或 charon 启动失败 |
| `14` | **对外网卡不可用**：`IKEV2_OUT_IF` 指定的名字在容器 netns 内不存在 |
| `18` | LE 模式缺阿里云凭证 |

`exit 11` 是刻意设计：forwarding 没开时客户端能拨通、能拿到 IP、ping 得通服务端，
**但出不了网** —— 与其跑出一个"看起来正常但用不了"的实例，不如启动时明确失败。
详见 [§12.8](#128-ipv6-forwarding-自愈)。

`exit 12` / `14` 是 v2.86-PR20 新增的**网络模式**相关失败码，语义未变（12 原本
已定义为"网络模式配置错误"），只是校验点从"只接受 host"扩展为三模式全量校验。
`exit 14` 在 L2 模式下是硬要求：不校验就会退化成静默故障（见 §11.1）。

`exit 10` 复用既有的 sysctl 相关退出码，L2 模式下用于 SLAAC 双 sysctl 的
启动后回读复核 —— L2 compose 文件里静态注入的 sysctl 若被其他因素改写，
RA 会被丢弃、客户端拿不到 v6 地址，与其带病运行不如启动即失败。

启动顺序（v2.86-PR20）：

```
entrypoint.sh
  ├─ source /etc/ikev2-panel/scripts/network-mode.sh   ← §0.0（缺文件 → exit 12）
  ├─ net_mode_normalize + net_mode_validate            ← §0.0（失败 → exit 12）
  ├─ detect_out_if（校验 netns 内存在性）               ← §0.1（失败 → exit 14）
  ├─ …其余探测…
  ├─ 写 /run/ikev2-panel/netmode.env                   ← §0.7
  ├─ enable_forwarding                                ← §3（失败 → exit 11）
  ├─ SLAAC 双 sysctl 运行时回读复核                     ← §3.2（失败 → exit 10）
  ├─ apply_ddos_protection（链按模式选择）              ← §4.6
  └─ charon → swanctl --load-all → Go 面板
```

`/run/ikev2-panel/netmode.env` 取代了原先的死变量 `IKEV2_ACTUAL_NETWORK_MODE`，
供容器内排查用（`docker exec ikev2-panel cat /run/ikev2-panel/netmode.env`）：

```
MODE=macvlan
OUT_IF=eth0
DRIVER=macvlan_mode=bridge
HAS_OWN_IP=1
PROCSYS_RW=0
DDOS_CHAIN=INPUT
```

### 11.5 容器外的可选操作

| 位置 | 说明 |
|------|------|
| `/etc/cron.daily` | 调 `renew-cert.sh`（LE 续签）、`audit-retention.sh`（审计清理） |
| `scripts/backup-restore.sh` | 在**宿主机**跑，`DATA_DIR` 指向卷挂载点 |

详见 [README.md](../README.md)。
## 12. 关键实现细节

本节写**契约与坑**，不复制源码 —— 抄进文档的代码必然漂移。
要看实现请直接打开 [`internal/`](../internal) 下对应文件。

### 12.1 配置下发的三条路径

| 场景 | 命令 | 影响面 |
|------|------|--------|
| charon 首次启动 | `swanctl --load-all` | 全部连接 |
| Go 进程重载全部配置 | `swanctl --load-all --noprompt`（`loader.go` 的 `reloadAllLocked`） | 全部连接 |
| charon 起来但没连接 | entrypoint 显式 `--load-all` 一次 | 全部连接 |
| **改虚拟 IP 段** | `swanctl --load-all --noprompt` | 全部连接 |

**不要**改成按连接名细粒度重载（`--load-conn ikev2-rw`）。v2.86-pr24 试过，已撤销，
三个原因（核实自 strongSwan 6.0 官方文档 + 本仓配置）：

1. **`swanctl` CLI 没有 `--load-conn`（单数）这个子命令。** 加载类子命令只有
   `--load-all` / `--load-conns` / `--load-pools` / `--load-creds` /
   `--load-authorities`；`--load-conns` 是复数且不接受连接名参数。
2. **零收益。** 本项目只有 `ikev2-rw` 一条连接（`configs/swanctl-ipv6-only.conf`），
   per-user 文件只写 `secrets` 块、不新建连接 —— "只重载这一条"与"重载全部"完全等价。
3. **我们改的是 pool 不是 conn。** 官方定义 `--load-all` = `--load-conns` +
   `--load-creds` + `--load-authorities` + `--load-pools`，只有它含 `--load-pools`；
   换成 `--load-conns` 会漏掉 pool 本身。

按名细粒度重载只存在于 **VICI 层**（VICI 协议支持 `load-conn`，CLI 不支持），
但那条路要内联完整连接定义（需新写 swanctl.conf 嵌套块解析器），且 govici v0.8.1
是纯客户端无 server、无法测试 —— 为零收益引入不可测的高风险代码不值得。

其余路径（增删用户、换证书）同样走 `--load-all` —— 它们改的也是全量配置。

写入路径是**先文件后重载**，失败必须回滚（见 `internal/web/user_pipeline.go`）。

### 12.2 LE 模式与续签

续签不在 Go 进程里做，是容器内 cron 跑 `scripts/renew-cert.sh`：

```
backup 当前证书 → acme.sh --renew → 失败则 rollback
```

Go 侧只读 `LAST_RENEW_FAILED` 标志文件（`internal/cert/acme.go`），
有标志就在面板顶部渲染告警横幅。

`enable_forwarding` 与 `persist_sysctl` 是两步：前者写内核开关，后者把值落到
宿主 `/host-sysctl.d/99-ikev2.conf`。**持久化失败不是致命错误**（只打 WARN），
真正的致命是内核开关写不进去（`exit 11`）。

### 12.3 吊销用户的三步顺序

```
① Swanctl.Terminate(username)              踢掉现存 SA
② Swanctl.RemoveUserConfAndReload(username)  ★ 真正让认证失效
③ Store.SetUserEnabled(id, false)           只改 DB 状态
```

**顺序不能颠倒，②必须在③之前**：

- ②才是 IKE 认证的真正开关 —— DB 的 `enabled` 列**完全不参与** IKE 认证
- 如果先③再②，②失败时 DB 说"已禁用"但 SA 还活着 → 安全上等于没禁用
- ①失败会**回滚③**（把 `enabled` 改回 `true`），因为"用户已禁用但 SA 仍可连接"是漏洞

删除用户是同一套，只是最后还要清限速文件。见 `internal/web/user_pipeline.go`。

### 12.4 VICI list-sas 解析

`list-sas` 是 **streaming** 响应：一次连接会持续推送 SA 状态变更，
所以必须循环读直到 EOF，而不是调一次就返回。

解析时要用 `unwrapSA()` 剥外层嵌套 —— charon 返回的 SA 列表带一层包装结构，
直接断言会拿到空值。

⚠️ **`list-sas` 不返回 `local-addr` / `remote-addr`。**地址是**拆成两个 key** 给的：

```
local-host=2408:832e:…:f6cf   local-port=4500
remote-host=2408:832e:…:c1d   remote-port=4500
```

早先按 `local-addr` 读，`SA.LocalAddr` / `SA.RemoteAddr` **恒为空串**，
首页「活跃隧道」的本地 / 远端两栏永远是空的。现在由 `getHostPort()` 拼成
`host[port]` —— 端口用方括号而非冒号，跟 `swanctl` CLI 自身显示一致，
IPv6 不需要额外转义。

这类 bug 的性质值得记住：**VICI 取不到值时 `Get()` 返回空串而不是报错**，
所以字段为空不会有任何日志，只有对着真机 `list-sas` 输出核对才能发现。
写完解析器一定要用真实 key 名做 fixture。

⚠️ `list-sas` 的调用频率要有节制：它是 charon 的 fd 密集操作，
高频轮询（尤其匿名端点能触发的轮询）会打满 charon。
VICI 的 10 秒超时由 `withTimeout` 统一注入，不要在调用点重复包。

### 12.5 mobileconfig 易错点

**静默失败类**（iOS 侧不报任何错，只是连不上或行为异常）：

| 坑 | 后果 |
|----|------|
| 写了 `<key>ExtendedAuthenticationEnabled</key>` | iOS **完全忽略**这个 key，写了等于没写 |
| 写了 `<key>DNSSettings</key>` | 这是**杜撰的 key**，Apple 文档里不存在；它曾导致整个 profile 被 iOS 判为 invalid |
| 漏掉 `ChildSecurityAssociationParameters` | ChildSA 回落到 3DES 等弱算法 |
| `ServerCertificateCommonName` 配错 | iOS 拿它匹配服务端证书 CN，不匹配就**在 SA_INIT 之后直接放弃**，连 IKE_AUTH 都不发，服务端日志里看不到任何认证记录。唯一线索是客户端那句 "an unexpected error occurred" |

另外：`PayloadIdentifier` 的外层与内层不能冲突；闭合标签不能写成 `</key>`。

**v2.86-PR19(A13) 新增的结构性陷阱**：

1. **`OnDemandProfile=manual` + `OnDemandEnabled=true` → 非法 plist。**
   `renderOnDemandRules` 对 manual 原本返回空字符串，模板是
   `<key>OnDemandRules</key>{{ .OnDemandRules }}` —— 渲染出 key 后面没值，plist 结构非法，
   **iOS 直接拒绝安装**。触发路径很现实：用户只在 profile 下拉选「完全手动」，
   `on_demand_enabled` 保持默认（=true），`Validate` 不拦。
   现在两层防护：`Validate` 拦这个组合 + 渲染端返回最小合法的 `<array/>`。
   原有测试恰好显式传了 `OnDemandEnabled=false`，绕开了这个组合。

2. **`IncludeAllNetworks=true` 会让 `ExcludeLocalNetworks` 失效。**
   Apple 语义下 include 是"VPN 接管**所有**流量含本地子网"，exclude 被覆盖，
   写 true 或 false 结果完全一样 —— 是一行死配置。
   原注释和 UI 文案都写"exclude=true 保留局域网访问"，这与 Apple 文档**相反**，
   会让用户以为打印机 / NAS / HomeKit / 路由器管理页还能访问，实际开着 VPN 时全部不可达。
   想要"全局走 VPN + 保留局域网"应是 `include=false + exclude=true`，
   但那会变成 split-tunnel，与服务端 `local_ts = 0.0.0.0/0, ::/0` 的全量隧道设计冲突。

3. **`captive_probe` preset 的 `Domains` 写成空字符串** ——
   Apple 要求非空域名列表，现已改为通配符 `*`。

4. **UI 表单不回填 = 保存即清空。**
   `parseMobileConfigOptsFromForm` 从空结构体开始解析，空字段 = nil = "用默认"。
   用户级表单若没有 `value` / `selected` 回填，管理员改任意一项就会把该用户
   **所有** overlay 静默清空。现已通过 `userDetailData.MCOpts` 解码后回填。

5. **`isValidIP` 过松。** 原来的手写校验只查"4 段 + 全数字"，不查每段 0–255，
   `300.300.300.300` 能通过并被写进 plist。现已改用 `net.ParseIP`。

6. **表单校验失败不能返回 `http.Error`。**
   `dns_servers` 是 `<textarea>`，前端没有任何约束兜底，非法输入必然能提交。
   早先 `http.Error(w, err.Error(), 400)` 的后果是用户落到一个**脱离站点的纯文本页**：
   没有导航、没有返回链接、已填内容全部丢失、英文错误串混进中文界面。
   现在走 PRG（Post/Redirect/Get），见 §3.11。

### 12.6 限速：u32 与 flower 按 family 分支

`scripts/ikev2-updown` 里 tc filter 的选型：

| family | filter | 原因 |
|--------|--------|------|
| IPv4 | `u32 match ip src <peer>` | 老内核都支持 |
| IPv6 | `flower src_ip <peer>` | `u32` 不支持 IPv6 |

⚠️ **filter 匹配的是 `src <客户端虚拟 IP>`，挂在出口网卡上，所以只限客户端上传方向**，
下载方向不限速。测速必须**从客户端往公网发**，从服务端发测不出效果。

限速值只在**建号时**写进限速文件，面板没有事后修改的入口。

### 12.7 原子写

所有可能被 charon 读到的文件（用户子配置、`panelstate`）都必须原子写：
先写同目录临时文件，再 `rename`。直接覆写会在 charon 读的瞬间拿到半截文件。

实现见 [`internal/fsutil/atomic.go`](../internal/fsutil/atomic.go)；
`internal/swanctl/atomic.go` 只是保留的转调薄壳。

### 12.8 IP forwarding 检查（v4 / v6）

host 网络模式下容器与宿主共享 net ns，**理论上**容器内写 `/proc/sys/net/...`
写的就是宿主内核。L2 模式（macvlan / ipvlan）下容器有独立 netns，写的是
**容器自己**的开关 —— 而 `net.ipv4.ip_forward` / `net.ipv6.conf.all.forwarding`
本身就是 per-netns 的，所以两种模式互不影响，但也互不代偿。

```
enable_forwarding()  读 /proc/sys/net/ipv4|ipv6/conf/*/forwarding
   ├─ 已是 1 → 直接返回（不发日志）
   ├─ 不是 1 → 尝试 echo 1 > 同一路径
   │    ├─ 成功 → persist_sysctl()（失败只 WARN）
   │    │            ├─ host 模式   → 写宿主 /host-sysctl.d/99-ikev2.conf
   │    │            └─ L2 模式    → 直接 return 0（见下）
   │    └─ 失败 → 见下方「自愈从来就没成功过」
```

> ⚠️ **L2 模式不写宿主 sysctl.d**（v2.86-PR20）。`net.ipv4.ip_forward` 是
> per-netns 的，写进宿主 `/host-sysctl.d/99-ikev2.conf` 对容器内那个 netns
> **毫无作用**，只会留下一条"看起来在起作用、实际作用于另一个 netns"的
> 误导性配置。因此 `persist_sysctl()` 在 L2 模式直接返回 0。
>
> L2 模式下 forwarding 归谁负责见本节末的「L2 模式的 forwarding 归谁负责」。

> ⚠️ **自愈从来就没成功过**（v2.86-PR19(A16) 实测，192.168.50.63）。
>
> 容器内 `/proc/sys` 是 **ro** 挂载：
>
> ```
> $ grep ' /proc/sys ' /proc/mounts
> proc /proc/sys proc ro,nosuid,nodev,noexec,relatime 0 0
> ```
>
> `echo 1 > /proc/sys/net/...` 在容器内**必然失败**（`Read-only file system`），
> `NET_ADMIN` 救不了 —— ro 是挂载属性，不是 capability。
>
> 也就是说 v4 / v6 的"自愈"从未执行过。IPv4 之所以一路绿灯，只是因为宿主
> 本来就开着 `ip_forward=1`，走的是函数开头"已开直接返回"分支。
> 之前提示文案里"检查 NET_ADMIN capability / docker daemon 需要 CAP_SYS_ADMIN"
> 会把人引向完全错误的方向。
>
> **所以 forwarding 必须由宿主负责**（`docker-compose.yml` 里也有同样说明）：
>
> ```bash
> # 立即生效
> sudo sysctl -w net.ipv4.ip_forward=1
> sudo sysctl -w net.ipv6.conf.all.forwarding=1
> # 持久化（推荐）
> printf 'net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n' \
>   | sudo tee /etc/sysctl.d/99-ikev2.conf
> ```
>
> compose 里的 `sysctls:` 块**不能**用来解决 —— host 网络模式下 runc 会以
> `not allowed in host network namespace` 拒绝（v2-79.2 已实测）。

**L2 模式（macvlan / ipvlan）的 forwarding 归谁负责**（v2.86-PR20）：
容器有独立 netns，开关就在容器那个 netns 里。宿主 `sysctl -w` 改的是宿主 netns，
对容器**无效**，所以必须由 Docker 在建容器时注入 —— 注入方式与实测结论见 §12.8.2。

**v4 / v6 现在对等检查**（v2.86-PR19(A16) 起）。历史上 IPv6 侧被包在
`if [ "$IKEV2_IPV6_ONLY" != "true" ]` 里，导致默认配置（`=true`）下 IPv6 forwarding
恒为 0，且因为客户端拿不到 IPv6 地址（见 §13.1.3）而不显形。已移除该条件 ——
依据是 §5 的 `local_ts` 恒为双栈，`IKEV2_IPV6_ONLY` 只控制监听/拨号用的网络栈，
不影响 ESP 内层协议族。

**失败时的行为差异是有意的**：

| | 为 0 时 | 理由 |
|---|---|---|
| IPv4 | **FATAL** `exit 11` | 没有任何转发 → 所有客户端都出不了网，是核心功能 |
| IPv6 | **WARN** + 宿主侧命令 | 客户端会 IPv4 回退，面板与 VPN 均可用，属降级不属故障 |

唯一豁免：宿主内核 `CONFIG_IPV6=n` → `/proc/sys/net/ipv6` 不存在 → 跳过 + WARN，不 exit 11。

排障时注意区分两类日志：
`[persist] not mounted / not writable` 是**可忽略的**，
`WARN: net.ipv6...cannot auto-enable` 和 `FATAL ... cannot auto-enable` 才是真问题。

### 12.8.1 L2 模式的 IPv6 SLAAC 链路

host 模式从宿主网卡上直接读地址；L2 模式下容器没有配 v6 地址，公网 IPv6 靠
**上游路由器发 RA → 容器 SLAAC 自动配置 GUA** 这条链路。

```
上游路由器 ──RA──► 容器 eth0
                    ├─ ① disable_ipv6=0   ← 由 service networks 块 endpoint sysctls 保证
                    ├─ ② accept_ra=2      ← endpoint sysctls（主）+ conf.default（netns 级兜底）
                    └─ ③ 地址收敛（DAD + 等一个 RA 周期）
```

**为什么 `accept_ra` 必须是 2**：内核的**功能性默认**是
"forwarding 关闭时接受 RA、开启时不接受"。本项目必开 IP forwarding（§12.8），
因此不显式写 `2`，内核会把处于默认态的 `accept_ra` 强制改成 `0`，
**所有 RA 被静默丢弃**，症状是"容器起得来、v4 正常、就是拿不到 v6"。
这个默认值不是 bug 而是设计（RFC 4861），所以必须显式覆盖。

所需配置全部在 **L2 compose 文件里静态注入**：

| 位置 | 配置 | 不设的后果 |
|------|------|-----------|
| service 段 `networks.ikev2-l2.driver_opts` | `com.docker.network.endpoint.sysctls: "net.ipv6.conf.IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2"` | Docker 建端点时把 `disable_ipv6` 置 1，收不到任何 RA（实测 `dis6=1 gua=0`） |
| service 段 `sysctls:` | `net.ipv4.conf.all.forwarding=1` | 容器 netns 不转发 → 客户端出不了网 → `exit 11` |
| service 段 `sysctls:` | `net.ipv6.conf.all.forwarding=1` | v6 不转发（IPv6_ONLY 场景下是致命的） |
| service 段 `sysctls:` | `net.ipv6.conf.default.accept_ra=2` | forwarding 一开，RA 被内核丢弃（endpoint 级 `accept_ra=2` 是主通道，这条是 netns 级兜底） |

⚠️ **为什么 `disable_ipv6` 必须走 endpoint sysctls**：本项目的 L2 网络不带 v6
子网（`EnableIPv6=false`），Docker（≥26.0.2，CVE-2024-32473 修复后）建端点时会
**显式**把端口的 `disable_ipv6` 置 1。service 段 `sysctls:` 只能写
`conf.all`/`conf.default`（写 `IFNAME`/`eth0` 被 Docker 拒绝，见 §12.8.2），
管不到这个端点级写入；`networks.<name>.driver_opts` 虽是合法 Compose 字段，
但那是**网络级**配置，接口级 sysctl 会被静默忽略 —— v2.86-PR20
"endpoint sysctls 不生效"的错误结论就源于把它放到了这一层。正确位置是
**service 段 `networks.<net>.driver_opts`**（端点级），`IFNAME` 是字面占位符，
Docker 建端点时替换为真实网卡名（官方文档 "Use router-assigned IPv6 addresses"）。

**运行时回读复核**（entrypoint §3.2，仅 L2 模式）：静态注入是"声明"，复核才是
"事实"。entrypoint 在 forwarding 处理完之后按 `net_mode_sysctl_checks()`
逐条回读 `conf.IFNAME.disable_ipv6` / `conf.IFNAME.forwarding` /
`conf.IFNAME.accept_ra`：

| 读到 | 行为 |
|------|------|
| 四条全对 | `[slaac] eth0: net/ipv6/conf/eth0/disable_ipv6=0 net/ipv4/conf/eth0/forwarding=1 net/ipv6/conf/eth0/forwarding=1 net/ipv6/conf/eth0/accept_ra=2 — RA/SLAAC 可用` |
| 任一不符 | FATAL `exit 10` + 打印实际不符项与修复动作 |

复核用 here-doc 而不是管道喂 `while`：管道会把循环体放进子 shell，
循环内的赋值传不回来，"有不符合项"就会被**静默当成通过**。

不加这个复核的话，若某个 Docker / kernel 组合静默忽略了这些注入，
实例会"正常启动、v4 可用、v6 永远拿不到地址"——与 §11.1 里 `IKEV2_OUT_IF`
同类的静默故障。

⚠️ **`IKEV2_SERVER_ADDR_V6` 不是静态地址方案**：它只让 charon 把该地址填进
`local_addrs`，**不负责把地址配到网卡上**。L2 模式下若上游不发 RA，指定
`IKEV2_SERVER_ADDR_V6` 只会让 charon 去 bind 一个本机并不拥有的地址。
正确的静态做法是在容器内用 `ip -6 addr add` 或 networkd 配置，而不是设这个变量。

**地址收敛时序**（实测 Docker 28.5.2，macvlan，192.168.50.63 parent=ens18）：
GUA 的出现时间取决于上游 RA 周期，叠加 DAD 后实测 **2~5 秒**（连续 3 次测量
稳定在 2s，但留不出余量）。因此 L2 模式下公网 IPv6 探测走
**6 次 × 3s = 最多 15 秒**有限重试（§0.2 / §1），
host 模式保持单次（行为不变）。日志会打印实际收敛耗时：

```
[entrypoint] [auto-detect] Public IPv6 converged after ~3s (RA/SLAAC/DAD)
```

若 15 秒还没拿到，说明上游确实没发 RA 或被网络设备拦截，
此时按 §1 的 `exit 10` 提示排查，不要盲目加长等待。

### 12.8.2 L2 sysctl 的注入通道：netns 级走 service 段 `sysctls`，接口级走 endpoint sysctls

这一节记录 v2.86-PR20 的实测，以及后续对其**错误结论**的纠正：v2.86-PR20 得出
"只有 service 段 `sysctls:` 可用"，是因为当时把 endpoint sysctls driver option
放到了**网络级**（`networks.<name>`）—— 那确实不生效；放到**端点级**
（service 段 `networks.<net>.driver_opts`）就生效了。测试环境：Docker Engine
28.5.2 / Compose v2.40.3，宿主 Debian 12，parent 接口 `ens18`，
子网 `192.168.50.0/24`，上游路由器发 RA。

| 写法 | 结果 |
|------|------|
| `networks.<name>.options.com.docker.network.endpoint.sysctls` | ❌ `docker compose config` 直接报 `additional properties 'options' not allowed` —— **不是合法 Compose 字段** |
| 同一 driver option 放 `networks.<name>.driver_opts`（网络级），逗号串 / JSON 格式 | ❌ 合法但**静默忽略**，容器内 `v6fwd=0 ra=1 gua=0` —— v2.86-PR20 的错误结论由此而来，实际是放错了层级 |
| service 段 `sysctls:` 写 `IFNAME` 占位符 | ❌ Docker 报 `open sysctl net.ipv4.conf.IFNAME.forwarding: unsafe procfs detected` |
| service 段 `sysctls:` 写 `eth0` | ❌ Docker 报 `interface specific sysctl setting "net.ipv4.conf.eth0.forwarding" must be supplied using driver option ...` —— 报错本身就是答案：接口级要走 driver option，但得放在**端点**上 |
| service 段 `sysctls:` 写 `conf.all` / `conf.default`（仅 netns 级三条） | ✅ `v4fwd=1 v6fwd=1` 及 `conf.default` 的 `accept_ra=2` 生效；但端点 `disable_ipv6` 仍被 Docker 置 1（无 v6 子网时），GUA 拿不到 |
| **service 段 `networks.<net>.driver_opts` 放 `com.docker.network.endpoint.sysctls`（端点级，`IFNAME` 占位符）** | ✅ 端点建好后回写，`disable_ipv6=0 accept_ra=2` 生效，上游 RA 可达 |

**最终形态**：

```yaml
services:
  ikev2-panel:
    sysctls:                  # netns 级：service 段只允许 conf.all / conf.default
      net.ipv4.conf.all.forwarding: "1"
      net.ipv6.conf.all.forwarding: "1"
      net.ipv6.conf.default.accept_ra: "2"
    networks:
      ikev2-l2:
        ipv4_address: 192.168.50.63
        driver_opts:          # 端点级：接口级 sysctl 的唯一正确位置
          com.docker.network.endpoint.sysctls: "net.ipv6.conf.IFNAME.disable_ipv6=0,net.ipv6.conf.IFNAME.accept_ra=2"

networks:
  ikev2-l2: {}                # ⚠️ 不要 enable_ipv6: true，也不要配 v6 子网 ——
                              # 无 v6 子网却开 enable_ipv6 会让 Docker 自动分
                              # ULA 段（fd00::/8），容器拿到不路由的 ULA + 假默认路由
```

⚠️ **host 文件绝对不能有 service 段 `sysctls:`**：host 模式下容器与宿主共享
netns，runc 会以 `not allowed in host network namespace` 拒绝，容器直接起不来。
`tests/network_mode_test.sh` 的 NM-39 守护这一点。

容器内 `/proc/sys` 是 **ro** 挂载（`proc /proc/sys proc ro,nosuid,nodev,noexec,relatime`），
`NET_ADMIN` 也写不了 —— 所以这些值只能由 Docker 在建容器时注入，
entrypoint 没有自愈余地，只能**回读复核**（§12.8.1）。

### 12.8.3 nft 规则完整性校验：先判断"适不适用"，再决定要不要重试

entrypoint §7.9 有一段重试循环：反复 `check_ikev2_rules`，不通过就
`re_add_ikev2_rules` 再试，直到规则稳定为止。设计意图是好的 —— nft 表可能被
docker 或 nft 服务重置，重试能自愈。

但 v2.86-PR20 之前，这个循环是**无条件**执行的，而它校验的规则表
（`ikev2` / `ikev26`）只在特定条件下才装载：§3.5 仅当
`IKEV2_IPV6_ONLY=true` **且**探测到 `V4_GW` 时才装表。于是纯 IPv6 主机
（无 IPv4 默认网关）走的是一条"压根不该有待校验的规则"的路径，却要跑满
`10×1s + 50×5s = 260 秒`，然后写下一个 `iptables-warn.flag`，让面板 banner
报「VPN 客户端无法访问公网」。

> 这是**假告警**：不是规则不稳定，而是那条路径不适用。更糟的是首次部署的人
> 只会看到容器启动卡 4 分钟，典型的"以为挂了"。

修复方式是让 §3.5 在真正进入装载分支时置 `NFT_EXPECTED=1`，§7.9 据此守卫：

```
§3.5 装表分支  →  NFT_EXPECTED=1
        ↓
§7.9  if NFT_EXPECTED != 1  →  打一行 "not applicable ... skipping" 直接跳过
        else                 →  原有 260 秒重试逻辑不变
```

日志里会明确写出跳过原因（`IPV6_ONLY` 与 `V4_GW` 的实际值），排查时一眼能确认
是"不适用"还是"真的失败"。真正适用的路径重试行为与上限**完全没有改动** ——
这是"该等的时候照等"，不是"把等待时间调短"。

### 12.9 前端两条硬边界：CSP 与模板转义

这两条互相咬合，改前端时**同时**破坏会得到"页面看起来正常但按钮全失灵"或
"XSS 已经成立但没有任何报错"。

#### 边界一：CSP 没有 `'unsafe-inline'`，所以内联事件处理器一律失效

生产 CSP 等价于（`internal/web/secure_headers.go`）：

```
script-src 'self' 'nonce-{每响应随机}'
```

**没有** `'unsafe-inline'`，**也没有** `'unsafe-hashes'`。因此：

| 写法 | 结果 |
|------|------|
| `<script nonce="{{.CSPNonce}}">…</script>` | ✅ 执行 |
| `onclick="return confirm('…')"` | ❌ 浏览器直接拒绝，控制台报 `Refused to execute inline event handler` |
| `<a href="…" onclick="…">` | ❌ 同上 |

所以**二次确认必须走 `data-confirm`**，由 `layout.html` 里 `initConfirmGuard()`
在 `document` 上做事件委托（capture 阶段）统一处理：

```html
<button data-confirm="确认重置 bob 的密码？">重置密码</button>
<form data-confirm="确认删除？"> … </form>
```

`<form data-confirm>` 走 `submit` 阶段；按钮走 `click` 阶段并显式跳过 `FORM`
（否则同一次点击会弹两次）。委托而非逐元素绑定，元素被动态替换也不失效。

> 历史教训：这个坑**静默**。CSP 拒绝内联处理器不报错、不影响 CSS，
> 页面"看起来完全正常"，只是破坏性操作点一下就真执行。
> 修法是加 `data-confirm`，**不是**给 CSP 加 `'unsafe-hashes'` ——
> 后者等于对所有内联处理器放行，XSS 防御就只剩"攻击者不写 onclick"了。

#### 边界二：`.Msg` 必须转义，`MsgHTML` 只能是字面量

banner 模板有两条渲染路径：

```gotemplate
<div class="banner-body">
  {{if .MsgHTML}}{{.MsgHTML | safeHTML}}{{else}}{{.Msg}}{{end}}
</div>
```

| 字段 | 走哪条 | 能放什么 |
|------|--------|---------|
| `.Msg` | Go 模板**自动转义** | 任意文本，**包括用户输入和外部 API 返回的错误串** |
| `.MsgHTML` | `safeHTML` | **仅代码里的字面量 HTML** |

`Flash.Message` 一律进 `.Msg`。`.MsgHTML` 里出现 `{{.Something}}` 就是 bug ——
`safeHTML` 之后模板不再转义，等于把该字段变成 XSS 注入点。需要"标题 + 动态值"时，
拆成两个元素：字面量标题进 `.MsgHTML`，动态值进同级独立元素走默认转义。

回归断言在 `internal/web/p0_banner_confirm_test.go`：
禁止模板里出现真实 inline 事件属性、禁止 `MsgHTML` 含模板插值、
禁止 `.Msg` 含字面 HTML 标签、XSS payload 进 `.Msg` 必须仍被转义。

#### FlashStore：为什么 flash 不走 query string

`?flash=` / `?error=` 这类 query 参数曾经是反射型 XSS 的入口（2026-10-08 复核确认）。
现在 flash 全部走 `internal/web/flash.go` 的 `FlashStore`：

- 存服务端内存，**不写 URL**，刷新/收藏/分享链接都不携带
- 绑定 session ID，**取一次即消费**
- 自动过期

### 12.10 滚动位置：两个独立机制都得管

"刷新后不在最顶部"这个症状有**两个互不相关**的原因，只修一个仍然复现：

| # | 机制 | 症状 | 修法 |
|---|------|------|------|
| 1 | 应用自己的 `sessionStorage['ikev2-scroll-y']` | 提交过一次表单后，**之后每次加载**（含点导航、刷新）都被甩回那个旧位置 | key 必须**一次性消费**：读出来立刻删。写在 `restore()` 里所有 `return` / 异常之前 |
| 2 | 浏览器原生 `history.scrollRestoration` | 刷新时浏览器自己恢复一遍，和应用逻辑打架 | `<head>` 里 `history.scrollRestoration = 'manual'`。**必须早于浏览器恢复时机**，写在 `<body>` 之后就晚了 |

恢复优先级：`hash` 锚点 → `sessionStorage` 记录 → 否则停在最顶部。
只有**真实发生导航**的表单提交才记位置：`submit` 被 `preventDefault` 掉时不能记，
否则残留记录会把下一次无关的加载甩到旧位置。

锚点不被 sticky 顶栏盖住靠 CSS，不需要 JS：

```css
html { scroll-padding-top: calc(var(--ikev2-topbar-h) + 0.75rem); }
```

程序化复位用 `behavior: 'instant'`，避免页面加载时出现一段动画位移。

### 12.11 文本选择：装饰禁选，数据可复制

管理后台双击 / 拖拽会误选一片界面文字，选中块高亮后还要再点一次才能取消。
但**不能一刀切禁选** —— 本面板里初始密码、服务器地址、swanctl 命令、证书参数、
报错详情都靠手动复制（报错还要复制去搜解决方案）。禁掉等于废功能。

采用行业惯例（GitHub / GitLab / Vercel 后台）：**界面装饰禁选，数据可复制**。

实现方向是「**默认禁 + 白名单放开**」，不是逐个禁用 —— 后者漏一处就失效，
而且新加的组件默认仍可被误选：

```css
/* style.css §2.1 */
body{ -webkit-user-select:none; user-select:none;}

/* 白名单：缺任何一条都是功能受损 */
code,pre,kbd,samp, input,textarea,select, option,
.banner-body, .one-time-secret,
.timeline-time,.timeline-event,.timeline-actor,.timeline-detail,
.kv-inline, .users-grid-row .cell-name, dd{
  -webkit-user-select:text; user-select:text;
}
.one-time-secret code{ -webkit-user-select:all; user-select:all;}
```

两个必须记住的点：

1. **`-webkit-user-select` 不能省。** Safari / iOS Safari 只认带前缀的那个，
   漏了整条策略在这些平台上静默失效。`internal/web/p2_text_selection_test.go`
   的 `TestSel_NoWebkitPrefixGap` 会全量扫描 `style.css`，任何一处漏前缀即失败
   —— 这条断言已经抓到过 `.dismiss` 与 `.settings-card > summary` 两处存量问题。

2. **`summary` 必须显式禁选。** 它是 UA 默认允许选择的少数元素之一，
   不覆盖的话折叠面板标题仍能误选（`.settings-card > summary` 另有一条带
   `::before` 箭头的独立规则）。

一次性密码用 `user-select:all` 而非 `text`：双击只选中一段，用户很容易漏掉
前后字符。

### 12.12 transform 不能同时承担定位和动效

写这个约定是因为踩过：`.password-toggle`（密码框里的 eye 图标）用
`transform:translateY(-50%)` 做垂直居中，而全局有

```css
button:active{ transform:translateY(1px); }   /* 全局按压反馈 */
```

`button:active` 特异性 `(0,1,1)` > `.password-toggle` 的 `(0,1,0)`，
`:active` 一命中就把 `translateY(-50%)` **整条覆盖** —— 居中失效，图标按压时
下坠约 18px。该按钮的 `transition` 又不含 `transform`，位移是瞬时的，
iOS 上 `:active` 粘住让现象更明显。症状是"点一下按钮跳出去"。

**两条规则**：

1. **绝对定位元素的居中走 `top`/`bottom` + `margin:auto`**，不用 `transform`。
   全局 `button:active` / `button[type=submit]:active` 都会动 `transform`，
   用它定位等于把定位交给一个会被覆盖的属性。

   ```css
   /* ✅ */
   .password-toggle{
     position:absolute; right:0.45rem;
     top:0; bottom:0; margin-top:auto; margin-bottom:auto;
   }
   ```

2. **浮在表单控件内部的图标按钮要显式断掉按压位移**（`transform:none`）。
   居中改掉后全局的 1px 仍然生效，而这类图标"浮在输入框里"，1px 位移
   同样被看成错位。它们的按压反馈已由 `:hover` / `[aria-pressed]` 的
   底色 + 描边 + 图标变色承担，不需要位移。

   流式按钮（登录按钮、表格操作按钮）**保留**全局的 1px 下沉 —— 那是有效的
   按压反馈，不要一刀切禁掉。

回归测试 `internal/web/p3_press_offset_test.go`：`TestP3_NoAbsoluteCenteringViaTransform`
在整个 `style.css` 上拦截「绝对定位 + `top:50%` + `translateY(-50%)`」组合，
防止同样的写法被复制到别的浮层按钮上。

> ⚠️ **改完 CSS 必须实机验证。** CSS 改动不进浏览器等于没改 ——
> 本地工作区改完就宣布"修好了"是错的，要重建镜像部署后在真机确认。
> 页面 CSS 走 `assetVersion()` cache-bust（基于 mtime），不需要手动清浏览器缓存。

## 13. 已知陷阱

只列**今天仍然成立**的坑。历史上已修掉的不列 —— 那是 [CHANGELOG](CHANGELOG.md) 的事。

### 13.1 配置与加载

| 陷阱 | 症状 | 怎么办 |
|------|------|--------|
| charon 起来了但连接列表是空的 | 客户端发 IKE 包没人应答 | entrypoint 必须显式调一次 `swanctl --load-all` |
| 改了密码不生效 | 旧口令还能连 | 凭据要 `--load-all` 才重载，单纯 reload 不行 |
| 改 IP 段时其他用户断流 | 旧连接被卸载重装 | 固有行为。本项目只有一条连接，细粒度重载无收益（[§12.1](#121-配置下发的三条路径)）；reload 失败会原子回滚磁盘配置 |
| DB 写成功但 swanctl 写失败 | 用户能登面板但连不上 VPN | 必须事务化：回滚 DB + 删文件（`user_pipeline.go`） |
| DB 的 `enabled` 不参与认证 | 以为禁用了其实还能连 | 真正开关是子配置是否存在（[§12.3](#123-吊销用户的三步顺序)） |

### 13.1.1 ⚠️ nft base chain 的 `accept` 不是终态

**这是本项目踩过的最深的一个坑**，症状极具迷惑性：VPN 拨号成功、CHILD_SA `INSTABLISHED`、
`swanctl --list-sas` 里 `in` 字节数在涨，但 `out` 恒为 0 —— 客户端能连上却完全上不了网。

**根因**：宿主 net ns 里 Docker daemon 建了 `table ip filter`，其 `FORWARD` chain 是

```
type filter hook forward priority filter; policy drop;
```

我们在 `scripts/ikev2.nft.template` 里写的也是 `hook forward priority 0; policy accept;
iifname "ipsec0" accept`。

**nft 的 `accept` 判定在 base chain 里只终止「当前这条 base chain」的求值，包会继续流向
同hook 上优先级更大的下一条 base chain**。所以哪怕把我们的 priority 调到 `-10` 抢在
docker 前面并 accept，包也会继续走 `priority filter(0)` → 空`DOCKER-USER` →
`DOCKER-FORWARD` → 无 accept → 命中 docker 的 `policy drop` 被丢弃。

实测证据（修复前）：

```
ikev2.mangle_forward (priority mangle=-150)   counter packets 1424   ← 包进了 forward hook
ikev2.postrouting `ip saddr 10.10.0.0/24` NAT counter packets 0      ← 却NAT 不到
iptables -L FORWARD -n -v: policy DROP 6048 packets
  1  6208 DOCKER-USER      ← 空链，包进进出出
  2  6208 DOCKER-FORWARD
```

**正确解法**：用 **iptables** 往 `DOCKER-USER` 链插规则 —— iptables 的 `-j ACCEPT` 是
**真终态**（终止链遍历 + 跳过其余 FORWARD 规则 + 不套用链尾 policy），而 `DOCKER-USER`
正是 Docker 官方文档指定的用户自定义规则插入点，不会被 daemon 清理。

实现见 `scripts/entrypoint.sh` 的 `ensure_docker_user_accept()`（v4/v6 × in/out 共 4 条，
先用 `-C` 检查再 `-I`，幂等）。

> 模板里那两条 `ipsec0 accept` 保留着，但它们当前**是no-op**（本链 policy 已是 accept），
> 只是意图声明。不要以为调它们的 priority 能修这个症状。

**另一个已排除的假设**：曾怀疑「nft 提交规则时要求 `iifname` 指向的接口已存在，
而 `ipsec0` 由 charon 在 §6 才创建」。在 192.168.50.63 上用 NET_ADMIN 容器实测 5 种组合，
全部通过：

```
iifname "nosuchif0" accept              → 加载成功
oifname "nosuchif0" accept              → 加载成功
含 ipsec0 引用的完整模板（接口全不存在）→ 加载成功
表已存在时 delete + nft -f              → 加载成功
表已存在时直接叠加 nft -f→ 加载成功
```

**接口不存在不是问题**。当时观察到的 `Operation not supported` 是排查过程中手工往宿主
nft 加载过重复 chain 定义污染环境所致。教训：不要在生产宿主上用 `nft -f` 直接热加载做实验，
`nft -f` 对同名 chain 是**追加**而非替换，会污染状态。

### 13.1.2 `eap_id` 的 `%identity` vs `%any`：约束 vs 索要

| 写法 | 语义 | 后果 |
|------|------|------|
| `eap_id = %identity` | **约束** —— 要求 EAP 用户名必须等于客户端 IKEv2 ID，不等就拒 | iOS 把 `LocalIdentifier` 标成 `ID_FQDN` 而非 EAP 类型，**类型不匹配同样算 not matched** |
| `eap_id = %any` | **索要** —— 发 EAP-Identity Request 向客户端要用户名，不做等值比较 | 官方 EAP 模板（windowsEapServerConf）用这个 |

日志对照：

```
constraint check failed: EAP identity '%identity' (ID_FQDN) required,
 not matched by 'rewind' (ID_FQDN)
```

注意这个报错的迷惑性 —— **两边字面都是 `rewind`，仍然失败**，因为约束比较的是
「ID 类型 + 值」，`ID_FQDN` ≠ EAP 类型。

对 Android 更重要：strongSwan app 可能用 `ID_RSA_SIGN` / `ID_FQDN` / `ID_KEY_ID`
等多种 local ID 类型，`%any` 对 ID 类型不敏感，是**最兼容**的选择。

配套前提：`eap_id = %any` 需要服务端装了 `eap-identity` 插件才能发出该请求。缺插件时报
`EAP-Identity request configured, but not supported`，与 `eap_id` 写法无关。

### 13.1.3 ⚠️ 单个 pool 的 `addrs` 不支持混合地址族

**症状**：配置里明明写了 IPv6 段，`swanctl` 也报"加载成功"，但客户端只拿到 IPv4 地址，
且**没有任何警告或报错**。

本项目 `configs/swanctl-ipv6-only.conf` 的写法：

```
pools {
    ikev2-pool {
        addrs = 10.10.0.0/24, fd00:1::/64     # ← IPv6 段会被静默丢弃
        dns   = 1.1.1.1, 8.8.8.8, 2606:4700:4700::1111, 2001:4860:4860::8888
    }
}
```

**实测证据**（192.168.50.63，strongSwan 6.0.1，两组对照）：

| `addrs` | `swanctl --list-pools` |
|---|---|
| `fd00:1::/64` | `fd00:1::1` 0 / 0 / **2147483646** ✅ |
| `10.10.0.0/24, fd00:1::/64` | `10.10.0.1` 0 / 0 / **254** ❌ |

IPv6 单独用完全正常（64 位地址空间 = 2^64−2 条），一旦和 IPv4 混写，IPv6 段直接消失。
swanctl 依然输出：

```
loaded pool 'ikev2-pool'
successfully loaded 1 pools, 0 unloaded
```

**正确写法**（官方 [Virtual IP Addresses](https://docs.strongswan.org/docs/latest/features/vip.html#_responder_configuration)
要求按地址族拆成多个 pool）：

```
connections.ikev2-rw.pools = v4pool, v6pool
pools.v4pool.addrs = 10.10.0.0/24
pools.v6pool.addrs = fd00:1::/64
```

配套条件：客户端要拿到双栈，需在 `vips` 里显式请求 `0.0.0.0, ::`。

**对本项目的实际影响：目前没有，且不应贸然修**。

当前客户端（iOS / Android）只拿到 `10.10.0.1`，走 IPv4，功能完整 —— CHILD_SA 的
remote TS 就是 `10.10.0.1/32`，本来就不会产生 IPv6 内层流量。所以
`nft table ip6 ikev26` 的 counter 恒为 0 是**正确行为**，不是转发故障。

改成双栈属于**行为变更**（客户端地址形态变化），且有一个已知风险：entrypoint 里记录过
"家用路由器对 LAN→WAN 的 IPv6 出向有防火墙限制（`curl -6 ipv6.google.com` timeout）" ——
客户端一旦真的开始发 IPv6，可能反而变慢甚至不如现在的 IPv4 路径。要做需单独评估与验证。

> 排查陷阱：验证 pool 时 `swanctl --load-pools` 可能报
> `vici pool ikev2-pool has online leases, unable to replace` —— 有在线租约时
> 换不了配置。先 `swanctl --terminate --child ikev2-rw` 再重载。
> 注意 `--terminate` 不带 `--child` 会报 `missing terminate selector`。

### 13.2 VICI 与权限

| 陷阱 | 症状 | 怎么办 |
|------|------|--------|
| `charon.vici` socket 权限 | govici 连接失败 | vici 插件**硬编码 `0770 root:root`，无配置项**；改 `strongswan.conf` 没用。只能保证进程同 namespace 且能访问该组 |
| `--load-all` 读不到 conf | charon 报错 | 面板进程以 root 跑，文件写 0600 |

### 13.3 客户端兼容性

| 陷阱 | 症状 | 怎么办 |
|------|------|--------|
| mobileconfig 没内联 CA | iOS 弹"证书不可信" | 必须内联 base64 CA |
| Android 内置 IKEv2 客户端缺 CA | Android 弹"证书不可信" | 客户端侧导入 CA 文件 |
| server cert CN 与客户端填的 ID 不一致 | `IKE_AUTH` 失败 | CN 必须等于 mobileconfig 的 `RemoteIdentifier` |
| iOS 拒绝 ECDSA 证书 | iPhone 静默连接失败 | 证书用 RSA 2048 |
| iOS 17+ 把 UDP 53 也路由进 ESP | 联网正常但 Safari 报"没连接互联网" | mobileconfig 必须带 `<key>DNS</key>` dict |
| 客户端收不到过期时间 | 过期后客户端不提示 | mobileconfig 不含过期字段，靠服务端拒绝（[§12.5](#125-mobileconfig-易错点)） |

### 13.4 运行环境

| 陷阱 | 症状 | 怎么办 |
|------|------|--------|
| 宿主被占 IPv4/IPv6 UDP 500 | charon `Address already in use` | 默认 IPv6-only 可绕开大部分；仍冲突就停掉占用方 |
| 宿主 IPv4/IPv6 forwarding 为 0 | v4：客户端能连上但出不了网；v6：同上但客户端会 IPv4 回退，**表面上"能上网"** | entrypoint 无条件自愈，失败则 `exit 11`（[§12.8](#128-ipv6-forwarding-自愈)） |
| 宿主换网卡名 | 容器找不到出接口 | 不设 `IKEV2_OUT_IF` 让 entrypoint 自动探测 |
| 宿主 LAN 段与 VPN 段冲突 | 客户端 VIP 与 LAN 同段 | 不设 `IKEV2_VPN_SUBNET` 让 entrypoint 自动选 |
| ISP 重拨 IPv6 前缀 | `local_addrs` 写死导致连不上 | `ipv6watch.go` 每 60s 扫 `/proc/net/if_inet6`，变了就改并重载 |
| LE 续期失败 | 证书过期，全员断连 | `renew-cert.sh` 会备份 + 置 `LAST_RENEW_FAILED`，面板首页红字横幅 |
| ACME HTTP-01 的 80 端口被占 | 签发失败 | 本项目默认走 DNS-01，不受影响 |
| IPv6 ESP 需要 kernel 5.8+ | 老内核上 IPv6 客户端连不上 | 换宿主内核或用双栈 |
| 字节数超 2^53 | 面板流量显示精度丢失 | SQLite 侧是 int64 没问题；前端展示要做单位换算 |
| 改了 `IKEV2_NETWORK_MODE` 但没重建容器 | 容器仍跑旧模式：设了 macvlan 却在 host netns 里 | entrypoint 检测到 `docker0` 可见会 WARN；**必须 `docker compose down && ./scripts/up.sh`** |
| L2 模式下沿用宿主网卡名做 `IKEV2_OUT_IF` | 客户端能连 VPN 但出不了网（MASQUERADE 静默失效） | entrypoint 已 FATAL `exit 14` 拦截；修复是**删掉这一行让它自动探测**（容器内通常是 `eth0`） |
| L2 模式下容器拿不到 v6 地址 | 容器起得来、v4 正常、就是没有 v6 | 查上游是否发 RA；entrypoint §3.2 已复核 sysctl，不符则 `exit 10`（[§12.8.1](#1281-l2-模式的-ipv6-slaac-链路)） |
| macvlan / ipvlan 下容器 ping 不到宿主 | 内核层隔离，不是 bug | 需要宿主↔容器互通就建 shim，见 [README](../README.md#三网络模式与前置条件) |
| `IKEV2_L2_SUBNET` 写了 v6 子网 | Docker 会接管 v6 并分 ULA，公网不可达 | `up.sh` / entrypoint 均 FATAL 拒绝；v6 一律走 RA/SLAAC |

### 13.5 架构性限制

| 限制 | 说明 |
|------|------|
| **libipsec 单核瓶颈** | ESP 走用户态解密，单 SA 绑单线程，**不能多核扩展、不支持网卡 offload**。定位是家用 / 小团队，不是高带宽场景 |
| **限速只作用于上传** | tc filter 匹配 `src`，下载方向不限（[§12.6](#126-限速u32-与-flower-按-family-分支)） |
| **DDNS 会自己建记录** | 记录不存在时自动 `AddDomainRecord`（需 RAM 有 `alidns:AddDomainRecord`）；想彻底停掉对外解析要在面板里关掉 DDNS 开关，删记录会被下个周期重新建回来 |
| **govici 仍是 v0.x** | 锁定版本；用到的子集（`list-sas` / `terminate` / `version`）是稳定命令 |
| **macvlan / ipvlan 只在 Linux 可用** | Docker Desktop（macOS / Windows）不支持；rootless 模式支持不完整（[§11.1](#111-网络模式host--macvlan--ipvlanv286-pr20)） |
| **L2 模式下容器与宿主隔离** | 内核层隔离，需要互通得建 shim；这是选 L2 模式要付的代价 |
| **L2 模式 IPv6 完全依赖上游 RA** | 上游不发 RA 就拿不到 GUA；`IKEV2_SERVER_ADDR_V6` 只填 charon 的 `local_addrs`，不负责配地址（[§12.8.1](#1281-l2-模式的-ipv6-slaac-链路)） |
| **切换网络模式必须重建容器** | 环境变量改动只在创建时生效；entrypoint 会 WARN 提示，但正确做法永远是 `down` + `up` |

## 14. 本地开发与调试

### 14.1 不上 Docker 跑面板

```bash
sudo apt install strongswan strongswan-swanctl
sudo ipsec start

export IKEV2_DATA_DIR=$HOME/.ikev2-panel-dev
export IKEV2_LISTEN_ADDR=127.0.0.1:8443
export IKEV2_SERVER_CN=vpn.example.com
go run ./cmd/ikev2-panel
```

⚠️ **dev 模式的判定依据是 `/etc/swanctl` 是否存在**，不是 VICI 连不通。
命中 dev 模式后：用户配置写到 `<DataDir>/swanctl-conf.d/`，并**跳过所有 VICI 操作**。

`Makefile` 里有 `make dev` / `make dev-stop` / `make dev-logs`。

### 14.2 常用排查命令

```bash
# charon 在干什么
tail -f ./logs/charon.log

# 当前 SA
docker compose exec ikev2-panel swanctl --list-sas

# 直接试一条配置命令（不经过面板）
docker compose exec ikev2-panel swanctl --load-conn --list

# DNS 记录到底存不存在（别用 dig，见 docs/README 排查方法论）
docker compose exec ikev2-panel /usr/local/bin/dnsq <domain> <rr>

# 看数据库（/data 是命名卷，宿主上不在项目目录）
docker exec ikev2-panel sqlite3 /data/panel.db \
  "select username, enabled, expires_at, speed_limit_mbps from users;"
```

### 14.3 改配置不重启 charon

写完 `swanctl.conf.d/` 下的文件后 `swanctl --load-all` 即可，
不需要重启 charon（但会中断现有 SA）。

### 14.4 验证清单

改动 swanctl / 认证 / 路由相关代码后，至少跑一遍 [§9.4](#94-必做的真机验收) 的 14 步。
纯 Go 逻辑改动 `make test`（= `go test ./...`）即可；涉及并发的包加 `make race`。

---

## 15. 文档状态

| 文档 | 覆盖什么 | 权威性 |
|------|----------|--------|
| 本文件 | 包结构、数据流、模块边界、数据模型、部署、陷阱 | 改代码前看 |
| [API.md](API.md) | 49 条端点、三条鉴权带、请求/响应体 | 接前端 / 加端点时看 |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | 按症状组织的排查路径 | 出问题时看 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更 + 破坏性变更标注 | 升级前必看 |
| [.env.example](../.env.example) | **全部环境变量** | 配置项的唯一权威 |

**本文件描述代码当前状态，不是设计意图。** 若发现与代码不符，以代码为准并顺手改文档。
版本历史只在 CHANGELOG 里维护，不要在本文档里写"v2-xx 改成了什么"。
- [ ] v2-79.1 用户实测（生产环境验收）