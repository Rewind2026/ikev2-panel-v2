# IKEv2 Panel v2

> 1–50 人规模的 IKEv2/IPsec VPN 管理面板（IPv6-only 默认，IPv4 可选）。
> **v2-79 起：所有网络参数自动探测，用户唯一必填只有 `IKEV2_SERVER_CN`**。
>
> **v2-79.1**：ECDH 协商 + 私钥权限 + mobileconfig DNSSettings 格式三处 Critical 修复。
>
> **v2-79.2**：Android 11+ 原生客户端 EAP-MSCHAPv2 + iOS 18 ESP DH 修复。
> 设计与架构文档见仓库 `docs/` 目录：
> - [`docs/design.md`](docs/design.md) — 设计文档（重点 §1.5 libipsec 选型 + §15~20 v2-79 自动探测 + §22.2 v2-79.2 Android 原生）
> - [`docs/architecture.md`](docs/architecture.md) — 架构理解文档（重点 §18 govici 重构 + §3.5 VICI 协议）

---

## 项目状态

### 里程碑

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | 项目骨架：Docker + entrypoint + updown + swanctl 主配置 | ✅ |
| M2 | 存储层：SQLite (modernc.org/sqlite, WAL)，users/sessions/admins 三表 | ✅ |
| M3 | 认证：bcrypt 管理员 + cookie + 随机密码生成 + 限速保护 | ✅ |
| M4 | 用户管理：swanctl 子配置 CRUD + 失败回滚 | ✅ |
| M5 | 客户端配置：mobileconfig（自签内联 / LE 不内联）+ QR 码 + sswan + CA cert | ✅ |
| M6 | 限速/过期/流量：tc updown + collector + expiry checker | ✅ |
| M7 | LE 模式：acme.sh 集成 + 续签健康检查 + 失败回退 | ✅ |
| M8 | Dockerfile 自编译强Swan + libipsec + govici VICI 协议 | ✅ |
| **M9** | **v2-79 零硬编码部署 + 公网 IPv6 自动变化感知（ipv6watch）** | ✅ |

### M9 v2-79 改动总览（commit 27，2026-09-17）

| Commit | 内容 | 关键文件 |
|---|---|---|
| **27** | entrypoint.sh §0 自动探测链 + 所有硬编码改为可选 | [`scripts/entrypoint.sh`](scripts/entrypoint.sh), [`configs/swanctl-ipv6-only.conf`](configs/swanctl-ipv6-only.conf), [`docker-compose.yml`](docker-compose.yml), [`.env`](.env), [`.env.example`](.env.example) |

**v2-79 设计动机**：解决用户的 5 个真实痛点——
1. **公网 IPv6 动态变化**（ISP 重拨） → `internal/swanctl/ipv6watch.go`（v2-72 已存在，v2-79 加强文档）+ §0.2 自动探测
2. **网卡名硬编码**（ens18 / eth0 因部署环境而异） → §0.1 自动探测（`ip -4 route show default`）
3. **网关 / 网段变化** → §0.4 / §0.5 自动探测 + §0.6 自动选不冲突的 VPN 虚拟 IP 段
4. **"改了宿主机配置"误解** → §19.4 文档明确：只改容器内，绝不动宿主
5. **host 网络不允许怎么办** → §18 文档化 + docker-compose 注释保留 bridge/ipvlan 模板（v2-80+ 真正自动切换）

**v2-79 关键文件改动清单**：

| 文件 | 改动 |
|---|---|
| `scripts/entrypoint.sh` | §0.1~§0.6 自动探测链 + §3.5 MASQUERADE 源用 `%IKEV2_VPN_SUBNET%` + §5 占位符替换扩展 |
| `configs/swanctl-ipv6-only.conf` | `local_addrs = %IKEV2_LOCAL_ADDRS_DIRECTIVE%`（可省略） + `local_ts = %IKEV2_LOCAL_TS%` + `addrs = %IKEV2_VPN_SUBNET%, fd00:1::/64` |
| `docker-compose.yml` | `IKEV2_SERVER_ADDR_V6=` / `IKEV2_OUT_IF=` 留空 + 新增 `IKEV2_NETWORK_MODE=auto` + `IKEV2_VPN_SUBNET=auto` |
| `.env` / `.env.example` | 所有字段都注释"留空 → 自动探测" |
| `internal/cert/mobileconfig.go` | `PanelVersion = "v2-79"` |
| `internal/swanctl/ipv6watch.go` | 不动（v2-72 已就位，v2-79 文档化） |

### v2-79.1 增量（Critical 修复）

三个 agent 并行审查后筛选的、对你这个家用 IPv6 VPN 部署场景真正有影响的 3 个问题：

