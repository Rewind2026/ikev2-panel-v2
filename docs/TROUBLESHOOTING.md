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
8. [数据 / 证书看不到](#8-数据--证书看不到)
9. [面板 UI 异常](#9-面板-ui-异常)（确认弹窗 / 横幅乱码 / 滚动位置 / 复制假成功 / 双层滚动条）

> 前 8 节是 **VPN 与容器**问题，第 9 节是**纯前端**问题。
> VPN 正常但界面不对，直接跳第 9 节。

---

## 1. 容器启动失败

> 先看退出码 —— `entrypoint.sh` 每个阶段失败都 `exit <码>`，`restart: unless-stopped`
> 会把它变成无限重启。`docker compose logs ikev2-panel | grep FATAL` 一眼定位。

| 退出码 | 含义 | 对应小节 |
|--------|------|----------|
| 10 | 找不到公网 IPv6 地址（`IKEV2_IPV6_ONLY=true` 时）／ L2 模式 SLAAC sysctl 不符 | [1.1](#11-找不到公网-ipv6-地址退出码-10) · [1.3.4](#134-l2-模式拿不到-ipv6退出码-10macvlan--ipvlan) |
| 11 | IP forwarding 启用失败 | [1.2](#12-ip-forwarding-启用失败退出码-11) |
| 12 | 网络模式配置错误 / charon 启动失败 | [1.3](#13-网络模式配置错误退出码-12) · [1.4](#14-charon-启动失败退出码-12) |
| 14 | 探测不到对外网卡 / `IKEV2_OUT_IF` 在容器内不存在 | [1.5](#15-探测不到对外网卡退出码-14) |
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

### 1.3 网络模式配置错误（退出码 12）

支持 `host` / `macvlan` / `ipvlan` 三种（v2.86-PR20）。以下每一条都会 FATAL。

#### 1.3.1 模式值非法

**症状**：

```
[entrypoint] ERR: IKEV2_NETWORK_MODE=bridge 已于 v2.86-PR14 移除
[entrypoint] ERR:   合法值: host | macvlan | ipvlan
[entrypoint] FATAL: IKEV2_NETWORK_MODE=bridge 配置错误（详见上方 ERR）
```

**解法**：`bridge` 模式不存在，改成 `host` / `macvlan` / `ipvlan`。

> ⚠️ **`IKEV2_NETWORK_MODE` 不决定用哪个 compose 文件** —— 文件由 `-f` 决定。
> 换成对应文件重建即可：
>
> ```bash
> docker compose -f docker-compose.macvlan.yml up -d
> ```

#### 1.3.2 L2 变量缺失或非法

**症状**（`macvlan` / `ipvlan`）：

```
[entrypoint] ERR: IKEV2_NETWORK_MODE=macvlan 缺少必填变量: IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY
```

或 `IKEV2_L2_SUBNET 只支持 IPv4`、`IKEV2_L2_PARENT 不能是 lo`、
`macvlan_mode=passthru 与 IPAM 互斥` 等。

**解法**：`.env` 里补齐三项并重建：

```bash
IKEV2_NETWORK_MODE=macvlan
IKEV2_L2_PARENT=eth0                  # 宿主物理网卡
IKEV2_L2_SUBNET=203.0.113.0/24        # 宿主所在 IPv4 子网，只支持 IPv4
IKEV2_L2_GATEWAY=203.0.113.1          # 该子网的宿主网关

docker compose -f docker-compose.macvlan.yml up -d
```

⚠️ **gateway 必须落在 subnet 里**，否则 FATAL（entrypoint 与 `up.sh` 双重校验）。

⚠️ **`macvlan_mode=passthru` 不支持 IPAM**，所以本项目的 macvlan 模式
不接受 `passthru`，改用 `bridge`（`private` / `vepa` 可以）。

##### 1.3.2.1 用第三方面板部署（1Panel / 宝塔 / dpanel 等）时变量传不进容器

**症状**：容器起来了，但日志反复刷同一行，最后无限重启：

```
[entrypoint] ERR: IKEV2_NETWORK_MODE=ipvlan 缺少必填变量: IKEV2_L2_PARENT IKEV2_L2_SUBNET IKEV2_L2_GATEWAY
[entrypoint] FATAL: IKEV2_NETWORK_MODE=ipvlan 配置错误（详见上方 ERR）
```

**根因**：这三个变量在 compose 文件里出现**两次**，走两条**互相独立**的插值路径：

| 位置 | 插值结果给谁 | 是否进容器 |
|------|--------------|-----------|
| `networks:` 段的 `parent` / `subnet` / `gateway` | Docker daemon 建网 | ❌ 不进 |
| `environment:` 段的同名三项 | 容器环境变量 | ✅ 进 |

entrypoint 读的是**容器里**那一份。第三方面板的编排有自己的目录和自己的
`.env` 管理，**不读项目目录下的 `.env`** —— 于是：

- 如果 `networks:` 段被改成了 `:-<默认值>`（面板模板常为了先让 compose
  起得来这么改），compose 插值通过、容器创建成功；
- 但 `environment:` 段若还是 `:-`（缺失即空串），容器里三个变量就是**空串**；
- entrypoint 的 `net_mode_validate` 判为「缺少必填变量」→ `exit 12` →
  `restart: unless-stopped` → 无限重启。

**为什么日志上看不出来**：日志里只有「缺少必填变量」，看不出真实原因是
「变量压根没传进容器」。这个不对称只在 compose 文件被改过时才暴露。

**解法**（二选一）：

1. **改 compose 顶部的 `x-l2-*` 三行**（推荐，v2.90 起的标准做法）：

   打开 `docker-compose.macvlan.yml` / `docker-compose.ipvlan.yml`，最顶部有
   醒目注释块，下面三行是唯一需要改的地方：

   ```yaml
   x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-eth0}"
   x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-192.168.50.0/24}"
   x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-192.168.50.1}"
   ```

   `environment:` 段和 `networks:` 段引用的是**同一个 anchor**，
   所以不存在「只改了一边」的可能。改完直接 `up`，不需要 `.env`。

   > ⚠️ **把值填在 `${...:-}` 的括号里，不要把外壳删掉。**
   > 外壳在，`.env` 用户才能用 `.env` 覆盖（写成裸字面量 `"eth0"` 会让
   > `.env` 永久失效）；不写 `.env` 的话外壳留着也不影响 —— 括号里就是最终生效值。

   > ⚠️ 若面板的「环境变量」里已经存在 `IKEV2_L2_*` 三项，那些值会**优先于**
   > 你在 compose 里写的默认值（`shell env > .env > compose 顶部默认值`，是 Docker
   > Compose 的标准行为）。此时要么改面板的环境变量，要么把 compose 里的
   > 默认值去掉、只靠面板传。

2. **在面板的「环境变量」里补齐三项**（`.env` 路线，依然完全支持）：

   ```bash
   IKEV2_L2_PARENT=eth0
   IKEV2_L2_SUBNET=192.168.50.0/24     # 换成你宿主机的真实子网
   IKEV2_L2_GATEWAY=192.168.50.1       # 换成你宿主机的真实网关
   ```

**先确认真实值**，别沿用示例里的地址：

```bash
ip -o -4 route show default     # 第一列 dev 后面那个 = 父接口名；第三列 = 网关
ip -o -4 addr show <父接口>     # inet 那一列的 CIDR = 子网
```

> **v2.90 的做法：anchor 而非必填插值。**
>
> v2.89 曾把两份 L2 compose 的 `environment:` 段改成 `${...:?}`（缺失即报错），
> 缺变量会在 `up` 阶段直接失败：
>
> ```
> error while interpolating services.ikev2-panel.environment.IKEV2_L2_PARENT:
> required variable IKEV2_L2_PARENT is missing a value: 必填：宿主父接口名，如 eth0
> ```
>
> 这确实把故障暴露点从「运行期刷屏」提前到了「部署期报错」，但代价是
> **逼用户额外准备一个 `.env`** —— 而「拉到项目 → 编辑 compose → up」才是
> compose 的主用法，那条路上很多人根本不会碰 `.env`。
>
> v2.90 改用 YAML anchor：值在文件顶部定义一次，两条插值路径引用同一个值，
> 「两段不一致」这个故障模式被**结构性消除**，用户只改顶部三行即可，零 `.env`。
> `.env` 路线不受影响（anchor 里套的就是 `${VAR:-默认}`）。
>
> 由 `tests/network_mode_test.sh` 的 **NM-57** 强制：两份 L2 compose 的
> `environment:` 段与 `networks:` 段必须引用同一个 anchor，且 anchor 定义
> 必须保留 `${VAR:-默认}`（否则 `.env` 用户失去覆盖能力）。

#### 1.3.3 规格表文件缺失

**症状**：`FATAL: /etc/ikev2-panel/scripts/network-mode.sh not found (image build incomplete)`。

**解法**：镜像构建有问题（Dockerfile 里应有 `COPY scripts/network-mode.sh ...`
且构建期 sanity check 通过）。重建镜像。

> 同一个退出码 12 也用于「charon 启动失败」，两者靠日志关键字区分：
> 前者有 `IKEV2_NETWORK_MODE`，后者有 `FATAL: charon failed to start`（见 [1.4](#14-charon-启动失败退出码-12)）。

### 1.3.4 L2 模式 sysctl 不符合预期（退出码 10，macvlan / ipvlan）

**症状**：

```
[entrypoint] FATAL: eth0 sysctl 不符合预期 → IPv6 SLAAC 不可用
[entrypoint]   不符项： net/ipv6/conf/eth0/accept_ra=1(期望 2)
```

**含义**：L2 模式的公网 IPv6 靠上游路由器发 RA、容器 SLAAC 自动获取。
`accept_ra` 不为 2，RA 就会被内核丢弃（因为本项目必开 IP forwarding）。

**排查**：

```bash
# 1) 看容器实际拿到的 sysctl（这是 compose 注入的结果）
docker inspect ikev2-panel --format '{{.HostConfig.Sysctls}}'

# 2) 看容器内运行时的真实值
docker exec ikev2-panel sh -c 'cat /proc/sys/net/ipv4/conf/eth0/forwarding \
  /proc/sys/net/ipv6/conf/eth0/forwarding \
  /proc/sys/net/ipv6/conf/eth0/accept_ra \
  /proc/sys/net/ipv6/conf/eth0/disable_ipv6'

# 3) 确认 disable_ipv6 是 0（由 networks 段 enable_ipv6 决定，不由 sysctls 决定）
docker network inspect ikev2-panel-v2-main_ikev2-l2 \
  --format '{{.EnableIPv6}} {{.Driver}}'
```

**解法**：

1. 确认走的是 L2 compose 文件（`docker network inspect` 看到的 driver 应是
   `macvlan` / `ipvlan`，不是 `host`）；
2. 确认 `networks.ikev2-l2` 段有 `enable_ipv6: true`，service 段有三条
   `sysctls`（`net.ipv4.conf.all.forwarding` / `net.ipv6.conf.all.forwarding` /
   `net.ipv6.conf.default.accept_ra`）。这是唯一可用的注入通道，
   其他写法实测都不生效 —— 见 [architecture.md §12.8.2](architecture.md)；
3. 确认上游路由器**对这个网段真的在发 RA**（有些企业网 / 云 VPC 不发）；
4. `accept_ra=2` 是必须的 —— 本项目要开 IP forwarding，内核在 forwarding 打开时
   会把默认态的 `accept_ra` 抑制掉，不显式写 2 就收不到 RA。

与 [1.1](#11-找不到公网-ipv6-地址退出码-10) 的区别：1.1 是"地址始终拿不到"，
本节是"sysctl 状态本身就不对"，后者是前者的直接原因。

> **地址收敛需要时间**：SLAAC 的 GUA 要等上游一个 RA 周期再叠加 DAD，
> 实测 2~5 秒。entrypoint 在 L2 模式下会重试最多 15 秒，
> 日志里会打印 `[auto-detect] Public IPv6 converged after ~Ns`。
> 超过 15 秒仍失败才是真问题，不要盲目加长等待。

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

**症状 A**（host 模式）：`FATAL: cannot detect outbound interface`。

**症状 B**（macvlan / ipvlan 模式）：

```
[entrypoint] FATAL: IKEV2_OUT_IF=ens18 在容器内不存在
[entrypoint]   当前网络模式: macvlan（容器有独立 netns，网卡名与宿主不同）
```

**排查**：

```bash
ip -4 route show default
ip -6 route show default
ip -o link show up
docker exec ikev2-panel ip -br link        # 容器里的真实网卡名
```

**解法**：

- 症状 A：三条都空（宿主没有默认路由）→ 宿主网络没配好。
  只是名字不是预期的 → 显式指定 `IKEV2_OUT_IF=<iface>`。
- 症状 B：**从 `.env` 删掉 `IKEV2_OUT_IF` 这一行**，让 entrypoint 自动探测
  （L2 模式下容器网卡通常是 `eth0`）。这是最省事的做法。

⚠️ **症状 B 不修会退化成静默故障**：nft 模板会生成一条永不匹配的
`oifname` 规则 → MASQUERADE 失效 → 客户端能连上 VPN 但出不了网。
这是本次改造里唯一"启动成功但功能不可用"的坑，所以 entrypoint 改成 FATAL 而非 WARN。

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

### 1.7 L2 模式（macvlan / ipvlan）特有排障

> 只在 `IKEV2_NETWORK_MODE=macvlan|ipvlan` 时适用，host 模式见上文。

#### 1.7.1 确认当前真的跑在 L2 模式

```bash
docker inspect ikev2-panel --format '{{.HostConfig.NetworkMode}}'
# host    → host
# macvlan → 容器名（Docker 会给自定义网络起一个 project 前缀的容器名）
# ipvlan  → 同上
```

`/run/ikev2-panel/netmode.env` 是 entrypoint 写的运行时快照，最直接：

```bash
docker exec ikev2-panel cat /run/ikev2-panel/netmode.env
# MODE=macvlan
# OUT_IF=eth0
# DRIVER=macvlan_mode=bridge
# HAS_OWN_IP=1
# PROCSYS_RW=0
# DDOS_CHAIN=INPUT
```

若 `MODE=macvlan` 但日志有 `WARN: declared macvlan but the container still
shares the host netns (docker0 visible)` → **用错 compose 文件了**
（用了 `docker-compose.yml` 而不是 `docker-compose.macvlan.yml`）：

```bash
docker compose -f docker-compose.macvlan.yml down
docker compose -f docker-compose.macvlan.yml up -d
```

#### 1.7.2 容器没有 v4 地址

```bash
docker inspect ikev2-panel --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
docker network inspect <项目名>_ikev2-l2 | grep -i macvlan -A3
```

| 现象 | 原因 / 处置 |
|------|-----------|
| 没挂到 `ikev2-l2` 上 | 用的是 `docker-compose.yml`（host）而不是 L2 文件；或 `.env` 里模式名拼写不对 |
| IP 为空、事件日志报 `failed to allocate gateway` | `IKEV2_L2_SUBNET` 与宿主实际网段不符（同一二层上有别的 DHCP 池），或网关不在子网内 |
| 报 `could not find an available, non-overlapping IPv4 address pool` | `IKEV2_L2_SUBNET` 与已有 Docker 网段（如默认 `172.17.0.0/16`）重叠 → 换一个不重叠的子网 |
| **容器压根没创建**（`docker compose up` 就报 `device or resource busy`） | 宿主虚拟化层限制，**不是配置问题** → [1.7.6](#176-device-or-resource-busy宿主虚拟化层限制) |
| IP 拿到了但 ping 不通网关 | 交换机 / 云安全组不允许多个源 MAC（macvlan）→ 改用 `ipvlan`；或网络是 `/32` 主机路由，点对点配置 |
| 容器 ping 不到**宿主** | **这是设计如此**，macvlan/ipvlan 在内核层隔离父子，不是 bug。需要互通就建 shim |
| 配了固定 IP 后**宿主入站全断** | 固定 IP 撞上宿主自己的地址 → [1.7.7](#177-固定容器-ipv4-配错ikev2_l2_ipv4v291-起) |

#### 1.7.3 容器拿不到 v6 地址

```bash
docker exec ikev2-panel ip -6 addr show eth0
docker exec ikev2-panel cat /proc/sys/net/ipv6/conf/eth0/accept_ra
docker exec ikev2-panel sysctl net.ipv6.conf.all.forwarding
tcpdump -i <parent> -n icmp6          # 宿主上抓 RA
```

| 现象 | 原因 / 处置 |
|------|-----------|
| 抓不到 RA | 上游不发 RA（企业网 / 部分云 VPC）→ 换网段，或放弃 L2 模式 |
| 抓得到 RA 但地址没配上来 | `accept_ra` 不是 2 → entrypoint 已 `exit 10` 拦截（[1.3.4](#134-l2-模式拿不到-ipv6退出码-10macvlan--ipvlan)） |
| 地址是 `fd00::/8`（ULA） | 有人在 IPAM 里配了 v6 子网，Docker 抢了 RA → 本项目已拒绝 v6 子网，检查是不是手工改过 L2 compose 文件 |
| 地址是 `fe80::` 开头 | RA 收到了但没给全局前缀（DAD 未完成）→ 等 2s 再看，或上游只发 link-local |

⚠️ **`IKEV2_SERVER_ADDR_V6` 解决不了这个问题**：它只填 charon 的
`local_addrs`，不负责把地址配到网卡上。设了它但网卡上没有对应地址，
charon 会 bind 失败或收不到包。

#### 1.7.4 客户端能连 VPN 但出不了网

按可能性从高到低排查：

| 序 | 检查 | 命令 |
|----|------|------|
| 1 | `IKEV2_OUT_IF` 语义漂移 | entrypoint 已 FATAL 拦截（[1.5](#15-探测不到对外网卡退出码-14)）；若没触发说明已自动探测成 `eth0` |
| 2 | 容器 netns 内 forwarding 是否为 1 | `docker exec ikev2-panel sysctl net.ipv4.ip_forward`（应为 1） |
| 3 | MASQUERADE 规则是否加载 | `docker exec ikev2-panel nft list table ip ikev2` |
| 4 | NAT 路由是否在 | `docker exec ikev2-panel ip route show` |
| 5 | 容器本身能否出网 | `docker exec ikev2-panel ping -c3 <公网 IP>` —— 不能出网是环境问题，不是项目问题 |

⚠️ **宿主上 `sysctl -w net.ipv4.ip_forward=1` 对 L2 容器无效**：
forwarding 是 per-netns 的，改的是宿主那个 netns。这是最容易被误导的地方。

#### 1.7.5 宿主与容器需要互通（建 shim）

macvlan / ipvlan 在内核层隔离宿主与容器（`host` 模式不会有这个问题）。
需要互通时在**宿主**上建一个 shim 接口：

```bash
# macvlan（parent 换成你的实际父接口）
ip link add macvlan-shim link eth0 type macvlan mode bridge
ip addr add 203.0.113.9/24 dev macvlan-shim
ip link set macvlan-shim up
ip route add 203.0.113.50/32 dev macvlan-shim   # 换成容器实际拿到的 IP

# ipvlan 用 type ipvlan mode l2 代替
```

查容器 IP：

```bash
docker inspect ikev2-panel --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
```

删除 shim：

```bash
ip link del macvlan-shim
```

⚠️ shim 本身也要走宿主防火墙策略，别以为建了就能通。

#### 1.7.6 `device or resource busy`（宿主虚拟化层限制）

**症状**：`docker compose up` 在**创建网络**这一步就失败，容器根本没起来：

```
Error response from daemon: device or resource busy
```

**这是宿主虚拟化层的限制，不是配置写错。** 内核不允许在该父接口上挂
macvlan / ipvlan，**两种模式都会失败**，改任何参数都绕不过去。

> 注意区分：报 `could not find an available, non-overlapping IPv4 address pool`
> 是**另一个问题**（`IKEV2_L2_SUBNET` 与已有 Docker 网段撞了），见 [1.7.2](#172-容器没有-v4-地址)。

**直接用一次探测代替反复试 compose**（探测网络比试 compose 快得多，且失败不留半成品）：

```bash
# macvlan
docker network create ikev2-l2-probe --driver macvlan -o parent=eth0 -o macvlan_mode=bridge
# ipvlan
docker network create ikev2-l2-probe --driver ipvlan -o parent=eth0 -o ipvlan_mode=l2
docker network rm ikev2-l2-probe     # 探测完记得删
```

报 `device or resource busy` → 就是宿主限制，换 host 模式。

⚠️ **不要凭接口名（`parentbus virtio`）直接下结论** —— 它只是风险信号。
实测两台机器的 `ens18` **都是 `parentbus virtio`**，结论却相反：

| 机器 | 宿主 `ens18` | macvlan | ipvlan |
|------|---------------|---------|--------|
| 云主机（Docker 28.5.2） | `parentbus virtio` | ❌ `device or resource busy` | ❌ `device or resource busy` |
| 群晖 NAS（Docker 28.5.2） | `parentbus virtio` | ✅ 正常创建 | ✅ 正常创建，且容器跑起来了 |

群晖那台实测：ipvlan 容器拿到 `192.168.50.2/24`，**IPv6 GUA 经 SLAAC 正常获取**，
`RestartCount=0`。所以**以探针命令的实际输出为准**，探针通过就说明这台机器的
虚拟化层放行。

**实测对照**（Docker 28.5.2，云主机）：

| parent | 类型 | macvlan | ipvlan |
|--------|------|---------|--------|
| `ens18` | virtio | ❌ `device or resource busy` | ❌ `device or resource busy` |
| `docker0` | bridge | — | ✅ 正常创建 |

同一份 compose 配置只换 parent 就成功，**证明配置本身没问题**。

**解法**（按优先级）：

1. 留在 **host 模式** —— 宿主 500/4500 没被占用就用它，最省事，也是绝大多数情况下的正解。
2. 换一块**非 virtio** 网卡作 parent（物理网卡 / e1000 类型的 vNIC）。
3. 探针在当前 parent 上失败、但换别的 parent 探针通过 → 用那个 parent 就行。

⚠️ shim（[1.7.5](#175-宿主与容器需要互通建-shim)）在宿主限制的 parent 上通常也不通 ——
shim 本身也是建在 parent 上的 macvlan 子接口，同样被内核拒绝。

#### 1.7.7 固定容器 IPv4 配错（`IKEV2_L2_IPV4`，v2.91 起）

v2.91 起 L2 模式支持固定容器地址，在 compose 顶部第四行配：

```yaml
x-l2-ipv4:    &l2_ipv4    "${IKEV2_L2_IPV4:-192.168.50.77}"
```

**留空 = Docker IPAM 自动分配**（compose 会把空值键丢掉，展开成 `ikev2-l2: {}`）。

##### ⚠️ 最严重：填了宿主自己正在用的地址 → 宿主入站连接全断

**症状**：容器正常创建、正常启动、日志无任何报错，然后**宿主的 SSH 直接被
reset**（`Connection reset by peer`），面板也连不上；网关 ping 得通，
但所有入站连接断掉。

**根因**：docker daemon 只拒「不在子网内」和「网络自己已占用」的地址，
**不知道宿主某个接口上已经有这个 IP**。容器起来后抢走宿主该 IP 的 ARP，
宿主所有入站流量被容器吸走。

**实测（192.168.50.63，Docker 28.5.2）**：固定 IP 填 `192.168.50.63`
（宿主自己的地址）→ 容器 `Up`、无报错 → 宿主 SSH 立刻被 reset，两次复现。

**处置**：

```bash
# 1. 删掉容器和它抢的网络，宿主网络即刻恢复
docker rm -f ikev2-panel
docker network rm <项目名>_ikev2-l2

# 2. 换一个宿主未占用的地址，或干脆留空让 Docker 自动分配
```

**预防**：

- `scripts/up.sh` 从 v2.91 起会在 `up` **之前**比对宿主所有接口的 IPv4，
  冲突则拒绝启动（容器不会创建，宿主网络完好）。
- 用 `docker compose up`（不经 `up.sh`）的话，自己先查一遍：

  ```bash
  ip -o -4 addr show | grep -F "192.168.50.77"   # 有输出 = 这个地址已被占用，别用
  ```

##### 其他三种配错

| 现象 | 原因 | 处置 |
|------|------|------|
| `no configured subnet or ip-range contain the IP address` | 固定 IP 不在 `IKEV2_L2_SUBNET` 内 | 换成落在子网内的地址 |
| `Address already in use` | 固定 IP = 网关（`.1`）或网络地址（`.0`）/ 广播（`.255`） | 换一个 |
| `ERR: IKEV2_L2_IPV4='...' 不是合法 IPv4 地址` / `不在 IKEV2_L2_SUBNET='...' 内` / `不能与网关地址相同` | `scripts/up.sh` 部署前拦截 | 按提示改 |

**核对固定地址有没有真的生效**（不用猜）：

```bash
# 容器实际拿到的地址
docker inspect ikev2-panel --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'

# 容器网卡上的地址
docker exec ikev2-panel ip -4 addr show eth0

# 启动日志里的回显（留空时显示 auto-assigned by Docker IPAM）
docker logs ikev2-panel 2>&1 | grep 'static ipv4'
```

⚠️ **固定地址写在 `service` 段的 `networks.<name>.ipv4_address`**，
不是顶层 `networks` 段的 `ipam` 里 —— 塞进 ipam 会被 Docker **静默忽略**
（无报错、地址不生效）。

#### 1.7.8 L2 模式下 web 打不开：TCP 连上了但 TLS 握手被重置（v2.92 修复）

**症状**（L2 模式，macvlan / ipvlan）：

- 浏览器访问 `https://<容器IP>:8443` **打不开**，转圈或直接报连接被重置。
- 但 `ping <容器IP>` **通**。
- 容器**看起来完全正常**：`docker ps` 显示 `Up`、重启次数 0、
  `docker logs` 里 `https listening addr=0.0.0.0:8443` 也在。
- **容器内部访问一切正常**：
  ```bash
  docker exec ikev2-panel curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1:8443/login
  # → 200
  docker exec ikev2-panel curl -sk -o /dev/null -w '%{http_code}\n' https://192.168.50.66:8443/login
  # → 200
  ```
- **走 IPv6 访问正常**：容器有 SLAAC 全局地址，用那个地址访问能拿到面板证书。
- 容器日志里刷满：
  ```
  http: TLS handshake error from 192.168.50.130:62665: write tcp 192.168.50.66:8443->192.168.50.130:62665: write: connection reset by peer
  ```

**根因**：strongSwan charon 的 kernel-netlink 插件会装一条策略路由规则：

```
220:    from all lookup 220
```

它把**所有**出向流量的查表强制到 `table 220`。而 `table 220` 里原本只有一条
`default via <网关>` —— **同网段的直连路由是缺的**。于是容器发往同网段对端
（比如你的电脑）的**回包也被 default 带走，绕网关出去**：

- 回包源 MAC 被换成**网关的 MAC**（抓包可见）；
- 路径变成「容器 → 网关 → 对端」，与入站方向不对称；
- 网关按不对称路径处理，直接**注入 RST**。

浏览器侧就表现为「TCP 握上了、TLS 握手立刻被重置」。

**为什么 host 模式没事**：host 模式下容器共享宿主 netns，宿主有
`ip rule 10: from <宿主IP> lookup route_ens18` 这条**优先级更高**的规则，
源地址是宿主 IP 的流量走 `route_ens18` 表，压根不进 `table 220`。
L2 模式容器是独立 netns，没有这条规则，只能靠 `table 220` 自己带直连路由。

**确认是不是这个问题**（两条命令就够）：

```bash
# 1. 同网段回包是否绕网关？
#    出现 "via <网关>" = 绕网关了 = 中招
#    出现 "dev eth0"    = 直连 = 正常
docker exec ikev2-panel ip route get 192.168.50.176

# 2. table 220 里有没有直连(scope link)路由？
docker exec ikev2-panel ip route show table 220
# 中招时长这样（只有 default，缺直连）：
#   default via 192.168.50.1 dev eth0
# 正常时多一行：
#   192.168.50.0/24 dev eth0 scope link
```

**修复**：v2.92 起 `scripts/entrypoint.sh` §3.5 在加 `default` 的同时，
会把 `main` 表里 `dev $OUT_IF` 的直连（`scope link`）路由**复制**进 `table 220`：

```
[entrypoint] table 220 default route added via 192.168.50.1 dev eth0
[entrypoint] table 220 direct route added: 192.168.50.0/24 dev eth0 scope link
```

这样：

- 同网段回包 → 命中 `table 220` 的直连路由 → 源 MAC 是容器自己的 → 不绕网关；
- 出公网 → 仍走 `table 220` 的 `default via 网关` → **XFRM 出向行为完全不变**。

**v2.92 之前的临时处置**（容器内改一下，重启会丢）：

```bash
docker exec ikev2-panel ip route replace 192.168.50.0/24 dev eth0 scope link table 220
```

⚠️ 网段要换成你自己 `IKEV2_L2_SUBNET` 的值。

**升级到 v2.92 后** 这个问题自动消失，不需要任何手工操作。

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

DB 是单文件 SQLite：`/data/panel.db`。`/data` 是 Docker **命名卷**（卷名恒为
`ikev2-panel-data`），数据落在宿主 Docker 数据根目录下，**不在项目目录下** ——
容器重建不会丢，但在仓库根目录找不到 `./data` 是正常的。
挂载点路径用 `docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}'` 取，
**别硬编码 `/var/lib/docker`**（数据根目录可以改，详见 [§8.1](#81-在项目目录找不到-data)）。

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

## 8. 数据 / 证书看不到

### 8.1 在项目目录找不到 `./data`

**这不是故障。** `/data` 挂的是 Docker **命名卷**，不是 bind mount：

```yaml
# docker-compose.yml
services.ikev2-panel.volumes:
  - type: volume
    source: ikev2-data
    target: /data
volumes:
  ikev2-data:
    name: ikev2-panel-data   # 实际卷名，恒定
```

数据在宿主的 Docker 数据根目录下，所以仓库根目录里永远不会有 `./data`
（`./logs` 才是 bind mount，能直接看到）。

```bash
docker exec ikev2-panel ls -la /data        # 日常排查用这个，最省事
docker volume inspect ikev2-panel-data     # 看 Mountpoint
```

⚠️ **别硬编码 `/var/lib/docker/volumes/...`。** Docker 数据根目录可以改，
192.168.50.63 就是 `Docker Root Dir: /vol1/docker`，实际卷路径是
`/vol1/docker/volumes/ikev2-panel-data/_data`。硬编码路径在那台机上直接不存在。
唯一可靠取法：

```bash
docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}'
```

### 8.2 数据真的丢了（卷被换掉）

**症状**：面板用户全没了、LE 证书要重新签、面板显示正常但一切像全新部署。

**原因**：早期 compose 用短名 `{项目目录名}_ikev2-data`，项目目录改名后
compose 找不到旧卷会**静默建一个同名空卷** —— 容器正常启动、面板可访问、
零报错，但 `panel.db`、LE 证书、acme account 全留在老卷里。
192.168.50.63 实测踩过这个坑。

现在 compose 顶层写死 `name: ikev2-panel-data`，与目录名解耦，不会再发生。
从旧版本迁过来的用户补一次改名：

```bash
docker volume ls | grep ikev2     # 找到老卷名
docker volume rename <老卷名> ikev2-panel-data
docker compose up -d              # 重新挂载
```

确认挂对了：

```bash
docker inspect ikev2-panel --format '{{range .Mounts}}{{.Name}} -> {{.Destination}}{{"\n"}}{{end}}'
# 期望输出 ikev2-panel-data -> /data
```

### 8.3 备份脚本提示"无任何运行时数据可备份"

**症状**：`backup-restore.sh` 报 `无任何运行时数据可备份(尚未部署?DATA_DIR 默认 /data)`。

**原因**：脚本默认 `DATA_DIR=/data`，那是**容器内**路径，在宿主上不存在。
`/data` 在命名卷里，不设 `DATA_DIR` 就一条都找不到。

```bash
DATA_DIR="$(docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}')" \
  ./scripts/backup-restore.sh backup
```

另外脚本只备份 `panel.db`、`panel-state/`、`le/` 三项 ——
`ca/`、`server/`、`acme-sh/`、`panel-tls/` 都不在内。要完整备份直接打包整个卷：

```bash
docker run --rm -v ikev2-panel-data:/src:ro -v /opt/ikev2-backups:/dst \
  alpine tar czf /dst/data-full.tar.gz -C /src .
```

### 8.4 容器重建后限速计数清零

**这是正常的，不是 bug。** 限速状态在 `/var/lib/ikev2-panel/limits`
（容器可写层，不在任何挂载里），容器重建即清空。
`/data/limits` 是 dev 分支的回落路径，生产环境不会用到。

限速**配置**存在 `panel.db`，重启后按配置重新下发，不影响限速生效。
实测 192.168.50.63：`docker logs` 里 `limit dir: /var/lib/ikev2-panel/limits (container)`，
且 `docker inspect` 的 Mounts 只有 `/data`、`/var/log`、`/host-sysctl.d` 三条。

---

## 9. 面板 UI 异常

前端问题**不影响 VPN 数据通路**，VPN 正常但面板界面不对时查这里。

### 9.1 破坏性操作没有二次确认弹窗

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

### 9.2 页面顶部横幅显示成一串标签文本

**症状**：横幅里出现 `&lt;strong&gt;…&lt;/strong&gt;` 这种字面量。

**原因**：富文本走了 banner 的 `.Msg` 字段，而 `.Msg` 受 Go 模板**自动转义**保护
（这是 XSS 防线，不能动）。

**解法**：字面量 HTML 用 `MsgHTML`，动态值（密码、用户名、外部 API 错误串）
拆成同级独立元素走默认转义。**不要**把变量拼进 `MsgHTML`。

### 9.3 刷新后页面不在最顶部

两个独立原因，只修一个仍会复现（详见 architecture §12.10）：

| 原因 | 判据 |
|------|------|
| `sessionStorage['ikev2-scroll-y']` 提交后没被删 | F12 → Application → Session Storage 里能翻到残留的旧值 |
| 浏览器原生 `history.scrollRestoration` 没关 | 停用缓存后强刷仍回到旧位置 |

**临时绕过**：F12 → Console 执行 `sessionStorage.clear()` 后刷新。

### 9.4 点「复制」显示已复制，剪贴板里却是空的

**症状**：按钮闪「已复制 ✓」，但粘贴出来不是内容。

**原因**：`navigator.clipboard` 不可用时降级到 `execCommand('copy')`，
早先这条路径吞掉异常、无条件报成功。

**排查**：F12 → Console 看有没有 `NotAllowedError` / `document is not focused`。
非 HTTPS 或非用户手势触发时降级路径也会失败。

**当前行为**：失败会如实显示「复制失败，请手动选中复制」，不再是假成功。

### 9.5 终端卡片出现两层横向滚动条

**症状**：命令卡片里内层 `<pre>` 和外层卡片各有一条横向滚动条，手机上几乎没法操作。

**原因**：两层都设了 `overflow-x: auto`。

**解法**：只保留外层滚动 —— 卡片 `overflow-x: auto`，
内部 `<pre>` 用 `overflow-x: visible`。**注意别把卡片的也改成 `visible`**，
那会让超长命令把整张卡片撑破容器。

### 9.6 表单校验失败后落到一个纯文本页

**症状**：提交非法值后页面变成没有导航的白底黑字，还带着一串英文错误。

**原因**：handler 用了 `http.Error(w, ..., 400)`。已修（见 §3.11 数据流），
如果你在自己的分支上又看到了，先确认 `failMCOpts()` 走的是 303 而不是 400。

### 9.7 「活跃隧道」的本地 / 远端是空的

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

### 9.8 页面文字双击就能全选出来

**这是 `user-select` 策略缺失或白名单被误删。** 当前设计是
**界面装饰禁选、数据可复制**（见 architecture §12.11），所以：

| 现象 | 判定 |
|------|------|
| 导航 / 按钮 / chip / 卡片标题能选中 | 正常就该禁选，若能选中说明 `body` 的 `user-select:none` 没了 |
| 服务器地址 / 命令 / 报错详情**选不中** | ❗ 白名单被误删 —— 这是功能受损，不是观感问题 |
| 折叠面板 `<summary>` 标题能选中 | 该元素是 UA 默认允许选择的，必须显式禁选 |

在 **iOS Safari** 上尤其容易漏：只写 `user-select` 而不写
`-webkit-user-select` 会被**静默忽略**，整条策略看起来完全没生效。
`internal/web/p2_text_selection_test.go` 的 `TestSel_NoWebkitPrefixGap`
会全量扫描 `style.css` 抓这种漏写。

自己跑一遍断言：

```bash
go test ./internal/web/ -run TestSel_ -v
```

### 9.9 点 eye 切换密码时按钮整体跳一下

**症状**：登录页 / 修改密码页点「显示密码」的 eye 按钮，图标瞬间下坠半个身位
（约 18px）再弹回。iPhone 上 `:active` 会粘住，现象被拉长，更明显。

**根因是 CSS 层叠冲突，不是 JS**：

```css
.password-toggle{ transform:translateY(-50%); }   /* 用 transform 做垂直居中 */
button:active    { transform:translateY(1px);  }   /* 全局按压动效 */
```

`button:active` 特异性 `(0,1,1)` > `.password-toggle` 的 `(0,1,0)`，`:active`
一命中就把 `translateY(-50%)` **整条覆盖**，居中失效；又因为 `.password-toggle`
的 `transition` 不含 `transform`，位移没有任何过渡，是瞬时跳变。

**已修（两层，缺一不可）**：

1. 居中改用 `top:0; bottom:0; margin-top:auto; margin-bottom:auto`，
   定位不再经过 `transform`。
2. 再加 `.password-toggle:active{ transform:none; }` 显式抵消全局按压位移。

**只做第 1 层会残留 1px 跳动** —— 居中改掉后全局 `button:active` 的
`translateY(1px)` 照样生效。这个图标是**浮在输入框内部**的，不是流式按钮，
1px 也被看成"错位"；它的按压反馈已由 `:hover` / `[aria-pressed]` 的底色 +
描边 + 图标变色承担，不需要位移。

**规矩**：
- transform 不能同时承担定位和按压动效。绝对定位元素的居中一律走
  `top`/`bottom` + `margin:auto`；按压反馈（全局 1px 下沉）可以保留，但不能靠它定位。
- 浮在表单控件**内部**的图标按钮（eye / 复制 / 清除），额外断掉按压位移。

自己跑一遍断言：

```bash
go test ./internal/web/ -run TestP3_ -v
```

`TestP3_NoAbsoluteCenteringViaTransform` 会在**整个 style.css** 上拦截
「绝对定位 + `top:50%` + `translateY(-50%)`」这个组合，防止同样的写法被复制到
别的浮层按钮上。

> ⚠️ 改完 CSS **必须实机验证**。CSS 改动不进浏览器就等于没改 ——
> 本地工作区改完直接告诉用户"修好了"是错的，要重建镜像部署后在真机确认。
> 页面 CSS 走 `assetVersion()` cache-bust（基于 mtime），不需要手动清浏览器缓存。

### 9.10 登录页版本号显示 `dev`

**这通常不是 bug，是构建时没传版本号。**

版本号在**构建期**注入，运行期只读文件：

```
Dockerfile            ARG PANEL_VERSION=dev → RUN echo "${PANEL_VERSION}" > /etc/ikev2-panel-version
cmd/ikev2-panel/main.go  读 /etc/ikev2-panel-version；读不到 → fallback "dev"
web/templates/login_content.html   版本 <code>{{.Version}}</code>
```

确认当前值：

```bash
docker exec ikev2-panel cat /etc/ikev2-panel-version
# 或看镜像元数据
docker inspect ikev2-panel --format '{{index .Config.Labels "org.opencontainers.image.version"}}'
```

| 场景 | 显示 | 是否正常 |
|------|------|---------|
| 从 ghcr.io 拉 `:latest` / tag 镜像 | `main-<sha>` 或 tag 号 | 正常，CI 传了 `PANEL_VERSION` |
| 本地/服务器上手工 `docker build` | `dev` | 默认值，**要手动补 build-arg** |

CI（`.github/workflows/docker-publish.yml`）会按 tag / main push 算出
`PANEL_VERSION` 传给 Dockerfile，所以正式镜像不会出现 `dev`。自建镜像要显式传：

```bash
docker build -t ikev2-panel-v2:local \
  --build-arg PANEL_VERSION="$(git describe --tags --always)" \
  --build-arg VCS_REF="$(git rev-parse --short HEAD)" \
  --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" .
```

用 `docker compose build` 时不用管，`docker-compose.yml` 已经把
`PANEL_VERSION: ${PANEL_TAG:-latest}` 透传过去了。

> 注意：`cert.PanelVersion`（写进 `.mobileconfig` 的 `PayloadDescription`）是
> 另一处独立的常量，与本条无关，它不随镜像变化。

### 9.11 排查手法

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
#    L2 模式下容器有独立 IP，这两条查到的是宿主自己的服务，与本项目无关

# 9. 当前网络模式（三模式通用）
docker exec ikev2-panel cat /run/ikev2-panel/netmode.env
docker inspect ikev2-panel --format '{{.HostConfig.NetworkMode}}'

# 10. L2 模式专查：地址 / SLAAC sysctl / 转发
docker exec ikev2-panel ip -br addr
docker exec ikev2-panel sysctl net.ipv4.ip_forward
docker exec ikev2-panel cat /proc/sys/net/ipv6/conf/eth0/accept_ra
docker exec ikev2-panel cat /proc/sys/net/ipv6/conf/eth0/disable_ipv6

# 11. 网络模式规格表单元测试（不依赖 docker）
./tests/network_mode_test.sh
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
