# IKEv2 Panel v2

单容器 IKEv2/IPsec VPN 管理面板：镜像内自带自编译 strongSwan（`kernel-libipsec`）和 Go 面板进程，
用一个 Web 面板管理用户、签发客户端配置、限速、DDNS 和证书续签。默认 IPv6-only，可切双栈。

适合家用 / 小团队自建 VPN（1–50 人），不适合对外提供公共服务。

## 关键特性

| 特性 | 说明 |
|------|------|
| 单容器 | strongSwan + 面板合成一个镜像，`docker compose up -d` 即可 |
| 网络零配置 | 出口网卡、公网 IP、VPN 虚拟网段由 entrypoint 自动探测，只有域名必须手填 |
| 用户自助接入 | 面板生成 iOS `.mobileconfig`（含二维码）、Android 原生参数页、strongSwan `.sswan` |
| 免内核 XFRM | ESP 走用户态 `kernel-libipsec`，不依赖宿主内核 XFRM 模块，OpenVZ/LXC 也能跑 |
| 证书自愈 | 默认自签 CA + 面板证书，容器销毁后从数据卷恢复；也可切 Let's Encrypt（DNS-01） |
| DDNS | 阿里云 DNS API 同步 A / AAAA 记录，公网 IP 变了自动改解析 |
| 断线自愈 | forwarding、nftables 规则、用户配置加载均有启动自愈 + 失败即拒绝启动 |
| 可观测 | `/healthz`、`/readyz`、`/metrics`（Prometheus 文本格式）、审计日志落盘 |

---

## 快速开始

### 前置条件

- Linux 宿主机，内核有 `/dev/net/tun`，且有公网 IPv6（IPv6-only 模式的硬性要求）
- Docker 与 Docker Compose v2
- 宿主防火墙放行 UDP 500、UDP 4500（IKEv2 与 NAT-T）

### 步骤

```bash
# 1. 取配置
cp .env.example .env

# 2. 至少改这一项（证书 CN + mobileconfig 的 RemoteIdentifier 都用它）
$EDITOR .env
#    IKEV2_SERVER_CN=vpn.example.com

# 3. 启动（scripts/up.sh = 建 ./logs 目录 + docker compose up -d）
./scripts/up.sh

# 4. 打开面板
#    https://<宿主IP>:8443/login
```

首次启动会自签一套 CA + 服务器证书 + 面板证书并写入数据卷，因此浏览器会报自签证书不受信任，
这是预期行为（客户端通过内嵌 CA 信任，不受影响）。

### 取初始管理员密码

首次启动会创建管理员 `admin`，密码同时打印到 stdout 并持久化到文件：

```bash
# 从日志取
docker compose logs ikev2-panel | grep -A3 "默认管理员已创建"

# 或从文件取（容器销毁 / 日志轮转后依然有效）
docker compose exec ikev2-panel cat /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
```

该文件**已存在时不会被覆盖**，所以重启容器不会把已改过的密码改回去。
面板检测到它存在就会一直提示"还在用默认密码"，改完密码后删掉它即可。

### 用 Let's Encrypt（可选）

自签模式下客户端零配置，但浏览器始终报警告。切 LE 需要公网域名 + 阿里云 DNS API 凭证：

```bash
# .env 里改这几项
IKEV2_CERT_MODE=letsencrypt
IKEV2_DOMAIN=vpn.example.com
```

凭证推荐直接在面板的「阿里云 API 凭证」卡片里填（写入 `/data/panel-state/aliyun.creds`），
不必写进 `.env`。也可以用环境变量，优先级从高到低：

| 变量 | 说明 |
|------|------|
| `IKEV2_ALIYUN_KEY_ID` / `IKEV2_ALIYUN_KEY_SECRET` | 推荐的统一命名 |
| `ALIYUN_ACCESS_KEY_ID` / `ALIYUN_ACCESS_KEY_SECRET` | 旧命名，加载时打 deprecation 警告 |
| `Ali_Key` / `Ali_Secret` | acme.sh 原生命名，最老的一层 |

> 切到 LE 是**破坏性变更**：所有已分发的 `.mobileconfig` 内嵌的是旧 CA，
> 换证书后必须重新生成并分发，否则客户端集体失配断连。升级前先看 [CHANGELOG.md](docs/CHANGELOG.md)。