| # | 问题 | 修复 |
|---|---|---|
| C5 | iOS 18 默认推 ecp256 → 当前 proposals 只列 MODP2048 → 降级耗电 | `swanctl-ipv6-only.conf:44` 追加 `ecp256` / `curve25519` 系列 |
| C4 | server.key.pem 私钥被写成 0o644（任何同主机用户可读）| `cmd/ikev2-panel/main.go:installCertsToSwanctl` 按文件类型拆权限——cert 0o644，key 0o600 |
| C6 | mobileconfig DNSSettings 用了 `<array><dict>` 错误格式，iOS 17/18 静默忽略整个节点 | 改为 `<dict><key>DNS</key><array>...</array></dict>` |

**没修的其他 23 个问题**（DB 事务化、密码走 query string、reload 竞态、fd 上限、panic recover 等）全部进 v2-80+ backlog。

### v2-79.2 增量（ESP DH + Android 原生客户端支持）

v2-79.1 完成后，用户对四平台兼容性提出疑问。agent 审查发现两个真问题，**加上一次方案撤回**：初版基于 agent 错误判断"Android 11+ 原生只支持 PSK"做了 PSK 段，被用户实测 + AOSP 源码反驳后撤回，最终方案比初版更简单。

#### 部署注意事项（host 网络 + user namespace + forwarding 自愈）

v2-79.2 移除 `docker compose sysctls:` 配置。原因：host 网络 + user namespace 下 docker daemon 拒绝写 `net.ipv4.ip_forward`（报错 `not allowed in host network namespace`）。

新的 forwarding 自愈链路（**首次部署到新机器完全无人值守**）：

1. **entrypoint §3 自愈 + 自动持久化**（v2-79.2 新增，推荐）
   - 镜像内 `scripts/entrypoint.sh:251-271` 检测 forwarding 是否开
   - 没开 → `echo 1 > /proc/sys/net/...`（host 模式共享 net ns，写的就是宿主内核）
   - 自愈成功后**自动追加到宿主 `/etc/sysctl.d/99-ikev2.conf`**（需要 docker-compose.yml 的 bind mount）
   - 宿主重启时 `systemd-sysctl.service` 自动加载该 conf，下次部署自带配置
   - 已实测验证（删 conf + 关 forwarding → 重启容器 → 自动重建）

   bind mount 配置（`docker-compose.yml:94-95`）：
   ```yaml
   - /etc/sysctl.d:/host-sysctl.d:rw
   ```

2. **手工预置 `/etc/sysctl.d/99-ikev2.conf`**（可选，初次部署前手工写）
   ```
   net.ipv6.conf.all.forwarding = 1
   net.ipv4.ip_forward = 1
   ```

3. **docker compose sysctls**（传统方式，但 user namespace 下被禁，**仅 bridge/ipvlan 模式可用**）

```yaml
# docker-compose.yml 里 sysctls 块已注释：
#   sysctls:
#     net.ipv6.conf.all.forwarding: 1
#     net.ipv4.ip_forward: 1
```

实际验证：`cat /proc/sys/net/ipv4/ip_forward` 在宿主机和容器内显示同一值（host net ns 共享）。

**最终改动（两条 Critical）**：

| # | 类型 | 问题 | 修复 |
|---|---|---|---|
| F1 | 真 Critical | v2-79.1 只改了 IKE SA proposals，ESP SA 独立重协商 DH 仍走 MODP2048 → iOS 18 数据通道耗电 | `swanctl-ipv6-only.conf:36` `esp_proposals` 同步追加 ECDH 系列 |
| F2 | 真 Critical | `internal/cert/generate.go:5` 注释错误"iOS 拒 ECDSA，必须 RSA 2048"（实际 iOS 17+ 已支持） | 注释改为"默认 RSA 2048 兼容所有客户端；切 ECDSA 会破坏现有 mobileconfig" |

**Android 11+ 原生客户端最终方案（重要）**：

**iOS 和 Android 走同一个 server 端 `ikev2-rw` connection 段**（EAP-MSCHAPv2 + 证书）—— 不需要单独的 PSK 段、不需要 strongSwan app、不需要任何额外代码。

AOSP 11+ 的 `android.net.Ikev2VpnProfile.Builder.setAuthUsernamePassword(user, pass, serverRootCa)` 官方 Javadoc 明文：
> "Setting this will configure IKEv2 authentication using EAP-MSCHAPv2."

设置里那个 `IKEv2/IPSec MSCHAPv2` 选项（默认第一个）就是这个 API 消费的，直接接受用户名密码+证书，跟 iOS mobileconfig 一致。

**Android 用户使用流程**（系统原生，不装任何 app）：
1. 管理员在面板 → 用户详情 → 点 "Android 11+ 原生（无需第三方 app）" → 看到 `服务器地址 / IPSec 标识符 / 用户名 / 密码` + CA 证书下载链接
2. Android 端：设置 → 网络和互联网 → VPN → 右上角 `+` → 类型选 `IKEv2/IPsec MSCHAPv2`（默认第一个）→ 填 4 个字段 → 保存 → 点连接
3. **iOS 用户不受影响**（同一段 `ikev2-rw` EAP）

