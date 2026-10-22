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

### 三网络模式与前置条件

支持 `host`（默认）、`macvlan`、`ipvlan` 三种，v2.86-PR20 起。

| 模式 | compose 文件 | netns | 地址来源 | 什么时候用 |
|------|-------------|-------|---------|-----------|
| `host` | `docker-compose.yml` | 与宿主共享 | 宿主网卡 | **默认**。零改动兼容，宿主已有公网 v6 就直接用 |
| `macvlan` | `docker-compose.macvlan.yml` | 独立 | IPAM 分配 v4 + 上游 RA 下发 v6 | 宿主上 500/4500/8443 已被别的服务占用，但容器需要在同一二层里占一个独立 IP。**支持多项目共享同一条网络** |
| `ipvlan` | `docker-compose.ipvlan.yml` | 独立 | 同上，但与宿主共享 MAC | 同 macvlan，但上游（云厂商 / 交换机）不允许出现第二个源 MAC。**支持多项目共享同一条网络** |

**三个模式 = 三个独立完整的 compose 文件**，各管各的，不叠加：

```bash
docker compose up -d                                # host
docker compose -f docker-compose.macvlan.yml up -d  # macvlan
docker compose -f docker-compose.ipvlan.yml up -d   # ipvlan
```

也可以用 `./scripts/up.sh` —— 它读 `.env` 里的 `IKEV2_NETWORK_MODE` 自动选文件，
额外做两件事：建 `./logs` 目录（bind mount 目标），以及在调 docker 之前拦下
L2 必填变量缺失 / 填了 v6 子网这类配置错误。

不需要 `!reset` / `!override` 标签，所以**任何 Docker Compose 版本都能用**，不设版本下限。

> ⚠️ **三份文件的服务定义必须保持一致**（image / capabilities / volumes / tmpfs / environment）。
> 这是三独立文件方案唯一的结构性风险 —— 改一处漏两处的后果不是报错，而是
> 「某个模式下静默新建空卷 → panel.db / LE 证书全丢」。改完 compose 请跑：
>
> ```bash
> ./tests/network_mode_test.sh     # NM-29~NM-36 强制校验三份一致
> ```

**L2 模式：改 compose 顶部三行，不需要 `.env`**