---

## 部署要点

以下每条都对应 [`docker-compose.yml`](docker-compose.yml) 里的实际配置。

### 网络：只有 host 模式

`network_mode: host` 是**唯一支持的模式**，容器与宿主共享网络命名空间。

- **compose 里没有 `ports:` 映射**，host 模式下写了也不生效 —— 端口是容器直接在宿主上监听的。
- 不要去改 `/etc/docker/daemon.json` 配 IPv6 或转发开关：host 模式不走 Docker 的
  iptables / sysctl 管理，改那些只会在宿主上留下无关配置。
- 公网 IPv6 必须可达，所以 bridge 模式（Docker 默认从 `fd00::/8` 分 ULA，公网不可路由）
  天然不可用。`.env` 里若把 `IKEV2_NETWORK_MODE` 设成 `bridge` / `ipvlan`，
  entrypoint 会直接 FATAL 退出并说明原因。

### Capabilities

```yaml
cap_drop: [ALL]
cap_add: [NET_ADMIN, NET_RAW, NET_BIND_SERVICE, CHOWN, DAC_OVERRIDE]
security_opt: [no-new-privileges:true]
devices: [/dev/net/tun]
```

| Capability | 用途 |
|------------|------|
| `NET_ADMIN` | nftables 规则、路由表、forwarding 自愈 |
| `NET_RAW` | `tc` qdisc / class / filter（限速） |
| `NET_BIND_SERVICE` | 监听 500 / 4500 特权端口 |
| `CHOWN` | entrypoint 调整 `/etc/swanctl` 权限 |
| `DAC_OVERRIDE` | 读取 `/proc/sys/net` 下的转发开关 |

不要随手加 `SYS_ADMIN` 或 `privileged: true` —— 这个镜像不需要它们。

### 数据卷

```yaml
volumes:
  - ikev2-panel-v2_ikev2-data:/data
  - ./logs:/var/log:rw
  - /etc/sysctl.d:/host-sysctl.d:rw
```

命名卷的实际名字是 `{项目目录名}_ikev2-data`，即默认的 `ikev2-panel-v2_ikev2-data`。
**这意味着改名仓库目录会换一个卷名**：Compose 找不到旧卷就静默建一个空的，
面板数据与证书看起来"消失"且不报错。迁移或改名后请显式对齐：

```bash
# 改名后把旧卷挂到新名字上
docker volume rename ikev2-panel-v2_ikev2-data <新目录名>_ikev2-data
```

`/data` 下持久化四类数据：`panel.db`（用户 / 审计）、`panel-state/`（运行时配置与凭证）、
`le/`（LE 证书）、`acme-sh/`（acme.sh 工作目录，避免重启触发 LE 速率限制）。

`/etc/sysctl.d:/host-sysctl.d:rw` 只在 forwarding 自愈成功时才会被写入 `99-ikev2.conf`，
让宿主重启后仍然生效；宿主上原本没有该文件时，自愈不会留下任何配置痕迹。

### 端口与防火墙

| 端口 | 协议 | 用途 | 建议 |
|------|------|------|------|
| 500 | UDP | IKEv2 协商 | 对所有客户端开放 |
| 4500 | UDP | IKEv2 NAT-T（`kernel-libipsec` 强制） | 对所有客户端开放 |
| 8443 | TCP | 管理面板 HTTPS（默认 `0.0.0.0:8443`） | **限制源 IP** |
| — | ESP / IP 协议号 50 | 不需要 | libipsec 走 UDP/4500 |

host 网络下 8443 直接暴露在公网，面板自身没有 RBAC 和 2FA，务必用防火墙或反向代理限制来源。
面板前面挂了反代时才设 `IKEV2_TRUSTED_PROXY=true`，否则登录限流只按直连地址计数。

### 日志

业务日志 bind 到宿主 `./logs`（`scripts/up.sh` 会建目录并 `chmod 1777`），
容器 stdout/stderr 另由 Docker 的 `json-file` 驱动按 20 MB × 5 滚动。

```bash
tail -f ./logs/audit.log        # 审计日志
tail -f ./logs/charon.log       # IKE daemon
docker compose logs -f          # 容器 stdout
```