**v2-79.2 净代码变化**：删除约 280 行（初版 PSK 段），新增约 15 行（F1+F2 修复），整体代码量比 v2-79.1 少。

**v2-79.2 用户体验**：

```bash
# 改之前的 .env（v2-78）：
IKEV2_SERVER_ADDR_V6=2408:832e:8a5:1000:be24:11ff:fefb:1bb  # 必填
IKEV2_OUT_IF=ens18                                          # 必填
# + 换环境必须改 .env + 重 build

# v2-79 的 .env（推荐）：
IKEV2_SERVER_CN=ikev2.rewind2023.cn   # 唯一必填
IKEV2_CERT_MODE=letsencrypt
IKEV2_DOMAIN=ikev2.rewind2023.cn
Ali_Key=...
Ali_Secret=...
# 其他全部留空 → entrypoint.sh §0 自动探测
```

---

## 快速开始

### 本地开发（无 strongSwan，无 docker）

```bash
# 1. 编译（dev 模式：自动写到 dataDir/，VICI socket 不存在时静默跳过）
go build -o bin/ikev2-panel ./cmd/ikev2-panel

# 2. 跑
export IKEV2_DATA_DIR=$HOME/.ikev2-panel-dev
export IKEV2_LISTEN_ADDR=127.0.0.1:8443
export IKEV2_LOG_LEVEL=debug
export IKEV2_SERVER_CN=vpn.test.local
export IKEV2_COOKIE_SECURE=false
./bin/ikev2-panel
```

启动日志会打印首次生成的默认管理员密码。

**dev 模式要点**（M8 commit 25）：
- 旧：`WithSkipReload()` 检查 `swanctl` 二进制是否在 PATH
- 新：`WithSkipVici()` 检查 `charon.vici` unix socket 是否存在
- dev 模式下 socket 不存在时静默跳过所有 reload / terminate / list-sas（仅记日志）

### 容器化（生产，v2-79 零硬编码）

```bash
# 1. 构建镜像（自编译 strongSwan 6.0.1 + libipsec，约 5-10 分钟首次）
docker build --network=host -t ikev2-panel:v2-79 .

# 2. 按 .env.example 修改环境变量（v2-79：除域名 + LE 凭证外都可留空）
cp .env.example .env
$EDITOR .env
# 至少填：IKEV2_SERVER_CN（必填）+ IKEV2_DOMAIN + Ali_Key + Ali_Secret（LE 模式）

# 3. 启动（默认 host 网络，见下方部署兼容性矩阵）
docker compose up -d

# 4. 看日志（重点关注 [auto-detect] 行）
docker compose logs -f | grep -E '(auto-detect|FATAL)'
```

预期启动日志：

```
[entrypoint] [auto-detect] Outbound interface: ens18
[entrypoint] [auto-detect] Public IPv6: 2408:832e:8a5:1000:be24:11ff:fefb:1bb
[entrypoint] [auto-detect] IPv4 gateway: 192.168.50.1
[entrypoint] [auto-detect] LAN subnets: 192.168.50.0/24
[entrypoint] [auto-detect] VPN subnet: 10.10.0.0/24 (LAN: 192.168.50.0/24)
[entrypoint] Using IPv6 address: 2408:832e:8a5:1000:be24:11ff:fefb:1bb
[entrypoint] IP forwarding OK (v4=1, v6=1)
...
[entrypoint] table 220 default route added via 192.168.50.1 dev ens18
[entrypoint] FORWARD -i ipsec0 ACCEPT added
[entrypoint] FORWARD -o ipsec0 ACCEPT added
[entrypoint] MASQUERADE 10.10.0.0/24 -> ens18 added
[entrypoint] local_addrs = 2408:...:1bb (auto-detected v6)
[entrypoint] IPv6 MASQUERADE applied on ens18
[entrypoint] Replacing placeholders in /etc/swanctl/swanctl.conf...
...
[entrypoint] charon ready (took 1.5s)
[entrypoint] Loading swanctl connections...
```

---

## ⚠️ 仅限自用 / 内部团队

v2 设计上**不适用于对外提供服务**：

- 用户 VPN 密码明文存库（妥协项，理由见 design §4.3）
- 单一管理员账号，无 RBAC / 审计日志
- 无 2FA / 防 DDoS / IP 白名单

如需对外提供服务，请加 reverse proxy + fail2ban，或等待 v3。

---

## 部署兼容性矩阵（M8）

### 1. 操作系统（宿主机）