打开 `docker-compose.macvlan.yml` / `docker-compose.ipvlan.yml`，最上面有一个
醒目的注释块，下面三行是唯一需要改的地方（**出厂是「留空 + 中文报错」状态**）：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-${IKEV2_L2_PARENT:?请在 compose 顶部 x-l2-parent 填父接口名（如 ens18）}}"
x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-${IKEV2_L2_SUBNET:?请在 compose 顶部 x-l2-subnet 填宿主所在 IPv4 子网（如 192.168.50.0/24）}}"
x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-${IKEV2_L2_GATEWAY:?请在 compose 顶部 x-l2-gateway 填网关（如 192.168.50.1）}}"
```

**把 `:-` 后面的整段内容替换成你的值**（外层 `${...}` 外壳保留）：

```yaml
x-l2-parent:  &l2_parent  "${IKEV2_L2_PARENT:-ens18}"
x-l2-subnet:  &l2_subnet  "${IKEV2_L2_SUBNET:-192.168.50.0/24}"
x-l2-gateway: &l2_gateway "${IKEV2_L2_GATEWAY:-192.168.50.1}"
```

外层 `${...}` 外壳**不能删**。留着它，`.env` 用户才能用 `.env` 覆盖；
删了（写成 `"ens18"`），这个值就变成硬编码，`.env` 永久失效。

> **留空会怎样**：compose 在**拉镜像之前**就停下来，把中文提示原样打出来：
> ```
> error while interpolating x-l2-subnet: required variable IKEV2_L2_SUBNET is missing a value:
> 请在 compose 顶部 x-l2-subnet 填宿主所在 IPv4 子网（如 192.168.50.0/24）
> ```
> 不会拉镜像、不会建容器、不会建网络。63 实测（Docker 28.5.2 / Compose v2.40.3）。

查真实值（在**宿主**上执行）：

```bash
ip -o -4 route show default     # 第一列 dev 后面那个 = 父接口名；第三列 = 网关
ip -o -4 addr show <父接口>     # inet 那一列的 CIDR = 子网
```

改完直接 `up`。这三个值在文件里定义一次，`environment:` 段和 `networks:` 段
引用的是同一个 anchor —— 不可能只改一边，也不需要在两个地方各写一遍。

> 优先级：**shell 环境变量 > `.env` 文件 > compose 里的字面量**。
> 愿意用 `.env` 的照旧可用，三个变量名不变，anchor 里套的就是 `${VAR:-默认}`。
> 优先级是 Docker Compose 的标准行为，不是本项目特有的。

> ⚠️ **`SUBNET` / `GATEWAY` 留空是故意的**：这两项每台机器都不同，没有通用默认值。
> 写死一个「看起来能跑」的值比留空更危险 —— 容器会正常起来但网段是错的，
> 排查成本远高于多看一眼注释。留空时 compose / entrypoint 都会明确告诉你缺哪一项。

**可选：固定容器 IPv4**

默认 Docker 从子网里自动分配一个地址（实测拿到 `.2`）。需要固定地址时
（比如防火墙规则里写死、或多个实例要区分），在顶部加第四行：

```yaml
x-l2-ipv4:    &l2_ipv4    "${IKEV2_L2_IPV4:-192.168.1.10}"
```

约束：必须落在 `SUBNET` 内、不能等于 `GATEWAY`、宿主上不能已被占用。
最后一条最要紧 —— 撞上宿主已用的地址会让**宿主的入站连接全部断开**
（实测 SSH 直接被 reset），`scripts/up.sh` 会在启动前拦下来。

> ⚠️ **用第三方面板部署（1Panel / 宝塔 / dpanel 等）时特别注意**：
> 面板的编排有**自己的目录**，直接编辑它那份 compose 即可（改顶部三行）。
> 但要注意面板可能会用**自己的 `.env` 覆盖同名变量** —— 若面板里已存在
> `IKEV2_L2_*` 三项，值会优先于你在 compose 里写的字面量。
> 排查步骤见 [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md#1321-用第三方面板部署1panel--宝塔--dpanel-等时变量传不进容器)。

**共享网络（多项目可加入同一条）**

L2 模式的网络默认就叫**驱动名** —— macvlan 模式叫 `macvlan`，ipvlan 模式叫
`ipvlan`（不是 `<项目名>_ikev2-l2`），**所有 project 加入的是同一条网络**。

为什么要这样 —— Docker 的 IPAM 是全局的，「各 project 各建各的」根本走不通：

| 限制 | 报错 |
|------|------|
| Docker IPAM 全局 | `invalid pool request: Pool overlaps with other one on this address space` |
| ipvlan parent 独占 | `network di-xxx is already using parent interface ens18`（换子网也不行） |

也就是说，**同一张网卡上本来就只能有一条 ipvlan 网络**。与其撞车，不如显式共享。

**两条路径都支持，且互通**（v2.97 起）：

| 路径 | 怎么起 | 谁建网络 | 网络已存在时 |
|------|--------|---------|-------------|
| **纯 compose**（Docker 管理器 GUI / `docker compose up -d`） | 改 compose 顶部三行 → up | **compose 自己建** | 复用（带正确 label 时）；无 label 会报 label 不匹配 |
| **up.sh**（可选增强） | `./scripts/up.sh` | **up.sh 建**，并逐项校验参数 | 复用 + 校验参数；不符则报错列差异 |

两条路径建出来的网络**互相可用**（63 实测）：
`up.sh` 建网时带 `--label com.docker.compose.network=ikev2-l2`，
所以纯 compose 路径能直接复用它（只出 `was not created by compose` 的 warning，
不影响启动）。

`./scripts/up.sh` 的处置逻辑（macvlan / ipvlan **完全一致**）：

| 情况 | 行为 |
|------|------|
| 网络不存在 | **自动创建**（`docker network create --driver <驱动> --subnet=… --gateway=… --ipv6 --label com.docker.compose.network=ikev2-l2 <名字>`） |
| 同名网络已存在、参数一致 | **直接复用**，不修改任何东西 |
| 同名网络已存在、参数不一致 | **报错并逐项列出差异**（子网 / 网关 / 父接口 / 模式…），不自动删改 |
| 同名不存在，但宿主机上已有参数兼容的同驱动网络 | **自动接管**那条（你自建的名字不一样也能认出来） |
| ipvlan 模式下 parent 上已有别的 ipvlan 网络 | 明确报冲突，给出「复用它 / 换网卡」两条路 |

要加入**别人已经建好的**那条网络，把名字写对即可 —— 三种写法等价：

```bash
# 1. 环境变量
IKEV2_L2_NETWORK_NAME=ipvlan ./scripts/up.sh