---

## 运行时配置生效时机

改完配置后什么时候生效，取决于值从哪里来。

| 配置 | 改完何时生效 |
|------|--------------|
| `IKEV2_SERVER_CN` / `IKEV2_CERT_MODE` / `IKEV2_DOMAIN` | **重启容器**（entrypoint 启动时替换 swanctl.conf 占位符） |
| `IKEV2_IPV6_ONLY` / `IKEV2_OUT_IF` / `IKEV2_VPN_SUBNET` | **重启容器** |
| `IKEV2_LISTEN_ADDR` / `IKEV2_LOG_LEVEL` / `IKEV2_DISPLAY_TIMEZONE` | **重启容器** |
| `IKEV2_SESSION_TTL` / `IKEV2_DEFAULT_USER_PASSWORD_LEN` | **重启容器** |
| 面板里改的证书配置、客户端虚拟网段 | **重启容器**（`cert.conf` / `subnet.conf` 由 entrypoint 启动时读；网段另有"保存并立即生效"按钮，不重启也能改） |
| 面板里填的阿里云凭证 | **无需重启**，下一轮 DDNS 同步现读（`CredentialGetter`） |
| 面板里的 DDNS 开关 / 记录类型 / RR / 同步周期 | **立即生效**，进程内直接生效 |
| 面板里的 mobileconfig 默认值 | **立即生效** |
| 面板 HTTPS 证书（`panel-tls/`） | 收到 `SIGHUP` 时重载 |

> **SIGHUP 只做一件事：重载面板自己的 TLS 证书。**
> `kill -HUP` 不会重新读取任何环境变量，也不会刷新阿里云凭证 ——
> 凭证是每轮同步现读的，根本不需要信号；环境变量则必须重启容器。

---

## 备份与恢复

[`scripts/backup-restore.sh`](scripts/backup-restore.sh) 在**宿主机**上运行，
用 `DATA_DIR` 指向命名卷的挂载点（默认 `/data` 是容器内路径，宿主机上没有）：

```bash
DATA_DIR=/var/lib/docker/volumes/ikev2-panel-v2_ikev2-data/_data ./scripts/backup-restore.sh backup
```

| 子命令 | 作用 |
|--------|------|
| `backup` | 备份运行时数据 + 代码 git bundle |
| `backup-data` | 只备份运行时数据 |
| `list` | 列出已有备份 |
| `verify <备份目录>` | 校验（含 SQLite 完整性检查） |
| `restore <备份目录>` | 覆盖恢复运行时数据，需输入 `yes` 确认，完成后需 `docker compose restart` |
| `snapshot <名称>` / `rollback <ref>` | 代码快照与回滚指引 |

备份默认落在 `/opt/ikev2-backups`，每个备份目录带 `MANIFEST.txt`。
`restore` 会把 `panel.db` 权限复位为 `0600`。

---

## 已知限制与故障自愈

### 限速只作用于客户端上传方向

`scripts/ikev2-updown` 里的 tc filter 匹配的是 **源地址为客户端虚拟 IP** 的报文
（`u32 match ip src <peer>` / `flower src_ip <peer>`），挂在出口网卡上。
所以限速约束的是**客户端 → 公网**的流量，**下载方向不限速**。
需要限制下载的话得在客户端侧或路由器上另做。

限速单位是 Mbps，`0` 表示不限速。IPv6 限速依赖 `tc flower`（内核 4.1+），
不支持时只打 WARN、不阻断 SA 建立。

### libipsec 是单核瓶颈

ESP 走用户态解密，单 SA 绑定单线程，**无法多核扩展，也不支持网卡硬件 offload**。

| 场景 | 是否适合 |
|------|----------|
| 1–20 人，每人 ≤ 5 Mbps | 适合 |
| 20–50 人，每人 ≤ 5 Mbps | 可用，CPU 建议支持 AES-NI |
| 50+ 人，或总带宽 > 500 Mbps，或延迟敏感业务 | 不适合，应改用内核 XFRM |

MOBIKE 默认开启，客户端切网时复用现有 IKE SA，切换不掉线。

### forwarding 自愈，失败即拒绝启动