| OS | 版本 | 备注 |
|---|---|---|
| Debian | 12+ (bookworm) | ✅ 推荐：与 Dockerfile 同源，最大兼容 |
| Ubuntu | 22.04+ (jammy) | ✅ 测试通过 |
| 其他 glibc | ≥ 2.36 | ⚠️ 理论支持，strongSwan 运行时库需对应版本 |

> 宿主机 OS **不影响** 容器内的 strongSwan 6.0.1（容器自带 runtime）。

### 2. 内核要求

| 项 | 要求 | 原因 |
|---|---|---|
| `/dev/net/tun` 设备 | 必须可用 | `kernel-libipsec` 通过 TUN 设备注入解密后流量 |
| `CONFIG_TUN` | 内核编译时启用 | 几乎所有主流发行版默认启用 |
| 内核 XFRM 模块 | **不需要** | libipsec 走用户态，不依赖 XFRM（这是 M8 关键改进） |
| IPv6 NAT (`CONFIG_IP6_NF_NAT`) | IPv6 MASQUERADE 需要 | Debian/Ubuntu 默认编入；OpenVZ 7 可能缺失 |
| IP forwarding | `sysctl net.ipv4.ip_forward=1` + `net.ipv6.conf.all.forwarding=1` | **三层兜底**：① `docker-compose.yml sysctls:` 让 docker daemon 自动开 ② `entrypoint.sh §3 enable_forwarding()` 自愈（host 模式 rw 时直接 echo 1） ③ 仍失败才 FATAL 提示手动 `sudo sysctl -w` |

### 3. 网络模式（关键决策）

**M8 commit 24 后的最终结论**：**默认 `network_mode: host`**。

| 网络模式 | 公网 IPv6 可达 | ESP 兼容 | 端口冲突风险 | 推荐度 |
|---|---|---|---|---|
| **`network_mode: host`**（默认） | ✅ 直接共享宿主网卡 | ✅ libipsec 走 UDP/4500 | ⚠️ 与宿主机其他服务抢 UDP 500/4500 | ⭐⭐⭐ |
| bridge + `ipvlan`（L2） | ✅ 子网内可路由 | ✅ libipsec | ⚠️ 需手工建 ipvlan 网络 | ⭐⭐ |
| bridge（docker 默认） | ❌ **不可达** | ✅ libipsec | ✅ 无冲突 | ❌ **禁用**：docker 给容器分配 fd00::/8 ULA |
| bridge + macvlan | ⚠️ 不支持多 MAC | ✅ libipsec | — | — |

**为什么 docker 默认 bridge 不可用**（实机验证 192.168.50.176）：
```
$ docker exec ikev2-panel ip -6 addr show eth0
inet6 fd00:d0c::242:ac11:2/64 scope global nodad  ← ULA，公网不可达！
```

Docker bridge 默认从 `fd00::/8` ULA 段分配 IPv6，**即使是 global scope 也无法在公网路由**。即便配置 `daemon.json` 开启 IPv6 + `ip6tables`，仍然只会得到 ULA 段。唯一解法是 host 网络或 ipvlan。

**ipvlan 启用方法**（如需网络隔离）：
```bash
# 1. 宿主上建 ipvlan 网络（替换 ens18 和子网为你自己的）
docker network create -d ipvlan \
  --subnet=2408:832e:8a5:1000::/64 \
  --gateway=2408:832e:8a5:1000::1 \
  -o ipvlan_mode=l2 \
  -o parent=ens18 \
  ikev2-ipvlan

# 2. 取消 docker-compose.yml 顶部 network_mode: host 注释，并启用 networks 块
#    （ipv4_address / ipv6_address 替换成你的地址）
```

### 4. 容器 capabilities

| Capability | host 模式 | bridge+ipvlan | 用途 |
|---|---|---|---|
| `NET_ADMIN` | ✅ 必需 | ✅ 必需 | iptables MASQUERADE、ip link 探测、ip route |
| `NET_BIND_SERVICE` | ✅ 必需 | ✅ 必需 | 监听 privileged 端口 500/4500 |
| `SYS_ADMIN` | ✅ 必需 | ❌ 不需要 | host 模式下 tc 限速、ip rule 路由 |
| `NET_RAW` | ❌ 不需要 | ❌ 不需要 | libipsec 走 TUN，不需要 raw socket |
| `cap_drop ALL` | ✅ 强烈推荐 | ✅ 强烈推荐 | 最小权限原则 |

### 5. 设备

| 设备 | 必需 | 原因 |
|---|---|---|
| `/dev/net/tun` | ✅ | `kernel-libipsec` 注入解密后流量到 TUN，协议栈再走正常路由表 |

### 6. 端口与防火墙