# 2. .env 文件
#    IKEV2_L2_NETWORK_NAME=ipvlan

# 3. compose 顶部（第四个 anchor）
x-l2-netname: &l2_netname "${IKEV2_L2_NETWORK_NAME:-ipvlan}"
```

> 显式指定名字 = 明确要求用那一条。若那条不存在而另有参数兼容的，
> `up.sh` **不会**悄悄换成另一条，而是报错让你选。

> ⚠️ **共享网络的 IPAM 归第一个建它的人所有**。后来者配置里的 subnet / gateway
> 只用于「校验是否匹配」，不再决定实际网段。
> 纯 compose 路径下 `docker compose down` 会尝试删这条网络，但**别的容器还挂着时
> 不会真删**（Docker 报 `Resource is still in use` 后放弃）—— 63 实测：
> P1 down 后网络仍在，P2 容器照常拿到 `192.168.50.3`。

> ⚠️ **两种驱动的差别只在 parent 独占性**：
> ipvlan 的 parent 独占（一张网卡上只能一条 ipvlan 网络），
> macvlan 不独占（一张网卡上可以有多条 macvlan 网络，只要子网不重叠）。
> 但**共享本身两者都支持** —— 实测同一条 macvlan 网络上可以挂多个容器、
> 跨 project，各拿独立 IP 与独立 MAC。

**L2 模式前置条件**：

| 条件 | 说明 |
|------|------|
| Linux | macvlan / ipvlan 是 Linux 内核特性，Docker Desktop 不支持 |
| kernel ≥ 3.9（macvlan）/ ≥ 4.2（ipvlan） | 低于此版本直接没有这两个 driver |
| 非 rootless | rootless 下支持不完整 |
| 上游对该网段**发 RA** | L2 模式的公网 IPv6 靠 RA/SLAAC 自动获取，没有 RA 就没有 v6 |
| 云厂商可能拦新 MAC | macvlan 收不到 ARP/RA 时，改用共享 MAC 的 ipvlan |
| **父接口可能被虚拟化层限制** | 云主机（KVM/超卖）上最常见的硬墙，macvlan 和 ipvlan 都会失败，见下方 |

**先自查父接口**（两条命令，10 秒决定能不能用 L2）：

```bash
ip -d link show eth0 | head -1
# 看 parentbus 那行：virtio 是「可能有风险」，不是「一定不行」
docker network create ikev2-l2-probe \
  --driver macvlan -o parent=eth0 -o macvlan_mode=bridge
# Error response from daemon: device or resource busy → 宿主虚拟化层限制，改用 host 模式
docker network rm ikev2-l2-probe
```

⚠️ **`parentbus virtio` 只是风险信号，不是判决**。实测两台机器结论相反：

- 云主机（Docker 28.5.2，宿主 `ens18` 为 `parentbus virtio`）：macvlan 与 ipvlan
  **都**报 `device or resource busy`；同一配置把 parent 换成 `docker0` 则 ipvlan
  正常创建。
- 群晖 NAS（Docker 28.5.2，宿主 `ens18` 同样为 `parentbus virtio`）：macvlan 与
  ipvlan **都能正常创建网络并跑起容器**（ipvlan 容器拿到 `192.168.50.2/24`，
  IPv6 GUA 经 SLAAC 正常获取）。

所以**以探针命令的实际输出为准**，不要凭接口名推断。探针通过就说明这台机器的
虚拟化层放行；探针失败就是宿主限制，**不是配置写错**，改任何参数都绕不过去。

**已知限制**：

- **macvlan / ipvlan 在内核层隔离宿主与容器** —— 容器 ping 不到宿主。
  需要互通就建 shim：

  ```bash
  ip link add macvlan-shim link eth0 type macvlan mode bridge
  ip addr add 203.0.113.9/24 dev macvlan-shim && ip link set macvlan-shim up
  ip route add <容器IP>/32 dev macvlan-shim
  # 删除：ip link del macvlan-shim
  ```

- **`IKEV2_L2_SUBNET` 只接受 IPv4**。填 v6 子网会让 Docker IPAM 接管 IPv6 并
  分配 ULA（`fd00::/8`），公网不可达，所以入口直接拒绝。

- **`IKEV2_OUT_IF` 的语义随模式变化**：host 模式是**宿主**网卡名，
  macvlan / ipvlan 是**容器**网卡名（通常 `eth0`）。沿用宿主网卡名会被
  FATAL 拦截（否则会静默失效成"能连 VPN 但出不了网"）。
  **L2 模式下建议直接删掉这一行让它自动探测。**

- **切换模式必须重建容器**（compose 文件决定网络）：

  ```bash
  docker compose -f docker-compose.macvlan.yml down && docker compose -f docker-compose.macvlan.yml up -d
  ```

- host 模式仍然**不要**去改 `/etc/docker/daemon.json` 配 IPv6 或转发开关：
  host 模式不走 Docker 的 iptables / sysctl 管理，改那些只会在宿主上留下无关配置。

详见 [architecture.md §11.1](docs/architecture.md) 与
[TROUBLESHOOTING.md §1.7](docs/TROUBLESHOOTING.md)。

### Capabilities

```yaml
cap_drop: [ALL]
cap_add: [NET_ADMIN, NET_RAW, NET_BIND_SERVICE, CHOWN, DAC_OVERRIDE, FOWNER]
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
| `FOWNER` | 写非属主文件（LE 证书签发、panelstate 属主切换） |

