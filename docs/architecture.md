# IKEv2 VPN 面板 v2 架构理解文档（全新）

**日期：** 2026-09-16
**配套文档：** [`2026-09-16-ikev2-panel-v2-design.md`](../specs/2026-09-16-ikev2-panel-v2-design.md)
**目标读者：** 接手开发 v2 的工程师
**本文档目的：** 让一个新加入的工程师，**无需读设计文档**，就能知道代码怎么组织、模块怎么协作、怎么本地跑起来。

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

## 1. 项目结构（建议）

```
ikev2-panel-v2/                      # 新仓库，跟 v1 完全脱钩
├── cmd/
│   └── ikev2-panel/
│       └── main.go                  # 入口：flag 解析 + wire 各模块 + HTTP serve
├── internal/
│   ├── config/
│   │   └── config.go                # 环境变量/默认值
│   ├── store/
│   │   ├── store.go                 # SQLite 打开、迁移、连接池
│   │   ├── users.go                 # users 表 CRUD
│   │   ├── sessions.go              # sessions 表 CRUD
│   │   └── admins.go                # admins 表 CRUD
│   ├── cert/
│   │   ├── generate.go              # 启动时检测/生成 CA + server cert + 面板 cert（自签模式）
│   │   ├── mobileconfig.go          # .mobileconfig 字符串渲染
│   │   ├── acme.go                  # Let's Encrypt 模式：调 acme.sh --issue / --renew
│   │   └── acme_renew.go            # 后台 goroutine：每日检查证书有效期 + 触发续签脚本
│   ├── health/
│   │   └── certcheck.go             # 60s 检查 LAST_RENEW_FAILED 标志 + 渲染告警横幅
│   ├── swanctl/
│   │   ├── writer.go                # 用户子配置读写
│   │   ├── loader.go                # VICI load-all / load-creds（M8 commit 25）
│   │   ├── parser.go                # VICI list-sas streaming（替代原 swanctl --list-sas --raw 解析）
│   │   ├── terminate.go             # VICI terminate-IKE / terminate-all
│   │   ├── ipv6watch.go             # 公网 IPv6 变化感知（v2-72 起；v2-79 文档化）
│   │   └── ipv6.go                  # 根据 IKEV2_IPV6_ONLY 生成主配置模板
│   ├── limit/
│   │   ├── limiter.go               # 写限速文件，tc 调用封装
│   │   └── collector.go             # 解析 SA 输出并累加到数据库
│   ├── expiry/
│   │   └── expiry.go                # 定时扫描过期用户，禁用 + 强制下线
│   ├── web/
│   │   ├── server.go                # http.ServeMux 路由注册
│   │   ├── middleware.go            # auth, csrf, ratelimit, logging
│   │   ├── handlers_auth.go         # /login, /logout
│   │   ├── handlers_users.go        # /users CRUD
│   │   ├── handlers_home.go         # / 首页
│   │   ├── handlers_health.go       # /healthz
│   │   └── templates.go             # html/template 加载
│   └── auth/
│       ├── password.go              # 随机密码生成 + bcrypt 校验
│       └── session.go               # cookie + CSRF 工具
├── web/
│   ├── templates/                   # html/template 文件
│   │   ├── layout.html
│   │   ├── login.html
│   │   ├── home.html
│   │   ├── users_list.html
│   │   ├── user_new.html
│   │   └── user_detail.html
│   └── static/
│       └── style.css                # 单一 CSS，朴素样式
├── Dockerfile                       # 单 stage
├── docker-compose.yml               # 单 service
├── entrypoint.sh                    # 容器入口：生成证书 + 启动 charon + 启动 Go
├── .env.example                     # 环境变量样例
├── backup.sh                        # 数据备份脚本（建议 cron 跑）
├── README.md                        # 用户层文档：怎么部署、怎么用
└── go.mod
```

**目标行数：**

| 模块 | 预期行数 |
|---|---|
| `cmd/ikev2-panel/main.go` | ~80 |
| `internal/config/` | ~60 |
| `internal/store/` | ~450 |
| `internal/cert/` | ~400 |
| `internal/swanctl/` | ~300 |
| `internal/limit/` | ~150 |
| `internal/expiry/` | ~80 |
| `internal/auth/` | ~120 |
| `internal/web/` | ~700 |
| `web/templates/` | ~350 |
| `web/static/style.css` | ~100 |
| `Dockerfile` + `docker-compose.yml` | ~40 |
| `entrypoint.sh` | ~50 |
| `ikev2-updown` (shell 脚本) | ~50 |
| **总计** | **~2930 行** |

**对比 v1：v1 是 8000+ 行，v2 是 ~2930 行。**

---

## 2. 模块依赖图

```
                 ┌────────────────────┐
                 │  cmd/ikev2-panel   │
                 │     main.go        │
                 └──────────┬─────────┘
                            │
       ┌────────────────────┼────────────────────┐
       │                    │                    │
       ▼                    ▼                    ▼
┌─────────────┐    ┌────────────────┐    ┌──────────────┐
│  config     │    │  web/server    │    │  cert/       │
│             │    │  (http mux)    │    │  generate    │
└─────────────┘    └────────┬───────┘    └──────────────┘
                           │
        ┌──────────────────┼──────────────────┬──────────────┐
        │                  │                  │              │
        ▼                  ▼                  ▼              ▼
  ┌──────────┐    ┌────────────────┐    ┌────────────┐  ┌──────────────┐
  │ auth/    │    │ web/handlers_* │    │ store/     │  │ swanctl/     │
  │ session  │    │                │    │ users      │  │ writer+loader│
  └──────────┘    └────────┬───────┘    └────────────┘  └──────────────┘
                           │
                  ┌────────┴─────────┐
                  │                  │
                  ▼                  ▼
          ┌────────────────┐  ┌──────────────┐
          │ cert/          │  │ swanctl/     │
          │ mobileconfig   │  │ parser       │
          └────────────────┘  └──────────────┘
```

**依赖规则：**

- `cmd/` 只 wire，不含业务逻辑
- `web/handlers_*` 通过构造函数注入 `store`/`swanctl`/`cert`，**不直接 new**
- `store/` 只依赖 `database/sql` + `modernc.org/sqlite`
- `swanctl/` 只依赖 `os/exec` + 字符串解析
- `cert/` 只依赖标准库 + `crypto/x509`
- `auth/` 只依赖 `crypto/bcrypt` + `crypto/rand`
- **没有循环依赖**

---

## 3. 数据流

### 3.1 启动数据流（v2-79 含 §0 自动探测链）

```
entrypoint.sh                                    # 容器入口（PID 1）
  │
  ├─► §0 自动探测链（v2-79 设计）
  │     ├─► §0.1 detect_out_if()        # 网卡：env > ip -4 route default > ip -6 > 首个非 lo UP
  │     ├─► §0.2 detect_public_ipv6()   # IPv6：env > /proc/net/if_inet6 (2000::/3)
  │     ├─► §0.3 detect_public_ipv4()   # IPv4：env > OUT_IF 上非 RFC1918 首个（仅 IPv6_ONLY=false）
  │     ├─► §0.4 detect_v4_gateway()    # IPv4 GW：ip -4 route default
  │     ├─► §0.5 detect_lan_subnets()   # LAN 段：ip -4 addr 聚合 /24
  │     └─► §0.6 detect_vpn_subnet()    # VPN 子网：从候选（10.10/13/17/42/66.0.0/24）选不冲突
  │
  ├─► §1 IPv6 公网地址硬校验（IPv6_ONLY=true 时）
  │     └─► 还没探测到 → FATAL 退出 10
  │
  ├─► §3 IP forwarding 校验（三层兜底：L1 compose sysctls → L2 自愈 → L3 FATAL）
  │     ├─► L1 docker-compose.yml sysctls: 让 docker daemon 起容器前改宿主内核
  │     ├─► L2 entrypoint §3 enable_forwarding() 自愈：host 模式 rw 时 echo 1
  │     └─► L3 全失败 = FATAL 退出 11（提示加 sysctls / sudo sysctl / CAP_SYS_ADMIN）
  │           都没开 = FATAL 退出 11
  │
  ├─► §3.5 修出向路由 + FORWARD ACCEPT + MASQUERADE（v2-78 + v2-79 自动化）
  │     ├─► table 220 default via $V4_GW dev $OUT_IF
  │     ├─► iptables FORWARD ACCEPT ipsec0 ↔ $OUT_IF
  │     └─► iptables MASQUERADE $IKEV2_VPN_SUBNET → $OUT_IF  （v2-79：源用探测的子网）
  │
  ├─► §4 MASQUERADE（iptables/ip6tables + addrtype ! --dst-type LOCAL）
  │
  ├─► §5 占位符替换 swanctl.conf
  │     ├─► %IKEV2_LOCAL_ADDRS_DIRECTIVE%   ←  $IKEV2_SERVER_ADDR_V6（v6 → v4 → 留空）
  │     ├─► %IKEV2_LOCAL_TS%                ←  ::/0 或 0.0.0.0/0, ::/0
  │     ├─► %IKEV2_VPN_SUBNET%              ←  $IKEV2_VPN_SUBNET（v2-79 新增）
  │     ├─► %IKEV2_SERVER_CN%               ←  用户必填
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

---

## 4. 关键接口设计（Go interface）

### 4.1 `store.UserStore`

```go
type UserStore interface {
    Create(ctx context.Context, user *User) error
    GetByID(ctx context.Context, id int64) (*User, error)
    GetByUsername(ctx context.Context, username string) (*User, error)
    List(ctx context.Context) ([]*User, error)
    UpdatePassword(ctx context.Context, id int64, password string) error
    SetEnabled(ctx context.Context, id int64, enabled bool) error
    UpdateNote(ctx context.Context, id int64, note string) error
    Delete(ctx context.Context, id int64) error
    Count(ctx context.Context) (int, error)
}
```

### 4.2 `store.SessionStore`

```go
type SessionStore interface {
    Create(ctx context.Context, session *Session) error
    GetByID(ctx context.Context, id string) (*Session, error)
    Delete(ctx context.Context, id string) error
    DeleteExpired(ctx context.Context) error
}
```

### 4.3 `store.AdminStore`

```go
type AdminStore interface {
    Create(ctx context.Context, admin *Admin) error
    GetByUsername(ctx context.Context, username string) (*Admin, error)
    UpdatePassword(ctx context.Context, id int64, passwordHash string) error
    GetDefault(ctx context.Context) (*Admin, error)
}
```

### 4.4 `swanctl.Manager`（**升级为 VICI/govici**）

**v2.1 修订**：原 M4 阶段，`swanctl.Manager` 通过 `os/exec` 调用 `swanctl --load-all` / `--list-sas --raw` / `--terminate` 等命令，输出用字符串解析。M8 阶段改为通过 [strongswan/govici](https://github.com/strongswan/govici) 直接与 charon 的 **VICI** Unix socket 通信，**去掉字符串解析**与 `os/exec` 调用。

**为什么改**：

| 维度 | `os/exec` + 字符串解析（v2.0） | VICI + govici（v2.1 / M8） |
|---|---|---|
| 命令调用 | 每次操作 spawn 进程（`swanctl --list-sas`） | 通过 Unix socket（`/var/run/charon.vici`） |
| 协议稳定性 | `swanctl` 输出格式在不同版本有差异，需锁版本 | VICI 是 strongSwan 官方公开的稳定协议 |
| 解析复杂度 | 自写状态机解析 `--raw` 输出（~150 行 parser.go） | `govici.UnmarshalMessage` 直接拿结构化数据 |
| 实时事件 | 轮询（每 5 分钟 list-sas） | `Subscribe("ike-sa-updated")` 推送（可选，v2.1 暂不启用） |
| 错误信息 | exit code + stderr 字符串 | 协议层返回结构化错误 |
| 依赖 | 仅 stdlib | + `github.com/strongswan/govici/vici`（MIT，官方维护，v0.8.0） |

**接口保持不变**（M8 不破坏调用方）：

```go
type Manager interface {
    // 配置管理（仍然写文件 + swanctl --load-all，govici 的 load-conn/load-cert/load-shared 是另一条路，M8 暂不切换）
    WriteUserConf(username, password string) error
    RemoveUserConf(username string) error
    ReloadAll() error

    // 状态查询（govici 实现，替代原 swanctl --list-sas --raw 字符串解析）
    ListSAs(ctx context.Context) ([]SA, error)
    Version(ctx context.Context) (string, error)

    // 强制下线（govici 实现，替代原 swanctl --terminate --ike-id）
    Terminate(ctx context.Context, ikeID string) error
}
```

**实现策略**：

```go
// internal/swanctl/manager.go
import "github.com/strongswan/govici/vici"