| 协议 | 端口 | 用途 | 防火墙建议 |
|---|---|---|---|
| UDP | 500 | IKEv2 协商 | 公网开放 |
| UDP | 4500 | IKEv2 NAT-T（libipsec 强制） | 公网开放 |
| IP proto 50 (ESP) | — | ❌ **不需要** | libipsec 走 UDP/4500 后无需开放 ESP |
| TCP | 8443 | 管理面板 HTTPS | **限制源 IP**（host 模式下直接暴露公网） |
| TCP | 22 (SSH) | — | 不要与 panel 8443 冲突 |

### 7. DNS

- AAAA 记录 → `IKEV2_SERVER_ADDR_V6`
- **不要配 A 记录**（IPv4 优先会导致客户端走 v4 失败）
- mobileconfig 里 `RemoteIdentifier` 用域名，不要裸用 IPv6 地址（证书 SAN 必须匹配域名）

---

## libipsec 性能边界

### 原理

[`kernel-libipsec`](https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html) 是 strongSwan 官方提供的**用户态 ESP 实现**：
- ESP 包不再走内核 XFRM，而是封装到 UDP/4500（NAT-T）由 charon 用户态解密
- 解密后通过 TUN 设备注入回协议栈，正常走路由表

### 性能特征

| 维度 | 数值 | 说明 |
|---|---|---|
| 单包开销 | +8 ~ +12 字节 | UDP/4500 + UDP header + libipsec nonce |
| 单核吞吐（openssl plugin，软算） | 200 ~ 500 Mbps | AES-GCM-128，软件实现 |
| 单核吞吐（aesni plugin + AES-NI） | 1.5 ~ 3 Gbps | 需 CPU 支持 AES-NI（几乎所有 2013+ 服务器 CPU） |
| 多核扩展 | ❌ **单核瓶颈** | libipsec 单 SA 绑定到单线程，无法利用多核 |
| 硬件 offload | ❌ **不支持** | 内核 XFRM 才支持 NIC IPsec offload |
| 包缓冲 | 全包内存缓冲 | 不适合大包/长肥管道 |
| 延迟增加 | +0.1 ~ +0.5 ms | 相比内核 XFRM 多一次用户态拷贝 |

### 推荐适用范围

| 场景 | 是否适合 libipsec | 备注 |
|---|---|---|
| 1–20 用户，每用户 ≤ 5 Mbps | ✅ **理想** | 总带宽 ≤ 100 Mbps 完全在软算能力内 |
| 20–50 用户，每用户 ≤ 5 Mbps | ✅ 可用 | 总带宽 ≤ 250 Mbps，需 CPU 支持 AES-NI |
| 50+ 用户 / 单用户 > 10 Mbps / 总 > 500 Mbps | ❌ **改用内核 XFRM** | 走宿主机 systemd 安装 + `charon.plugins.kernel-libipsec.load = no` |
| 延迟敏感（VoIP/游戏/高频交易） | ⚠️ | +0.1–0.5ms 延迟可接受；如不能接受，改内核 XFRM |
| 多核 CPU 想要扩展吞吐 | ❌ | libipsec 是单核瓶颈；改内核 XFRM |

### 性能调优建议（如确实需要接近上限）

1. **CPU 选型**：优先选支持 AES-NI 的型号（Intel Xeon E3/E5、AMD EPYC）
2. **算法选择**：IKEv2 proposal 用 `aes128gcm16-prfsha256-ecp256`（AES-GCM 单 pass 即可加解密）
3. **MTU**：UDP/4500 封装后包长 +12 字节，建议客户端 MTU 设为 1280（IPv6 默认）
4. **不要开 mobike**：libipsec + mobike 重协商频率高时会拖慢吞吐
5. **监控**：`swanctl --stats` 看 `bytes_in/out` per-SA，如果单 SA > 200Mbps 持续 5 分钟以上，建议拆分用户

### 切换到内核 XFRM（高吞吐场景）

如果你确实需要 > 500 Mbps 总带宽：

1. **宿主机装 strongSwan 6.0.1**（不用容器）
   ```bash
   apt install strongswan strongswan-swanctl libstrongswan-extra-plugins
   ```
2. **关闭 libipsec，启用 kernel-netlink**：
   ```conf
   # /etc/strongswan.d/charon/kernel-libipsec.conf
   kernel-libipsec { load = no }
   ```
3. **ESP 走原生 IP proto 50**（不需要 UDP/4500 端口）
4. **Go 进程同样可用**：Govici 走 VICI socket，协议与后端无关

---

## 模块结构