容器作为 VPN 路由器需要宿主打开转发。`compose` 里**没有 `sysctls:` 段** ——
host 网络模式下 runc 会以 `not allowed in host network namespace` 拒绝它。
转发开关由 entrypoint 在容器内直接写 `/proc/sys/net/...`（host 模式共享 net ns，
写的就是宿主内核），成功后再把值持久化到宿主 `/etc/sysctl.d/99-ikev2.conf`。

两步都失败时 entrypoint **FATAL `exit 11`**，容器起不来。这是刻意设计：
宁可启动失败，也不要跑出一个"看起来正常但客户端连上后上不了网"的实例。

nftables 规则同理：启动时校验自定义表 `ikev2` / `ikev26` 是否生效，缺失则重建重试
（快阶段 1s × 10，慢阶段 5s × 50）。彻底失败会写哨兵文件，面板顶部告警。

### VICI socket 权限没有配置项

strongSwan 的 `vici` 插件把 socket 权限**硬编码为 `0770 root:root`**，
strongSwan 侧**不存在**任何能改这个权限的配置项（改 `charon.conf` 无效）。
当前镜像里面板进程以 root 运行，可以直接访问，所以不影响使用。
若将来把面板改成非 root 运行遇到 `permission denied`，
只能从 socket 的属组入手，不要指望 strongSwan 配置。

### 其他

- 用户 VPN 密码明文存库（EAP-MSCHAPv2 的取舍），数据库文件权限 `0600`。
- 单一管理员账号，无 RBAC / 2FA。
- 二维码里的面板地址取自访问用的 `Host`，部署时必须用域名或 IP 访问，不能用 `localhost`。
- **二维码只对 iOS / macOS 有效。** 它下发的是 `.mobileconfig`（Apple MDM 专有格式），
  Android 浏览器无法解析 —— Android 用户请用 `/users/<id>/android`（系统原生填参）
  或 `/users/<id>/sswan`（strongSwan App 导入）。二维码里的 token 是一次性的，
  在 Android 上扫了会白白作废。
- 本地开发模式（判定依据是 `/etc/swanctl` 目录是否存在，而非 VICI 连通性）
  把用户配置写到 `<DataDir>/swanctl-conf.d/` 并跳过所有 VICI 操作，仅供调试。

---

## 文档地图

| 你要做的事 | 看这里 |
|------------|--------|
| 要改代码 | [architecture.md](docs/architecture.md) |
| **要改前端模板** | [architecture.md §12.9](docs/architecture.md)（CSP 与模板转义两条硬边界） |
| 要调接口 | [API.md](docs/API.md) |
| 出问题了 | [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)（§1-7 服务端 · §8 纯前端） |
| 升级前必看 | [CHANGELOG.md](docs/CHANGELOG.md) |
| 配置项清单 | [.env.example](.env.example)（唯一权威） |

文档分层约定见 [docs/README.md](docs/README.md)：一个问题只应有一个权威出处，
配置项清单只维护一份（在 `.env.example`），版本变更只维护一份（在 CHANGELOG）。
**文档描述代码当前状态，不描述设计意图**；发现文档与代码不符时以代码为准。

### 改前端前必须知道的两条边界

写在 [architecture.md §12.9](docs/architecture.md)，这里只列结论：

1. **CSP 没有 `'unsafe-inline'`** —— `onclick="return confirm(...)"` 会被浏览器
   **静默拒绝**，破坏性操作的二次确认等于没有。二次确认一律用 `data-confirm`。
   不要为了"让它能用"去放松 CSP。
2. **banner 的 `.Msg` 永远转义，`MsgHTML` 只能是代码里的字面量。**
   动态值（密码、用户名、外部 API 错误串）不进 `MsgHTML`，拆成同级独立元素。

两条都有回归测试锁住（`internal/web/p0_banner_confirm_test.go`）。

## 模块结构

Go 单体，内部按包分层：`cmd/ikev2-panel`（入口）→ `internal/*`（auth / cert / config /
runtime / panelstate / store / swanctl / web / ddns / limit / expiry 等），
外加 `scripts/`（entrypoint、updown、续签、备份）和 `configs/`（swanctl 模板）。
完整包结构、依赖方向和数据流见 [architecture.md §1](docs/architecture.md)。