type viciManager struct {
    session *vici.Session
    confDir string
}

func (m *viciManager) ListSAs(ctx context.Context) ([]SA, error) {
    msg, err := m.session.Call(ctx, "list-sas", nil)
    if err != nil { return nil, err }
    // govici.UnmarshalMessage(msg, &saList) — 直接拿结构体
    // 不再需要 parser.go 的状态机
    return saList, nil
}

func (m *viciManager) Terminate(ctx context.Context, ikeID string) error {
    req := vici.NewMessage().Set("ike-id", ikeID).Set("child-id", ikeID)
    _, err := m.session.Call(ctx, "terminate", req)
    return err
}
```

**风险与缓解**：

- govici v0.8.0 仍是 pre-1.0，API 可能小幅变化。我们用到的子集（`list-sas`、`terminate`、`version`）是稳定命令
- Docker 镜像必须确保 `charon.vici` Unix socket 可访问（`/var/run/charon.vici`，容器内 0666 权限）
- dev 模式（无 swanctl）走 `SkipVici` 短路，单元测试走 mock `vici.Session`

**与 M4 旧实现的兼容**：`internal/swanctl/parser.go`、`terminate.go`、`loader.go` 在 M8 阶段**重写**，但不删除 git 历史——便于回退。

### 4.5a `limit.Limiter`

```go
type Limiter interface {
    // 写限速文件（Go 端管理，updown 脚本读取）
    WriteLimitFile(username string, mbps int64) error
    RemoveLimitFile(username string) error

    // tc 命令封装（实际在 updown 脚本里调用，Go 端不直接 exec tc）
    // 此接口只暴露文件操作
}
```

### 4.5b `limit.Collector`

```go
type Collector interface {
    Run(ctx context.Context)  // 启动定时任务，每 5 分钟跑一次
}
```

### 4.5c `expiry.Checker`

```go
type Checker interface {
    Run(ctx context.Context)  // 启动定时任务，每 60 秒跑一次
}
```

### 4.5 `cert.Generator`

```go
type Generator interface {
    EnsureCA(dataDir string) (caCertPEM []byte, err error)
    EnsureServerCert(dataDir string, caCert, caKey []byte, cn string) (certPEM, keyPEM []byte, err error)
    EnsurePanelCert(dataDir string, caCert, caKey []byte) (certPEM, keyPEM []byte, err error)
    RenderMobileconfig(username, serverAddr, serverID string, caCertPEM []byte) ([]byte, error)
}
```

**接口而非具体类型**：便于测试时 mock，但生产代码只有一个实现。

---

## 5. 配置管理

### 5.1 环境变量（容器内，v2-79：所有网络参数都可选）

| 变量 | 默认值 | 必填？ | 说明 |
|---|---|---|---|
| `IKEV2_DATA_DIR` | `/data` | ✅ | 持久化目录 |
| `IKEV2_LISTEN_ADDR` | `0.0.0.0:8443` | — | HTTPS 监听地址 |
| `IKEV2_LOG_LEVEL` | `info` | — | 日志级别：debug/info/warn/error |
| `IKEV2_LOG_FORMAT` | `text` | — | text / json |
| `IKEV2_SERVER_CN` | `ikev2.local` | ✅ 唯一 | 服务器证书 CN（mobileconfig 里 RemoteIdentifier 也是这个） |
| `IKEV2_SERVER_ADDR_V6` | 空 | — | 服务器 IPv6 地址（v2-79：留空 → §0.2 自动从 `/proc/net/if_inet6` 取 2000::/3） |
| `IKEV2_SERVER_ADDR_V4` | 空 | — | 服务器 IPv4 地址（仅 `IKEV2_IPV6_ONLY=false` 时用，v2-79：留空 → §0.3 自动从网卡取） |
| `IKEV2_IPV6_ONLY` | `true` | — | 默认 IPv6-only——避开宿主 IPv4 UDP 500/4500 被 SoftEther 占 |
| `IKEV2_OUT_IF` | 空 | — | 出接口（v2-79：留空 → §0.1 自动从 `ip -4 route show default` 取；fallback `eth0`） |
| `IKEV2_NETWORK_MODE` | `auto` | — | v2-79 新增：auto / host / bridge / ipvlan（v2-80+ 真正自动切换） |
| `IKEV2_VPN_SUBNET` | `auto` | — | v2-79 新增：auto / 10.10.0.0/24 等（auto → §0.6 自动选不冲突） |
| `IKEV2_CERT_MODE` | `self-signed` | — | `self-signed` / `letsencrypt` 二选一 |
| `IKEV2_DOMAIN` | 空 | LE 必填 | LE 模式必填：用于 ACME DNS-01 验证的域名（v2-76 改用阿里云 DNS API） |
| `IKEV2_ACME_EMAIL` | 空 | — | LE 注册邮箱（仅 LE 模式需要） |
| `IKEV2_ACME_WEBROOT` | `/var/www/acme` | — | HTTP-01 挑战 webroot（仅 LE 模式，但 v2-76 起默认走 DNS-01） |
| `Ali_Key` | 空 | LE 必填 | 阿里云 DNS API Key（v2-76 起改用 DNS-01） |
| `Ali_Secret` | 空 | LE 必填 | 阿里云 DNS API Secret |
| `IKEV2_DEFAULT_USER_PASSWORD_LEN` | `12` | — | 默认随机密码长度 |
| `IKEV2_SESSION_TTL` | `24h` | — | 会话有效期 |
| `IKEV2_COOKIE_SECURE` | `true` | — | dev 测试可置 false |

**不通过环境变量配置的**：

- CA/Server/Panel 证书路径（固定在 `${IKEV2_DATA_DIR}` 下）
- swanctl.conf 主配置（在镜像里）
- 用户子配置目录（固定在 `/etc/swanctl/conf.d`）
- SQLite 路径（固定在 `${IKEV2_DATA_DIR}/panel.db`）

### 5.2 启动时配置校验

```
main.go:
  config.Load() 失败 → 退出码 1 + 错误信息
  dataDir 不存在或不可写 → 退出码 1
  无法监听端口 → 退出码 1