```
ikev2-panel-v2/
├── cmd/ikev2-panel/main.go          # 入口
├── internal/
│   ├── auth/                         # bcrypt + session
│   ├── cert/                         # RSA 自签证书 + mobileconfig + LE 状态
│   ├── config/                       # 环境变量
│   ├── expiry/                       # 过期检查 goroutine
│   ├── limit/                        # 限速文件 + collector goroutine
│   ├── store/                        # SQLite + CRUD
│   ├── swanctl/                      # 子配置 + VICI reload + parser + terminate
│   │   ├── writer.go                 #   用户 conf.d 文件写
│   │   ├── loader.go                 #   VICI load-all / load-creds
│   │   ├── terminate.go              #   VICI terminate-IKE / terminate-all
│   │   └── parser.go                 #   VICI list-sas streaming
│   └── web/                          # HTTP server + handlers + templates
├── web/templates/                    # html/template
├── web/static/                       # style.css
├── scripts/
│   ├── entrypoint.sh                 # IPv6 校验 + MASQUERADE + charon 启动
│   ├── ikev2-updown                  # tc 限速
│   └── renew-cert.sh                 # LE 续签（失败回退）
├── configs/swanctl-ipv6-only.conf    # 主配置（IPv6-only）
├── Dockerfile
└── docker-compose.yml
```

---

## 测试

```bash
# 跑所有测试
go test ./...

# 加 verbose
go test -v ./...
```

覆盖率（按 package）：

- `auth`：5 个测试（密码生成 + 长度限制 + 随机性 + bcrypt + session ID）
- `cert`：5 个测试（CA 生成 + Server cert + mobileconfig + UUID + 文件存在）
- `expiry`：1 个测试（swanctl mock 接口）
- `limit`：3 个测试（限速文件读写 + collector 单次 tick）
- `store`：4 个测试（用户 CRUD + 过期用户 + session CRUD + admin CRUD）
- `swanctl`：12 个测试（M8 commit 25 改造：VICI Message 结构化 + dev 模式 SkipVici）

> M8 commit 25 后 `swanctl` 测试改为手工构造 `vici.Message`，不再依赖 `swanctl --list-sas` 文本输出格式。

---

## 端到端验收（按 design §12）

启动服务后（dev 模式或容器内）：

| 步骤 | 验证 | 命令 |
|---|---|---|
| 1. docker compose up -d | 启动成功 | `docker compose logs -f` |
| 2. 看启动日志 | 默认管理员密码打印 | stdout |
| 3. 浏览器登录 | HTTPS 警告 + 登录成功 | `https://<host>:8443/login` |
| 4. 新增用户 | 数据库 + swanctl 配置文件生成 | `cat $DATA/swanctl-conf.d/<user>.conf` |
| 5. iOS 扫码 | mobileconfig 下载 + 安装 | `https://<host>:8443/users/<id>/mobileconfig` |
| 6. 真机连接 | strongSwan client 连通 | iOS/Android |
| 7. 验证 VICI | charon.vici unix socket 存在 | `docker exec ikev2-panel ls -l /var/run/charon.vici` |
| 8. 验证 ESP | UDP/4500 抓包看到 ESP-in-UDP | `tcpdump -i any -n udp port 4500` |

---

## 已知限制

- IPv6-only 时限速 tc 只能作用于 IPv4 流量（tc u32 不识别 IPv6）
- 单向上传限速（下载不限速，需在客户端侧另配）
- libipsec 不支持硬件 offload / 多核扩展（详见上文性能边界）
- dev 模式容器外不能 reload（VICI socket 不存在）— 容器内是生产路径
- mobileconfig URL 在 QR 码里用 `r.Host`，**部署时必须用域名/IP 访问**（不能用 localhost）

---

## 部署清单（容器）

### v2-79 简化版（推荐）

修改 `.env`：
- **`IKEV2_SERVER_CN`**（必填）：你的域名（mobileconfig RemoteIdentifier + 证书 CN 用）
- **`IKEV2_CERT_MODE=letsencrypt` + `IKEV2_DOMAIN` + `Ali_Key` + `Ali_Secret`**（LE 模式必填）
- 其他字段全部留空 → entrypoint.sh §0 自动探测

```env
# .env 最简配置（v2-79）
IKEV2_SERVER_CN=ikev2.your-domain.com
IKEV2_CERT_MODE=letsencrypt
IKEV2_DOMAIN=ikev2.your-domain.com
Ali_Key=xxx
Ali_Secret=yyy
# IKEV2_SERVER_ADDR_V6=    ← 留空自动探测
# IKEV2_OUT_IF=            ← 留空自动探测
# IKEV2_IPV6_ONLY=true     ← 默认就是 true
```

### v2-78 之前的版本（兼容）

如果保留 `IKEV2_SERVER_ADDR_V6` / `IKEV2_OUT_IF` 等字段，entrypoint 仍优先使用（不覆盖）。

防火墙：
- UDP 500 + 4500（IKEv2 + NAT-T）— **ESP 协议号 50 不需要**
- TCP 8443（管理面板）— 建议只允许管理员 IP

DNS：
- AAAA 记录指向 `IKEV2_SERVER_ADDR_V6`（自动探测的值；可以登录面板看"当前服务器信息"）
- **不要配 A 记录**（IPv4 优先会失败）

