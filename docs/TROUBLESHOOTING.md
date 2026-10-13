# 故障排查 / Troubleshooting

> **找症状 → 跟着排查命令跑 → 找到解法。**
> 命令默认在容器内执行：`docker compose exec ikev2-panel bash`。
> 涉及宿主机操作时会额外标注 `[host]`。
> 行为为什么是这样 → [architecture.md](architecture.md)；接口细节 → [API.md](API.md)；
> 某个行为是哪次改的 → [CHANGELOG.md](CHANGELOG.md)。

## 目录

1. [容器启动失败](#1-容器启动失败)
2. [mobileconfig 装不上 / 连不上](#2-mobileconfig-装不上--连不上)
3. [Let's Encrypt 续签失败](#3-lets-encrypt-续签失败)
4. [DDNS 不更新](#4-ddns-不更新)
5. [限速不生效](#5-限速不生效)
6. [admin 密码忘](#6-admin-密码忘)
7. [DB 损坏](#7-db-损坏)
8. [面板 UI 异常](#8-面板-ui-异常)（确认弹窗 / 横幅乱码 / 滚动位置 / 复制假成功 / 双层滚动条）

> 前 7 节是 **VPN 与容器**问题，第 8 节是**纯前端**问题。
> VPN 正常但界面不对，直接跳第 8 节。

---

## 1. 容器启动失败

> 先看退出码 —— `entrypoint.sh` 每个阶段失败都 `exit <码>`，`restart: unless-stopped`
> 会把它变成无限重启。`docker compose logs ikev2-panel | grep FATAL` 一眼定位。

| 退出码 | 含义 | 对应小节 |
|--------|------|----------|
| 10 | 找不到公网 IPv6 地址（`IKEV2_IPV6_ONLY=true` 时） | [1.1](#11-找不到公网-ipv6-地址退出码-10) |
| 11 | IP forwarding 启用失败 | [1.2](#12-ip-forwarding-启用失败退出码-11) |
| 12 | 网络模式不是 host / charon 启动失败 | [1.3](#13-网络模式不是-host退出码-12) · [1.4](#14-charon-启动失败退出码-12) |
| 14 | 探测不到对外网卡 | [1.5](#15-探测不到对外网卡退出码-14) |
| 15 | nft 规则集加载失败 | [1.6](#16-nft-规则集没生效) |
| 17 / 18 / 19 | LE 模式缺域名 / 签发失败 / 装证书失败 | [3. Let's Encrypt](#3-lets-encrypt-续签失败) |

### 1.1 找不到公网 IPv6 地址（退出码 10）

**症状**：容器反复重启，日志里是

```
[entrypoint] FATAL: no global IPv6 address found
[entrypoint] This project REQUIRES a public IPv6 address when IKEV2_IPV6_ONLY=true
```

**排查**：

```bash
ip -6 addr show                       # 有没有 2000::/3 的 global 地址（不是 fe80:: / fd00::）
ip -6 route show default              # 有没有 IPv6 默认路由
```

**解法**：

- 宿主根本没配公网 IPv6 → 先在宿主/路由器上开通 IPv6，这是硬前提。
- 宿主有 IPv6 但探测不到 → 多网卡 / 默认路由不在预期接口时，显式指定：
  ```bash
  # .env
  IKEV2_OUT_IF=eth0
  IKEV2_SERVER_ADDR_V6=2001:db8::1
  ```
- 确实不需要 IPv6 → `IKEV2_IPV6_ONLY=false`（此时改走 IPv4，见 [1.2](#12-ip-forwarding-启用失败退出码-11)）。

### 1.2 IP forwarding 启用失败（退出码 11）

**症状**：容器反复重启，日志里是下面**两条之一**：

```
[entrypoint] FATAL: net.ipv4.ip_forward=0 and cannot auto-enable          # 一定会检查
[entrypoint] FATAL: net.ipv6.conf.all.forwarding=0 and cannot auto-enable # v2.86-PR19(A16) 起一定会检查
```

> **v2.86-PR19(A16) 变更**：以前 IPv6 forwarding 只在 `IKEV2_IPV6_ONLY=false`
> 时才检查，导致默认配置（`=true`）下它**永远不会被开启**。详见
> [1.2.1](#121-ipv6-forwarding-恒为-0-被-ipv4-回退掩盖)。

**为什么不是 docker daemon 配置问题**：本项目固定 `network_mode: host`，容器与宿主
**共享网络命名空间**，不走 docker 的 iptables / sysctl 管理通道。改
`/etc/docker/daemon.json`（`ipv6: true` / `ip6tables: true`）对本项目**无效**。

同理，compose 里的 `sysctls:` 块也已被移除 —— host 网络模式下 runc 会以
`not allowed in host network namespace` 拒绝它。

**实际机制**：`scripts/entrypoint.sh` 的 `enable_forwarding()` 直接往
`/proc/sys/net/...` 写（host netns 下写的就是宿主内核），成功后再由
`persist_sysctl()` 把值落到宿主 `/etc/sysctl.d/99-ikev2.conf` 以便宿主重启后保持。

**排查**（先分清是哪一类失败）：

```bash
# 1. 当前值（宿主和容器内显示同一个值）
cat /proc/sys/net/ipv4/ip_forward
cat /proc/sys/net/ipv6/conf/all/forwarding

# 2. 自愈有没有成功过 —— 成功会打这一行
docker compose logs ikev2-panel | grep auto-heal
#   [entrypoint] [auto-heal] /proc/sys/net/ipv4/ip_forward set to 1 (host net ns, effective on host)

# 3. 失败时区分「写不进去」和「持久化挂载缺失」——两者后果完全不同
docker compose logs ikev2-panel | grep persist
#   [entrypoint] [persist] wrote net.ipv4.ip_forward = 1 to /host-sysctl.d/99-ikev2.conf
#   [entrypoint] [persist] /host-sysctl.d not mounted, skipping host-side persist   ← 只是跳过持久化，不致命
#   [entrypoint] [persist] /host-sysctl.d not writable, skipping host-side persist  ← 同上，不致命
```

| 现象 | 含义 | 处置 |
|------|------|------|
| 容器起不来，日志 `FATAL: ... cannot auto-enable` | `/proc/sys/net/...` **不可写**，自愈失败 | 见下方「写不进去」 |
| 容器正常，但日志有 `not mounted` / `not writable` | forwarding 已开，只是**没持久化**到宿主 | 可忽略；宿主重启后靠容器自愈重新拉起。若想消除该日志，确认 `docker-compose.yml` 里 `/etc/sysctl.d:/host-sysctl.d:rw` 这条挂载存在且宿主目录可写 |

**「写不进去」的处置** `[host]`：

```bash
# 1. 确认 capability 没被裁掉（compose 的 cap_add 必须含 NET_ADMIN）
docker inspect ikev2-panel --format '{{json .HostConfig.CapAdd}}'

# 2. 确认没有把 no-new-privileges / seccomp 改成会拦下的组合
docker inspect ikev2-panel --format '{{json .HostConfig.SecurityOpt}}'

# 3. 宿主侧兜底：手工打开（验证「是不是内核/宿主层面拒绝写」）
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv6.conf.all.forwarding=1
```

手工 `sysctl -w` 能成功 → 说明内核允许，只是容器内 capability 被裁了，回去修
`docker-compose.yml` 的 `cap_add`。手工也失败 → 宿主 sysctl 被硬锁（部分
VPS/OpenVZ 面板限制），需要在宿主机面板里放开。

> 唯一的例外：宿主内核没编 IPv6（`CONFIG_IPV6=n`）→ `/proc/sys/net/ipv6`
> 目录不存在 → entrypoint 跳过检查并打
> `[entrypoint] WARN: kernel built without IPv6, skipping net.ipv6.conf.all.forwarding`。
> 这种宿主不会 exit 11（它本来就不需要 IPv6 转发）。

### 1.2.1 IPv6 forwarding 为 0

**症状**：一切正常 —— 容器不重启、VPN 能连、能上网 —— 但：

```bash
cat /proc/sys/net/ipv6/conf/all/forwarding   # 0，且重启后仍是 0
nft list table ip6 ikev26 | grep counter    # 所有 counter 恒为 packets 0 bytes 0
```

**注意**：counter 为 0 在当前版本下**是正常的**，原因见下面「为什么 counter 是 0」。
`forwarding` 为 0 才是需要修的。

**根因（两层）**：

**第一层**：`enable_forwarding ipv6` 曾经被包在
`if [ "$IKEV2_IPV6_ONLY" != "true" ]` 里，而默认配置就是 `=true`，
所以这个函数**一次都没被调用过**。IPv4 那侧一直是无条件检查，IPv6 侧却多一层条件，
本身就说明是遗漏而非设计 —— v2.86-PR12.10 起 `local_ts` 恒为双栈，
`IKEV2_IPV6_ONLY` 只控制"监听/拨号用哪个网络栈"，**不影响 ESP 内层协议族**。
v2.86-PR19(A16) 已移除该条件。

**第二层（更关键）**：**entrypoint 里的"自愈"从来就没成功过**，两个协议族都没成功过。

容器内 `/proc/sys` 是 **ro** 挂载：

```
$ grep ' /proc/sys ' /proc/mounts
proc /proc/sys proc ro,nosuid,nodev,noexec,relatime 0 0
```

`echo 1 > /proc/sys/net/...` 在容器内必然失败（`Read-only file system`）。
`NET_ADMIN` capability 救不了 —— ro 是挂载属性，不是权限。

IPv4 之所以一直绿灯，只是因为宿主本来就开着 `ip_forward=1`，
走的是函数开头"已开就直接返回"的分支，从没触发过写入。

> 因此 v2.86-PR19(A16) 的日志文案也一并修正：IPv4 未开启 → `FATAL exit 11`
> （没有任何转发，所有客户端都出不了网）；IPv6 未开启 → `WARN` 并提示宿主命令
> （客户端会 IPv4 回退，属降级不属故障）。

**为什么 counter 是 0（不是转发故障）**：

客户端**只拿到 IPv4 地址**，因为 strongSwan 的 pool 混写了地址族，
IPv6 段被静默丢弃 —— 详见 §1.2.2。CHILD_SA 的 remote TS 是 `10.10.0.1/32`，
本来就不会产生 IPv6 内层流量。

所以：**先确认客户端拿到的是 IPv4 还是 IPv6，再决定要不要查 forwarding。**

```bash
docker exec ikev2-panel swanctl --list-sas | grep remote
# remote 10.10.0.1/32  → 只有 IPv4，counter 0 正常
# remote 10.10.0.1/32, fd00:1::x/128 → 双栈，此时 forwarding=0 才会真正丢包
```

**修复（必须在宿主上做）**：

```bash
sudo sysctl -w net.ipv6.conf.all.forwarding=1

# 持久化（推荐，否则宿主重启后丢回 0）
printf 'net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n' \
  | sudo tee /etc/sysctl.d/99-ikev2.conf
sudo sysctl --system
```

改完确认 ens18 的 SLAAC 地址没有丢失（`ip -6 addr show dev ens18`，`valid_lft` 应持续续期），
以及 IPv6 出网通不通：

```bash
ping -6 -c3 240c::6666
```

> compose 里的 `sysctls:` 块**解决不了**这个问题 —— host 网络模式下 runc 会以
> `not allowed in host network namespace` 拒绝（v2-79.2 已实测）。

### 1.2.2 客户端只拿到 IPv4 地址（pool 混写地址族）

**症状**：`configs/swanctl-ipv6-only.conf` 里明明写了 IPv6 段，
`swanctl` 也报 `successfully loaded`，但客户端只拿到 IPv4：

```
$ docker exec ikev2-panel swanctl --list-pools
ikev2-pool   10.10.0.1    0 / 0 / 254       ← 只有 254 条，IPv6 段没了

$ docker exec ikev2-panel swanctl --list-sas | grep remote
remote 10.10.0.1/32                         ← 没有 fd00:1::x
```

**根因**：**单个 pool 的 `addrs` 不支持混合地址族**。混写时 IPv6 段被**静默丢弃**，
没有任何警告或报错。

对照实测（strongSwan 6.0.1）：

| `addrs` | `--list-pools` 结果 |
|---|---|
| `fd00:1::/64` | `fd00:1::1` 0 / 0 / **2147483646** ✅ |
| `10.10.0.0/24, fd00:1::/64` | `10.10.0.1` 0 / 0 / **254** ❌ |

**官方写法**（[Virtual IP Addresses](https://docs.strongswan.org/docs/latest/features/vip.html#_responder_configuration）
要求按地址族拆 pool）：

```
connections.ikev2-rw.pools = v4pool, v6pool
pools.v4pool.addrs = 10.10.0.0/24
pools.v6pool.addrs = fd00:1::/64
```

**当前是否需要修**：**不需要**。客户端拿 IPv4 完全正常工作，功能无损。

真要改成双栈需注意两个前提：

1. 客户端要在 `vips` 里显式请求 `0.0.0.0, ::`，否则仍只拿一个。
2. 宿主/路由器对 LAN→WAN 的 IPv6 出向可能有防火墙限制（本项目在 192.168.50.63 上
   实测过 `curl -6 ipv6.google.com` 超时）。客户端一旦真发 IPv6，可能反而变慢。

**排查陷阱**：有在线租约时 `swanctl --load-pools` 会报
`vici pool ikev2-pool has online leases, unable to replace`。先断开 VPN，
或 `swanctl --terminate --child ikev2-rw`（注意不带 `--child` 会报
`missing terminate selector`）。

### 1.3 网络模式不是 host（退出码 12）

**症状**：

```
[entrypoint] FATAL: IKEV2_NETWORK_MODE=bridge is not supported
[entrypoint]   解决:从 .env 删掉 IKEV2_NETWORK_MODE 那一行(或改成 host)
```

**解法**：本项目**只支持 host 网络**（需要直绑公网 IPv6、且 UDP 500/4500 不做端口映射）。
把 `.env` 里的 `IKEV2_NETWORK_MODE` 删掉或改成 `host`，然后 `docker compose up -d`。

> 同一个退出码 12 也用于「charon 启动失败」，两者靠日志关键字区分：
> 前者有 `IKEV2_NETWORK_MODE`，后者有 `FATAL: charon failed to start`（见 [1.4](#14-charon-启动失败退出码-12)）。

### 1.4 charon 启动失败（退出码 12）

**症状**：日志 `FATAL: charon failed to start`，且会附带打印 `/var/log/charon.log` 最后 30 行。

**排查**：

```bash
docker compose logs ikev2-panel | grep -A30 "charon failed to start"
tail -50 /var/log/charon.log
```

**解法**：最常见是证书没就位。

- LE 模式 → 签发失败，见 [3. Let's Encrypt](#3-lets-encrypt-续签失败)。
- 自签模式 → 证书由 Go 进程在 charon 起来**之后**才生成，属于预期时序；
  若持续失败，检查 `/data/` 卷是否可写（`ls -la /data`）。

### 1.5 探测不到对外网卡（退出码 14）

**症状**：`FATAL: cannot detect outbound interface`。

**排查**：容器是 host netns，网卡探测结果应与宿主一致。

```bash
ip -4 route show default
ip -6 route show default
ip -o link show up
```

**解法**：三条都空（宿主没有默认路由）→ 宿主网络没配好。
只是名字不是预期的 → 在 `.env` 里显式指定 `IKEV2_OUT_IF=<iface>`。

### 1.6 nft 规则集没生效

转发规则在 nft **自定义表** `ikev2` / `ikev26` 里（不是 docker 的 `filter`/`nat`
表，所以 docker daemon 重写规则时碰不到）。客户端能拨号但上不了网时先看这里。

**症状**：拨号成功、`swanctl --list-sas` 显示 ESTABLISHED，但客户端访问不了公网。

**排查**：

```bash
# 1. 表在不在？counter 有没有在涨（有非 0 数据 = 流量确实在转发）
docker compose exec ikev2-panel nft list table ip ikev2
docker compose exec ikev2-panel nft list table ip6 ikev26

# 2. MSS clamp 链在不在（VPN 隧道 MTU ~1400，大包被丢会表现为「网页打不开但能 ping 通小包」）
docker compose exec ikev2-panel nft list table ip ikev2 | grep mangle
# 期望能看到 mangle_forward / mangle_output 两个 chain

# 3. 重试循环的最终结果
docker compose logs ikev2-panel | grep "ruleset verified\|not stable"
#   期望 [entrypoint] nft ikev2/ikev26 ruleset verified after N retries

# 4. 彻底不稳时会写 sentinel 文件（内容形如 iptables_unstable=true retries=60 ...）
docker compose exec ikev2-panel cat /run/ikev2-panel/iptables-warn.flag
```

**解法**：

```bash
# 手动重新提交（先删 stale 表再原子重建，与脚本内行为一致）
docker compose exec ikev2-panel nft delete table ip ikev2
docker compose exec ikev2-panel nft delete table ip6 ikev26
docker compose exec ikev2-panel nft -f /run/ikev2-panel/ikev2.nft
docker compose exec ikev2-panel nft list table ip ikev2   # 验证回来了
```

彻底加载失败（`FATAL: nft -f failed`，退出码 15）→ 检查宿主是否装了 nftables、
以及 `/run/ikev2-panel/ikev2.nft` 是否存在（由 `scripts/ikev2.nft.template`
渲染生成）。

---

## 2. mobileconfig 装不上 / 连不上

### 2.1 iOS 不弹「安装描述文件」

**症状**：Safari 打开 mobileconfig 链接，下载后无提示。

**排查**：该路由需要已登录（未登录会 302 跳 `/login`），用面板登录后的 cookie 验：

```bash
curl -I -b 'session=<面板登录后的 cookie>' \
  https://your-domain:8443/users/3/mobileconfig
# 期望 Content-Type: application/x-apple-aspen-config; charset=utf-8
#   且 Content-Disposition: attachment; filename="<用户名>.mobileconfig"
```

**解法**：

- 登录后从「用户详情页 → 下载描述文件」重新取一次，Safari 对已下载过的同名文件不会二次弹窗
- 检查 Safari 是否拦截弹窗（设置 → Safari → 拦截弹窗 → 关）
- 直接打开「设置 → 通用 → VPN 与设备管理」看是否已下载但未提示

### 2.2 装完连不上（iOS / 通用）

**症状**：iOS 显示「无法连接到 VPN」。

**排查**：

```bash
docker compose exec ikev2-panel tail -f /var/log/charon.log
```

| 日志关键字 | 含义 | 处置 |
|-----------|------|------|
| `NO_PROPOSAL_CHOSEN` | 提议算法协商不上 | 检查 `/data/` 里的服务端证书是否与 mobileconfig 内联 CA 匹配（证书被重新生成过就会出现） |
| `AUTHENTICATION_FAILED` | 用户名/密码错 | 面板 → 用户详情页 → 重置密码 |

**补充排查**：RemoteAddress 用域名还是 IP 字面量很关键。
若填的是 IP，iOS 会去证书的 `IPAddresses` SAN 里找，找不到就报「服务器身份不可信」。
**用 DDNS 域名**（证书 `dNSName` SAN 里有），别用 IP 字面量。

### 2.3 Android 原生连不上（11+）

**症状**：Android 设置 → VPN → 连接失败。

**排查**：查看 Android 系统 VPN 日志（开发者选项 → 日志查看器，或 `adb logcat | grep -i ike`）。

常见错误：

| 日志关键字 | 处置 |
|-----------|------|
| `MSCHAPV2 failed` | 用户名/密码错 |
| 证书不受信任 | 仅**自签模式**需要装 CA：浏览器打开 `https://your-domain:8443/ca.cert.pem` 下载，装到「系统」信任存储（用户级存储对 VPN 不生效）。**Let's Encrypt 模式不要装**，系统已信任 ISRG Root，在 VPN 设置里把「CA 证书」留空即可 |

填参提示（AOSP IKEv2/IPSec MSCHAPv2 表单）：

| 字段 | 填什么 |
|------|--------|
| 名称 | 任意 |
| 类型 | IKEv2/IPSec MSCHAPv2 |
| 服务器 | 面板地址（DDNS 域名，非 IP） |
| IPsec 标识符 | **用户名**（与下面「用户名」相同） |
| 用户名 / 密码 | 面板用户详情页展示的值 |
| CA 证书 | LE 模式留空；自签模式选已安装的 CA |
| 证书类型 | 留空或选「CA 证书」——**不要选「RSA 证书」**，那会要求客户端证书，本面板是纯密码认证 |
| 服务器标识 | 留空或填证书 CN（DDNS 域名） |

### 2.3.1 Android strongSwan App 连不上

**先确认走对了路**：面板的**二维码只支持 iOS / macOS**。二维码指向 `/install/<token>`，服务端返回的是 `.mobileconfig`（Apple MDM 专有 plist，`PayloadType = com.apple.vpn.managed`），**Android 浏览器只会下载一个装不上的文件**，而且 token 是一次性的（用掉即作废）。

Android 用户应改用：

- **系统原生**：`/users/<id>/android` 页面的手工填参指引（见 2.3）
- **strongSwan App**：从用户详情页下载 `.sswan`，App 内「设置 → 导入配置」

`.sswan` 导入后的两个必查项：

1. **密码是空的** —— 文件不含密码（`secret = ""`），需在 App 连接属性里手填
2. **本地 ID 要填用户名** —— 否则服务端发 EAP-Identity Request 时客户端无从应答

App 内看日志：设置 → 日志查看器，或 `adb logcat | grep -i charon`。

**常见错误**：

| 日志关键字 | 处置 |
|-----------|------|
| `EAP-Identity request configured, but not supported` | 服务端 `eap-identity` 插件缺失（面板 Dockerfile 漏了 `--enable-eap-identity`） |
| `constraint check failed: EAP identity` | 服务端配了 `eap_id = %identity`（约束语义），应改为 `%any`（索要语义） |
| `unable to resolve hostname` | `.sswan` 里的 `remote_addrs` 域名没解析，检查 DDNS 是否正常 |
| `AUTHENTICATION_FAILED` | 密码错，或该用户已被禁用 |

### 2.4 能拨号但上不了网（已连接但无流量）

**症状**：显示「已连接」，但任何 app 都打不开网页。`swanctl --list-sas` 显示 ESTABLISHED。

这类问题的根因基本都在转发/路由侧，逐层排：

```bash
# 1. SA 协商成功？local 应该是双栈 TS，remote 是客户端虚拟 IP
docker compose exec ikev2-panel swanctl --list-sas
# 期望 ESTABLISHED + INSTALLED + local '0.0.0.0/0 ::/0'

# 2. 转发链有没有计数（详见 §1.6）
docker compose exec ikev2-panel nft list table ip ikev2
docker compose exec ikev2-panel nft list table ip6 ikev26

# 3. XFRM 出向路由有没有 default（table 220）
ip route show table 220
```

**常见根因**：

- **`local_ts` 不是双栈**（只有 `::/0`）→ 客户端拿到 IPv4 虚拟 IP 后发的 IPv4 inner
  packet 不匹配 TS，内核 libipsec 直接 DROP，症状正是「连上但没流量」。
  entrypoint 默认写死双栈 `0.0.0.0/0, ::/0`；出现这个现象说明
  `/etc/swanctl/swanctl.conf` 被手工改过或被外部程序覆盖了。
- **nft 表缺失或计数为 0** → 走 [1.6](#16-nft-规则集没生效)。
- **`table 220` 没有 default route** → 出向包被丢。
- **MSS 未 clamp** → 小包通、大包不通（表现：能 ping 通但网页打不开）。见 §1.6 第 2 条。

---

## 3. Let's Encrypt 续签失败

### 3.1 LAST_RENEW_FAILED 标志

**症状**：面板首页红色横幅「LE 续签失败」。

**排查**：

```bash
ls -la /data/le/LAST_RENEW_FAILED     # 存在 = 上次续签失败并已回滚到备份
docker compose exec ikev2-panel tail -50 /var/log/ikev2-renew.log        # 每天主任务
docker compose exec ikev2-panel tail -50 /var/log/ikev2-renew-retry.log  # 12h 后的重试
docker compose exec ikev2-panel tail -50 /var/log/acme-renew.log        # acme.sh 原始输出
```

**解法**：先按错误关键字定位，再手动重试。

| 日志关键字 | 含义 | 处置 |
|-----------|------|------|
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | AccessKey 错或签名不匹配 | 面板 → 阿里云 API 凭证 重新填（写 `/data/panel-state/aliyun.creds`），**重启容器**生效（LE 凭证是 entrypoint 启动时注入 acme.sh 的） |
| `DomainRecordNotExist` | **该账号下没有这个域名** | 见 [3.2](#32-domainrecordnotexist)，不要去加 A/AAAA 记录 |
| `rate limit exceeded` | Let's Encrypt 速率限制 | 见 [3.3](#33-lets-encrypt-速率限制) |
| `acme.sh --issue failed` / `acme.sh --install-cert failed` | 首次签发阶段失败（容器起不来，退出码 18 / 19） | 容器**根本没起到 cron 这一步**，所以既没有 `LAST_RENEW_FAILED` 也没有续签日志。直接看启动日志里 acme.sh 输出的最后 20 行；缺凭证时 acme.sh 的报错具有误导性（看起来像 `Error adding TXT record`），实际是 `Ali_Key`/`Ali_Secret` 没注入 |

修好后手动重试：

```bash
docker compose exec ikev2-panel bash -c \
  'rm -f /data/le/LAST_RENEW_FAILED && /etc/ikev2-panel/scripts/renew-cert.sh your.domain.com'
```

> **已知不一致（待确认）**：`scripts/renew-cert.sh` 调 acme.sh 用的是
> `--home /root/.acme.sh`，而 `scripts/entrypoint.sh` 签发时用的是
> `--home /data/acme-sh`。若重试报「找不到证书 / 账号」，就是这个路径不一致导致的。

### 3.2 DomainRecordNotExist

**这个错误码不是「A/AAAA 记录不存在」**，而是**阿里云账号下没有这个域名（zone）**。
DNS-01 验证写的是 `_acme-challenge.<domain>` TXT 记录，**不要求主记录已存在**，
所以「先手动加一条解析」是无效处置。

**排查**：这个错误码只有阿里云 API 能给出权威答案 —— 本地 `dig` 查不到不能作为
「域名不在账号下」的证据（见文末「排查方法论」）。两个途径：

- 面板首页 DDNS 卡 →「查询阿里云记录值」（内部调阿里云 `DescribeDomainRecords`，
  只读，不改记录）
- 直接登录 [阿里云 DNS 控制台](https://dns.console.aliyun.com/) 看域名列表

> 仓库里有 `cmd/dnsq` 这个查询工具（同样直问阿里云 API），但**当前 Dockerfile
> 只构建 `cmd/ikev2-panel`**，镜像里并没有 `/usr/local/bin/dnsq` 这个二进制。
> 要用得自己 `go build -o dnsq ./cmd/dnsq` 再 `docker cp` 进容器。

**解法**：

1. 域名不在账号下 → 阿里云 DNS 控制台「添加域名」，或改用该域名所属账号的 AccessKey。
2. 域名和 AccessKey 都对仍报错 → 检查 RAM 用户是否有该域名的
   `alidns:DescribeDomainRecords` / `alidns:UpdateDomainRecord` 权限（自动补建记录还需
   `alidns:AddDomainRecord`）。

> 代码侧：`internal/dns/errors.go` 的 `Classify()` **没有单列**这个码，
> 会落到 `ClassUnknown` → 按可重试处理，白等 1s/4s/9s 三次退避后才失败。
> 所以日志里这个错误出现时，等了十几秒才报错是预期行为，不代表还有别的错误。

### 3.3 Let's Encrypt 速率限制

**症状**：日志 `rate limit exceeded`，且**改任何配置都没用**，只能等。

**原因**：LE 每标识每小时 5 次失败授权上限。反复重启容器调试时每次重启都触发一次
授权，5 次就撞满。

**处置**：从错误报文里读 `retry after`，等到那个时间点再试。调试期间尽量别反复重启容器。

### 3.4 DNS-01 challenge 卡住

**症状**：续签 5+ 分钟没结果。

**排查**：

```bash
docker compose exec ikev2-panel tail -30 /var/log/acme-renew.log
```

**解法**：通常是阿里云 DNS API 限频。续签 cron 的时间是**每次容器启动时随机生成**的
（02:00–04:59 之间随机分钟，另有 12 小时后的一次重试），本身就避开了固定时刻的
全球撞车。若仍频繁限流，在面板里调大 DDNS 同步周期、减少不必要的同步即可。

---

## 4. DDNS 不更新

### 4.1 凭证 / 域名缺失

**症状**：面板 DDNS 卡显示「未配置」，或同步一直失败。

**排查**：

```bash
ls -la /data/panel-state/aliyun.creds   # 面板填过凭证就会有
cat /data/panel-state/ddns-runtime.conf   # 运行时开关 + 域名 + RR + 节流时间戳
cat /data/le/LAST_DDNS_FAILED 2>/dev/null # 上次同步失败的详细原因（成功时自动删除）
```

**解法**：面板 → 阿里云 API 凭证 → 填 AccessKey ID + Secret。凭证写入
`/data/panel-state/aliyun.creds`，**DDNS 下个周期自动生效，不需要重启容器**；
但 LE 签发/续签用的 acme.sh 凭证是 entrypoint 启动时注入的，改完要**重启容器**。

### 4.2 记录不存在时会怎样

DDNS 是**查-比-改-建**的：查询不到记录时自动调 `AddDomainRecord` 建一条（值 = 当前探测到的
公网 IP，TTL 600s），日志为 `ddns: no existing record, creating it`。这一行为与
ddns-updater 的 aliyun / cloudflare provider 一致。

创建失败（如 RAM 没勾 `alidns:AddDomainRecord`）会**如实报失败**，不会静默跳过：

```bash
docker compose logs ikev2-panel | grep -E "no existing record|AddDomainRecord"
```

**解法**：① 给 RAM 用户补 `alidns:AddDomainRecord` 权限，下个周期自动补建；
② 或点面板 DDNS 卡的「在阿里云创建 A / AAAA → <IP>」手动补建
（`POST /api/ddns/create-record`）—— 首次配置、还没探测到 IP 时只能走这条。

### 4.3 alidns 错误码分类

同步失败时按 `internal/dns/errors.go` 的 `Classify()` 分类，行为不同：

| 分类 | 触发错误码 | 程序行为 | 处置 |
|------|-----------|---------|------|
| `AuthError` | `InvalidAccessKeyId*`、`IncompleteSignature`、`SignatureDoesNotMatch`、`Forbidden.AccessKeyDisabled` | 立即失败，不重试 | 换正确的 AccessKey |
| `RAMError` | `Forbidden.RAM`、`DomainRecordNotBelongToRAM`、`NoPermission`、`AccessDenied`，以及所有未枚举的 `Forbidden.*` | 立即失败，不重试 | 给 RAM 用户加 `alidns:DescribeDomainRecords` + `alidns:UpdateDomainRecord` + `alidns:AddDomainRecord` |
| `ThrottlingError` | `Throttling*` | 退避重试 | 等节流窗口 |
| `TransientError` | `InternalError`、`ServiceUnavailable` | 退避重试 | 通常自愈 |
| `NotFoundError` | `DomainRecordNotFound`（记录在查询与更新之间被删） | 立即失败，不重试 | 记录已不存在，重试不可能恢复；下个周期会按 4.2 自动重建 |
| `UnknownError` | 其他 | 退避重试 | 看原始 Code |

### 4.4 节流窗口不释放

**症状**：明明 IP 变了，但短时间内没同步。

**排查**：

```bash
cat /data/panel-state/ddns-runtime.conf   # last_sync_a / last_sync_aaaa 是上次同步时间（unix 秒）
```

同类型两次同步的最小间隔是 60 秒，节流时间戳**持久化在 statefile 里**，
容器重启不会清零。

**解法**：

- 等满 60 秒自然触发；或点面板 DDNS 卡的「立即同步」（`POST /api/ddns/sync-now`，
  同样受节流约束，刚同步过会 skip）。
- 想强制立刻同步：把对应的 `last_sync_a` / `last_sync_aaaa` 改成 0 再点「立即同步」。

---

## 5. 限速不生效

> **先分清方向**：tc filter 匹配的是 `src <客户端虚拟 IP>`，挂在**出口网卡**上，
> 所以**只限客户端「上传」（客户端 → 公网）方向，不限下载方向**。
> 测限速请**从客户端往公网发流量**（传大文件 / `iperf3 -c`），在服务端往客户端拉
> 是测不出来的 —— 那是下载方向，本来就不限。

限速链路有三个环节，缺一不可：
`面板写限速文件` → `SA up 时 updown 脚本建 tc class` → `数据包命中 filter`。

### 5.1 面板有没有写限速文件

```bash
docker compose exec ikev2-panel ls -la /var/lib/ikev2-panel/limits/
docker compose exec ikev2-panel cat /var/lib/ikev2-panel/limits/<用户名>
# 内容是纯数字，单位 Mbps；0 = 不限速（updown 脚本见到 0 直接跳过，不建 class）
```

没有文件 → 该用户建号时没设限速。限速值是**建号时**写进文件的
（`POST /users` 的 `speed_limit_mbps` 字段），面板没有事后改限速的入口。

要改已存在用户的限速，直接改文件 + 让客户端重新拨号即可（updown 在 SA up 时读文件）：

```bash
docker compose exec ikev2-panel bash -c 'echo 50 > /var/lib/ikev2-panel/limits/<用户名>'
# 客户端断开重连 → 新的 tc class 按 50 Mbps 建
```

写入失败时建号会被回滚（DB 行 + swanctl conf 一起撤掉），面板提示
「写入限速文件失败（请检查容器内 /var/lib/ikev2-panel/limits 权限）」。

### 5.2 tc qdisc / class 没建

**症状**：用户拨入后完全不限速。

**排查**：

```bash
# 出口网卡名：updown 脚本默认用 eth0，容器启动时探测到的真实网卡未必是它
docker compose exec ikev2-panel ip -4 route show default
docker compose exec ikev2-panel sh -c 'echo $IKEV2_OUT_IF'

# 根 qdisc 应存在（所有用户共用，建一次）
docker compose exec ikev2-panel tc qdisc show dev eth0
# 期望看到 qdisc htb 1: root

# 该用户的 class
docker compose exec ikev2-panel tc class show dev eth0
docker compose exec ikev2-panel tc -s class show dev eth0   # 看 bytes 计数是否在涨
```

**排查 family.env**：`ikev2-updown` 靠这个文件决定用哪种 filter，按顺序找两个位置：

```bash
docker compose exec ikev2-panel cat /run/ikev2-panel/family.env
# 期望 FAMILY=ipv4|ipv6|dual（主路径，每次容器启动由 entrypoint 重写）
```

`/run/ikev2-panel/family.env` 是**主路径**；`/etc/ikev2-panel/family.env` 只是
**兼容回落**（旧镜像遗留）。主路径不存在且回落也没有 → `FAMILY` 取默认值 `ipv4`，
IPv6-only 部署下就会走错 filter 类型（表现为限速不生效）。

**解法**：

- 根 qdisc 没有 → 重新拨号触发 SA up，或重启容器。
- `family.env` 缺失 → 重启容器重新生成。
- **网卡名不是 `eth0`** → 显式设置，否则 tc 全加在错误的设备上：
  ```bash
  # .env
  IKEV2_OUT_IF=eth0
  ```

### 5.3 IPv6 用户被绕过

默认 `FAMILY=ipv6` 时，脚本走 `tc flower`（需内核 4.1+）而不是 `u32`。

**排查**：

```bash
docker compose exec ikev2-panel tc filter show dev eth0 parent 1:
# 期望看到 protocol ipv6 ... flower ... src_ip fd00:1::2
docker compose logs ikev2-panel | grep "kernel lacks"
# 有这行 = 内核不支持 flower，该用户 IPv6 限速被禁用
```

**解法**：升级内核到 4.1+。脚本对这种情况是 WARN 但不阻塞 SA 建立
（先连接后限速），所以表现为「能连上但不限速」。

### 5.4 单位写错

`ikev2-updown` 用 `rate "${LIMIT}mbit"`，`LIMIT` 是面板里的 Mbps 数字。
所以面板填 `50` → `50mbit` = 50 Mbps。

**注意 tc 速率后缀区分大小写**：`mbit` = Mbit/s（兆比特每秒），`Mbit` 会被 tc
当成**字节**速率（1 `Mbit` 只有 0.125 MB/s）。另外 `1000mbit` 是 1 Gbps 而不是
1 Mbps —— 排查时别把这两者看混。

---

## 6. admin 密码忘

### 6.1 还在用初始密码

默认 admin 密码除打印到启动日志外，**还会落盘**：

```bash
docker compose exec ikev2-panel cat /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
```

文件存在时面板首页会显示「还在用默认密码」横幅。**登录后请立刻改掉**（见 6.2），
改密后建议删掉这个文件。

### 6.2 已改过密码但忘了

面板有自助改密页，**需要先能登录**：

```
https://your-domain:8443/account
```

（对应 `POST /api/account/password`，见 [API.md](API.md)。）

### 6.3 完全进不去（数据库直改）

admins 表固定只有一行（`id = 1`），哈希是 bcrypt **cost 12**。

```bash
# 1. 生成新 hash（镜像里没有 python3 和 bcrypt 模块，两个都要装）
docker compose exec -u root ikev2-panel bash -c '
  apt-get update -qq && apt-get install -y -qq python3-bcrypt >/dev/null 2>&1
  python3 -c "import bcrypt; print(bcrypt.hashpw(b\"new-password\", bcrypt.gensalt(12)).decode())"
'
# 复制输出的 hash（$2b$ 前缀，Go 侧能正常校验）

# 2. 写回 DB
docker compose exec ikev2-panel sqlite3 /data/panel.db \
  "UPDATE admins SET password_hash = '<上面的hash>', updated_at = strftime('%s','now') WHERE id = 1;"
```

> 面板进程正在运行，改完 DB 后不用重启 —— 下次登录会用新密码校验。
> 改完顺手删掉 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`，否则首页横幅会一直提示「还在用默认密码」。

### 6.4 紧急：重置为默认密码

删掉 admin 行 + 初始密码文件，重启后程序会重新生成一套并打印到日志：

```bash
docker compose exec ikev2-panel bash -c '
  rm -f /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
  sqlite3 /data/panel.db "DELETE FROM admins WHERE id = 1;"
'
docker compose restart ikev2-panel
docker compose logs ikev2-panel | grep -A5 "默认管理员已创建"
```

> **警告**：删除 admin 行会让所有已登录的管理员会话失效。新密码只出现在启动日志和
> `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt` 里 —— **立刻抄走**，日志会滚掉。

---

## 7. DB 损坏

DB 是单文件 SQLite：`/data/panel.db`（在命名卷里，容器重建不会丢）。

### 7.1 sqlite3 .recover

**症状**：启动报 `database disk image is malformed`。

**解法**：

```bash
docker compose exec ikev2-panel bash -c '
  cp /data/panel.db /data/panel.db.broken
  sqlite3 /data/panel.db ".recover" | sqlite3 /data/panel.db.recovered
  mv /data/panel.db.recovered /data/panel.db
'
docker compose restart ikev2-panel
```

### 7.2 VACUUM 整理

**症状**：DB 文件大但实际数据少（碎片）。

**解法**：

```bash
docker compose exec ikev2-panel sqlite3 /data/panel.db "VACUUM;"
```

### 7.3 完全重建（最后手段）

**解法** `[host]`：

```bash
# 警告：所有用户、admin、审计日志、运行时配置全部丢失
docker compose exec ikev2-panel bash -c 'rm -f /data/panel.db /data/panel.db-*'
docker compose restart ikev2-panel
# 程序会重建空库 + 生成默认 admin 密码
docker compose logs ikev2-panel | grep -A5 "默认管理员已创建"
```

注意 `/data/panel-state/` 和 `/data/le/` 是独立文件，不会被这个操作清掉。

---

## 8. 面板 UI 异常

前端问题**不影响 VPN 数据通路**，VPN 正常但面板界面不对时查这里。

### 8.1 破坏性操作没有二次确认弹窗

**症状**：点「删除」「停用」「重置密码」立刻执行，没有确认框。

**原因**：模板里用了 `onclick="return confirm('…')"`，而 CSP 是

```
script-src 'self' 'nonce-…'      # 没有 'unsafe-inline'，也没有 'unsafe-hashes'
```

浏览器会**直接拒绝**内联事件处理器（控制台报
`Refused to execute inline event handler`）。页面看起来完全正常，只有这一处静默失效。

**确认是不是这个原因**：F12 → Console，看有没有上面那行报错。

**解法**：模板里改用 `data-confirm="确认文案"`，由 `layout.html` 的
`initConfirmGuard()` 事件委托处理。**不要**为了"让它能用"去给 CSP 加
`'unsafe-hashes'` —— 那等于对所有内联处理器放行。见 architecture §12.9。

### 8.2 页面顶部横幅显示成一串标签文本

**症状**：横幅里出现 `&lt;strong&gt;…&lt;/strong&gt;` 这种字面量。

**原因**：富文本走了 banner 的 `.Msg` 字段，而 `.Msg` 受 Go 模板**自动转义**保护
（这是 XSS 防线，不能动）。

**解法**：字面量 HTML 用 `MsgHTML`，动态值（密码、用户名、外部 API 错误串）
拆成同级独立元素走默认转义。**不要**把变量拼进 `MsgHTML`。

### 8.3 刷新后页面不在最顶部

两个独立原因，只修一个仍会复现（详见 architecture §12.10）：

| 原因 | 判据 |
|------|------|
| `sessionStorage['ikev2-scroll-y']` 提交后没被删 | F12 → Application → Session Storage 里能翻到残留的旧值 |
| 浏览器原生 `history.scrollRestoration` 没关 | 停用缓存后强刷仍回到旧位置 |

**临时绕过**：F12 → Console 执行 `sessionStorage.clear()` 后刷新。

### 8.4 点「复制」显示已复制，剪贴板里却是空的

**症状**：按钮闪「已复制 ✓」，但粘贴出来不是内容。

**原因**：`navigator.clipboard` 不可用时降级到 `execCommand('copy')`，
早先这条路径吞掉异常、无条件报成功。

**排查**：F12 → Console 看有没有 `NotAllowedError` / `document is not focused`。
非 HTTPS 或非用户手势触发时降级路径也会失败。

**当前行为**：失败会如实显示「复制失败，请手动选中复制」，不再是假成功。

### 8.5 终端卡片出现两层横向滚动条

**症状**：命令卡片里内层 `<pre>` 和外层卡片各有一条横向滚动条，手机上几乎没法操作。

**原因**：两层都设了 `overflow-x: auto`。

**解法**：只保留外层滚动 —— 卡片 `overflow-x: auto`，
内部 `<pre>` 用 `overflow-x: visible`。**注意别把卡片的也改成 `visible`**，
那会让超长命令把整张卡片撑破容器。

### 8.6 表单校验失败后落到一个纯文本页

**症状**：提交非法值后页面变成没有导航的白底黑字，还带着一串英文错误。

**原因**：handler 用了 `http.Error(w, ..., 400)`。已修（见 §3.11 数据流），
如果你在自己的分支上又看到了，先确认 `failMCOpts()` 走的是 303 而不是 400。

### 8.7 「活跃隧道」的本地 / 远端是空的

**症状**：首页隧道列表有连接，但本地 / 远端地址两栏空白。

**先排除**：这是**容器重建后没有新连接**时的正常表现 ——
重建容器会重启 strongSwan，所有 IKE 会话被断开，需要手机重新连接一次。

有连接仍然为空 → parser 读错了 VICI key。真实 key 是
`local-host` + `local-port` + `remote-host` + `remote-port`，
**不存在** `local-addr` / `remote-addr`。见 architecture §12.4。

验证：

```bash
docker compose exec ikev2-panel swanctl --list-sas
# 对照输出里的 host / port 两列
```

### 8.8 排查手法

前端问题容易误判成后端问题。定顺序：

| 顺序 | 手段 |
|------|------|
| 1 | 看**线上真实 HTML**（`curl -sk https://<host>:8443/users/3 -b cookie.txt`），不要凭印象 |
| 2 | 看浏览器 Console 有没有 CSP / 404 报错 |
| 3 | 查 CSS **特异性和源码顺序**（同优先级时后者覆盖前者） |
| 4 | 权威文档核实行为，不靠推测 |
| 5 | 自动化浏览器探针**失败不等于结论不成立** —— 工具超时也要用前三步的证据定性 |

---

## 附：常用诊断命令

```bash
# 1. 容器整体状态 + 退出码
docker compose ps
docker compose logs --tail=100 ikev2-panel
docker inspect ikev2-panel --format '{{.State.ExitCode}}'   # 1.1 表格里的退出码

# 2. charon 实时日志
docker compose exec ikev2-panel tail -f /var/log/charon.log

# 3. SA 状态
docker compose exec ikev2-panel swanctl --list-sas

# 4. 面板健康检查（db / vici / le / ddns 四项，任一失败 503）
curl -s http://localhost:8443/healthz

# 5. Prometheus metrics
curl -s http://localhost:8443/metrics

# 6. 审计日志（也可登录面板 → 顶部「日志」）
docker compose exec ikev2-panel sqlite3 /data/panel.db \
  "SELECT datetime(timestamp,'unixepoch'), actor, event, details FROM audit_log ORDER BY id DESC LIMIT 20;"

# 7. 用户列表
docker compose exec ikev2-panel sqlite3 /data/panel.db \
  "SELECT id, username, enabled, speed_limit_mbps, datetime(expires_at,'unixepoch') FROM users;"

# 8. UDP 500/4500 端口占用 `[host]`（host netns，容器内外看到的是同一批 socket）
ss -lunp | grep -E ':(500|4500)\b'
```

---

## 排查方法论

两条来自真机排查的规则，都是「看起来像 A 问题、实际是 B 问题」的类型。

### 判断「资源是否存在」→ 权威 API > 本地 dig/ping

`dig` 返回空**只能**说明「我这台机的递归链路拿不到」，推不出「记录不存在」。
曾据此误判「DNS 记录不存在 → DDNS 没生效 → LE 与 DDNS 循环依赖死锁」，**三条结论全错** ——
同一时刻阿里云 API 上记录一直好着，而该机递归 DNS 是坏的。

| 判断什么 | 用什么 |
|---------|--------|
| DNS 记录是否存在 | 阿里云 API —— 面板 DDNS 卡「查询阿里云记录值」，或仓库里的 `cmd/dnsq`（需自行 build，见 [3.2](#32-domainrecordnotexist)） |
| 云上资源是否存在 | 对应云的 OpenAPI |
| 服务是否在监听 | `ss -lntup`（UDP 看 `-lunp`） |
| 文件是否存在 | `stat`（后两个和 `dig` 一样可信） |

### 反复重启容器调试 → 会撞 Let's Encrypt 限流

见 [3.3](#33-lets-encrypt-速率限制)。

---

## 报告 bug

如果以上步骤都没解决，到 GitHub Issues 提交时附上：

```bash
# [host] 退出码 + 容器状态（退出码只在宿主侧能看到）
docker inspect ikev2-panel --format 'exit={{.State.ExitCode}} oom={{.State.OOMKilled}} err={{.State.Error}}'
docker compose ps

# 容器内快照
docker compose exec ikev2-panel bash -c '
  echo "=== version ===" && cat /etc/ikev2-panel-version 2>/dev/null || echo dev
  echo "=== healthz ===" && curl -s http://localhost:8443/healthz
  echo "=== forwarding ===" && cat /proc/sys/net/ipv4/ip_forward /proc/sys/net/ipv6/conf/all/forwarding
  echo "=== family.env ===" && cat /run/ikev2-panel/family.env 2>/dev/null
  echo "=== nft tables ===" && nft list table ip ikev2 2>&1 | head -20
  echo "=== limits ===" && ls -la /var/lib/ikev2-panel/limits/ 2>&1
  echo "=== charon.log (last 30) ===" && tail -30 /var/log/charon.log
  echo "=== ikev2-renew.log (last 30) ===" && tail -30 /var/log/ikev2-renew.log 2>/dev/null
  echo "=== users ===" && sqlite3 /data/panel.db "SELECT id,username,enabled,speed_limit_mbps FROM users;"
  echo "=== audit_log (last 20) ===" && sqlite3 /data/panel.db "SELECT datetime(timestamp,\"unixepoch\"),actor,event,details FROM audit_log ORDER BY id DESC LIMIT 20;"
' > /tmp/diag-$(date +%Y%m%d-%H%M).txt
```

把 `/tmp/diag-*.txt` 内容贴到 issue。