三种网络模式共用同一套 `cap_add`，无需按模式调整。
不要随手加 `SYS_ADMIN` 或 `privileged: true` —— 这个镜像不需要它们。

### 数据卷

```yaml
# 挂载点
- type: volume
  source: ikev2-data      # 顶层 key
  target: /data
- ./logs:/var/log:rw
- /etc/sysctl.d:/host-sysctl.d:rw

# 顶层定义
volumes:
  ikev2-data:
    name: ikev2-panel-data
```

**`/data` 是 Docker 命名卷，不是 bind mount。** 数据落在 Docker 数据根目录下的
`volumes/ikev2-panel-data/_data`，**不在项目目录下** —— 在仓库根目录找 `./data`
永远找不到，这是预期行为而非部署失败。

⚠️ **数据根目录不一定是 `/var/lib/docker`。** 192.168.50.63 实测：
`docker info` 显示 `Docker Root Dir: /vol1/docker`，实际路径是
`/vol1/docker/volumes/ikev2-panel-data/_data`。**别硬编码路径**，用命令动态取：

```bash
docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}'
```

| 宿主可见？ | 挂载 |
|---|---|
| ✅ 直接可见 | `./logs`、`/etc/sysctl.d` |
| ❌ 只在卷里 | `/data`（数据库 / 证书 / 配置） |

卷名由顶层 `name: ikev2-panel-data` 显式锁定，**与项目目录名解耦**：
改名仓库目录、多份 clone 共存都指向同一个卷。早先用 `{目录名}_ikev2-data` 时，
目录改名会让 Compose 静默建一个空卷 —— `panel.db`、LE 证书"消失"且零报错
（192.168.50.63 实测）。从旧命名迁过来的用户执行一次：

```bash
docker volume rename ikev2-panel-v2_ikev2-data ikev2-panel-data
```

`/data` 下持久化（192.168.50.63 实测目录清单）：

| 路径 | 内容 |
|---|---|
| `panel.db` | SQLite 用户 / 审计（旁边有 `-wal` / `-shm`） |
| `panel-state/` | `cert.conf`、`aliyun.creds`、`ddns-runtime.conf`、`ratelimit.json`（**登录**限流计数）、`INITIAL_ADMIN_PASSWORD.txt` |
| `le/` | Let's Encrypt `fullchain.pem` / `privkey.pem` |
| `acme-sh/` | acme.sh 工作目录（丢了会触发 LE 速率限制） |
| `ca/` | 自签 CA 证书与私钥 |
| `server/` | IKEv2 服务端证书与私钥 |
| `panel-tls/` | 面板 HTTPS 证书 |

**带宽限速文件不在 `/data` 里。** 两个「限流」别混：

| | 位置 | 容器重建后 |
|---|---|---|
| 登录限流计数 | `/data/panel-state/ratelimit.json`（持久化） | 保留 |
| 带宽限速（tc） | `/var/lib/ikev2-panel/limits`（容器可写层） | 清零 |

带宽限速文件丢失不影响功能：限速**配置**存在 `panel.db` 里，容器启动时按配置
重新下发。`main.go` 只在该目录**不存在**时才回退到 `/data/limits`，那是 dev 分支。

在宿主上直接查看：

```bash
docker exec ikev2-panel ls -la /data       # 容器视角，日常排查用这个
docker volume inspect ikev2-panel-data    # 看 Mountpoint（注意不是 /var/lib/docker）
```