```

**不做的**：JSON/YAML/TOML 配置文件。环境变量 + 镜像内置默认值已经足够。

---

## 6. 错误处理策略

### 6.1 三类错误

| 类型 | 例子 | 行为 |
|---|---|---|
| **用户输入错误** | 用户名重复、非法字符 | 422 + 错误消息回显表单 |
| **预期系统错误** | swanctl 不存在、磁盘满 | 500 + 详细错误到日志 + 简短消息到 UI |
| **意外错误** | panic、nil 指针 | 500 + 栈到日志 + 通用消息到 UI |

### 6.2 关键路径必须有错误处理

| 路径 | 必须处理 |
|---|---|
| DB 写入 | 任何 `Exec`/`Query` 失败必须返回 |
| 文件写入 | 创建 swanctl 子配置失败必须返回 |
| `swanctl --load-all` | 失败必须回滚 DB + 删除文件 |
| TLS 证书加载 | 失败 = 启动失败，不带病运行 |

### 6.3 不做的

- 错误聚合服务（Sentry 等）
- 自动重试（用户重复点击即可）
- 错误码体系（HTTP 状态码够了）
- i18n 错误消息（中文硬编码）

---

## 7. 日志策略

### 7.1 Go 进程日志

- 用 Go 标准库 `log/slog`（1.21+）
- 输出到 stdout，Docker 自动转 journald
- 格式：JSON（生产）/ text（开发，由 `IKEV2_LOG_LEVEL=debug` 切换）

### 7.2 日志级别

| 级别 | 何时用 |
|---|---|
| Debug | 请求处理详情、SQL 参数、swanctl 命令完整输出 |
| Info | 启动、停止、用户 CRUD、配置 reload 结果 |
| Warn | swanctl reload 失败但已自动回滚；证书即将到期 |
| Error | 数据库错误、文件 IO 错误、未处理异常 |

### 7.3 不做的

- 业务日志入库（直接 docker logs 看）
- 日志聚合 / ELK
- 审计日志
- 用户行为日志

---

## 8. 测试策略

### 8.1 必须写的测试

| 模块 | 测试类型 |
|---|---|
| `auth/password.go` | 单元测试：GeneratePassword 随机性、VerifyPassword 正反例 |
| `auth/session.go` | 单元测试：CSRF token 生成与校验 |
| `cert/generate.go` | 单元测试：CA/Server/Panel 证书生成可加载；mobileconfig 渲染可解析 |
| `store/*` | 集成测试：用 in-memory SQLite 跑 CRUD |
| `swanctl/parser.go` | 单元测试：解析 `--list-sas --json` 已知输出 |

### 8.2 不写的测试

- HTTP handler 端到端测试（用真机测更直接）
- 性能/压力测试（几十人规模无意义）
- E2E 浏览器测试（管理员一人手测即可）

### 8.3 必做的端到端真机测试

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

9. **限速**：新建用户 `bob` 指定 5 Mbps → 连接 → `iperf3 -c <server-ip> -t 30` → 下行 ≤ 6 Mbps
10. **限速**：`bob` 限速改 0 → `iperf3` 重测 → 达到物理上限
11. **多用户限速**：新建 `charlie` 限速 10 Mbps → 同时连接 bob (5M) + charlie (10M) → 各自速度符合限速
12. **过期**：新建 `dave` 过期时间设 1 分钟后 → 等待 → 客户端连接被拒（看 swanctl 日志）
13. **过期恢复**：`dave` 过期时间改回 30 天后 → 客户端能继续连接
14. **流量统计**：`bob` 连接后下载 1GB 文件 → 面板 `bytes_in_total` 增加约 1GB

**共 14 步 = 全部功能验收完成。**

---

## 9. 本地开发（不上 Docker）

```bash
# 1. 装 strongSwan
sudo apt install strongswan strongswan-swanctl

# 2. 起 charon
sudo ipsec start

# 3. 跑 Go 程序（直连到本机 strongSwan）
export IKEV2_DATA_DIR=$HOME/.ikev2-panel-dev
export IKEV2_LISTEN_ADDR=127.0.0.1:8443
go run ./cmd/ikev2-panel

# 4. 浏览器开 https://127.0.0.1:8443/login
```

**注意**：本地开发和容器内运行的差异：

- 容器内 Go 程序以 root 运行，可写 `/etc/swanctl/conf.d`
- 本地开发要么用 root 跑，要么把 `/etc/swanctl/conf.d` chown 给当前用户

**v2 不做**：dev 容器、docker-compose-dev.yml、热重载工具。

---

## 10. 部署架构

### 10.1 单机部署

```
公网 IPv6 ──┬──► UDP 500/4500 ──► strongSwan (charon)
            │                       │
            │                       ▼  (kernel-libipsec: 用户态软加密 + UDP 4500 封装)
            │                   内网 10.0.0.0/24 (VPN 客户端)
            │
            └──► TCP 8443 ────► Go 面板 (HTTPS)
                                    │
                                    ▼
                                SQLite 数据库
```

**v2.1 修订**：默认走 **kernel-libipsec 后端**，所有 ESP 走 UDP 4500（NAT-T 强制封装）。Docker 默认 bridge 网络即可，无需 host 网络。详见设计文档 §1.5。

### 10.2 docker-compose.yml 关键字段（**v2.1 修订**）

```yaml
version: "3.8"
services:
  ikev2-panel:
    image: ikev2-panel:v2  # 用户自己 build
    container_name: ikev2-panel
    restart: unless-stopped
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN          # libipsec 需要创建 TUN 设备 + 设置路由表
      - NET_BIND_SERVICE   # 监听 8443 (保险)
      # SYS_ADMIN 不再需要（XFRM 由 libipsec 在用户态处理）
    ports:
      - "500:500/udp"       # IKE 协商
      - "4500:4500/udp"     # NAT-T + libipsec 强制 ESP-in-UDP 封装
      - "8443:8443"         # 面板（建议用 nginx 反代或防火墙限制来源 IP）
    volumes:
      - ikev2-data:/data    # 持久化：DB + 证书 + 限速文件 + swanctl-conf.d
    environment:
      IKEV2_LISTEN_ADDR: "0.0.0.0:8443"
      IKEV2_SERVER_CN: "vpn.example.com"      # 用户改成自己的域名
      IKEV2_SERVER_ADDR_V6: "2001:db8::1"     # 公网 IPv6
      IKEV2_IPV6_ONLY: "true"
      IKEV2_CERT_MODE: "self-signed"          # 或 letsencrypt
      IKEV2_NETWORK_MODE: "bridge"            # 默认；可选 "host"（专家模式）
      IKEV2_LOG_LEVEL: "info"

volumes:
  ikev2-data:
```

**对比 v2.0（旧）**：

- **删除** `network_mode: host`（解决 ESP 兼容性问题的同时也牺牲了网络隔离）
- **删除** `cap_add: SYS_ADMIN`（不再依赖内核 XFRM）
- **增加** `IKEV2_NETWORK_MODE` 环境变量，host 网络作为专家开关保留

### 10.3 端口注意（**v2.1 修订**）

- **500/udp** 和 **4500/udp**：bridge 网络端口映射即可（libipsec 强制 ESP-in-UDP 封装，不需要独立 ESP 通道）
- **8443**：bridge 端口映射；**强烈建议**用 nginx 反代或防火墙限制来源 IP
- **ESP 协议号 50**：不再依赖（libipsec 在用户态处理，UDP 4500 透传）

**专家模式**（`IKEV2_NETWORK_MODE=host`）：保留 host 网络选项，但**仅在 KVM/Xen 硬件虚拟化 + 已知宿主 ESP 转发正常**时使用。Dockerfile 同时编译 `kernel-libipsec` 与 `kernel-netlink`，运行时由 strongSwan 配置选择后端。

**性能边界**：

- libipsec 模式（默认）：≤20 用户 + ≤5 Mbps/用户 = ≤100 Mbps 总带宽。超出应改用方案 A（宿主机 systemd 服务）
- 详细性能说明见设计文档 §1.5.3

---

## 11. 关键代码片段（参考示例）

### 11.1 swanctl 子配置生成

```go
// internal/swanctl/writer.go
package swanctl

import (
    "fmt"
    "os"
    "path/filepath"
)

const userConfDir = "/etc/swanctl/conf.d"

func (m *Manager) WriteUserConf(username, password string) error {
    content := fmt.Sprintf(`secrets {
    eap-%s {
        id = %s
        secret = %q
    }
}
`, username, username, password)

    path := filepath.Join(userConfDir, username+".conf")
    return os.WriteFile(path, []byte(content), 0600)
}

func (m *Manager) RemoveUserConf(username string) error {
    path := filepath.Join(userConfDir, username+".conf")
    err := os.Remove(path)
    if os.IsNotExist(err) {
        return nil
    }
    return err
}

func (m *Manager) ReloadAll() error {
    cmd := exec.Command("swanctl", "--load-all")
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("swanctl --load-all failed: %w (output: %s)", err, out)
    }
    return nil
}
```

### 11.2 移动配置生成（核心片段）

```go
// internal/cert/mobileconfig.go
package cert

import (
    "encoding/base64"
    "fmt"
    "text/template"
)

const mobileconfigTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>PayloadContent</key>
    <array>
        <dict>
            <key>PayloadType</key>
            <string>com.apple.security.root</string>
            <key>PayloadCertificateFileName</key>
            <string>ca.cert.pem</string>
            <key>PayloadContent</key>
            <data>{{ .CACertBase64 }}</data>
        </dict>
        <dict>
            <key>PayloadType</key>
            <string>com.apple.vpn.managed</string>
            <key>PayloadIdentifier</key>
            <string>com.example.ikev2.{{ .Username }}</string>
            <key>PayloadUUID</key>
            <string>{{ .UUID }}</string>
            <key>PayloadDescription</key>
            <string>IKEv2 VPN</string>
            <key>UserDefinedName</key>
            <string>IKEv2 VPN</string>
            <key>VPNType</key>
            <string>IKEv2</string>
            <key>IKEv2Settings</key>
            <dict>
                <key>RemoteAddress</key>
                <string>{{ .ServerAddr }}</string>
                <key>LocalIdentifier</key>
                <string>{{ .Username }}</string>
                <key>RemoteIdentifier</key>
                <string>{{ .ServerID }}</string>
                <key>AuthenticationMethod</key>
                <string>Certificate</string>
                <key>ExtendedAuthenticationEnabled</key>
                <true/>
            </dict>
        </dict>
    </array>
    <key>PayloadDisplayName</key>
    <string>IKEv2 VPN</string>
    <key>PayloadIdentifier</key>
    <string>com.example.ikev2.{{ .Username }}</string>
    <key>PayloadUUID</key>
    <string>{{ .UUID }}</string>
    <key>PayloadType</key>
    <string>Configuration</string>
    <key>PayloadVersion</key>
    <integer>1</integer>
</dict>
</plist>
`

type mobileconfigParams struct {
    CACertBase64 string
    Username     string
    ServerAddr   string
    ServerID     string
    UUID         string
}

func RenderMobileconfig(username, serverAddr, serverID string, caCertPEM []byte) ([]byte, error) {
    params := mobileconfigParams{
        CACertBase64: base64.StdEncoding.EncodeToString(caCertPEM),
        Username:     username,
        ServerAddr:   serverAddr,
        ServerID:     serverID,
        UUID:         generateUUID(), // 实现细节略
    }

    tmpl, err := template.New("mobileconfig").Parse(mobileconfigTmpl)
    if err != nil {
        return nil, err
    }
    var buf bytes.Buffer
    if err := tmpl.Execute(&buf, params); err != nil {
        return nil, err
    }
    return buf.Bytes(), nil
}
```

### 11.3 swanctl --list-sas 解析

```go
// internal/swanctl/parser.go
package swanctl

import (
    "encoding/json"
    "os/exec"
)

type SA struct {
    UniqueId   string                  // IKE_SA unique ID
    IkeState   string                  // "ESTABLISHED", "CONNECTING", ...
    RemoteAddr string                  // "192.168.50.63[4500]"
    LocalAddr  string                  // "192.168.50.75[500]"
    RemoteId   string                  // remote identity (eap_id, e.g. "alice")
    Children   map[string]ChildSA      // child SA 名字 → ChildSA
}

type ChildSA struct {
    BytesIn    int64  `json:"bytes_in"`
    BytesOut   int64  `json:"bytes_out"`
    LocalTs    string // "0.0.0.0/0"
    RemoteTs   string // "0.0.0.0/0"
}

func (m *Manager) ListSAs() ([]SA, error) {
    cmd := exec.Command("swanctl", "--list-sas", "--raw")
    out, err := cmd.Output()
    if err != nil {
        return nil, err
    }
    // 解析 swanctl --list-sas --raw 输出（每行一个 key=value）
    // ... 省略具体解析代码
    return nil, nil // stub
}

func (m *Manager) Terminate(ctx context.Context, ikeID string) error {
    cmd := exec.CommandContext(ctx, "swanctl", "--terminate", "--ike-id", ikeID)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("swanctl --terminate failed: %w (output: %s)", err, out)
    }
    return nil
}
```

### 11.4 限速文件读写

```go
// internal/limit/limiter.go
package limit

import (
    "fmt"
    "os"
    "path/filepath"
)

const LimitDir = "/var/lib/ikev2-panel/limits"

func (l *Limiter) WriteLimitFile(username string, mbps int64) error {
    if err := os.MkdirAll(LimitDir, 0700); err != nil {
        return err
    }
    path := filepath.Join(LimitDir, username)
    return os.WriteFile(path, []byte(fmt.Sprintf("%d", mbps)), 0600)
}

func (l *Limiter) RemoveLimitFile(username string) error {
    path := filepath.Join(LimitDir, username)
    err := os.Remove(path)
    if os.IsNotExist(err) {
        return nil
    }
    return err
}
```

### 11.5 updown 脚本（关键逻辑，修订版）

```bash
#!/bin/bash
# /usr/local/bin/ikev2-updown
# 由 swanctl 在 SA up/down 时调用
set -e

LIMIT_DIR="/var/lib/ikev2-panel/limits"
CLASS_DIR="/var/lib/ikev2-panel/classids"
OUT_IF="${IKEV2_OUT_IF:-eth0}"

# swanctl 5.9+ 的 PLUTO_PEER_ID 是 "CN=alice" 形式，需要剥掉
USERNAME="${PLUTO_PEER_ID#CN=}"
LIMIT_FILE="${LIMIT_DIR}/${USERNAME}"
CLASS_FILE="${CLASS_DIR}/${USERNAME}"

# 启动时建立根 qdisc（只一次，所有用户共用）
ensure_root_qdisc() {
  if ! tc qdisc show dev "$OUT_IF" | grep -q "qdisc htb"; then
    tc qdisc add dev "$OUT_IF" root handle 1: htb default 999 || true
    tc class add dev "$OUT_IF" parent 1: classid 1:999 htb \
      rate 1000mbit ceil 1000mbit || true
  fi
}

case "$PLUTO_VERB" in
  up)
    ensure_root_qdisc
    if [ -f "$LIMIT_FILE" ]; then
      LIMIT_MBPS=$(cat "$LIMIT_FILE")
      if [ "$LIMIT_MBPS" -gt 0 ]; then
        mkdir -p "$CLASS_DIR"
        if [ -f "$CLASS_FILE" ]; then
          CLASS_ID=$(cat "$CLASS_FILE")
        else
          CLASS_ID=$((RANDOM % 9000 + 100))
          echo "$CLASS_ID" > "$CLASS_FILE"
        fi
        # 添加 leaf class
        tc class add dev "$OUT_IF" parent 1: classid 1:$CLASS_ID htb \
          rate "${LIMIT_MBPS}mbit" ceil "${LIMIT_MBPS}mbit"
        # u32 filter 匹配 VIP
        tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
          match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:$CLASS_ID
        echo "[updown] Applied ${LIMIT_MBPS}Mbps for user ${USERNAME} (VIP ${PLUTO_PEER_SOURCEIP}, class ${CLASS_ID})"
      fi
    fi
    ;;
  down)
    # **关键**：只删该用户的 class，不重建根 qdisc
    if [ -f "$CLASS_FILE" ]; then
      CLASS_ID=$(cat "$CLASS_FILE")
      tc class del dev "$OUT_IF" classid 1:$CLASS_ID 2>/dev/null || true
      rm -f "$CLASS_FILE"
      echo "[updown] Removed class ${CLASS_ID} for user ${USERNAME}"
    fi
    # 不要 tc qdisc del root！
    ;;
esac
```

**关键修订（避免 v2 初版的两个 bug）**：

| Bug | 修复 |
|---|---|
| down 时 `tc qdisc del root` 删掉所有用户的规则 | down 时**只删该用户的 class** |
| `$RANDOM` Class ID 多用户并发冲突 | Class ID **持久化到文件** `/var/lib/ikev2-panel/classids/<username>` |
| 每次 up 都重建 root qdisc（多余） | `ensure_root_qdisc` 函数检查一次，只在需要时建 |
| Class ID 范围不确定 | 固定在 100-9099 范围 |

### 11.6 流量采集 goroutine

```go
// internal/limit/collector.go
package limit

import (
    "context"
    "time"
)

type Collector struct {
    swanctl SwanctlManager
    store   UserStore
    period  time.Duration
}

func NewCollector(m SwanctlManager, s UserStore) *Collector {
    return &Collector{swanctl: m, store: s, period: 5 * time.Minute}
}

func (c *Collector) Run(ctx context.Context) {
    ticker := time.NewTicker(c.period)
    defer ticker.Stop()
    
    for {
        select {
        case <- ctx.Done():
            return
        case <- ticker.C:
            c.collect(ctx)
        }
    }
}

func (c *Collector) collect(ctx context.Context) {
    sas, err := c.swanctl.ListSAs()
    if err != nil {
        log.Error("collector: list-sas failed", "err", err)
        return
    }
    
    // 累加每个用户的 bytes
    userBytes := make(map[string][2]int64) // [in, out]
    for _, sa := range sas {
        if sa.IkeState != "ESTABLISHED" {
            continue
        }
        var in, out int64
        for _, child := range sa.Children {
            in += child.BytesIn
            out += child.BytesOut
        }
        prev := userBytes[sa.RemoteId]
        userBytes[sa.RemoteId] = [2]int64{prev[0] + in, prev[1] + out}
    }
    
    for username, bytes := range userBytes {
        if err := c.store.IncrementBytes(ctx, username, bytes[0], bytes[1]); err != nil {
            log.Error("collector: increment bytes failed", "user", username, "err", err)
        }
    }
}
```

### 11.7 过期检查 goroutine

```go
// internal/expiry/expiry.go
package expiry

import (
    "context"
    "time"
)

type Checker struct {
    swanctl SwanctlManager
    store   UserStore
    period  time.Duration
}

func NewChecker(m SwanctlManager, s UserStore) *Checker {
    return &Checker{swanctl: m, store: s, period: 60 * time.Second}
}

func (c *Checker) Run(ctx context.Context) {
    ticker := time.NewTicker(c.period)
    defer ticker.Stop()
    
    for {
        select {
        case <- ctx.Done():
            return
        case <- ticker.C:
            c.check(ctx)
        }
    }
}

func (c *Checker) check(ctx context.Context) {
    expired, err := c.store.GetExpired(ctx, time.Now().Unix())
    if err != nil {
        log.Error("expiry: get-expired failed", "err", err)
        return
    }
    
    for _, user := range expired {
        // 强制下线
        if err := c.swanctl.Terminate(ctx, user.Username); err != nil {
            log.Warn("expiry: terminate failed", "user", user.Username, "err", err)
        }
        // 标记为停用
        if err := c.store.SetEnabled(ctx, user.ID, false); err != nil {
            log.Error("expiry: disable failed", "user", user.Username, "err", err)
        }
        log.Info("expiry: user expired", "user", user.Username, "expired_at", user.ExpiresAt)
    }
}
```

---

## 12. 已知陷阱（开发时必须注意）

| 陷阱 | 症状 | 解决 |
|---|---|---|
| `swanctl --load-all` 文件权限 | charon 读不到 conf | Go 进程以 root 跑，写文件 0600 |
| `swanctl --load-all` 不重载 secrets | 改了密码不生效 | 用 `--load-all` 而不是 `--load-conns` |
| charon 第一次启动需要 `--load-all` | 启动后连接失败 | entrypoint.sh 启动后调一次 |
| 容器 host 网络 + 端口冲突 | UDP 500 被宿占用 | 文档说明；建议停占用的服务 |
| 容器内 strongSwan `cap_sys_admin` 缺失 | charon 起不来 | docker-compose 加 cap_add |
| ~~**ESP 协议号 50 不能 bridge**~~ | bridge 网络下 ESP 丢包 | **已修复（v2.1）**：启用 kernel-libipsec 插件，ESP 强制走 UDP 4500 封装（NAT-T）；Dockerfile 加 `--enable-kernel-libipsec`；compose 改 bridge 网络 |
| **libipsec 不适用高带宽场景** | 超过 100 Mbps 总带宽时丢包/延迟 | **已知限制**：README 明确"≤20 用户 + ≤5 Mbps/用户"；超出应改用方案 A（宿主机 systemd） |
| **govici v0.x API 不稳定** | go get 升级后编译失败 | **锁定 v0.8.0**；用到的子集（list-sas/terminate/version）是稳定命令；测试覆盖 mock vici.Session |
| **charon.vici Unix socket 不可访问** | govici 连接失败 | 容器内 strongswan.conf `charon { vici { socket = unix:///var/run/charon.vici } }`；确保 0666 权限；Go 进程与 charon 同 namespace |
| mobileconfig 没内联 CA | iOS 弹"证书不可信" | 必须内联 base64 编码 CA 证书 |
| Android 内置 IKEv2 客户端缺 CA | Android 弹"证书不可信" | Android 客户端导入 CA 文件 |
| Server cert CN 不匹配客户端填的 ID | IKE_AUTH 失败 | CN 必须等于 mobileconfig 里的 `RemoteIdentifier` |
| DB 写成功但 swanctl 写失败 | 用户能登面板但连不上 VPN | 必须事务化：DB 回滚 + 文件删除 |
| **限速 tc 规则不清理** | 旧用户的 qdisc 累积，新用户连接失败 | updown 脚本 `down` 时 `tc qdisc del ... root` |
| **tc HTB 缺少 default class** | 没有规则的包匹配不到 class | `tc qdisc add ... htb default 30` |
| **PLUTO_PEER_ID 不是用户名** | swanctl 5.9+ 用 DN 形式 `CN=alice` 而不是 `alice` | updown 脚本里要剥掉 `CN=` 前缀 |
| **过期用户配置还在** | 过期后还能连接 | expiry goroutine 要 `--terminate --ike-id` 强踢 |
| **`swanctl --list-sas --json` 字段变化** | 解析失败 / 字段为空 | 锁版本（强Swan 6.1.0），CI 跑解析测试 |
| **bytes_in_total 大数溢出** | 超过 2GB SQLite INT 没问题（int64）但 JS Number 会丢精度 | 前端展示用 `BigInt` 或转为 GB 字符串 |
| **`swanctl --terminate` 不支持 EAP 用户名** | 终止命令失败 | 用 `--ike-id` 配合 `%identity` 占位符 |
| **移动配置文件不带 expires_at** | 用户改密码/限速后客户端不会自动更新 | mobileconfig 不包含过期字段，靠管理员侧处理 |
| ~~**宿主持 SoftEther 占 IPv4 UDP 500**~~ | charon 启动失败：`Address already in use` | **已修复**：`IKEV2_IPV6_ONLY=true` 默认 + entrypoint IPv6 校验 |
| **宿主持 SoftEther 占 IPv6 UDP 500** | charon 启动失败 | 极少见（SoftEther L2TP/IPsec 不走 IPv6），但仍可能；建议改 SoftEther 监听 |
| ~~**iOS mobileconfig `RemoteAddress` 只接受单值**~~ | 早期 iOS 不支持双栈数组 | **已修复**：只用 `RemoteAddress` 单地址，不混用 `ServerAddresses` 数组 |
| ~~**iPhone 拨号成功但无法访问公网（VPN 能通 / 公网不通）**~~ | 出向 XFRM table 220 缺 default route + iptables FORWARD DROP + MASQUERADE 缺 | **v2-78 已修复**：`entrypoint.sh §3.5` 加 `table 220 default via $V4_GW` + `iptables -I FORWARD -i/o ipsec0 -j ACCEPT` + `iptables -t nat -A POSTROUTING -s $IKEV2_VPN_SUBNET -o $OUT_IF -j MASQUERADE` |
| **iPhone 拨号成功但 captive.apple.com 探测超时（Safari/WX 报"没连接互联网"）** | iOS 17+ 把所有 UDP 53 也按 0.0.0.0/0 强制路由进 ESP，server 端没推 DNS → 客户端解析走不通 | **v2-78 已修复**：mobileconfig 加 `<key>DNS</key>` dict（含 `ServerAddresses` + `DNSProtocol=Cleartext`，v2.86-PR12.23 修正）+ swanctl.conf `pools.dns` 双推 |
| **ACME HTTP-01 80 端口被 SoftEther 占** | 证书申请失败 | 改用 DNS-01 或停 SoftEther 80 端口（v2-76 起默认走 DNS-01） |
| **Let's Encrypt 续期失败** | 证书过期，所有用户断连 | **已加回退**：`/etc/ikev2-panel/scripts/renew-cert.sh` 检测新证书 < 30 天 → cp 备份证书 + 设置 LAST_RENEW_FAILED；Go 端 health.certcheck 60s 检测标志 → 首页红字横幅 |
| **iOS 拒绝 ECDSA 证书** | iPhone/iPad 连接 IKEv2 静默失败 | **已修复**：续签脚本强制 `--keylength 2048`（RSA） |
| **DNS AAAA 记录缺失** | IPv6 客户端无法解析 | 文档强提示：必须配 AAAA；entrypoint 不校验（容器内看不到公网 DNS） |
| **strongSwan IPv6 ESP 需要 kernel 5.8+** | 客户端 IPv6 连接失败 | Debian 12 默认 kernel 6.1 ✅；旧系统不支持 |
| **ISP 重拨公网 IPv6 前缀变化** | swanctl.conf `local_addrs` 写死，prefix 变后客户端找不到 VPN | **v2-72 已修复**：`internal/swanctl/ipv6watch.go` 每 60s 扫 `/proc/net/if_inet6`，变化 → sed + swanctl --load-all |
| ~~**限速 tc down 时 qdisc del root**~~ | 全用户限速失效 | **已修复**：只删单 class，root qdisc 只建一次 |
| ~~**tc Class ID 用 $RANDOM 冲突**~~ | 多用户并发时规则错乱 | **已修复**：Class ID 持久化到 `/var/lib/ikev2-panel/classids/<username>` |
| **IPv6 限速不生效** | tc u32 只识别 IPv4 | 已知限制，README 文档明示 |
| **宿主换网卡名后容器找不到 OUT_IF** | 硬编码 `IKEV2_OUT_IF=ens18` 跟新网卡不符 | **v2-79 已修复**：§0.1 自动探测（`ip -4 route show default`）；手动可传 `IKEV2_OUT_IF=eth0` |
| **宿主 LAN 段与 VPN 内部段 10.10.0.0/24 冲突** | 客户端 VIP 跟 LAN 设备同段 → 路由冲突 | **v2-79 已修复**：§0.6 自动从候选（10.10/13/17/42/66.0.0/24）选不冲突；手动可传 `IKEV2_VPN_SUBNET=10.99.0.0/24` |
| **用户误以为改了宿主机网络配置** | v2-78 的 iptables FORWARD/MASQUERADE 在容器内，但用户没意识到 docker network namespace 隔离 | **v2-79 文档澄清**：见 design §19.4 / README 故障排查。只改容器内，绝不动宿主 |
| **宿主 IPv6 forwarding 未开，客户端拨号后无法访问公网** | docker host 网络模式下默认把 forwarding 重置为 0 | **v2-79 已修复**：三层兜底——L1 `docker-compose.yml sysctls:` + L2 `entrypoint.sh §3 enable_forwarding()` 自愈（host 模式 rw 时 echo 1，等同于改宿主）+ L3 FATAL 退出 11；详见 design §16.4 |
| **iOS 18 协商降级到 MODP2048** | swanctl proposals 只列 `modp2048`，iOS 18 默认推 `ecp256` 会回退到非 ECDH | **v2-79.1 已修复**：`proposals` 追加 `ecp256` / `curve25519` 系列；Dockerfile 已 `--enable-curve25519` |
| **server.key.pem 私钥 0o644 同主机可读** | `installCertsToSwanctl` 所有文件统一 0o644 | **v2-79.1 已修复**：按文件类型拆权限——cert 0o644，key 0o600 |
| **mobileconfig DNSSettings 格式错误** | 用了 `<array><dict>...</dict></array>`，iOS 17/18 静默忽略整个节点 → captive.apple.com 探测超时 | **v2-79.1 已修复**：改为 `<dict><key>DNS</key><array>...</array></dict>` → **v2.86-PR12.23 进一步修正**：顶层 `<key>DNS</key><dict><key>ServerAddresses</key><array>...</array><key>DNSProtocol</key><string>Cleartext</string></dict>` 才是 iOS 14+ 规范形态（DNSSettings 是杜撰 key，本身永远不会被识别） |

---

## 13. 与 v1 的关系

**本文档与 v1 完全脱钩。**

- v1 代码：留作历史参考，不复用
- v1 设计文档：作废，参考价值有限（v1 的"非目标"列表可以参考"哪些坑要避开"）
- v1 部署：服务器上还跑着 v1 镜像，v2 完成后单独切换

**v2 项目新仓库、零依赖、零迁移。**

---

## 14. 开发环境准备（全新开发）

> 这一步是**开发者第一次拉到代码要做的**。满足这些前提才能 `go run ./cmd/ikev2-panel` 看到面板。

### 14.1 必备工具

| 工具 | 版本 | 用途 |
|---|---|---|
| Go | 1.22+ | 编译运行 Go 程序 |
| strongSwan | 6.1.0（推荐 Debian 官方包，版本不强制锁） | 本地 charon |
| sqlite3 CLI | 任意 | 看数据库（可选） |
| Docker + docker compose | 任意 | 容器化部署测试（可选，本地跑不需要） |

### 14.2 本地起 charon

```bash
# Debian/Ubuntu
sudo apt install strongswan strongswan-swanctl

# 起 charon（前台，调试时用；后台 ipsec start 也行）
sudo ipsec start --nofork
```

**注意：本地 charon 会监听 IPv4 UDP 500/4500**——因为没有 SoftEther 占端口，开发环境直接走 IPv4 即可。生产环境才需要 IPv6-only。

### 14.3 本地 Go 进程直连本机 charon

```bash
# 数据目录用本地临时目录（避免污染生产数据）
export IKEV2_DATA_DIR=$HOME/.ikev2-panel-dev
export IKEV2_LISTEN_ADDR=127.0.0.1:8443
export IKEV2_LOG_LEVEL=debug

# 直接跑
go run ./cmd/ikev2-panel
```

**注意权限**：
- Go 程序要写 `/etc/swanctl/conf.d/` 和 `/etc/swanctl/x509/`，需要 root
- 解决方案 A：用 `sudo go run`（不推荐，$GOPATH 会乱）
- 解决方案 B：`sudo chown -R $USER /etc/swanctl/conf.d /etc/swanctl/x509 /etc/swanctl/private`（推荐）
- 解决方案 C：在容器里开发（架构 §10 有 docker-compose.yml 模板可参考）

### 14.4 验证开发环境就绪

```bash
# 1. charon 能跑
sudo ipsec status

# 2. Go 程序能起 + 8443 端口监听
go run ./cmd/ikev2-panel &
curl -k https://127.0.0.1:8443/login  # 看到 HTML = 成功
```

**不需要做**（v2 开发阶段）：
- 不需要真实 IPv6 地址（用 IPv4 调试）
- 不需要 LE 证书（用自签证书）
- 不需要真实 iOS/Android 设备（用 Linux strongSwan 客户端模拟）

---

## 15. 里程碑拆 commit（全新开发顺序）

> **不要一上来就写 3000 行**。按下面顺序每个里程碑 = 1 个（或几个）commit。每个里程碑结束都能跑、能测。

### M1：项目骨架

**commit 1**：`chore: init go module + empty main.go`
- `go mod init github.com/yourname/ikev2-panel-v2`
- 写 `cmd/ikev2-panel/main.go`，只 print "ikev2-panel starting" 然后退出

**commit 2**：`chore: add Dockerfile + docker-compose.yml`
- 单 stage，强Swan 官方镜像作为 base
- compose 暴露 UDP 500/4500 + TCP 8443

**commit 3**：`feat(entrypoint): IPv6 check + charon start + swanctl reload retry`
- `entrypoint.sh` 完整脚本
- 跑通 `docker compose up -d` 后容器内 `swanctl --list-sas` 能输出

**完成定义**：`docker compose up -d` 一次成功；容器日志有"Using IPv6 address"（或 dev 环境跳过）；charon 在容器里运行。

### M2：存储层

**commit 4**：`feat(store): SQLite open + Migrate 001_init`
- `internal/store/store.go` + `internal/store/migrate.go`
- 跑一次能建三张表（users/sessions/admins）

**commit 5**：`feat(store): users CRUD`
- `internal/store/users.go`：Create / GetByID / GetByUsername / List / UpdatePassword / SetEnabled / Delete / Count

**commit 6**：`feat(store): sessions + admins CRUD`
- `internal/store/sessions.go` + `internal/store/admins.go`

**完成定义**：写一个临时 `_test.go` 跑通 CRUD，能 SELECT 出数据。

### M3：认证

**commit 7**：`feat(auth): password generate + bcrypt verify`
- `internal/auth/password.go` + 单元测试

**commit 8**：`feat(auth): session + CSRF`
- `internal/auth/session.go`：CreateSession / GetSession / DeleteSession + CSRF 生成校验

**commit 9**：`feat(web): login/logout handler + middleware`
- `internal/web/handlers_auth.go` + `middleware.go`
- 模板：`login.html`

**完成定义**：浏览器登录面板，能看到 cookie；POST /logout 清 cookie。

### M4：用户管理

**commit 10**：`feat(swanctl): writer + loader`
- `internal/swanctl/writer.go`：WriteUserConf / RemoveUserConf
- `internal/swanctl/loader.go`：ReloadAll

**commit 11**：`feat(web): users CRUD handler`
- `internal/web/handlers_users.go`：新增/列表/详情/重置密码/启停用/删除
- 模板：home.html / users_list.html / user_new.html / user_detail.html

**commit 12**：`feat(swanctl): terminate + parser`
- `internal/swanctl/parser.go`：解析 `--list-sas --raw`
- `internal/swanctl/terminate.go`：调用 `--terminate --ike-id`

**完成定义**：面板新增用户 → 数据库有记录 + `/etc/swanctl/conf.d/<user>.conf` 文件存在 + `swanctl --list-sas` 能看到 secret；Linux strongSwan 客户端能连。

### M5：客户端配置

**commit 13**：`feat(cert): mobileconfig render`
- `internal/cert/mobileconfig.go`
- 模板字段（按设计 §10.1）
- UUID 用 `crypto/rand` 生成

**commit 14**：`feat(cert): server cert generate (self-signed mode)`
- `internal/cert/generate.go`：CA + ServerCert + PanelCert 自动生成
- 第一次启动检测缺失 → 生成 → 写文件

**commit 15**：`feat(web): /users/{id}/mobileconfig + QR code`
- 引入 `skip2/go-qrcode`
- 二维码内容：mobileconfig URL

**完成定义**：iOS Safari 打开 mobileconfig URL → 提示安装 → 装上 → 用用户名密码连上。

### M6：限速 / 过期 / 流量

**commit 16**：`feat(limit): write limit file + ikev2-updown script`
- `internal/limit/limiter.go` + Dockerfile 复制 `ikev2-updown` 到 `/usr/local/bin/`
- 按架构 §11.5 实现持久化 classid + 单 class 删除

**commit 17**：`feat(limit): collector goroutine`
- `internal/limit/collector.go`：每 5 分钟调 `swanctl --list-sas` → 累加 bytes_in/out

**commit 18**：`feat(expiry): checker goroutine`
- `internal/expiry/expiry.go`：每 60 秒扫过期用户 → 调 `swanctl --terminate` + 标记 enabled=0

**完成定义**：新建用户限速 5Mbps → iperf 实测 ≤ 6Mbps；新建用户设 1 分钟后过期 → 自动断连。

### M7：可选功能（LE 模式）

**仅当用户要求时才做**：
- commit 19：`feat(cert): acme.sh integration`
- commit 20：`feat(cert): renew-cert.sh with backup-and-rollback`
- commit 21：`feat(health): certcheck goroutine`

**完成定义**：LE 模式跑起来，90 天后自动续签；模拟续签失败 → 横幅告警 + 服务不中断。

### M8：容器化兼容（govici + kernel-libipsec）（**v2.1 新增**）

**触发条件**：用户在 OpenVZ/LXC/部分 KVM 容器化 VPS 上验证时发现 ESP 包无法转发，或希望降低部署复杂度。

**目标**：把"宿主内核 XFRM"依赖拿掉，用 strongSwan 用户态 libipsec + Go 端 VICI 协议，**彻底解决容器化 VPS 的 ESP 兼容性问题**。

**commit 拆分**：

- **commit 22：`build(docker): Dockerfile enable kernel-libipsec`**
  - 自编译 strongSwan 6.1.0（用 `golang:1.22-bookworm` builder 阶段 + `debian:bookworm` runtime）
  - configure flags: `--enable-kernel-libipsec --enable-kernel-netlink --enable-vici --enable-swanctl --enable-eap-mschapv2 --enable-openssl --disable-gmp`
  - 删除对 `strongswan/strongswan:6.1.0` 官方镜像的依赖
  - 同时编译 `kernel-netlink` 备用（专家模式 host 网络下切换）
- **commit 23：`build(docker): entrypoint.sh 内做 IPv4 MASQUERADE`**
  - 容器内自己跑 `iptables -t nat -A POSTROUTING -s 10.7.0.0/24 -o eth0 -j MASQUERADE`
  - IPv4 forward sysctl：`net.ipv4.ip_forward=1`
  - 移除对宿主网络的依赖
- **commit 24：`chore(compose): bridge 网络 + 去除 SYS_ADMIN cap`**
  - 删除 `network_mode: host`
  - 删除 `cap_add: SYS_ADMIN`
  - 增加 `IKEV2_NETWORK_MODE` 环境变量（`bridge` / `host` 二选一）
  - README 文档明确说明：host 模式为"已知 ESP 转发正常的 KVM/Xen 用户"提供
- **commit 25：`refactor(swanctl): 引入 govici，替代 os/exec + 字符串解析`**
  - `go get github.com/strongswan/govici@v0.8.0`
  - 重写 `internal/swanctl/parser.go` → `manager_vici.go`（用 `vici.Session.Call(ctx, "list-sas", nil)`）
  - 重写 `internal/swanctl/terminate.go` → `vici.Session.Call(ctx, "terminate", msg)`
  - `internal/swanctl/loader.go` 保留文件写入 + `swanctl --load-all`（govici 的 `load-conn/load-cert/load-shared` 是后续优化点，M8 不做）
  - `internal/swanctl/writer.go` 保留（写文本配置文件不变）
  - 单元测试改 mock `vici.Session`（govici 提供 mock 接口）
  - 净效果：-300 行（parser + terminate 旧实现）+ 200 行（govici wrapper）
- **commit 26：`docs(readme): 部署兼容性矩阵 + libipsec 性能边界说明`**
  - 明确标注：libipsec 模式"≤20 用户 + ≤5 Mbps/用户 = ≤100 Mbps 总带宽"
  - 超出此范围的场景：建议方案 A（宿主机 systemd 服务，文档提供 install.sh）
  - 增加"为什么用 libipsec"段落（指向设计 §1.5）

**完成定义**：

1. **功能验证**：
   - OpenVZ 7 VPS（宿主机不转发 ESP）部署 → 客户端连得上、流量能通
   - KVM VPS（宿主机转发 ESP）部署 → 客户端连得上、流量能通
   - iOS/Android/Windows/macOS 客户端四种至少两种连上
2. **接口兼容**：§4.4 `swanctl.Manager` 接口不变，调用方（collector/expiry/handlers_users）零改动
3. **测试覆盖**：`go test ./...` 全部通过；新增 `internal/swanctl/manager_vici_test.go` mock govici
4. **文档**：设计文档 §1.5 已成稿；README 部署说明更新

**回退机制**：M8 不删除 M4 旧实现的 git 历史，紧急情况 `git revert commit 22~26` 即可回到 `os/exec` 实现。

**不做的**（避免 M8 范围爆炸）：

- 不迁移到 govici 的 `load-conn/load-cert/load-shared`（仍用 swanctl --load-all）
- 不启用 `Subscribe("ike-sa-updated")` 实时事件（保持 5 分钟轮询）
- 不写方案 A（宿主机 systemd）的 install.sh（用户真要切场景再说）

### M8.5：公网 IPv6 动态变化感知（commit 22-26 之前已存在，v2-79 文档化）

**commit 22 v2-26 之外**：v2-72 已经实现 `internal/swanctl/ipv6watch.go`，当时没有专门里程碑，但功能已就位：

- `internal/swanctl/ipv6watch.go`：每 60s 扫 `/proc/net/if_inet6`，变化 → sed 替换 swanctl.conf `local_addrs` + `swanctl --load-all`
- 在 `cmd/ikev2-panel/main.go` L358 显式启动（仅 IPv6-only 模式）

### M9：v2-79 零硬编码部署（commit 27，2026-09-17）

**触发条件**：v2-78 在用户生产环境跑通后，用户提出 5 个真实痛点（详见 design §15）。

**目标**：所有"宿主环境相关"的参数自动探测；用户唯一必填只有 `IKEV2_SERVER_CN`。镜像跨网络环境通用，不需重 build。

**commit 27 拆分**（v2-79 一个大 commit，对外逻辑完整）：

1. **`feat(entrypoint): §0 自动探测链`**
   - `scripts/entrypoint.sh §0.1~§0.6`：网卡 / IPv6 / IPv4 / IPv4 GW / LAN 段 / VPN 子网
   - 函数化（`detect_out_if()` / `detect_public_ipv6()` / `detect_v4_gateway()` / `detect_lan_subnets()`）
   - 退出码 14（探测不到网卡）、31（cd 不够） 等

2. **`feat(swanctl): conf 模板占位符化`**
   - `configs/swanctl-ipv6-only.conf`：
     - `local_addrs = %IKEV2_SERVER_ADDR_V6%` → `%IKEV2_LOCAL_ADDRS_DIRECTIVE%`（可省略）
     - `local_ts = 0.0.0.0/0, ::/0` → `%IKEV2_LOCAL_TS%`（按 IPv6_ONLY 自动选）
     - `addrs = 10.10.0.0/24, fd00:1::/64` → `%IKEV2_VPN_SUBNET%, fd00:1::/64`
   - `scripts/entrypoint.sh §5`：5 个 sed 替换
   - `scripts/entrypoint.sh §3.5`：MASQUERADE 用 `$IKEV2_VPN_SUBNET`

3. **`chore(compose): 所有网络参数都改成可选`**
   - `docker-compose.yml`：`IKEV2_SERVER_ADDR_V6=`（留空）、`IKEV2_OUT_IF=`（留空）
   - 新增 `IKEV2_NETWORK_MODE=${IKEV2_NETWORK_MODE:-auto}` 和 `IKEV2_VPN_SUBNET=${IKEV2_VPN_SUBNET:-auto}`
   - `.env` / `.env.example`：所有字段都注释说明"留空 → 自动探测"

4. **`docs: 设计/架构/README 同步 v2-79`**
   - 设计文档 §15~22（5 个痛点 → 解法）
   - 架构文档 §3.1 / §5.1 / §12 / §15 同步更新
   - README 加 M9 里程碑 + v2-79 简化部署清单 + 故障排查

**完成定义**：

1. **功能验证**：
   - `.env` 只填 `IKEV2_SERVER_CN` + LE 凭证 → 启动成功
   - 启动日志打印 6 个 `[auto-detect]` 行
   - 客户端拨号 → 拿到正确 VPN 子网内的 VIP → 流量通
   - 换网络环境（新网卡 / 新 LAN 段）→ **不需要重 build**，直接重启容器自动适配
   - ISP 重拨公网 IPv6 变化 → 60s 内 swanctl 自动 reload → 客户端无需操作
2. **接口兼容**：swanctl.Manager 接口不变，ipv6watch 是新增 module
3. **测试覆盖**：`go test ./...` 全部通过；新增 `internal/swanctl/ipv6watch_test.go`
4. **文档**：设计 §15~22 / 架构 §15 M9 / README M9 全部同步

**回退机制**：v2-79 不删 v2-78 的硬编码路径——`IKEV2_OUT_IF` / `IKEV2_SERVER_ADDR_V6` 仍然优先用户环境变量（如果有值）。回退 = 用户把这些 env 显式填回去。

**不做的**（避免 v2-79 范围爆炸）：

- 不做完整 DDNS（v2-80+ 等用户明确指定 DNS provider）
- 不做 bridge/ipvlan 自动切换（v2-80+）
- 不做"故障自愈"——DHCP 重新分配后自动更新路由表 + 通知客户端

### 拆分原则

- **每个 commit 跑得动**：不留下"半成品 commit"
- **每个 commit 测得了**：要么有单元测试，要么能手动跑通
- **不要超前**：M4 没完成前不要碰 M5
- **不要回填**：M5 完成后回头改 M4 的代码 = 拆 commit 失败

---

## 16. 本地开发调试技巧

### 16.1 看 charon 在做什么

```bash
# charon 日志（前台模式）
sudo ipsec start --nofork  # 直接看 stderr

# 或
sudo journalctl -u strongswan-starter -f
```

### 16.2 swanctl 命令直接试

```bash
# 加载所有配置
sudo swanctl --load-all

# 加载连接配置（不含 secrets）
sudo swanctl --load-conns

# 加载证书
sudo swanctl --load-creds

# 看所有 SA
sudo swanctl --list-sas --raw

# 终止某个用户的 SA
sudo swanctl --terminate --ike-id alice
```

调试 Go 端 `--list-sas --raw` 解析时，直接拿这条命令的输出做测试 fixture。

### 16.3 Go 进程调试

```bash
# 看 HTTP 请求/响应（debug 日志级别）
export IKEV2_LOG_LEVEL=debug
go run ./cmd/ikev2-panel

# 看 swanctl 调用详情
# 调高 log level 后，每个 exec.Command 的命令行 + 输出都在 stdout
```

### 16.4 改 swanctl 配置不重启 charon

```bash
# 改完配置不需要重启 charon，只需要：
sudo swanctl --load-all
# charon 进程一直在跑，只是重新读配置
```

### 16.5 看 SQLite 数据库

```bash
sqlite3 $HOME/.ikev2-panel-dev/panel.db
sqlite> .schema users
sqlite> SELECT id, username, enabled FROM users;
```

### 16.6 清空数据重新开始

```bash
# dev 环境专用
rm -rf $HOME/.ikev2-panel-dev
# 重启 Go 程序会自动建库 + 生成默认管理员密码
```

### 16.7 模拟生产环境（用容器）

```bash
# 在 docker compose 里跑（生产环境完整链路）
docker compose up -d
docker compose logs -f
docker exec -it <container> bash  # 进容器调试
```

**注意**：容器内 `/etc/swanctl/conf.d/` 由 Go 进程写，**不需要手动改**。

---

## 17. 环境变量速查

> 本地开发**最少需要设**：
>
> ```bash
> export IKEV2_DATA_DIR=$HOME/.ikev2-panel-dev
> export IKEV2_LISTEN_ADDR=127.0.0.1:8443
> ```
>
> 其他变量有默认值，**不动也能跑**。

完整环境变量列表（生产环境用）见 §5.1。本地开发常见的几个：

| 变量 | dev 环境推荐值 | 说明 |
|---|---|---|
| `IKEV2_DATA_DIR` | `$HOME/.ikev2-panel-dev` | 数据库 + 证书存放处；默认 `/data`（生产用），dev 必须改否则污染生产 |
| `IKEV2_LISTEN_ADDR` | `127.0.0.1:8443` | 默认 `0.0.0.0:8443` 在容器内合理，本地开发限 127.0.0.1 防意外暴露 |
| `IKEV2_LOG_LEVEL` | `debug` | 本地看详细日志；生产用 `info` |
| `IKEV2_IPV6_ONLY` | `false` | 本地有 IPv4 端口，dev 模式走 IPv4 调试更方便 |
| `IKEV2_CERT_MODE` | `self-signed` | dev 不折腾 LE |

**不要在 dev 环境设的**：
- `IKEV2_DOMAIN`（LE 模式才用）
- `IKEV2_ACME_EMAIL`（LE 模式才用）
- `IKEV2_OUT_IF`（只在容器里调限速脚本需要）

---

## 18. 参考项目与借鉴清单（开发时查阅）

> **本节目的**：开发 v2 时，遇到具体实现问题，**先到这里查**：有没有现成项目可借鉴、借鉴什么、哪些要改、哪些不适合我们。
>
> **规则**：
> 1. **不直接抄代码**。所有借鉴都要先回答："这段代码在我们的环境（容器 + IPv6-only + EAP-MSCHAPv2 + 1-50 人规模）下是否成立？"
> 2. **遇到与设计冲突时，文档优先级最高**——本文档 > 设计文档 > 参考项目 > 网上博客。
> 3. **参考项目是"踩过的坑"和"现成模板"，不是"最终方案"**。

### 18.1 参考项目清单（按优先级）

| 项目 | 链接 | 星数 | 关联性 | 我们能借鉴什么 | 我们要警惕什么 |
|---|---|---|---|---|---|
| **jawj/IKEv2-setup** | https://github.com/jawj/IKEv2-setup | 1.4k | ★★★★★ | LE 部署、mobileconfig 生成、certbot hook、各客户端脚本生成 | 没有 web 面板；IPv6 主动禁用（与我们冲突）；脚本一次性部署，不适合 Docker |
| **alireza0/s-ui** | https://github.com/alireza0/s-ui | ~6k | ★★★★ | Go web 面板架构、流量统计、SQLite 模型、单进程设计 | sing-box 不是 strongSwan；功能多且复杂，需取其思路而非代码 |
| **hwdsl2/docker-ipsec-vpn-server** | https://github.com/hwdsl2/docker-ipsec-vpn-server | 7.1k | ★★★ | Docker 容器化 strongSwan 的思路、环境变量管理 | 用 libreswan 不是 strongSwan；多协议（L2TP/IPsec/IKEv2）我们不要 |
| **acmesh-official/acme.sh** | https://github.com/acmesh-official/acme.sh | ~25k | ★★★★★ | LE 客户端、HTTP-01 standalone、--install-cert 自动 copy + reload | 文档直接引用其命令行参数（已写进设计 §1.4.1） |
| **hwdsl2/setup-ipsec-vpn** | https://github.com/hwdsl2/setup-ipsec-vpn | 28k | ★★ | 用户管理脚本模式 | IPsec/L2TP/IKEv2 三合一，与 v2 定位不符；不抄 |
| **strongSwan 官方 swanctl 文档** | https://docs.strongswan.org/docs/latest/ | - | ★★★★★ | `--load-all`、`--list-sas`、`--terminate` 等命令语义 | 无；直接照搬 |
| **strongSwan/govici**（v2.1 新增） | https://github.com/strongswan/govici | ~60 | ★★★★★ | Go 原生 VICI 客户端；替代 `os/exec` + 字符串解析实现 swanctl Manager | v0.x API 可能小幅变化；锁版本 v0.8.0；用到子集（list-sas/terminate/version）是稳定命令 |
| **kernel-libipsec 插件文档**（v2.1 新增） | https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html | - | ★★★★★ | 用户态 ESP 实现；Docker bridge 网络下解决 ESP 转发问题 | 性能弱于内核 XFRM 10–30%；不适用高带宽场景 |
| **strongSwan Cloud Platforms 指南**（v2.1 新增） | https://docs.strongswan.org/docs/latest/howtos/cloudPlatforms.html | - | ★★★★ | 容器化部署的官方建议；明确指出"Container-virtualized environments often do not offer a working IPsec stack" | 无 |
| **borisovonline/debian-personalvpn gist**（v2.1 新增） | https://gist.github.com/borisovonline/955b7c583c049464c878bbe43329a521 | ~20 | ★★★ | mobileconfig 完整模板；与我们的设计高度相似 | 一次性部署脚本，不能直接 fork；我们用 Go 程序渲染 |
| **andrewlkho/debian-strongswan gist**（v2.1 新增） | https://gist.github.com/andrewlkho/31341da4f5953b8d977aab368e6280a8 | ~60 | ★★★ | iOS 兼容性实践经验；mobileconfig payload 结构参考 | 他的 DN/SAN 配置与我们略有差异（IPv6 SAN） |

### 18.2 借鉴 vs 自研 决策矩阵

> 开发时遇到问题，先按这张表判断："借鉴"、"借鉴+改"、"自研"。

| 场景 | 借鉴对象 | 借鉴什么 | **必须改的地方** | 决策 |
|---|---|---|---|---|
| LE 首次签发 | acme.sh | `--issue` + `--install-cert` + `--reloadcmd` | 我们的 reloadcmd 不是 `systemctl reload`，是 `swanctl --load-creds` | **借鉴+改** |
| LE 续签 | acme.sh `--cron` + jawj hook 思路 | 续签前备份旧证书 | acme.sh 默认 hook 没有"备份 + 回退"逻辑，**必须自己写 `renew-cert.sh`** | **自研脚本**（已在设计 §1.4.1） |
| iOS mobileconfig 生成 | jawj 的 cat heredoc 模式 | XML 模板字段、UUID 加密随机生成 | 我们的 server address 是 IPv6 单值，不混用 `ServerAddresses` 数组 | **借鉴+改**（设计 §10.1） |
| strongSwan 主配置模板 | jawj 的 `swanctl.conf` 写法 | `proposals`、`esp_proposals` 列表写法 | 我们 `local_addrs` 是 IPv6-only，他的是 `0.0.0.0` | **借鉴+改**（设计 §9.1） |
| swanctl `--list-sas` 解析 | strongSwan 官方 JSON 输出格式 | 字段名（`uniqueid`、`ike-sa`、`child-sas`、`bytes-in`） | **不要用 `--json`**——swanctl 5.x 的 JSON 输出有 bug，用 `--raw` | **自研解析**（架构 §3.5） |
| 每用户 swanctl 子配置 | jawj 的 `ipsec.secrets` 追加 | 每行一个 eap-<user> | 他写到一个全局文件，我们每用户一个独立 `.conf`（更好做用户级 reload） | **借鉴+改**（设计 §9.2） |
| iptables 规则 | jawj 的 iptables 脚本 | FORWARD 链放行 ESP、NAT MASQUERADE | 他的脚本假设 systemd-networkd，**我们容器内自己写 iptables** | **自研**（容器内 iptables 由 Dockerfile 处理） |
| 限速 tc HTB | jawj 没有；strongSwan 官方 wiki 有 | `tc class add ... htb` 写法 | 他的方案没说 down 时怎么处理，**我们用持久化 classid + 单 class 删除** | **自研**（设计 §9.2.1） |
| 客户端 `.sswan` 文件（Android） | strongSwan 官方 | `.sswan` XML schema | 直接用，**无修改** | **直接借鉴** |
| AppleScript（Mac 用户配置） | jawj | 一键 AppleScript 写法 | 我们面板里给 macOS 用户是 strongSwan 配置文本，不写 AppleScript（降低复杂度） | **不借鉴** |
| PowerShell（Windows 用户配置） | jawj | `Add-VpnConnection` 命令 | 同上，给文本配置 | **不借鉴** |
| Go web 框架组织 | alireza0/s-ui | 路由分组、middleware、handler 拆分 | 他的功能太多（多用户层级、流量统计图、订阅链接）；我们**只要他单文件 SQLite + ServeMux 的核心** | **借鉴组织思路**（架构 §2） |
| bcrypt + session | 标准 Go 实践 | `crypto/bcrypt` + cookie session | 无 | **直接借鉴** |
| 移动配置文件二维码 | skip2/go-qrcode | 标准库 | 无 | **直接借鉴** |

### 18.3 已知的"不适合我们"陷阱

| 陷阱 | 出现在哪些项目 | 为什么不适合 | 我们怎么处理 |
|---|---|---|---|
| **"禁用 IPv6"** | jawj 明确说 IPv6 关掉 | 我们宿主持 SoftEther 占 IPv4 UDP 500，**必须用 IPv6** | **反向**：我们默认 IPv6-only |
| **"email 发送配置文件"** | jawj 把配置发邮件 | VPS 商常封 25 端口，且邮件易进垃圾箱 | **不学**：我们自己给面板下载链接 + QR 码 |
| **`forceencaps yes`** | jawj + 多家教程 | 强制 UDP 4500 封装，绕过 NAT 问题 | 我们 IPv6-only 不需要 NAT，**不启用** |
| **certbot 而不是 acme.sh** | jawj + 多数教程 | certbot 需要 Python + systemd timer | 容器内 acme.sh 纯 shell 更好；**我们用 acme.sh** |
| **多 admin 账号** | s-ui 等商用项目 | 1-50 人规模、单一管理员 | **不学**：单一 admin 表，`id=1` 限制 |
| **HTML 嵌入 JS / Alpine.js** | s-ui、adminLTE 风格 | 服务端渲染就够了，省体积 | **不学**：`html/template` 单 HTML |
| **AES-256 必备 / ECDSA 证书** | 部分博客"安全最佳实践" | iOS IKEv2 拒 ECDSA；AES-256 在低带宽设备有性能损耗 | **让步**：RSA 2048 + AES-128-GCM，兼容所有平台 |
| **systemd unit 文件** | jawj、strongSwan 官方 | Docker 容器内没有 systemd | **不学**：`entrypoint.sh` 直接 exec |

### 18.4 借鉴时的检查清单（开发时填写）

每当你准备"参考某项目代码"时，**先把这段代码复制到 PR 描述里，然后逐条回答**：

```markdown
## 参考代码来源：[项目名 / 文件 / 行号]

1. **这段代码解决什么问题？**
   - 答案：

2. **我们的环境（容器 / IPv6-only / SQLite / 单 admin / 1-50 人）下是否成立？**
   - 答案：

3. **如果成立，需要改哪些地方？**（至少 3 条）
   - 答案：

4. **如果改的太多，是不是直接自研更划算？**（>50% 代码要改 = 自研）
   - 答案：

5. **有没有更简单的方案？**（用 stdlib / 已有依赖完成）
   - 答案：
```

**只有 5 个问题都答完，才能开始抄代码。**

---

## 19. 文档状态

- [x] v2 用户审阅通过设计文档（2026-09-16）
- [x] v2 起步架构文档（2026-09-16）
- [x] M8 容器化兼容（v2.1，2026-09-16）
- [x] M8.5 ipv6watch 守护（v2-72，2026-09-16，已就位但未单列里程碑）
- [x] M9 零硬编码部署（v2-79，2026-09-17） — §3.1 / §5.1 / §12 / §15 同步
- [x] 镜像跨网络环境通用（v2-79 起，不再因 IPv6 / 网卡名硬编码需要重 build）
- [x] M9.5 v2-79.1 三处 Critical 修复（ECDH / 私钥权限 / DNSSettings 格式）— §12 / §15 同步
- [ ] v2-79.1 用户实测（生产环境验收）