### 公网 IPv6 动态变化的处理（v2-79 自动）

v2-79 已经内置 `internal/swanctl/ipv6watch.go`：
- 容器内每 60 秒扫描 `/proc/net/if_inet6`
- 公网 IPv6 变化 → 自动 `sed` + `swanctl --load-all` → 客户端无感

**DNS 同步**：客户端 mobileconfig 装的是域名（`IKEV2_SERVER_CN`），iOS 重拨会重新解析 → 拿到新 IP。所以你需要：
1. **手动**：ISP 重拨后手动把域名 AAAA 改到新 IP（不优雅但零依赖）
2. **半自动**：用 aliyun/cloudflare 域名解析 API + cron 同步（v2-80 计划）
3. **完整 DDNS**：v2-79 设计文档 §17.3 留作扩展点，需要你确认 DNS provider

### 换网络环境迁移（v2-79 零硬编码）

从家里 → 公司 → VPS 切换：

```bash
# 1. 停掉旧部署
cd /path/to/old/deploy
docker compose down

# 2. 在新机器上：
git clone <repo>
cd ikev2-panel-v2
cp .env.example .env
$EDITOR .env   # 只填 IKEV2_SERVER_CN + LE 凭证

# 3. 启动（不需要重 build，因为镜像自带 strongSwan）
docker compose up -d

# 4. 验证 [auto-detect] 日志
docker compose logs -f | grep auto-detect

# 5. 拷贝旧数据卷（如果有）
docker run --rm -v ikev2-panel-v2_ikev2-data:/from -v $(pwd):/to alpine cp -a /from/. /to/
# 然后在新机器上创建 volume：docker volume create ikev2-panel-v2_ikev2-data
# 再 cp -a 回去
```

**v2-79 之前的版本必须重 build 镜像**（因为 IPv6 写死在 swanctl.conf 模板里）；v2-79 之后**镜像通用**，换环境不用重 build。

### 故障排查（v2-79 自动探测相关）

| 现象 | 原因 | 排查 |
|---|---|---|
| 启动报 `FATAL: cannot detect outbound interface` | 容器内没有任何 UP 的非 lo 接口 | 检查 docker 网络模式（不能是 `network: none`）；或手动传 `IKEV2_OUT_IF=eth0` |
| 启动报 `FATAL: no global IPv6 address found` | 宿主没公网 IPv6，或容器网络 namespace 没拿到 | 宿主 `ip -6 addr` 是否有 `2000::/3` 段；或用 host 网络模式；或传 `IKEV2_SERVER_ADDR_V6=...` 手动指定 |
| 启动报 `[auto-detect] no IPv4 gateway` | IPv6-only 主机没有 IPv4 默认路由 | 正常；如果需要 IPv4 MASQUERADE 需 IPv4 路由 |
| 启动报 `[auto-detect] VPN subnet: 10.10.0.0/24 (LAN: 192.168.50.0/24)` 但 LAN 真冲突 | 自动选段失败 fallback | 手动传 `IKEV2_VPN_SUBNET=10.99.0.0/24` |
| iPhone 拨号后 captive.apple.com 探测失败 | 没推 DNS（v2-78 已加，推送应该是 1.1.1.1/8.8.8.8） | 检查 iPhone 设置 → VPN → DNS 栏；看不到就重装 mobileconfig |
| iPhone 拨号成功但上不了网 | MASQUERADE / FORWARD 规则没加（v2-78 §3.5） | `docker exec ikev2-panel iptables -t nat -L POSTROUTING` 看是否有 MASQUERADE |
| 公网 IPv6 变了 iPhone 重拨失败 | `ipv6watch` 没启动或太慢 | `docker logs ikev2-panel \| grep ipv6watch` 看是否有扫描日志；最多 60 秒感知 |

---

## 常见问题（FAQ）

### Q1：docker 默认 bridge 模式下，容器内能看到 IPv6 地址但客户端连不上？

**A**：Docker 给容器分配的 `fd00::/8` 是 ULA（RFC 4193），公网不可达。
- 验证：`docker exec ikev2-panel ip -6 addr show eth0`，看地址是否以 `fd` / `fc` 开头
- 解法：改用 `network_mode: host`（默认）或 ipvlan，详见上文部署矩阵 §3

### Q2：charon.vici socket permission denied？

**A**：检查 `cmd/ikev2-panel` 进程的 UID 是否能访问 `/var/run/charon.vici`。
- 容器内 ikev2-panel 以 root 运行，默认 OK
- 宿主机 systemd 安装时，charon 通常以 root 运行，socket 权限 0660（root:root）
- 如果用非 root 运行 ikev2-panel，需要加用户到 `root` 组或调 socket 权限：
  ```bash
  chmod 0666 /var/run/charon.vici  # 不推荐生产
  ```