`/etc/sysctl.d:/host-sysctl.d:rw` 只在 forwarding 自愈成功时才会被写入 `99-ikev2.conf`，
让宿主重启后仍然生效；宿主上原本没有该文件时，自愈不会留下任何配置痕迹。

### 端口与防火墙

| 端口 | 协议 | 用途 | 建议 |
|------|------|------|------|
| 500 | UDP | IKEv2 协商 | 对所有客户端开放 |
| 4500 | UDP | IKEv2 NAT-T（`kernel-libipsec` 强制） | 对所有客户端开放 |
| 8443 | TCP | 管理面板 HTTPS（默认 `0.0.0.0:8443`） | **限制源 IP** |
| — | ESP / IP 协议号 50 | 不需要 | libipsec 走 UDP/4500 |

三种模式**都不做端口映射**：

- **host** —— 容器直接在宿主上监听 500/4500/8443，与宿主已有服务抢端口。
  这是选 macvlan / ipvlan 的主要动机。
- **macvlan / ipvlan** —— 容器有自己的 IP，500/4500/8443 绑在那个 IP 上，
  与宿主服务完全不冲突。此时防火墙规则要指向**容器 IP**而不是宿主 IP，
  且容器 IP 随重建可能变化（除非在 IPAM 里固定地址）。

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
| `IKEV2_NETWORK_MODE` / `IKEV2_L2_*` | **重建容器** —— 选哪个 compose 文件由 `-f` 决定。这个变量在每份文件里硬编码为自身模式，改它没用 |
| `IKEV2_L2_NETWORK_NAME` | **重建容器** —— 决定加入哪条共享网络。改名时先确认目标网络在宿主上已存在且参数一致 |
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
用 `DATA_DIR` 指向命名卷在宿主上的挂载点 —— 默认值 `/data` 是**容器内**路径，
在宿主上不存在，不覆盖就会静默跳过（脚本会提示"无任何运行时数据可备份"）：

```bash
DATA_DIR="$(docker volume inspect ikev2-panel-data --format '{{.Mountpoint}}')" \
  ./scripts/backup-restore.sh backup
```

脚本实际只备份三项：`panel.db`、`panel-state/`、`le/`。
`acme-sh/`（LE 账号与 CA 缓存）、`panel-tls/`、`limits/` **不在备份范围内** ——
acme.sh 丢了会在下次签发时重新注册账号，代价是更接近 LE 速率限制；
真要完整备份请直接打包整个卷：

```bash
docker run --rm -v ikev2-panel-data:/src:ro -v /opt/ikev2-backups:/dst \
  alpine tar czf /dst/data-full.tar.gz -C /src .
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

**限速的作用域随网络模式变化**：tc 规则挂在 `IKEV2_OUT_IF` 上，而这张网卡
在 host 模式是宿主网卡、在 macvlan / ipvlan 是**容器自己的 eth0**。
功能不变（都是约束客户端出向流量），只是 qdisc 从宿主的队列挪进了容器 netns，
在宿主上 `tc qdisc show dev eth0` 查不到，别误判成「限速没生效」。
排查限速一律用 `docker compose exec ikev2-panel tc qdisc show dev <OUT_IF>`。

### libipsec 是单核瓶颈

ESP 走用户态解密，单 SA 绑定单线程，**无法多核扩展，也不支持网卡硬件 offload**。

| 场景 | 是否适合 |
|------|----------|
| 1–20 人，每人 ≤ 5 Mbps | 适合 |
| 20–50 人，每人 ≤ 5 Mbps | 可用，CPU 建议支持 AES-NI |
| 50+ 人，或总带宽 > 500 Mbps，或延迟敏感业务 | 不适合，应改用内核 XFRM |

MOBIKE 默认开启，客户端切网时复用现有 IKE SA，切换不掉线。

### forwarding 自愈，失败即拒绝启动

容器作为 VPN 路由器需要转发打开。**host 模式的 compose 里没有 `sysctls:` 段** ——
host 网络模式下容器与宿主共享 netns，runc 会以
`not allowed in host network namespace` 拒绝它。
L2 模式容器有自己的 netns，runc 允许写，所以由 compose 注入。

| 模式 | forwarding 在哪个 netns | 谁负责打开 |
|------|------------------------|-----------|
| `host` | 与宿主同一个 | **宿主**（`sysctl -w`），entrypoint 只会把值持久化到 `/etc/sysctl.d/99-ikev2.conf` |
| `macvlan` / `ipvlan` | 容器自己的 | **L2 compose 文件的 service 段 `sysctls:`** |

L2 模式注入的三条（L2 compose 文件里已写好，不需要用户操作）：

```yaml
sysctls:
  net.ipv4.conf.all.forwarding: "1"
  net.ipv6.conf.all.forwarding: "1"
  net.ipv6.conf.default.accept_ra: "2"   # SLAAC 必需
```

配套 `networks.ikev2-l2.enable_ipv6: true`。
实测细节与其他写法的失败原因见 [architecture.md §12.8.2](docs/architecture.md)。

```bash
# host 模式：转发必须由宿主负责
sudo sysctl -w net.ipv4.ip_forward=1
sudo sysctl -w net.ipv6.conf.all.forwarding=1
```

⚠️ **L2 模式下这条命令无效** —— `net.ipv4.ip_forward` 是 per-netns 的，
宿主上 `sysctl -w` 改的是宿主那个 netns，与容器无关。这也是 L2 模式不写
`/host-sysctl.d/99-ikev2.conf` 的原因：写了也是误导。

两步都失败时 entrypoint **FATAL `exit 11`**，容器起不来。这是刻意设计：
宁可启动失败，也不要跑出一个"看起来正常但客户端连上后上不了网"的实例。

nftables 规则同理：启动时校验自定义表 `ikev2` / `ikev26` 是否生效，缺失则重建重试
（快阶段 1s × 10，慢阶段 5s × 50）。彻底失败会写哨兵文件，面板顶部告警。

### L2 模式（macvlan / ipvlan）的代价

- **容器与宿主在内核层隔离**，互相 ping 不通，需要互通得建 shim。
- **IPv6 完全依赖上游发 RA**。不发 RA 就没有公网 v6；
  `IKEV2_SERVER_ADDR_V6` 只填 charon 的 `local_addrs`，**不负责把地址配到网卡上**，
  设了它但网卡上没有对应地址反而会让 charon bind 失败。
- **只在 Linux 可用**，Docker Desktop / rootless 支持不完整。
- **容器 IP 随重建可能变**，写死防火墙规则前先在 IPAM 里固定地址。
- 详见 [三网络模式与前置条件](#三网络模式与前置条件) 与
  [TROUBLESHOOTING.md §1.7](docs/TROUBLESHOOTING.md)。

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
| 出问题了 | [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)（§1-7 服务端 · §8 数据/证书看不到 · §9 纯前端） |
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

### 文本选择的约定

界面装饰（导航、按钮、chip、标题）**禁止选中**，避免双击 / 拖拽误选；
但密码、服务器地址、命令、证书参数、报错详情**必须能复制**。所以是
「`body` 默认禁选 + 白名单放开」，**不要全局 `user-select: none`** ——
那会把复制功能一起废掉。

新组件默认继承 `body` 的禁选；确定需要复制的（如新加的数据展示区），
显式写 `user-select: text`。任何 `user-select` 都**必须同时写
`-webkit-user-select`**，否则 Safari / iOS 上整条策略失效。
详见 [architecture.md §12.11](docs/architecture.md)，
回归测试 `internal/web/p2_text_selection_test.go`。

### 前端两条易踩的约定

1. **`transform` 不能同时承担定位和动效。** 绝对定位元素居中用
   `top`/`bottom` + `margin:auto`，不要用 `transform:translateY(-50%)` ——
   全局 `button:active{transform:translateY(1px)}` 特异性更高，会把它整条覆盖，
   按压时元素"跳出去"。浮在输入框里的图标按钮（eye / 复制）还要额外写
   `transform:none` 断掉按压位移。
   详见 [architecture.md §12.12](docs/architecture.md)，
   回归测试 `internal/web/p3_press_offset_test.go`。

2. **改完 CSS 要实机验证。** CSS 改动不进浏览器等于没改；重建镜像部署后
   在真机确认再收工。页面 CSS 走 `assetVersion()` cache-bust（基于 mtime），
   不需要手动清浏览器缓存。

## 模块结构

Go 单体，内部按包分层：`cmd/ikev2-panel`（入口）→ `internal/*`（auth / cert / config /
runtime / panelstate / store / swanctl / web / ddns / limit / expiry 等），
外加 `scripts/`（entrypoint、updown、续签、备份）和 `configs/`（swanctl 模板）。
完整包结构、依赖方向和数据流见 [architecture.md §1](docs/architecture.md)。