- 或在 `charon.conf` 里改 `vici { socket = unix:///var/run/charon.vici }` 加权限

### Q3：ipvlan 网络下容器拿不到 IPv6 地址？

**A**：检查 3 件事：
1. 宿主机网卡是否开启 `accept_ra=2` 和 `forwarding=1`：
   ```bash
   sysctl net.ipv6.conf.ens18.accept_ra=2
   sysctl net.ipv6.conf.all.forwarding=1
   ```
2. docker daemon 是否配置 IPv6：
   ```json
   // /etc/docker/daemon.json
   {
     "ipv6": true,
     "fixed-cidr-v6": "fd00:d0c::/64",
     "ip6tables": true,
     "experimental": true   // docker 26.x 需要
   }
   ```
   重启：`systemctl restart docker`
3. ipvlan parent 接口是否正确（必须是宿主物理网卡，不能是 bridge/vlan 子接口）

### Q4：docker daemon IPv6 报 `ip6tables rules are only available if experimental features are enabled`？

**A**：docker v26+ 把 ip6tables 移到了 experimental：
```bash
nohup dockerd --experimental > /tmp/dockerd.log 2>&1 &
```
或 daemon.json 里加 `"experimental": true`。

### Q5：strongSwan 6.0.1 编译报 `undefined reference to gmp_*`？

**A**：build stage 缺 `libgmp-dev`，在 Dockerfile 的 `apt-get install` 段加上：
```dockerfile
libgmp-dev
```

### Q6：list-sas 显示空但实际有客户端连上？

**A**：可能是 charon 启动竞态。第一次 reload `swanctl --load-all` 在 entrypoint 里做了，Go 进程的 `ListSAs()` 调用是异步的。
- 容器内日志看 `vici list-sas stream: ...` 错误
- 验证 socket：`docker exec ikev2-panel ls -l /var/run/charon.vici`
- 验证 charon 进程：`docker exec ikev2-panel ps aux | grep charon`

### Q7：dev 模式（容器外）启动后日志一直刷 "vici NewSession failed"？

**A**：dev 模式下 charon 不存在是正常的。Go 进程会用 `WithSkipVici()` 静默跳过所有 VICI 操作（仅记 warn 日志）。
- 确认 dev 模式检测：日志里搜 `skip vici` 或 `charon.vici not found`
- 如果是生产模式（容器内）报错，那就是真实问题，看上面 Q2/Q6

### Q8：libipsec 启用后 strongSwan 报 `no matching peer config found`？

**A**：99% 是 swanctl.conf 里 `local_addrs` 配置错。`%IKEV2_SERVER_ADDR_V6%` 占位符没替换成实际公网 IPv6。
- 验证：`docker exec ikev2-panel cat /etc/swanctl/swanctl.conf | grep local_addrs`
- 应该看到 `local_addrs = 2001:db8::75`（实际地址），不是 `%IKEV2_SERVER_ADDR_V6%`

### Q9：升级 strongSwan 6.0.1 → 6.1.0 后编译失败？

**A**：6.1.0 的 tar.bz2 在官方下载源经常 404。建议保持 6.0.1（已生产验证）。如必须升级：
1. 修改 `Dockerfile` 的 `STRONGSWAN_VERSION` + `STRONGSWAN_MD5`
2. MD5 从 https://download.strongswan.org/strongswan-X.Y.Z.tar.bz2.md5 获取
3. 跑 `docker build --no-cache` 重编译

### Q10：怎么从内核 XFRM 切到 libipsec（或反过来）？

**A**：都是修改 `/etc/strongswan.d/charon/` 下 plugin 配置：
```bash
# libipsec 模式（默认，容器场景）
echo 'kernel-libipsec { load = yes }' > /etc/strongswan.d/charon/kernel-libipsec.conf

# 内核 XFRM 模式（高吞吐场景）
echo 'kernel-libipsec { load = no }'  > /etc/strongswan.d/charon/kernel-libipsec.conf
echo 'kernel-netlink  { load = yes }' > /etc/strongswan.d/charon/kernel-netlink.conf
```
Dockerfile 默认两个都编译，运行时由 plugin load 标志切换。

---

## 设计/架构参考

- [`docs/design.md §1.5`](docs/design.md) — libipsec 选型详细分析
- [`docs/architecture.md §3.5`](docs/architecture.md) — VICI 协议介绍
- [`docs/architecture.md §18`](docs/architecture.md) — govici 重构详细设计
- [strongSwan kernel-libipsec 文档](https://docs.strongswan.org/docs/latest/plugins/kernel-libipsec.html)
- [strongSwan VICI 协议文档](https://docs.strongswan.org/docs/latest/plugins/vici.html)
- [govici GitHub](https://github.com/strongswan/govici) — strongSwan 官方 Go VICI 客户端
